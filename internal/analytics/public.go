package analytics

import "time"

// PublicView is the redacted, JSON-friendly snapshot served at /v1/stats.
//
// Per spec §6, this view EXCLUDES: per-key data, key names, request IDs,
// client IPs, hostnames, and per-request HTML content. Generation lives in
// this file so the redaction boundary is auditable in one place — never
// serialise *Recorder directly to the network.
type PublicView struct {
	Service          PublicService     `json:"service"`
	Totals           PublicTotals      `json:"totals"`
	LatencyMs        PublicLatency     `json:"latency_ms"`
	Throughput       PublicThroughput  `json:"throughput"`
	Concurrency      PublicConcurrency `json:"concurrency"`
	Browser          PublicBrowser     `json:"browser"`
	Windows          PublicWindows     `json:"windows"`
	FailuresByReason map[string]int64  `json:"failures_by_reason"`
	Snapshot         PublicSnapshot    `json:"snapshot"`
}

type PublicService struct {
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Status        string    `json:"status"`
}

type PublicTotals struct {
	Requests          int64   `json:"requests"`
	PDFsGenerated     int64   `json:"pdfs_generated"`
	Failures          int64   `json:"failures"`
	SuccessRate       float64 `json:"success_rate"`
	BytesIn           int64   `json:"bytes_in"`
	BytesOut          int64   `json:"bytes_out"`
	PDFPagesEstimated int64   `json:"pdf_pages_estimated"`
}

type PublicLatency struct {
	P50  int64 `json:"p50"`
	P90  int64 `json:"p90"`
	P95  int64 `json:"p95"`
	P99  int64 `json:"p99"`
	Mean int64 `json:"mean"`
	Max  int64 `json:"max"`
}

type PublicThroughput struct {
	CurrentRPS    float64   `json:"current_rps"`
	Peak1m        float64   `json:"peak_rps_1m"`
	Peak5m        float64   `json:"peak_rps_5m"`
	PeakAllTime   float64   `json:"peak_rps_all_time"`
	PeakAllTimeAt time.Time `json:"peak_rps_at,omitempty"`
}

type PublicConcurrency struct {
	MaxConfigured   int   `json:"max_configured"`
	CurrentInFlight int64 `json:"current_in_flight"`
	PeakInFlight    int64 `json:"peak_in_flight"`
}

type PublicBrowser struct {
	Healthy           bool       `json:"healthy"`
	Restarts          int64      `json:"restarts"`
	LastRestartAt     *time.Time `json:"last_restart_at,omitempty"`
	LastRestartReason string     `json:"last_restart_reason,omitempty"`
}

type PublicWindow struct {
	Requests        int64 `json:"requests"`
	PDFs            int64 `json:"pdfs"`
	Failures        int64 `json:"failures"`
	P95Ms           int64 `json:"p95_ms"`
	CoverageSeconds int64 `json:"coverage_seconds"`
}

type PublicWindows struct {
	Last1h  PublicWindow `json:"last_1h"`
	Last24h PublicWindow `json:"last_24h"`
}

type PublicSnapshot struct {
	LastPersistedAt *time.Time `json:"last_persisted_at,omitempty"`
	NextPersistAt   *time.Time `json:"next_persist_at,omitempty"`
}

// HealthSource lets the view query browser readiness without importing the
// render package directly.
type HealthSource interface {
	Healthy() bool
}

// BuildPublicView assembles the public-facing snapshot. Inputs:
//   - r: the recorder (lifetime totals + histograms + windows)
//   - h: a HealthSource for the renderer (current health, no other coupling)
//   - maxConcurrency: configured cap, surfaced for capacity reasoning
//   - snapshotInterval: used to compute next_persist_at
func BuildPublicView(r *Recorder, h HealthSource, maxConcurrency int, snapshotInterval time.Duration) PublicView {
	now := time.Now()

	totals := r.Totals()
	failures := totals.Failures
	success := totals.PDFs

	successRate := 1.0
	if totals.Requests > 0 {
		successRate = float64(success) / float64(totals.Requests)
	}

	hist := r.latency.Snapshot()
	inFlight, peakInFlight := r.InFlight()
	peakAll, peakAllAt := r.rps.PeakAll()

	view := PublicView{
		Service: PublicService{
			Name:          "DocPipe",
			Version:       r.Version(),
			StartedAt:     r.StartedAt(),
			UptimeSeconds: int64(now.Sub(r.StartedAt()).Seconds()),
			Status:        statusString(h),
		},
		Totals: PublicTotals{
			Requests:      totals.Requests,
			PDFsGenerated: totals.PDFs,
			Failures:      failures,
			SuccessRate:   round4(successRate),
			BytesIn:       totals.BytesIn,
			BytesOut:      totals.BytesOut,
			// Rough page estimate — 1 page per 100 KB of PDF. Cheap and
			// directionally honest for a "how many pages did we render" metric.
			PDFPagesEstimated: totals.BytesOut / (100 * 1024),
		},
		LatencyMs: PublicLatency{
			P50:  hist.Percentile(0.5),
			P90:  hist.Percentile(0.9),
			P95:  hist.Percentile(0.95),
			P99:  hist.Percentile(0.99),
			Mean: hist.Mean(),
			Max:  r.MaxLatencyMs(),
		},
		Throughput: PublicThroughput{
			CurrentRPS:    float64(r.rps.CurrentRPS()),
			Peak1m:        float64(r.rps.Peak1m()),
			Peak5m:        float64(r.rps.Peak5m()),
			PeakAllTime:   float64(peakAll),
			PeakAllTimeAt: peakAllAt,
		},
		Concurrency: PublicConcurrency{
			MaxConfigured:   maxConcurrency,
			CurrentInFlight: inFlight,
			PeakInFlight:    peakInFlight,
		},
		Browser:          buildBrowserView(r, h),
		Windows:          buildWindows(r, now),
		FailuresByReason: r.FailuresByReason(),
		Snapshot:         buildSnapshotInfo(r, snapshotInterval),
	}

	return view
}

func statusString(h HealthSource) string {
	if h == nil || h.Healthy() {
		return "healthy"
	}
	return "degraded"
}

func buildBrowserView(r *Recorder, h HealthSource) PublicBrowser {
	out := PublicBrowser{
		Healthy:  h == nil || h.Healthy(),
		Restarts: r.BrowserRestarts(),
	}
	if last := r.LastRestart(); last != nil {
		t := last.At
		out.LastRestartAt = &t
		out.LastRestartReason = last.Reason
	}
	return out
}

func buildWindows(r *Recorder, now time.Time) PublicWindows {
	hist := r.latency.Snapshot()
	p95 := hist.Percentile(0.95)

	s1h := r.rolling.Snapshot(now, 60)
	s24h := r.rolling.Snapshot(now, 60*24)

	// coverage_seconds is min(elapsed-since-rolling-start, minutes*60).
	coverage1h := minSeconds(now.Sub(r.rolling.StartedAt()), time.Hour)
	coverage24h := minSeconds(now.Sub(r.rolling.StartedAt()), 24*time.Hour)

	return PublicWindows{
		Last1h: PublicWindow{
			Requests:        s1h.Requests,
			PDFs:            s1h.PDFs,
			Failures:        s1h.Failures,
			P95Ms:           p95,
			CoverageSeconds: coverage1h,
		},
		Last24h: PublicWindow{
			Requests:        s24h.Requests,
			PDFs:            s24h.PDFs,
			Failures:        s24h.Failures,
			P95Ms:           p95,
			CoverageSeconds: coverage24h,
		},
	}
}

func buildSnapshotInfo(r *Recorder, interval time.Duration) PublicSnapshot {
	out := PublicSnapshot{}
	last, ok := r.LastSnapshotAt()
	if ok {
		t := last
		out.LastPersistedAt = &t
		if interval > 0 {
			next := t.Add(interval)
			out.NextPersistAt = &next
		}
	}
	return out
}

func minSeconds(elapsed, cap time.Duration) int64 {
	if elapsed > cap {
		elapsed = cap
	}
	if elapsed < 0 {
		return 0
	}
	return int64(elapsed / time.Second)
}

func round4(f float64) float64 {
	return float64(int64(f*10000)) / 10000
}
