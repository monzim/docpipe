package analytics

import (
	"sync/atomic"
	"time"
)

// SchemaVersion is the persisted-state version. Bump when the on-disk
// shape changes in a non-additive way; older snapshots must declare a
// lower number so Load() can migrate or reset safely.
const SchemaVersion = 1

// Snapshot is the JSON-friendly persisted form of Recorder state.
// Spec §7.6 — kept as a flat dataclass for easy round-trip.
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	SavedAt       time.Time `json:"saved_at"`

	Service ServiceSnap `json:"service"`
	Totals  TotalsSnap  `json:"totals"`

	FailuresByReason map[string]int64 `json:"failures_by_reason"`

	LatencyHistogram HistogramSnapshot `json:"latency_histogram"`
	PDFSizeHistogram HistogramSnapshot `json:"pdf_size_histogram"`

	Peaks   PeaksSnap   `json:"peaks"`
	Browser BrowserSnap `json:"browser"`
}

type ServiceSnap struct {
	StartedAtFirstRun time.Time `json:"started_at_first_run"`
	Version           string    `json:"version"`
}

type TotalsSnap struct {
	Requests int64 `json:"requests"`
	PDFs     int64 `json:"pdfs"`
	Failures int64 `json:"failures"`
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
}

type PeaksSnap struct {
	RPSAllTime   int64     `json:"rps_all_time"`
	RPSAllTimeAt time.Time `json:"rps_all_time_at"`
	Concurrency  int64     `json:"concurrency"`
	MaxLatencyMs int64     `json:"max_latency_ms"`
}

type BrowserSnap struct {
	Restarts          int64     `json:"restarts"`
	LastRestartAt     time.Time `json:"last_restart_at,omitempty"`
	LastRestartReason string    `json:"last_restart_reason,omitempty"`
}

// MarshalSnapshot extracts a Snapshot from the recorder state.
func MarshalSnapshot(r *Recorder) Snapshot {
	peakRPS, peakRPSAt := r.rps.PeakAll()
	s := Snapshot{
		SchemaVersion: SchemaVersion,
		SavedAt:       time.Now().UTC(),
		Service: ServiceSnap{
			StartedAtFirstRun: r.startedFirstRun.UTC(),
			Version:           r.version,
		},
		Totals: TotalsSnap{
			Requests: r.totalRequests.Load(),
			PDFs:     r.totalPDFs.Load(),
			Failures: r.totalFailures.Load(),
			BytesIn:  r.bytesIn.Load(),
			BytesOut: r.bytesOut.Load(),
		},
		FailuresByReason: r.failureMap(),
		LatencyHistogram: r.latency.Snapshot(),
		PDFSizeHistogram: r.pdfSize.Snapshot(),
		Peaks: PeaksSnap{
			RPSAllTime:   peakRPS,
			RPSAllTimeAt: peakRPSAt.UTC(),
			Concurrency:  r.peakInFlight.Load(),
			MaxLatencyMs: r.maxLatencyMs.Load(),
		},
	}
	if last := r.LastRestart(); last != nil {
		s.Browser = BrowserSnap{
			Restarts:          r.browserRestarts.Load(),
			LastRestartAt:     last.At.UTC(),
			LastRestartReason: last.Reason,
		}
	} else {
		s.Browser.Restarts = r.browserRestarts.Load()
	}
	return s
}

// ApplySnapshot restores totals, histograms, peaks, and browser stats from s.
// Rolling windows and current-process counters (in-flight) are deliberately
// not restored — see spec §7.6 restart-survival table.
func ApplySnapshot(r *Recorder, s Snapshot) (warnings []string) {
	r.SetStartedFirstRun(s.Service.StartedAtFirstRun)

	r.totalRequests.Store(s.Totals.Requests)
	r.totalPDFs.Store(s.Totals.PDFs)
	r.totalFailures.Store(s.Totals.Failures)
	r.bytesIn.Store(s.Totals.BytesIn)
	r.bytesOut.Store(s.Totals.BytesOut)

	for reason, n := range s.FailuresByReason {
		v, _ := r.failuresByReason.LoadOrStore(reason, new(atomic.Int64))
		v.(*atomic.Int64).Store(n)
	}

	if !r.latency.LoadSnapshot(s.LatencyHistogram) {
		warnings = append(warnings, "latency histogram boundaries changed; reset to zero")
	}
	if !r.pdfSize.LoadSnapshot(s.PDFSizeHistogram) {
		warnings = append(warnings, "pdf size histogram boundaries changed; reset to zero")
	}

	r.maxLatencyMs.Store(s.Peaks.MaxLatencyMs)
	r.peakInFlight.Store(s.Peaks.Concurrency)
	if s.Peaks.RPSAllTime > 0 {
		r.rps.SetPeakAll(s.Peaks.RPSAllTime, s.Peaks.RPSAllTimeAt)
	}

	r.browserRestarts.Store(s.Browser.Restarts)
	if !s.Browser.LastRestartAt.IsZero() {
		r.lastRestart.Store(&RestartEvent{At: s.Browser.LastRestartAt, Reason: s.Browser.LastRestartReason})
	}
	return warnings
}
