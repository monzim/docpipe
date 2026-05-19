package analytics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Failure reason taxonomy. Stable across versions — adding new reasons is
// safe, renaming requires snapshot migration.
const (
	ReasonTimeout        = "timeout"
	ReasonChromeCrash    = "chrome_crash"
	ReasonInvalidHTML    = "invalid_html"
	ReasonInvalidRequest = "invalid_request"
	ReasonRenderFailed   = "render_failed"
	ReasonInternal       = "internal_error"
)

// Recorder is the central in-memory observability struct.
//
// All fields are designed for lock-free or near-lock-free writes from the
// HTTP / render hot paths. Reads (Snapshot, Public) take a brief consistent-ish
// view; they don't block writers.
type Recorder struct {
	startedAt       time.Time
	startedFirstRun time.Time // earliest start across all process lives
	version         string

	totalRequests atomic.Int64
	totalPDFs     atomic.Int64
	totalFailures atomic.Int64
	bytesIn       atomic.Int64
	bytesOut      atomic.Int64

	failuresByReason sync.Map // string -> *atomic.Int64

	inFlight     atomic.Int64
	peakInFlight atomic.Int64

	maxLatencyMs atomic.Int64

	latency *Histogram
	pdfSize *Histogram

	rps     *SlidingWindow
	rolling *RollingWindow

	browserRestarts atomic.Int64
	lastRestart     atomic.Pointer[RestartEvent]

	// Per-key counters — kept for diagnostics, never exposed publicly.
	byKey sync.Map // string -> *KeyStats

	lastSnapshotAt atomic.Pointer[time.Time]
}

// RestartEvent records the most recent browser recycle.
type RestartEvent struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// KeyStats holds per-key counters. Exposed only for diagnostics/logs.
type KeyStats struct {
	Name     string
	Requests atomic.Int64
	PDFs     atomic.Int64
	Failures atomic.Int64
}

// New returns a fresh Recorder. version is exposed in /v1/stats.
func New(version string) *Recorder {
	now := time.Now()
	r := &Recorder{
		startedAt:       now,
		startedFirstRun: now,
		version:         version,
		latency:         NewHistogram(DefaultLatencyBuckets),
		pdfSize:         NewHistogram(DefaultPDFSizeBuckets),
		rps:             NewSlidingWindow(),
		rolling:         NewRollingWindow(),
	}
	return r
}

// Stop releases background goroutines (the RPS ticker).
func (r *Recorder) Stop() { r.rps.Stop() }

// IncInFlight is called when a render begins; DecInFlight when it ends.
// Implements render.Recorder.
func (r *Recorder) IncInFlight() {
	cur := r.inFlight.Add(1)
	for {
		peak := r.peakInFlight.Load()
		if cur <= peak || r.peakInFlight.CompareAndSwap(peak, cur) {
			break
		}
	}
}

// DecInFlight balances IncInFlight.
func (r *Recorder) DecInFlight() { r.inFlight.Add(-1) }

// BrowserRestarted records a renderer recycle event.
// Implements render.Recorder.
func (r *Recorder) BrowserRestarted(reason string, at time.Time) {
	r.browserRestarts.Add(1)
	r.lastRestart.Store(&RestartEvent{At: at, Reason: reason})
}

// RecordRequest is called at the START of every billed request. It bumps the
// request count and the RPS window.
func (r *Recorder) RecordRequest(keyName string, bytesIn int64) {
	r.totalRequests.Add(1)
	r.rps.Inc()
	if bytesIn > 0 {
		r.bytesIn.Add(bytesIn)
	}
	if keyName != "" {
		r.keyStats(keyName).Requests.Add(1)
	}
}

// RecordSuccess is called when a request renders a PDF successfully.
func (r *Recorder) RecordSuccess(keyName string, latencyMs, bytesOut int64) {
	r.totalPDFs.Add(1)
	if bytesOut > 0 {
		r.bytesOut.Add(bytesOut)
		r.pdfSize.Observe(bytesOut)
	}
	r.latency.Observe(latencyMs)
	for {
		cur := r.maxLatencyMs.Load()
		if latencyMs <= cur || r.maxLatencyMs.CompareAndSwap(cur, latencyMs) {
			break
		}
	}
	r.rolling.Record(time.Now(), 1, 1, 0, latencyMs, 0, bytesOut)
	if keyName != "" {
		r.keyStats(keyName).PDFs.Add(1)
	}
}

// RecordFailure is called when a request fails after passing auth + rate limit.
// `reason` should be one of the Reason* constants.
func (r *Recorder) RecordFailure(keyName, reason string, latencyMs int64) {
	r.totalFailures.Add(1)
	r.bumpReason(reason)
	r.latency.Observe(latencyMs)
	for {
		cur := r.maxLatencyMs.Load()
		if latencyMs <= cur || r.maxLatencyMs.CompareAndSwap(cur, latencyMs) {
			break
		}
	}
	r.rolling.Record(time.Now(), 1, 0, 1, latencyMs, 0, 0)
	if keyName != "" {
		r.keyStats(keyName).Failures.Add(1)
	}
}

// SetLastSnapshotAt is called by the disk store after each persist.
func (r *Recorder) SetLastSnapshotAt(t time.Time) {
	cp := t
	r.lastSnapshotAt.Store(&cp)
}

// LastSnapshotAt returns the time of the most recent successful disk persist,
// or zero if no snapshot has been written this process lifetime. Used by the
// public stats view to compute `snapshot.next_persist_at`.
func (r *Recorder) LastSnapshotAt() (time.Time, bool) {
	p := r.lastSnapshotAt.Load()
	if p == nil {
		return time.Time{}, false
	}
	return *p, true
}

// StartedAt returns the wall-clock time this process began.
func (r *Recorder) StartedAt() time.Time { return r.startedAt }

// StartedFirstRun returns the earliest startup time across process lives.
func (r *Recorder) StartedFirstRun() time.Time { return r.startedFirstRun }

// Totals returns the lifetime counters.
func (r *Recorder) Totals() TotalsSnap {
	return TotalsSnap{
		Requests: r.totalRequests.Load(),
		PDFs:     r.totalPDFs.Load(),
		Failures: r.totalFailures.Load(),
		BytesIn:  r.bytesIn.Load(),
		BytesOut: r.bytesOut.Load(),
	}
}

// FailuresByReason returns a snapshot of the failure breakdown.
func (r *Recorder) FailuresByReason() map[string]int64 { return r.failureMap() }

// InFlight returns the current in-flight and lifetime peak.
func (r *Recorder) InFlight() (current, peak int64) {
	return r.inFlight.Load(), r.peakInFlight.Load()
}

// MaxLatencyMs returns the highest observed render latency.
func (r *Recorder) MaxLatencyMs() int64 { return r.maxLatencyMs.Load() }

// BrowserRestarts returns the lifetime restart count.
func (r *Recorder) BrowserRestarts() int64 { return r.browserRestarts.Load() }

// Version returns the build version this recorder was created with.
func (r *Recorder) Version() string { return r.version }

func (r *Recorder) bumpReason(reason string) {
	if reason == "" {
		return
	}
	v, _ := r.failuresByReason.LoadOrStore(reason, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func (r *Recorder) keyStats(name string) *KeyStats {
	if v, ok := r.byKey.Load(name); ok {
		return v.(*KeyStats)
	}
	fresh := &KeyStats{Name: name}
	actual, _ := r.byKey.LoadOrStore(name, fresh)
	return actual.(*KeyStats)
}

// failureMap returns a copy of failures-by-reason. Caller may mutate.
func (r *Recorder) failureMap() map[string]int64 {
	out := make(map[string]int64)
	r.failuresByReason.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// LastRestart returns a copy of the most recent restart event, or nil.
func (r *Recorder) LastRestart() *RestartEvent {
	return r.lastRestart.Load()
}

// SetStartedFirstRun is used by the store on replay so reported
// "first started" survives across restarts.
func (r *Recorder) SetStartedFirstRun(t time.Time) {
	if !t.IsZero() {
		r.startedFirstRun = t
	}
}

// Histograms exposes the underlying histograms for the public view.
func (r *Recorder) Histograms() (latency, pdfSize *Histogram) { return r.latency, r.pdfSize }

// RPS exposes the sliding window.
func (r *Recorder) RPS() *SlidingWindow { return r.rps }

// Rolling exposes the rolling-window aggregator.
func (r *Recorder) Rolling() *RollingWindow { return r.rolling }
