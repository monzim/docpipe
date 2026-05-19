package httpx

import (
	"context"
	"net/http"
	"time"
)

// TimeoutMiddleware bounds the time spent inside the handler. On timeout it
// emits the uniform error envelope with code=render_timeout, rather than the
// default http.TimeoutHandler plaintext.
//
// Per spec §9 this is the last middleware before the handler, after analytics.
func TimeoutMiddleware(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			done := make(chan struct{})
			tw := &timeoutWriter{ResponseWriter: w}
			go func() {
				defer close(done)
				next.ServeHTTP(tw, r.WithContext(ctx))
			}()

			select {
			case <-done:
				return
			case <-ctx.Done():
				if tw.markTimedOut() {
					WriteError(w, r, CodeRenderTimeout,
						"request exceeded server timeout", nil)
				}
				// Allow the in-flight handler to unwind; we've already responded.
				<-done
			}
		})
	}
}

// timeoutWriter discards writes after the timeout has fired so the handler
// can't race with WriteError above.
type timeoutWriter struct {
	http.ResponseWriter
	timedOut bool
}

func (w *timeoutWriter) markTimedOut() bool {
	if w.timedOut {
		return false
	}
	w.timedOut = true
	return true
}

func (w *timeoutWriter) WriteHeader(code int) {
	if w.timedOut {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timeoutWriter) Write(b []byte) (int, error) {
	if w.timedOut {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}
