// Package render owns the headless Chromium lifecycle and the HTML → PDF
// translation. The design follows project-specification.md §8:
//
//   - One process-lifetime ExecAllocator. Per-request tabs derive from it.
//   - A semaphore caps in-flight renders to RenderConcurrency.
//   - A supervisor goroutine probes Chrome's health and recycles it on
//     failure or after a configurable render count.
//   - HTML is loaded via Page.setDocumentContent on the root frame, not by
//     mutating document.body — that preserves <head>, doctype, stylesheets.
//
// The package depends only on chromedp and the Recorder interface, so the
// renderer can be tested in isolation.
package render

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp"
)

// chromeFlags is the canonical set from spec §8.2. Mirrored in
// deploy/chrome-flags.txt for grep-ability — keep them in sync.
var chromeFlags = []chromedp.ExecAllocatorOption{
	chromedp.NoFirstRun,
	chromedp.NoDefaultBrowserCheck,
	chromedp.NoSandbox,
	chromedp.DisableGPU,
	chromedp.Headless,
	chromedp.Flag("headless", "new"),
	chromedp.Flag("disable-dev-shm-usage", true),
	chromedp.Flag("disable-software-rasterizer", true),
	chromedp.Flag("disable-background-networking", true),
	chromedp.Flag("disable-background-timer-throttling", true),
	chromedp.Flag("disable-backgrounding-occluded-windows", true),
	chromedp.Flag("disable-breakpad", true),
	chromedp.Flag("disable-crash-reporter", true),
	chromedp.Flag("noerrdialogs", true),
	chromedp.Flag("no-crash-upload", true),
	chromedp.Flag("disable-extensions", true),
	chromedp.Flag("disable-features", "TranslateUI,IsolateOrigins,site-per-process,Crashpad"),
	chromedp.Flag("disable-ipc-flooding-protection", true),
	chromedp.Flag("disable-renderer-backgrounding", true),
	chromedp.Flag("disable-sync", true),
	chromedp.Flag("enable-features", "NetworkService"),
	chromedp.Flag("force-color-profile", "srgb"),
	chromedp.Flag("hide-scrollbars", true),
	chromedp.Flag("metrics-recording-only", true),
	chromedp.Flag("mute-audio", true),
	chromedp.Flag("password-store", "basic"),
	chromedp.Flag("use-mock-keychain", true),
	chromedp.Flag("font-render-hinting", "none"),
}

// Restart reasons used in analytics.
const (
	RestartScheduled    = "scheduled_recycle"
	RestartHealthcheck  = "healthcheck_failed"
	RestartStartFailure = "start_failure"
)

// Config controls browser lifecycle. Sourced from internal/config.Config.
type Config struct {
	ChromePath          string
	Concurrency         int
	RenderTimeout       time.Duration
	RecycleAfter        int64
	HealthCheckInterval time.Duration
}

// Browser owns the Chromium process and tab scheduling.
//
// Public method set is small on purpose:
//   - Render: synchronous HTML → PDF with the supplied options.
//   - Healthy: snapshot of the last supervisor probe.
//   - Close: cancel allocator, wait for in-flight renders.
//
// All mutation of parentCtx/allocCancel happens behind ctxMu so concurrent
// renders can keep using a stable parent context while a recycle is in flight.
type Browser struct {
	cfg      Config
	log      *slog.Logger
	recorder Recorder

	sem chan struct{}

	ctxMu        sync.RWMutex
	allocCtx     context.Context
	allocCancel  context.CancelFunc
	parentCtx    context.Context
	parentCancel context.CancelFunc

	renderCount atomic.Int64
	recycleAt   atomic.Int64
	healthy     atomic.Bool
	recycling   atomic.Bool
	closed      atomic.Bool

	supervisorStop context.CancelFunc
	supervisorWG   sync.WaitGroup
}

// New launches Chromium with the configured flags and starts the supervisor.
// Returns an error if the initial launch fails — caller should refuse to
// start serving traffic.
func New(cfg Config, log *slog.Logger, rec Recorder) (*Browser, error) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if rec == nil {
		rec = NoopRecorder{}
	}
	b := &Browser{
		cfg:      cfg,
		log:      log,
		recorder: rec,
		sem:      make(chan struct{}, cfg.Concurrency),
	}
	b.recycleAt.Store(cfg.RecycleAfter)

	if err := b.spawn(); err != nil {
		return nil, fmt.Errorf("initial chromium spawn: %w", err)
	}

	supCtx, supCancel := context.WithCancel(context.Background())
	b.supervisorStop = supCancel
	b.supervisorWG.Add(1)
	go b.supervisor(supCtx)
	return b, nil
}

// spawn builds a fresh allocator + parent browser context and probes Chrome
// with a trivial Evaluate to confirm it actually launched. The parent context
// lives for the allocator's lifetime; per-request tabs derive from it.
//
// Caller must hold ctxMu in write mode (or be the constructor before any other
// goroutine sees b).
func (b *Browser) spawn() error {
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromeFlags...)
	if b.cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(b.cfg.ChromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	logf := func(format string, args ...any) {
		b.log.Debug("chromedp", "msg", fmt.Sprintf(format, args...))
	}
	errf := func(format string, args ...any) {
		b.log.Warn("chromedp_error", "msg", fmt.Sprintf(format, args...))
	}
	parentCtx, parentCancel := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(logf),
		chromedp.WithErrorf(errf),
	)

	// The first chromedp.Run on parentCtx is what launches the browser.
	// Critically, we must NOT wrap parentCtx with a timeout for this call —
	// chromedp binds the browser process lifetime to the context that first
	// runs against it, so a deferred timeout-cancel here would kill the
	// browser the moment spawn returns. Bound the launch via select instead.
	launchErr := make(chan error, 1)
	go func() {
		launchErr <- chromedp.Run(parentCtx, chromedp.Navigate("about:blank"))
	}()
	select {
	case err := <-launchErr:
		if err != nil {
			parentCancel()
			allocCancel()
			return fmt.Errorf("browser launch: %w", err)
		}
	case <-time.After(20 * time.Second):
		parentCancel()
		allocCancel()
		return errors.New("browser launch timed out after 20s")
	}

	b.allocCtx, b.allocCancel = allocCtx, allocCancel
	b.parentCtx, b.parentCancel = parentCtx, parentCancel
	b.healthy.Store(true)
	b.renderCount.Store(0)
	return nil
}

// Render converts html to a PDF using opts. It is safe to call concurrently;
// up to cfg.Concurrency renders run in parallel.
func (b *Browser) Render(ctx context.Context, html string, opts Options) ([]byte, error) {
	if b.closed.Load() {
		return nil, errors.New("browser closed")
	}
	if !b.healthy.Load() {
		return nil, errors.New("browser unhealthy")
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	b.recorder.IncInFlight()
	defer b.recorder.DecInFlight()

	// Bound concurrency. Honor caller cancellation.
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.sem }()

	b.ctxMu.RLock()
	parent := b.parentCtx
	b.ctxMu.RUnlock()

	if err := parent.Err(); err != nil {
		return nil, fmt.Errorf("browser ctx not live: %w", err)
	}

	tabCtx, cancelTab := chromedp.NewContext(parent)
	defer cancelTab()
	tabCtx, cancelTimeout := context.WithTimeout(tabCtx, opts.Timeout)
	defer cancelTimeout()

	pdf, err := runRender(tabCtx, html, opts)

	// Trigger async recycle if we've hit the threshold.
	if b.renderCount.Add(1) >= b.recycleAt.Load() {
		if b.recycling.CompareAndSwap(false, true) {
			go b.recycle(RestartScheduled)
		}
	}
	return pdf, err
}

// Healthy reports the last supervisor verdict. Used by /readyz.
func (b *Browser) Healthy() bool { return b.healthy.Load() && !b.closed.Load() }

// Close stops the supervisor, cancels Chrome, and waits for clean exit.
// Safe to call multiple times.
func (b *Browser) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	if b.supervisorStop != nil {
		b.supervisorStop()
	}
	b.supervisorWG.Wait()

	b.ctxMu.Lock()
	if b.parentCancel != nil {
		b.parentCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
	b.ctxMu.Unlock()
	return nil
}

// supervisor probes the browser at HealthCheckInterval and triggers recycle
// on failure. Exits when ctx is canceled (via Close).
func (b *Browser) supervisor(ctx context.Context) {
	defer b.supervisorWG.Done()
	interval := b.cfg.HealthCheckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.probe()
		}
	}
}

// probe checks the browser is responsive by opening and closing a throwaway
// tab. Evaluate against the bare parent context can hit "execution context
// destroyed" if the parent's initial about:blank tab has been GC'd — a fresh
// tab is the more reliable check.
func (b *Browser) probe() {
	b.ctxMu.RLock()
	parent := b.parentCtx
	b.ctxMu.RUnlock()
	if parent == nil {
		return
	}
	tabCtx, cancelTab := chromedp.NewContext(parent)
	defer cancelTab()
	probeCtx, cancel := context.WithTimeout(tabCtx, 5*time.Second)
	defer cancel()
	var ok int
	if err := chromedp.Run(probeCtx,
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(`1`, &ok),
	); err != nil {
		b.log.Warn("browser_probe_failed", "err", err)
		b.healthy.Store(false)
		if b.recycling.CompareAndSwap(false, true) {
			go b.recycle(RestartHealthcheck)
		}
		return
	}
	if ok != 1 {
		b.log.Warn("browser_probe_unexpected", "got", ok)
	}
	b.healthy.Store(true)
}

// recycle cancels the current allocator and brings up a fresh one. While the
// new browser is being warmed, renders see healthy=false and bail out.
// The recycling flag prevents overlapping recycles.
func (b *Browser) recycle(reason string) {
	defer b.recycling.Store(false)
	if b.closed.Load() {
		return
	}
	b.log.Info("browser_recycle_begin", "reason", reason)
	b.healthy.Store(false)

	b.ctxMu.Lock()
	if b.parentCancel != nil {
		b.parentCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
	err := b.spawn()
	b.ctxMu.Unlock()

	if err != nil {
		b.log.Error("browser_recycle_failed", "reason", reason, "err", err)
		b.recorder.BrowserRestarted(reason+":failed", time.Now())
		return
	}
	b.recorder.BrowserRestarted(reason, time.Now())
	b.log.Info("browser_recycle_complete", "reason", reason)
}
