package analytics

import (
	"sync"
	"testing"
)

func TestHistogram_BasicObserve(t *testing.T) {
	h := NewHistogram([]int64{10, 100, 1000})
	for _, v := range []int64{5, 50, 500, 5000} {
		h.Observe(v)
	}
	s := h.Snapshot()
	if s.Count != 4 {
		t.Errorf("count: got %d want 4", s.Count)
	}
	if s.Max != 5000 {
		t.Errorf("max: got %d want 5000", s.Max)
	}
	if s.Sum != 5555 {
		t.Errorf("sum: got %d want 5555", s.Sum)
	}
	// Bucket distribution: <=10 has 1, <=100 has 1, <=1000 has 1, >1000 has 1.
	want := []int64{1, 1, 1, 1}
	for i, w := range want {
		if s.Counts[i] != w {
			t.Errorf("bucket[%d]: got %d want %d", i, s.Counts[i], w)
		}
	}
}

func TestHistogram_Percentile(t *testing.T) {
	h := NewHistogram([]int64{10, 100, 1000})
	for i := 0; i < 100; i++ {
		h.Observe(5) // all in first bucket
	}
	s := h.Snapshot()
	if got := s.Percentile(0.5); got != 10 {
		t.Errorf("p50 got %d want 10 (top of bucket containing data)", got)
	}
	if s.Mean() != 5 {
		t.Errorf("mean got %d want 5", s.Mean())
	}
}

func TestHistogram_ConcurrentObserve(t *testing.T) {
	h := NewHistogram([]int64{10, 100, 1000})
	const n = 1000
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(v int64) {
			defer wg.Done()
			h.Observe(v)
		}(int64(i))
	}
	wg.Wait()
	if got := h.Snapshot().Count; got != n {
		t.Errorf("count under concurrency: got %d want %d", got, n)
	}
}

func TestHistogram_LoadSnapshot_MismatchedBoundaries(t *testing.T) {
	h := NewHistogram([]int64{10, 100})
	snap := HistogramSnapshot{
		Boundaries: []int64{20, 200},
		Counts:     []int64{1, 1, 1},
		Sum:        100,
		Count:      3,
	}
	if ok := h.LoadSnapshot(snap); ok {
		t.Error("expected LoadSnapshot to reject mismatched boundaries")
	}
}

func TestHistogram_RoundTrip(t *testing.T) {
	h1 := NewHistogram(DefaultLatencyBuckets)
	for _, v := range []int64{15, 200, 1500} {
		h1.Observe(v)
	}
	snap := h1.Snapshot()

	h2 := NewHistogram(DefaultLatencyBuckets)
	if !h2.LoadSnapshot(snap) {
		t.Fatal("LoadSnapshot rejected matching boundaries")
	}
	got := h2.Snapshot()
	if got.Count != snap.Count || got.Sum != snap.Sum || got.Max != snap.Max {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, snap)
	}
}
