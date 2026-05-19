// Package config loads, validates, and exposes the DocPipe runtime configuration.
//
// All configuration is environment-driven. See spec §5 for the full surface.
// Defaults are applied here; validation rejects illegal combinations loudly
// at startup so misconfiguration never reaches the request path.
package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// Service identity. Overridden at build time via -ldflags -X.
	defaultVersion = "2.0.0-dev"

	envPort                = "DOCPIPE_PORT"
	envHost                = "DOCPIPE_HOST"
	envEnv                 = "DOCPIPE_ENV"
	envLogLevel            = "DOCPIPE_LOG_LEVEL"
	envLogFormat           = "DOCPIPE_LOG_FORMAT"
	envAPIKeys             = "DOCPIPE_API_KEYS"
	envCORSOrigins         = "DOCPIPE_CORS_ORIGINS"
	envMaxBodyBytes        = "DOCPIPE_MAX_BODY_BYTES"
	envRenderTimeout       = "DOCPIPE_RENDER_TIMEOUT"
	envRenderConcurrency   = "DOCPIPE_RENDER_CONCURRENCY"
	envRateLimitRPS        = "DOCPIPE_RATE_LIMIT_RPS"
	envRateLimitBurst      = "DOCPIPE_RATE_LIMIT_BURST"
	envChromePath          = "DOCPIPE_CHROME_PATH"
	envBrowserRecycleAfter = "DOCPIPE_BROWSER_RECYCLE_AFTER"
	envBrowserHealthcheck  = "DOCPIPE_BROWSER_HEALTHCHECK_INTERVAL"
	envDataDir             = "DOCPIPE_DATA_DIR"
	envSnapshotInterval    = "DOCPIPE_SNAPSHOT_INTERVAL"
	envDailyRetention      = "DOCPIPE_DAILY_RETENTION_DAYS"
	envEnableSwagger       = "DOCPIPE_ENABLE_SWAGGER"
	envStatsPublic         = "DOCPIPE_STATS_PUBLIC"
)

// Environment names. `development` enables verbose logs and Swagger UI by default.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// LogFormat values.
const (
	LogFormatJSON    = "json"
	LogFormatConsole = "console"
)

// Config holds the validated runtime configuration. Treat as immutable once Load returns.
type Config struct {
	Version string

	Port int
	Host string
	Env  string

	LogLevel  string
	LogFormat string

	// APIKeys maps key name → secret. Loaded from DOCPIPE_API_KEYS as `name:secret` pairs.
	APIKeys map[string]string

	CORSOrigins []string

	MaxBodyBytes       int64
	RenderTimeout      time.Duration
	RenderConcurrency  int
	RateLimitRPS       float64
	RateLimitBurst     int
	ChromePath         string
	BrowserRecycleAt   int64
	BrowserHealthCheck time.Duration

	DataDir            string
	SnapshotInterval   time.Duration
	DailyRetentionDays int

	EnableSwagger bool
	StatsPublic   bool
}

// Load reads configuration from the process environment and validates it.
// Returns an error describing every problem found rather than crashing on the first.
func Load() (*Config, error) {
	c := &Config{Version: defaultVersion}
	var errs []string

	c.Port = envInt(envPort, 8080, &errs)
	c.Host = envString(envHost, "0.0.0.0")
	c.Env = envString(envEnv, EnvProduction)
	c.LogLevel = strings.ToLower(envString(envLogLevel, "info"))
	c.LogFormat = strings.ToLower(envString(envLogFormat, LogFormatJSON))

	// DOCPIPE_API_KEYS is OPTIONAL now. If unset, the auth subsystem will
	// auto-generate keys at startup and persist them to ${DOCPIPE_DATA_DIR}.
	// If set, those keys are authoritative; the persisted file is ignored.
	raw := strings.TrimSpace(os.Getenv(envAPIKeys))
	if raw != "" {
		keys, keyErr := parseAPIKeys(raw)
		if keyErr != nil {
			errs = append(errs, keyErr.Error())
		}
		c.APIKeys = keys
	}

	c.CORSOrigins = parseList(os.Getenv(envCORSOrigins))

	c.MaxBodyBytes = envInt64(envMaxBodyBytes, 10*1024*1024, &errs)
	c.RenderTimeout = envDuration(envRenderTimeout, 30*time.Second, &errs)
	c.RenderConcurrency = envInt(envRenderConcurrency, runtime.NumCPU(), &errs)
	c.RateLimitRPS = envFloat(envRateLimitRPS, 5, &errs)
	c.RateLimitBurst = envInt(envRateLimitBurst, 10, &errs)
	c.ChromePath = envString(envChromePath, "/usr/bin/chromium-browser")
	c.BrowserRecycleAt = envInt64(envBrowserRecycleAfter, 500, &errs)
	c.BrowserHealthCheck = envDuration(envBrowserHealthcheck, 30*time.Second, &errs)
	c.DataDir = envString(envDataDir, "./data")
	c.SnapshotInterval = envDuration(envSnapshotInterval, time.Hour, &errs)
	c.DailyRetentionDays = envInt(envDailyRetention, 30, &errs)
	c.EnableSwagger = envBool(envEnableSwagger, c.Env == EnvDevelopment, &errs)
	c.StatsPublic = envBool(envStatsPublic, true, &errs)

	// Cross-field validation.
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("%s=%q must be one of debug|info|warn|error", envLogLevel, c.LogLevel))
	}
	switch c.LogFormat {
	case LogFormatJSON, LogFormatConsole:
	default:
		errs = append(errs, fmt.Sprintf("%s=%q must be one of json|console", envLogFormat, c.LogFormat))
	}
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Sprintf("%s=%d out of range 1..65535", envPort, c.Port))
	}
	if c.RenderConcurrency < 1 {
		errs = append(errs, fmt.Sprintf("%s=%d must be >= 1", envRenderConcurrency, c.RenderConcurrency))
	}
	if c.RateLimitRPS <= 0 {
		errs = append(errs, fmt.Sprintf("%s=%v must be > 0", envRateLimitRPS, c.RateLimitRPS))
	}
	if c.RateLimitBurst < 1 {
		errs = append(errs, fmt.Sprintf("%s=%d must be >= 1", envRateLimitBurst, c.RateLimitBurst))
	}
	if c.MaxBodyBytes < 1024 {
		errs = append(errs, fmt.Sprintf("%s=%d must be >= 1024", envMaxBodyBytes, c.MaxBodyBytes))
	}
	if c.RenderTimeout < time.Second {
		errs = append(errs, fmt.Sprintf("%s=%v must be >= 1s", envRenderTimeout, c.RenderTimeout))
	}
	if c.BrowserRecycleAt < 10 {
		errs = append(errs, fmt.Sprintf("%s=%d must be >= 10", envBrowserRecycleAfter, c.BrowserRecycleAt))
	}
	if c.DailyRetentionDays < 1 {
		errs = append(errs, fmt.Sprintf("%s=%d must be >= 1", envDailyRetention, c.DailyRetentionDays))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return c, nil
}

// Addr returns the host:port string for the HTTP listener.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// IsDevelopment reports whether DOCPIPE_ENV is "development".
func (c *Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// parseAPIKeys parses a comma-separated list of `name:secret` pairs.
// Returns an error only for malformed input — empty is now a valid signal to
// fall back to auto-generated keys (see auth.LoadOrGenerate).
func parseAPIKeys(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New(envAPIKeys + " parsed empty")
	}
	out := make(map[string]string)
	for i, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, secret, ok := strings.Cut(pair, ":")
		name = strings.TrimSpace(name)
		secret = strings.TrimSpace(secret)
		if !ok || name == "" || secret == "" {
			return nil, fmt.Errorf("%s entry #%d is malformed (expected name:secret)", envAPIKeys, i+1)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s contains duplicate key name %q", envAPIKeys, name)
		}
		out[name] = secret
	}
	if len(out) == 0 {
		return nil, errors.New(envAPIKeys + " parsed empty after trimming")
	}
	return out, nil
}

func parseList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, errs *[]string) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not an integer", key, v))
		return def
	}
	return n
}

func envInt64(key string, def int64, errs *[]string) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not an integer", key, v))
		return def
	}
	return n
}

func envFloat(key string, def float64, errs *[]string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a float", key, v))
		return def
	}
	return f
}

func envBool(key string, def bool, errs *[]string) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a bool", key, v))
		return def
	}
	return b
}

func envDuration(key string, def time.Duration, errs *[]string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a duration", key, v))
		return def
	}
	return d
}
