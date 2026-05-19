// Package analytics implements DocPipe's in-process observability layer:
// counters, histograms, sliding windows, and disk-backed snapshots.
//
// Hot-path invariant (spec §7.1): recording an event must never block the
// renderer. All update paths in this file are lock-free atomics.
package analytics

import (
	"sort"
	"sync/atomic"
)

// DefaultLatencyBuckets is the fixed bucket schema for request latency in ms.
// Boundaries chosen to give useful detail through p99 of typical HTML → PDF
// renders (a few hundred ms median, multi-second tail).
//
// Counts are stored as [len(buckets)+1] — last slot catches > max.
var DefaultLatencyBuckets = []int64{5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000}

// DefaultPDFSizeBuckets is the schema for PDF output size in bytes.
var DefaultPDFSizeBuckets = []int64{1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216}

// Histogram is a fixed-bucket lock-free histogram.
//
// Concurrent calls to Observe are safe. Percentile computations read a
// snapshot of bucket counts; they don't lock writers out.
type Histogram struct {
	boundaries []int64
	counts     []atomic.Int64 // len = len(boundaries)+1
	sum        atomic.Int64
	count      atomic.Int64
	max        atomic.Int64
}

// NewHistogram returns a histogram with the given bucket boundaries.
// Boundaries must be strictly increasing.
func NewHistogram(boundaries []int64) *Histogram {
	h := &Histogram{
		boundaries: append([]int64(nil), boundaries...),
		counts:     make([]atomic.Int64, len(boundaries)+1),
	}
	return h
}

// Observe records v. Cost is O(log(buckets)) for the binary search.
func (h *Histogram) Observe(v int64) {
	idx := sort.Search(len(h.boundaries), func(i int) bool { return h.boundaries[i] >= v })
	h.counts[idx].Add(1)
	h.sum.Add(v)
	h.count.Add(1)
	for {
		cur := h.max.Load()
		if v <= cur || h.max.CompareAndSwap(cur, v) {
			break
		}
	}
}

// Snapshot returns a consistent-ish view of bucket counts and aggregates.
// "ish" because writers may interleave, but bucket counts plus the sum/count
// metadata reflect a recent moment and totals don't drift far between reads.
type HistogramSnapshot struct {
	Boundaries []int64 `json:"boundaries"`
	Counts     []int64 `json:"counts"`
	Sum        int64   `json:"sum"`
	Count      int64   `json:"count"`
	Max        int64   `json:"max"`
}

// Snapshot copies internal state into a HistogramSnapshot.
func (h *Histogram) Snapshot() HistogramSnapshot {
	counts := make([]int64, len(h.counts))
	for i := range h.counts {
		counts[i] = h.counts[i].Load()
	}
	return HistogramSnapshot{
		Boundaries: append([]int64(nil), h.boundaries...),
		Counts:     counts,
		Sum:        h.sum.Load(),
		Count:      h.count.Load(),
		Max:        h.max.Load(),
	}
}

// LoadSnapshot restores h from a previously-saved snapshot. If the boundaries
// in s differ from h's configured boundaries, returns false — caller decides
// whether to reset or attempt to migrate. Defensive bucket-mismatch handling.
func (h *Histogram) LoadSnapshot(s HistogramSnapshot) bool {
	if len(s.Boundaries) != len(h.boundaries) {
		return false
	}
	for i := range s.Boundaries {
		if s.Boundaries[i] != h.boundaries[i] {
			return false
		}
	}
	if len(s.Counts) != len(h.counts) {
		return false
	}
	for i, n := range s.Counts {
		h.counts[i].Store(n)
	}
	h.sum.Store(s.Sum)
	h.count.Store(s.Count)
	h.max.Store(s.Max)
	return true
}

// Percentile returns an approximate p-th percentile from the bucket
// distribution (p in 0..1). Linear interpolation within the matched bucket.
func (s HistogramSnapshot) Percentile(p float64) int64 {
	if s.Count == 0 {
		return 0
	}
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return s.Max
	}
	target := float64(s.Count) * p
	var cum int64
	for i, c := range s.Counts {
		cum += c
		if float64(cum) >= target {
			// Bucket [boundaries[i-1], boundaries[i]). i==0 is below boundaries[0];
			// i==len(boundaries) is above the last boundary (use Max).
			if i == 0 {
				return s.Boundaries[0]
			}
			if i >= len(s.Boundaries) {
				return s.Max
			}
			return s.Boundaries[i]
		}
	}
	return s.Max
}

// Mean returns the arithmetic mean — sum/count.
func (s HistogramSnapshot) Mean() int64 {
	if s.Count == 0 {
		return 0
	}
	return s.Sum / s.Count
}
