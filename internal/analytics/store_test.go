package analytics

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStore_PersistAndReplay(t *testing.T) {
	dir := t.TempDir()
	r1 := New("test")
	defer r1.Stop()

	r1.RecordRequest("k1", 1024)
	r1.RecordSuccess("k1", 250, 5000)
	r1.RecordSuccess("k1", 800, 12000)
	r1.RecordFailure("k1", ReasonTimeout, 30000)
	r1.BrowserRestarted(RestartHealthcheckSentinel(), time.Now())

	store1 := NewStore(dir, 30, time.Hour, r1, newTestLogger())
	if err := store1.Persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("state.json missing: %v", err)
	}

	// Fresh recorder + replay.
	r2 := New("test")
	defer r2.Stop()
	store2 := NewStore(dir, 30, time.Hour, r2, newTestLogger())
	if err := store2.Replay(); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if got := r2.totalRequests.Load(); got != 1 {
		t.Errorf("requests after replay: got %d want 1", got)
	}
	if got := r2.totalPDFs.Load(); got != 2 {
		t.Errorf("pdfs after replay: got %d want 2", got)
	}
	if got := r2.totalFailures.Load(); got != 1 {
		t.Errorf("failures after replay: got %d want 1", got)
	}
	if got := r2.maxLatencyMs.Load(); got != 30000 {
		t.Errorf("max latency after replay: got %d want 30000", got)
	}
	if got := r2.browserRestarts.Load(); got != 1 {
		t.Errorf("browser restarts: got %d want 1", got)
	}
}

func TestStore_ReplayMissing(t *testing.T) {
	r := New("test")
	defer r.Stop()
	store := NewStore(t.TempDir(), 30, time.Hour, r, newTestLogger())
	if err := store.Replay(); err != nil {
		t.Errorf("replay of missing state: %v", err)
	}
}

func TestStore_ReplayCorruptArchivesAndContinues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New("test")
	defer r.Stop()
	store := NewStore(dir, 30, time.Hour, r, newTestLogger())
	if err := store.Replay(); err != nil {
		t.Errorf("replay should not error on corrupt state: %v", err)
	}
	// The broken file should have been archived alongside.
	entries, _ := os.ReadDir(dir)
	var hasBroken bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "state.json.broken.") {
			hasBroken = true
		}
	}
	if !hasBroken {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected state.json.broken.<ts> archive, dir contents: %v", names)
	}
}

func TestStore_ReplayUnknownSchemaFutureRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"),
		[]byte(`{"schema_version":999,"saved_at":"2026-01-01T00:00:00Z","totals":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New("test")
	defer r.Stop()
	store := NewStore(dir, 30, time.Hour, r, newTestLogger())
	if err := store.Replay(); err == nil {
		t.Error("expected error refusing to start on unknown future schema")
	}
}

func TestStore_TickerPersists(t *testing.T) {
	dir := t.TempDir()
	r := New("test")
	defer r.Stop()
	r.RecordRequest("k", 0)
	r.RecordSuccess("k", 100, 1000)

	store := NewStore(dir, 30, 50*time.Millisecond, r, newTestLogger())
	ctx, cancel := context.WithCancel(context.Background())
	store.Start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()
	store.Stop()

	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Errorf("state.json should exist after ticker: %v", err)
	}
}

// RestartHealthcheckSentinel exists only so the test file doesn't need to
// import internal/render just for the reason string.
func RestartHealthcheckSentinel() string { return "healthcheck_failed" }
