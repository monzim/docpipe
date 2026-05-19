// Command docpipe is the HTTP entrypoint for the HTML → PDF service.
//
// Wiring order (must remain stable):
//  1. Config — fail fast on invalid env.
//  2. Logger — emit JSON or console, level from config.
//  3. Analytics — load prior snapshot from disk, start hourly ticker.
//  4. Renderer — launch Chromium, start supervisor (wired to analytics).
//  5. Signal context — cancels on SIGINT/SIGTERM so graceful shutdown runs.
//  6. HTTP server — registers /healthz, /readyz, and feature handlers.
//
// Build-time identifiers are injected via -ldflags -X main.version=...
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/monzim/docpipe/internal/analytics"
	"github.com/monzim/docpipe/internal/auth"
	"github.com/monzim/docpipe/internal/config"
	"github.com/monzim/docpipe/internal/handlers"
	"github.com/monzim/docpipe/internal/httpx"
	"github.com/monzim/docpipe/internal/observability"
	"github.com/monzim/docpipe/internal/render"
)

// sortedKeyNames returns key names alphabetically — used purely for stable
// startup output. The keys map iteration order isn't deterministic and a
// shuffling list of keys at boot would be confusing.
func sortedKeyNames(keys map[string]string) []string {
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set via -ldflags "-X main.version=... -X main.commit=... -X main.buildDate=...".
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "docpipe: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Version = version

	log := observability.New(cfg.LogLevel, cfg.LogFormat)
	log.LogAttrs(context.Background(), slog.LevelInfo, "startup",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("build_date", buildDate),
		slog.String("env", cfg.Env),
		slog.Int("render_concurrency", cfg.RenderConcurrency),
	)

	// Resolve API keys before anything else — auto-generate + persist if the
	// user hasn't set DOCPIPE_API_KEYS, so the service is usable on a fresh
	// volume with zero config. The persisted file is read on next restart.
	apiKeys, freshlyGenerated, err := auth.LoadOrGenerate(cfg.DataDir, cfg.APIKeys, log)
	if err != nil {
		return fmt.Errorf("api key resolution: %w", err)
	}
	if freshlyGenerated {
		// One-time prominent printout so the operator can copy the secrets
		// before they roll out of the log buffer. Printed to stderr (not slog)
		// to skip the structured-log formatter — easier to grep with `docker logs`.
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "═══════════════════════════════════════════════════════════════════════")
		fmt.Fprintln(os.Stderr, " DocPipe auto-generated API keys (saved to "+cfg.DataDir+"/api-keys.json)")
		fmt.Fprintln(os.Stderr, "═══════════════════════════════════════════════════════════════════════")
		for _, name := range sortedKeyNames(apiKeys) {
			fmt.Fprintf(os.Stderr, "  %-10s  %s\n", name, apiKeys[name])
		}
		fmt.Fprintln(os.Stderr, "═══════════════════════════════════════════════════════════════════════")
		fmt.Fprintln(os.Stderr, " To use: Authorization: Bearer <secret>   or   X-API-Key: <secret>")
		fmt.Fprintln(os.Stderr, " Set DOCPIPE_API_KEYS env to override; these are persisted otherwise.")
		fmt.Fprintln(os.Stderr, "═══════════════════════════════════════════════════════════════════════")
		fmt.Fprintln(os.Stderr, "")
	}

	recorder := analytics.New(version)
	defer recorder.Stop()

	store := analytics.NewStore(cfg.DataDir, cfg.DailyRetentionDays, cfg.SnapshotInterval, recorder, log)
	if err := store.Replay(); err != nil {
		return fmt.Errorf("analytics replay: %w", err)
	}

	browser, err := render.New(render.Config{
		ChromePath:          cfg.ChromePath,
		Concurrency:         cfg.RenderConcurrency,
		RenderTimeout:       cfg.RenderTimeout,
		RecycleAfter:        cfg.BrowserRecycleAt,
		HealthCheckInterval: cfg.BrowserHealthCheck,
	}, log, recorder)
	if err != nil {
		return fmt.Errorf("renderer: %w", err)
	}
	defer browser.Close()
	log.Info("renderer_ready")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the analytics persistence ticker AFTER replay so the first
	// scheduled write captures merged state.
	store.Start(ctx)
	defer func() {
		store.Stop()
		// One final flush so shutdown counters land on disk.
		if err := store.Persist(); err != nil {
			log.Error("analytics_final_persist_failed", "err", err)
		} else {
			log.Info("analytics_final_persist_complete")
		}
	}()

	convert := handlers.NewConvertHandler(browser)
	legacy := handlers.NewLegacyConvertHandler(browser, log)
	stats := handlers.NewStatsHandler(recorder, browser, cfg.RenderConcurrency, cfg.SnapshotInterval)
	keys := auth.NewStore(apiKeys)
	limiter := auth.NewLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)

	// Paths excluded from analytics — self-observability shouldn't pollute totals.
	skipAnalytics := map[string]bool{
		"/healthz":            true,
		"/readyz":             true,
		"/v1/stats":           true,
		"/v1/stats/dashboard": true,
	}

	srv := httpx.New(cfg, log,
		httpx.WithHealthCheck(browser),
		httpx.WithRoutes(func(r chi.Router) {
			// /v1/convert/* and legacy /api/html-to-pdf — auth + rate-limit
			// + analytics + timeout. The legacy path shares the same chain
			// so analytics and rate limits cover both.
			r.Group(func(r chi.Router) {
				r.Use(auth.Middleware(keys))
				r.Use(limiter.Middleware())
				r.Use(analytics.Middleware(recorder, skipAnalytics))
				r.Use(httpx.TimeoutMiddleware(cfg.RenderTimeout + 5*time.Second))
				r.Post("/v1/convert/html-to-pdf", convert.Handle)
				r.Post("/api/html-to-pdf", legacy.Handle)
			})

			// Dashboard — always public. The HTML alone reveals nothing
			// sensitive; the stats JSON it fetches is what gets gated.
			// Served at both `/` (canonical) and the legacy
			// `/v1/stats/dashboard` so existing bookmarks still work.
			r.Get("/", handlers.Dashboard)
			r.Get("/v1/stats/dashboard", handlers.Dashboard)

			// Stats JSON — optionally auth-gated via DOCPIPE_STATS_PUBLIC.
			// When false, the dashboard will load but its data fetches 401
			// and the error banner stays visible. That's the operator's call.
			r.Group(func(r chi.Router) {
				if !cfg.StatsPublic {
					r.Use(auth.Middleware(keys))
				}
				r.Get("/v1/stats", stats.Handle)
			})

			// Swagger UI — dev-facing, gated by config.
			if cfg.EnableSwagger {
				swagger, err := handlers.NewSwaggerHandler(cfg.SwaggerServers)
				if err != nil {
					log.Error("swagger_handler_build_failed", "err", err)
				} else {
					r.Get("/swagger", swagger.UI)
					r.Get("/swagger/", swagger.UI)
					r.Get("/swagger/openapi.yaml", swagger.Spec)
					log.Info("swagger_ui_enabled",
						"path", "/swagger/",
						"extra_servers", len(cfg.SwaggerServers),
					)
				}
			}
		}),
	)
	return srv.Run(ctx)
}
