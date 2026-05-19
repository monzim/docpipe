package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monzim/docpipe/internal/httpx"
)

func TestStore_Verify(t *testing.T) {
	s := NewStore(map[string]string{"a": "secret-a", "b": "secret-b"})
	if name, ok := s.Verify("secret-a"); !ok || name != "a" {
		t.Errorf("verify a: name=%s ok=%v", name, ok)
	}
	if name, ok := s.Verify("nope"); ok || name != "" {
		t.Errorf("verify nope should fail: name=%s ok=%v", name, ok)
	}
}

func TestStore_VerifyEmpty(t *testing.T) {
	if name, ok := (*Store)(nil).Verify("anything"); ok || name != "" {
		t.Errorf("nil store: name=%s ok=%v", name, ok)
	}
}

func TestMiddleware_NoStore_NoOp(t *testing.T) {
	called := false
	h := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(204)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatal("handler not called when store nil")
	}
}

func TestMiddleware_Missing(t *testing.T) {
	s := NewStore(map[string]string{"k": "v"})
	h := Middleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not run")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: %d want 401", rec.Code)
	}
}

func TestMiddleware_BadSecret(t *testing.T) {
	s := NewStore(map[string]string{"k": "real"})
	h := Middleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not run")
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer fake")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: %d want 403", rec.Code)
	}
}

func TestMiddleware_BearerAndXAPIKey(t *testing.T) {
	s := NewStore(map[string]string{"alice": "abc123"})
	var keyOnCtx string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyOnCtx = httpx.APIKeyName(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		header string
		value  string
	}{
		{"Authorization", "Bearer abc123"},
		{"Authorization", "bearer abc123"},
		{"X-API-Key", "abc123"},
	}
	for _, tc := range cases {
		keyOnCtx = ""
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(tc.header, tc.value)
		Middleware(s)(inner).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d want 200", tc.header, rec.Code)
		}
		if keyOnCtx != "alice" {
			t.Errorf("%s: ctx key = %q want alice", tc.header, keyOnCtx)
		}
	}
}

func TestLimiter_Allows(t *testing.T) {
	l := NewLimiter(100, 10)
	called := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(204)
	})
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(httpx.WithAPIKeyName(req.Context(), "alice"))
		rec := httptest.NewRecorder()
		l.Middleware()(inner).ServeHTTP(rec, req)
		if rec.Code != 204 {
			t.Errorf("call %d: code %d", i, rec.Code)
		}
	}
	if called != 5 {
		t.Errorf("called %d want 5", called)
	}
}

func TestLimiter_Throttles(t *testing.T) {
	l := NewLimiter(1, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	allowed := 0
	throttled := 0
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(httpx.WithAPIKeyName(req.Context(), "bob"))
		rec := httptest.NewRecorder()
		l.Middleware()(inner).ServeHTTP(rec, req)
		switch rec.Code {
		case 204:
			allowed++
		case http.StatusTooManyRequests:
			throttled++
			if rec.Header().Get("Retry-After") == "" {
				t.Error("missing Retry-After header")
			}
		default:
			t.Errorf("unexpected code %d", rec.Code)
		}
	}
	if allowed == 0 {
		t.Error("nothing was allowed")
	}
	if throttled == 0 {
		t.Error("nothing was throttled")
	}
}

func TestExtractKey(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*http.Request)
		want  string
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer abc") }, "abc"},
		{"lowercase bearer", func(r *http.Request) { r.Header.Set("Authorization", "bearer abc") }, "abc"},
		{"x-api-key", func(r *http.Request) { r.Header.Set("X-API-Key", "xyz") }, "xyz"},
		{"missing", func(r *http.Request) {}, ""},
		{"basic auth ignored", func(r *http.Request) { r.Header.Set("Authorization", "Basic xxxx") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			tc.setup(r)
			if got := extractKey(r); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
