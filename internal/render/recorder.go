package render

import "time"

// Recorder is the subset of the analytics surface the renderer needs.
// Concrete implementation lives in internal/analytics; this interface
// keeps the packages decoupled and lets tests pass a no-op.
type Recorder interface {
	IncInFlight()
	DecInFlight()
	BrowserRestarted(reason string, at time.Time)
}

// NoopRecorder satisfies Recorder without doing anything.
// Useful for benchmarks and unit tests that don't exercise analytics.
type NoopRecorder struct{}

func (NoopRecorder) IncInFlight()                       {}
func (NoopRecorder) DecInFlight()                       {}
func (NoopRecorder) BrowserRestarted(string, time.Time) {}
