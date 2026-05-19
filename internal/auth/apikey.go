// Package auth implements API-key authentication and per-key rate limiting.
//
// Keys are loaded once at startup from DOCPIPE_API_KEYS and stored in memory.
// Lookups use constant-time comparison so timing attacks can't enumerate
// secrets. The key NAME (not the secret) is attached to the request context
// for downstream middleware (logging, rate limiting, analytics).
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/monzim/docpipe/internal/httpx"
)

// Store holds the configured API keys.
//
// Internally we keep a slice of {name, secretHash} entries. Hashing the
// secret means raw values never sit in memory after startup. Lookup is
// O(n) over the slice — fine because n is small (typically <10 keys).
type Store struct {
	entries []entry
}

type entry struct {
	name   string
	digest [32]byte
}

// NewStore builds a Store from the parsed config map. Returns nil for an
// empty map, which Middleware treats as "no auth required" — useful for tests.
func NewStore(keys map[string]string) *Store {
	if len(keys) == 0 {
		return nil
	}
	s := &Store{entries: make([]entry, 0, len(keys))}
	for name, secret := range keys {
		s.entries = append(s.entries, entry{
			name:   name,
			digest: sha256.Sum256([]byte(secret)),
		})
	}
	return s
}

// Verify reports whether secret matches any configured key. The comparison
// is constant-time against every entry — total work is independent of which
// key matches (or whether any does).
func (s *Store) Verify(secret string) (name string, ok bool) {
	if s == nil {
		return "", false
	}
	got := sha256.Sum256([]byte(secret))
	matchedName := ""
	matched := 0
	for _, e := range s.entries {
		// ConstantTimeCompare returns 1 on match, 0 otherwise.
		if subtle.ConstantTimeCompare(got[:], e.digest[:]) == 1 {
			matched = 1
			matchedName = e.name
		}
	}
	return matchedName, matched == 1
}

// Middleware returns an HTTP middleware that requires a valid API key.
// Pass s=nil to make every route unauthenticated (don't do this in prod).
//
// Recognised credential sources, in order:
//
//	Authorization: Bearer <secret>
//	X-API-Key: <secret>
func Middleware(s *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s == nil {
				next.ServeHTTP(w, r)
				return
			}
			secret := extractKey(r)
			if secret == "" {
				httpx.WriteError(w, r, httpx.CodeUnauthorized,
					"missing API key (Authorization: Bearer ... or X-API-Key)", nil)
				return
			}
			name, ok := s.Verify(secret)
			if !ok {
				httpx.WriteError(w, r, httpx.CodeForbidden, "unknown API key", nil)
				return
			}
			ctx := httpx.WithAPIKeyName(r.Context(), name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
		if rest, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// Limiter is a per-key-name token-bucket rate limiter.
//
// Limiters are created lazily on first request from each key. Stale entries
// don't accumulate — we rely on the configured key set staying bounded.
type Limiter struct {
	rps     rate.Limit
	burst   int
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// NewLimiter returns a Limiter producing at most rps requests per second
// with the given burst capacity. Pass rps<=0 to disable rate limiting.
func NewLimiter(rps float64, burst int) *Limiter {
	return &Limiter{
		rps:     rate.Limit(rps),
		burst:   burst,
		buckets: make(map[string]*rate.Limiter),
	}
}

func (l *Limiter) bucket(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(l.rps, l.burst)
		l.buckets[key] = b
	}
	return b
}

// Middleware returns the per-key rate-limit middleware. Allows the request
// when the limiter is disabled (rps <= 0) or when no key name is on the
// context (auth has already rejected anything we'd want to limit).
func (l *Limiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l == nil || l.rps <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			key := httpx.APIKeyName(r.Context())
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}
			b := l.bucket(key)
			reservation := b.Reserve()
			delay := reservation.Delay()
			if delay <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			reservation.Cancel()
			// Round up to a whole second per RFC 7231 §7.1.3.
			retryAfter := strconv.FormatInt(int64(delay/time.Second)+1, 10)
			w.Header().Set("Retry-After", retryAfter)
			httpx.WriteError(w, r, httpx.CodeRateLimited,
				"rate limit exceeded for API key", map[string]any{
					"retry_after_seconds": retryAfter,
				})
		})
	}
}
