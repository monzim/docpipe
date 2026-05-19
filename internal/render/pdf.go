// HTML → PDF action.
//
// Order matters: any event-based wait strategy must install its listener
// BEFORE the action that fires the event. setDocumentContent fires
// Page.loadEventFired synchronously inside the call, so the listener can't
// be attached after.
package render

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// runRender executes the actions sequence and returns the PDF bytes.
//
// Flow:
//  1. Navigate to about:blank to get a frame ID.
//  2. Install the wait-strategy listener (BEFORE loading HTML).
//  3. Load the supplied HTML via Page.setDocumentContent.
//  4. Wait for the listener to signal "ready".
//  5. Print PDF.
func runRender(ctx context.Context, html string, opts Options) ([]byte, error) {
	var pdf []byte
	err := chromedp.Run(ctx,
		chromedp.Navigate("about:blank"),
		renderWithWait(html, opts, &pdf),
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("render timeout: %w", err)
		}
		return nil, fmt.Errorf("render failed: %w", err)
	}
	return pdf, nil
}

// renderWithWait sets up the wait-strategy listener, loads HTML, waits, then
// prints. Bundling these as one ActionFunc keeps the listener-then-trigger
// ordering correct.
func renderWithWait(html string, opts Options, out *[]byte) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		ready, cleanup := startWait(ctx, opts)
		defer cleanup()

		if err := loadHTML(ctx, html); err != nil {
			return err
		}

		select {
		case err := <-ready:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}

		return printPDF(ctx, opts, out)
	})
}

// startWait installs the listener for the configured WaitStrategy and returns
// a channel that fires nil on success / err on timeout. cleanup detaches.
func startWait(ctx context.Context, opts Options) (<-chan error, func()) {
	ready := make(chan error, 1)
	switch opts.WaitStrategy {
	case WaitNone:
		// Fire immediately — caller selects from the channel after loadHTML.
		ready <- nil
		return ready, func() {}

	case WaitSelector:
		go func() {
			waitCtx, cancel := context.WithTimeout(ctx, opts.WaitTimeout)
			defer cancel()
			ready <- chromedp.WaitVisible(opts.WaitSelector, chromedp.ByQuery).Do(waitCtx)
		}()
		return ready, func() {}

	case WaitLoad:
		stop := installLoadListener(ctx, opts.WaitTimeout, ready)
		return ready, stop

	case WaitNetworkIdle:
		// Two-phase: load first, then 500ms of network quiet.
		loaded := make(chan error, 1)
		stopLoad := installLoadListener(ctx, opts.WaitTimeout, loaded)
		stopNet := installNetworkIdleListener(ctx, opts.WaitTimeout, 500*time.Millisecond, loaded, ready)
		return ready, func() {
			stopLoad()
			stopNet()
		}

	default:
		ready <- fmt.Errorf("unsupported wait strategy %q", opts.WaitStrategy)
		return ready, func() {}
	}
}

func loadHTML(ctx context.Context, html string) error {
	tree, err := page.GetFrameTree().Do(ctx)
	if err != nil {
		return fmt.Errorf("get frame tree: %w", err)
	}
	if tree == nil || tree.Frame == nil {
		return errors.New("frame tree empty")
	}
	return page.SetDocumentContent(tree.Frame.ID, html).Do(ctx)
}

// installLoadListener resolves the returned channel when Page.loadEventFired
// arrives. The listener uses ctx for detachment; returned func() is a no-op
// since chromedp.ListenTarget auto-detaches on ctx done.
func installLoadListener(ctx context.Context, timeout time.Duration, out chan<- error) func() {
	var once sync.Once
	chromedp.ListenTarget(ctx, func(ev any) {
		if _, ok := ev.(*page.EventLoadEventFired); ok {
			once.Do(func() { out <- nil })
		}
	})
	stopCh := make(chan struct{})
	go func() {
		select {
		case <-time.After(timeout):
			once.Do(func() { out <- fmt.Errorf("page load timeout after %v", timeout) })
		case <-ctx.Done():
			once.Do(func() { out <- ctx.Err() })
		case <-stopCh:
		}
	}()
	return func() { close(stopCh) }
}

// installNetworkIdleListener counts in-flight requests via network events.
// When the loaded chan signals success and there's been `quiet` time without
// any in-flight requests, it fires nil on out. Errors propagate.
func installNetworkIdleListener(ctx context.Context, timeout, quiet time.Duration, loaded <-chan error, out chan<- error) func() {
	var inFlight atomic.Int64
	var idleSince atomic.Int64
	idleSince.Store(time.Now().UnixNano())
	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev.(type) {
		case *network.EventRequestWillBeSent:
			inFlight.Add(1)
			idleSince.Store(time.Now().UnixNano())
		case *network.EventLoadingFinished, *network.EventLoadingFailed:
			inFlight.Add(-1)
			idleSince.Store(time.Now().UnixNano())
		}
	})

	done := make(chan struct{})
	go func() {
		// Block until load fires or fails.
		if err := <-loaded; err != nil {
			out <- err
			return
		}
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				out <- ctx.Err()
				return
			case <-deadline.C:
				// Don't fail — the page rendered, it's just chatty. Proceed.
				out <- nil
				return
			case <-done:
				return
			case <-tick.C:
				if inFlight.Load() > 0 {
					continue
				}
				elapsed := time.Since(time.Unix(0, idleSince.Load()))
				if elapsed >= quiet {
					out <- nil
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// printPDF emits the PDF bytes into *out.
func printPDF(ctx context.Context, opts Options, out *[]byte) error {
	w, h := opts.PaperDimensions()
	req := page.PrintToPDF().
		WithPrintBackground(opts.PrintBackground).
		WithLandscape(opts.Landscape).
		WithScale(opts.Scale).
		WithPaperWidth(w).
		WithPaperHeight(h).
		WithMarginTop(opts.Margin.Top).
		WithMarginRight(opts.Margin.Right).
		WithMarginBottom(opts.Margin.Bottom).
		WithMarginLeft(opts.Margin.Left).
		WithDisplayHeaderFooter(opts.DisplayHeaderFooter).
		WithHeaderTemplate(opts.HeaderTemplate).
		WithFooterTemplate(opts.FooterTemplate).
		WithPreferCSSPageSize(opts.PreferCSSPageSize)
	if opts.PageRanges != "" {
		req = req.WithPageRanges(opts.PageRanges)
	}
	buf, _, err := req.Do(ctx)
	if err != nil {
		return err
	}
	*out = buf
	return nil
}
