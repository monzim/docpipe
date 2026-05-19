// Package observability owns the structured logger used everywhere in the service.
//
// We use stdlib log/slog. The handler is selected at startup based on
// DOCPIPE_LOG_FORMAT (json|console) and the level from DOCPIPE_LOG_LEVEL.
package observability

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger configured for the given level and format.
// Unknown level falls back to info; unknown format falls back to JSON.
func New(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "console", "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
