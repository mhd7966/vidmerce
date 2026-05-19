// Package logger wraps log/slog with project conventions: leveled logging,
// JSON-or-text format chosen at startup, and a single New() entry point so
// the rest of the codebase never instantiates a logger directly.
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New constructs an slog.Logger configured for the given level and format.
// Format is one of: "text" (human-friendly) or "json" (machine-friendly).
// Unknown levels fall back to Info. Returns a logger writing to stdout.
func New(level, format string) *slog.Logger {
	return NewWith(os.Stdout, level, format)
}

// NewWith is identical to New but writes to the provided writer; useful in tests.
func NewWith(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     parseLevel(level),
		AddSource: false,
	}

	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
