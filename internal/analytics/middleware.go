package analytics

import (
	"net/http"
	"time"

	"github.com/monzim/docpipe/internal/httpx"
)

// MetricsResponseWriter is the recorded handler's writer. Captures status
// and bytes written so the middleware can emit them after the handler runs.
type MetricsResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *MetricsResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *MetricsResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Middleware records request count, latency, bytes, and outcome.
//
// Mounted AFTER auth + rate-limit so 401/429 traffic doesn't pollute success
// metrics. Skipped paths (observability endpoints) bypass recording entirely
// to avoid self-monitoring noise.
func Middleware(r *Recorder, skip map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if skip[req.URL.Path] {
				next.ServeHTTP(w, req)
				return
			}
			start := time.Now()
			key := httpx.APIKeyName(req.Context())
			r.RecordRequest(key, req.ContentLength)

			rec := &MetricsResponseWriter{ResponseWriter: w}
			next.ServeHTTP(rec, req)

			latencyMs := time.Since(start).Milliseconds()
			switch {
			case rec.status >= 200 && rec.status < 300:
				r.RecordSuccess(key, latencyMs, int64(rec.bytes))
			case rec.status == http.StatusGatewayTimeout:
				r.RecordFailure(key, ReasonTimeout, latencyMs)
			case rec.status == http.StatusBadRequest:
				r.RecordFailure(key, ReasonInvalidRequest, latencyMs)
			case rec.status >= 500:
				r.RecordFailure(key, ReasonRenderFailed, latencyMs)
			default:
				// 3xx/4xx (other than 400) — count as a request but no failure bump.
				// They were already counted in RecordRequest.
			}
		})
	}
}
