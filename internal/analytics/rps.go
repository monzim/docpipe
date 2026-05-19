package analytics

import (
	"sync"
	"sync/atomic"
	"time"
)

// SlidingWindow tracks request counts over the last 60 seconds with 1-second
// resolution. The current second is the slot writers increment; the previous
// second is the slot CurrentRPS() reads (so reads always reflect a complete
// 1-second sample). A background goroutine advances the cursor every tick.
//
// Tracks 1-minute peak via the ring contents and 5-minute / all-time peaks
// via separate atomics updated by the ticker.
type SlidingWindow struct {
	slots [60]atomic.Int64
	cur   atomic.Int64 // current slot index
	mu    sync.Mutex

	peak1m    atomic.Int64
	peak5m    atomic.Int64
	peakAll   atomic.Int64
	peakAllAt atomic.Int64 // unix nanos

	stop chan struct{}
	wg   sync.WaitGroup

	// peak5mBuf holds a per-second log of counts for computing the rolling
	// 5-minute peak. Sized 300, advanced together with the main ring.
	peak5mBuf [300]int64
	peak5mIdx atomic.Int64
}

// NewSlidingWindow returns a started window. Call Stop to release resources.
func NewSlidingWindow() *SlidingWindow {
	w := &SlidingWindow{stop: make(chan struct{})}
	w.wg.Add(1)
	go w.tick()
	return w
}

// Inc records a request happening "now" — increments the current second's slot.
func (w *SlidingWindow) Inc() {
	idx := w.cur.Load() % int64(len(w.slots))
	w.slots[idx].Add(1)
}

// CurrentRPS returns the count from the previous fully-completed second.
func (w *SlidingWindow) CurrentRPS() int64 {
	cur := w.cur.Load()
	prev := (cur - 1 + int64(len(w.slots))) % int64(len(w.slots))
	return w.slots[prev].Load()
}

// Peak1m returns the maximum 1-second count in the last 60s.
func (w *SlidingWindow) Peak1m() int64 {
	var maxv int64
	for i := range w.slots {
		if v := w.slots[i].Load(); v > maxv {
			maxv = v
		}
	}
	return maxv
}

// Peak5m returns the rolling 5-minute peak (best second in last 300).
func (w *SlidingWindow) Peak5m() int64 { return w.peak5m.Load() }

// PeakAll returns the all-time peak (survives across restarts via snapshot).
func (w *SlidingWindow) PeakAll() (int64, time.Time) {
	v := w.peakAll.Load()
	t := time.Unix(0, w.peakAllAt.Load())
	return v, t
}

// SetPeakAll restores the all-time peak from a snapshot.
func (w *SlidingWindow) SetPeakAll(v int64, at time.Time) {
	w.peakAll.Store(v)
	w.peakAllAt.Store(at.UnixNano())
}

// tick advances the cursor every second, zeroing the slot that's rotating
// out and updating the 5-minute and all-time peak trackers.
func (w *SlidingWindow) tick() {
	defer w.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.mu.Lock()
			// The slot we're about to rotate INTO holds the value from 60s ago;
			// snapshot it before zeroing so it can feed the 5-minute log.
			next := (w.cur.Load() + 1) % int64(len(w.slots))
			about := w.slots[(w.cur.Load())%int64(len(w.slots))].Load()
			// Stamp the count for the now-just-completed second into the 5m buf.
			idx5 := w.peak5mIdx.Add(1) % int64(len(w.peak5mBuf))
			w.peak5mBuf[idx5] = about
			// Refresh 5m peak.
			var maxv int64
			for _, v := range w.peak5mBuf {
				if v > maxv {
					maxv = v
				}
			}
			w.peak5m.Store(maxv)
			// All-time peak.
			if about > w.peakAll.Load() {
				w.peakAll.Store(about)
				w.peakAllAt.Store(time.Now().UnixNano())
			}
			// Zero the slot rotating out (== next slot).
			w.slots[next].Store(0)
			w.cur.Store(next)
			w.mu.Unlock()
		}
	}
}

// Stop halts the background ticker.
func (w *SlidingWindow) Stop() {
	select {
	case <-w.stop:
		return
	default:
	}
	close(w.stop)
	w.wg.Wait()
}
