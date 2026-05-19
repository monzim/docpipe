package analytics

import (
	"sync"
	"sync/atomic"
	"time"
)

// rollingSlot holds aggregated counters for a single minute.
type rollingSlot struct {
	requests     atomic.Int64
	pdfs         atomic.Int64
	failures     atomic.Int64
	latencySum   atomic.Int64
	latencyCount atomic.Int64
	bytesIn      atomic.Int64
	bytesOut     atomic.Int64
	// stamp is the unix-minute at which this slot was last written. Used to
	// invalidate stale slots when a process pause causes the cursor to skip.
	stamp atomic.Int64
}

// RollingWindow exposes 1h / 24h aggregates with 1-minute granularity.
// Memory footprint ≈ 1440 × ~72 bytes ≈ 100 KB.
//
// Per spec §7.6 these windows do NOT survive restarts — that's why they're
// not part of the Snapshot type.
type RollingWindow struct {
	slots     [1440]rollingSlot // 24h × 60min
	mu        sync.Mutex
	cur       atomic.Int64 // slot index
	startedAt time.Time
}

// NewRollingWindow returns a window aligned to the current minute.
func NewRollingWindow() *RollingWindow {
	now := time.Now()
	w := &RollingWindow{startedAt: now}
	// Initial slot stamp = current minute so the rotator knows when to zero it.
	w.slots[0].stamp.Store(now.Unix() / 60)
	return w
}

// advance rotates the cursor forward to the current minute, zeroing slots
// that have aged out. Called by every record so cost is amortised.
func (w *RollingWindow) advance(now time.Time) int64 {
	minute := now.Unix() / 60
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := w.cur.Load()
	last := w.slots[cur].stamp.Load()
	steps := minute - last
	if steps <= 0 {
		return cur
	}
	if steps >= int64(len(w.slots)) {
		// Pause longer than 24h — reset everything.
		for i := range w.slots {
			w.slots[i] = rollingSlot{}
		}
		newSlot := int64(0)
		w.slots[newSlot].stamp.Store(minute)
		w.cur.Store(newSlot)
		return newSlot
	}
	next := cur
	for i := int64(0); i < steps; i++ {
		next = (next + 1) % int64(len(w.slots))
		// Zero the slot rotating in.
		w.slots[next] = rollingSlot{}
		w.slots[next].stamp.Store(last + i + 1)
	}
	w.cur.Store(next)
	return next
}

// Record updates the current-minute slot.
func (w *RollingWindow) Record(now time.Time, requests, pdfs, failures, latencyMs, bytesIn, bytesOut int64) {
	idx := w.advance(now)
	s := &w.slots[idx]
	if requests > 0 {
		s.requests.Add(requests)
	}
	if pdfs > 0 {
		s.pdfs.Add(pdfs)
	}
	if failures > 0 {
		s.failures.Add(failures)
	}
	if latencyMs > 0 {
		s.latencySum.Add(latencyMs)
		s.latencyCount.Add(1)
	}
	if bytesIn > 0 {
		s.bytesIn.Add(bytesIn)
	}
	if bytesOut > 0 {
		s.bytesOut.Add(bytesOut)
	}
}

// WindowStats aggregates a Window over the most recent `minutes` slots.
type WindowStats struct {
	Requests    int64
	PDFs        int64
	Failures    int64
	LatencyMs   int64 // mean
	BytesIn     int64
	BytesOut    int64
	CoverageMin int64 // how many minutes of data are populated (0..minutes)
}

// Snapshot returns aggregates over the trailing `minutes` slots.
// Pass 60 for last_1h, 1440 for last_24h.
func (w *RollingWindow) Snapshot(now time.Time, minutes int) WindowStats {
	w.advance(now)
	if minutes <= 0 {
		return WindowStats{}
	}
	if minutes > len(w.slots) {
		minutes = len(w.slots)
	}
	cur := w.cur.Load()
	currentMinute := now.Unix() / 60

	var (
		out        WindowStats
		latencySum int64
		latencyN   int64
		populated  int64
	)
	for i := 0; i < minutes; i++ {
		idx := (cur - int64(i) + int64(len(w.slots))) % int64(len(w.slots))
		s := &w.slots[idx]
		stamp := s.stamp.Load()
		if stamp == 0 || currentMinute-stamp >= int64(minutes) {
			continue
		}
		populated++
		out.Requests += s.requests.Load()
		out.PDFs += s.pdfs.Load()
		out.Failures += s.failures.Load()
		out.BytesIn += s.bytesIn.Load()
		out.BytesOut += s.bytesOut.Load()
		latencySum += s.latencySum.Load()
		latencyN += s.latencyCount.Load()
	}
	if latencyN > 0 {
		out.LatencyMs = latencySum / latencyN
	}
	out.CoverageMin = populated
	return out
}

// StartedAt reports when this RollingWindow began collecting. Used by the
// public stats to expose `coverage_seconds` after a restart.
func (w *RollingWindow) StartedAt() time.Time { return w.startedAt }
