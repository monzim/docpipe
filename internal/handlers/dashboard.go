package handlers

import (
	"net/http"

	"github.com/monzim/docpipe/internal/webassets"
)

// Dashboard serves the embedded HTML dashboard at GET /v1/stats/dashboard.
// The HTML polls /v1/stats every 5s via vanilla JS — no framework, no CDN.
func Dashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(webassets.Dashboard())
}
