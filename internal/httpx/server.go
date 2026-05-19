// Package httpx owns the HTTP layer: router wiring, middleware chain, the
// uniform JSON error envelope, and graceful shutdown.
//
// The package deliberately exposes no exported router type — callers receive
// a fully-configured *http.Server from New and call its Run method.
package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"github.com/monzim/docpipe/internal/config"
)

// HealthChecker reports browser readiness for /readyz. M2 wires a real
// implementation; M1 ships with an always-healthy stub.
type HealthChecker interface {
	Healthy() bool
}

// alwaysHealthy is the default HealthChecker until M2 lands the renderer.
type alwaysHealthy struct{}

func (alwaysHealthy) Healthy() bool { return true }

// Server bundles the http.Server and its lifecycle handles.
type Server struct {
	cfg    *config.Config
	log    *slog.Logger
	router *chi.Mux
	srv    *http.Server
	health HealthChecker
}

// New constructs the server with the standard middleware chain and the
// liveness/readiness endpoints already mounted. Feature handlers are added
// by callers via Mount and ServerOption.
func New(cfg *config.Config, log *slog.Logger, opts ...Option) *Server {
	s := &Server{cfg: cfg, log: log, health: alwaysHealthy{}}
	s.router = chi.NewRouter()

	skipLog := map[string]bool{"/healthz": true, "/readyz": true}

	// Phase 1 of the chain — independent of any feature. Auth, rate-limit,
	// and analytics are mounted by the auth/analytics packages later.
	s.router.Use(RecoverMiddleware(log))
	s.router.Use(RequestIDMiddleware)
	s.router.Use(LoggerMiddleware(log, skipLog))
	s.router.Use(BodyLimitMiddleware(cfg.MaxBodyBytes))
	if len(cfg.CORSOrigins) > 0 {
		s.router.Use(cors.Handler(cors.Options{
			AllowedOrigins:   cfg.CORSOrigins,
			AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodOptions},
			AllowedHeaders:   []string{"Content-Type", "Authorization", "X-Requested-With", "X-API-Key", "Accept", requestIDHeader},
			ExposedHeaders:   []string{requestIDHeader, "Deprecation", "Sunset"},
			AllowCredentials: false,
			MaxAge:           300,
		}))
	}

	s.router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.router.Get("/readyz", s.readyz)

	for _, opt := range opts {
		opt(s)
	}

	s.srv = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
		// Slow clients shouldn't tie up the renderer. Allow a generous body window.
		ReadTimeout:  cfg.RenderTimeout + 30*time.Second,
		WriteTimeout: cfg.RenderTimeout + 30*time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// Option configures a Server during construction.
type Option func(*Server)

// WithHealthCheck supplies the readiness probe. Pass the browser instance.
func WithHealthCheck(h HealthChecker) Option {
	return func(s *Server) {
		if h != nil {
			s.health = h
		}
	}
}

// WithRoutes mounts feature handlers on the underlying chi.Router.
// The function receives the same router used by built-in routes — middleware
// already mounted in New applies to every route registered here.
func WithRoutes(register func(r chi.Router)) Option {
	return func(s *Server) { register(s.router) }
}

// WithMiddleware appends additional middleware to the chain in declaration order.
// Called before WithRoutes so middleware applies to routes added afterwards.
func WithMiddleware(mws ...func(http.Handler) http.Handler) Option {
	return func(s *Server) {
		for _, mw := range mws {
			s.router.Use(mw)
		}
	}
}

// Run starts the listener and blocks until ctx is canceled, then shuts
// down gracefully with a fixed timeout.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http_listen", "addr", s.cfg.Addr(), "env", s.cfg.Env)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.log.Info("http_shutdown_initiated")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutCtx); err != nil {
		s.log.Error("http_shutdown_error", "err", err)
		return err
	}
	s.log.Info("http_shutdown_complete")
	return nil
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.health.Healthy() {
		WriteJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		return
	}
	WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "browser_unhealthy"})
}
