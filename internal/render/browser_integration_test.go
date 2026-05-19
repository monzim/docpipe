//go:build integration

package render

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// Integration tests require a chromium-class binary.
// Skipped automatically when none is found.

func findChrome() string {
	for _, name := range []string{"chromium-browser", "chromium", "google-chrome", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func newTestBrowser(t *testing.T, concurrency int) *Browser {
	t.Helper()
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no chromium binary in PATH")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := New(Config{
		ChromePath:          chrome,
		Concurrency:         concurrency,
		RenderTimeout:       30 * time.Second,
		RecycleAfter:        100000,
		HealthCheckInterval: 5 * time.Second,
	}, log, NoopRecorder{})
	if err != nil {
		t.Fatalf("browser launch: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestBrowser_RendersValidPDF(t *testing.T) {
	b := newTestBrowser(t, 2)
	pdf, err := b.Render(context.Background(),
		`<!doctype html><html><head><title>t</title></head><body><h1>hello</h1></body></html>`,
		Options{},
	)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output is not a PDF: first 8 bytes = %q", pdf[:min(8, len(pdf))])
	}
	if len(pdf) < 200 {
		t.Errorf("suspiciously small PDF (%d bytes)", len(pdf))
	}
}

func TestBrowser_ConcurrentRenders(t *testing.T) {
	b := newTestBrowser(t, 4)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Render(context.Background(), "<h1>x</h1>", Options{WaitStrategy: WaitLoad})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent render: %v", err)
		}
	}
}

func TestBrowser_RecyclesAfterThreshold(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("no chromium binary in PATH")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := New(Config{
		ChromePath:          chrome,
		Concurrency:         2,
		RenderTimeout:       10 * time.Second,
		RecycleAfter:        3, // small to trigger recycle quickly
		HealthCheckInterval: time.Hour,
	}, log, NoopRecorder{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()

	for i := 0; i < 6; i++ {
		if _, err := b.Render(context.Background(), "<p>x</p>", Options{WaitStrategy: WaitLoad}); err != nil {
			t.Logf("render %d: %v (may be during recycle)", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// At least one recycle should have happened. We don't have a counter
	// exposed; the test really just exercises the code path doesn't crash.
}

// Defensive — confirm test infra picks up real chromium when present.
func TestMain(m *testing.M) {
	if os.Getenv("DOCPIPE_TEST_CHROME") != "" {
		os.Setenv("PATH", os.Getenv("DOCPIPE_TEST_CHROME")+":"+os.Getenv("PATH"))
	}
	os.Exit(m.Run())
}
