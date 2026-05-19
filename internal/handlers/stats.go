package handlers

import (
	"net/http"
	"time"

	"github.com/monzim/docpipe/internal/analytics"
	"github.com/monzim/docpipe/internal/httpx"
)

// StatsHandler serves GET /v1/stats. Output is the public, redacted view —
// never the raw Recorder.
type StatsHandler struct {
	r              *analytics.Recorder
	health         analytics.HealthSource
	maxConcurrency int
	snapshotEvery  time.Duration
}

// NewStatsHandler binds the recorder + browser health source.
func NewStatsHandler(r *analytics.Recorder, h analytics.HealthSource, maxConcurrency int, snapshotEvery time.Duration) *StatsHandler {
	return &StatsHandler{
		r:              r,
		health:         h,
		maxConcurrency: maxConcurrency,
		snapshotEvery:  snapshotEvery,
	}
}

// Handle implements the stats endpoint.
func (h *StatsHandler) Handle(w http.ResponseWriter, r *http.Request) {
	view := analytics.BuildPublicView(h.r, h.health, h.maxConcurrency, h.snapshotEvery)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, view)
}
