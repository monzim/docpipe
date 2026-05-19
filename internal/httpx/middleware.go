// Middleware chain pieces. Spec §9 fixes the order:
//
//	recover → requestID → logger → bodyLimit → CORS → auth → rateLimit → analytics → timeout → handler
//
// CORS, auth, rate-limit, analytics, and timeout are mounted in server.go.
// This file owns recover/requestID/logger/bodyLimit.
package httpx

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/oklog/ulid/v2"
)

const requestIDHeader = "X-Request-ID"

// RequestIDMiddleware accepts an incoming X-Request-ID or mints a fresh ULID.
// The value is attached to the request context and echoed on the response.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" {
			id = ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggerMiddleware emits a structured access log entry per request.
// Skipped paths (e.g. /healthz) keep observability traffic out of operational metrics.
func LoggerMiddleware(log *slog.Logger, skip map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if skip[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := newRecorder(w)
			next.ServeHTTP(rec, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes_out", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", RequestID(r.Context())),
				slog.String("api_key", APIKeyName(r.Context())),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// RecoverMiddleware turns panics into 500s with the uniform error envelope.
// Stack trace is logged but never returned to the client.
func RecoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.LogAttrs(r.Context(), slog.LevelError, "panic_recovered",
						slog.String("request_id", RequestID(r.Context())),
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
					)
					WriteError(w, r, CodeInternalError, "internal server error", nil)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimitMiddleware caps the request body length. Excess yields 413 with
// the uniform envelope. This must run before any body-parsing middleware.
func BodyLimitMiddleware(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			// We can't always detect overflow until the handler reads. Let downstream
			// surface a 413 by checking errors from the body reader. For requests
			// with a declared Content-Length we can short-circuit.
			if r.ContentLength > maxBytes {
				WriteError(w, r, CodePayloadTooLarge,
					fmt.Sprintf("request body exceeds maximum of %d bytes", maxBytes), nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// IsBodyTooLarge reports whether err is the MaxBytesReader overflow sentinel.
func IsBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
