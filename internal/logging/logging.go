// Package logging builds the process logger.
//
// InferGate logs structured records from the first request onward, because the
// gateway's job is to explain traffic that is already flowing through it. Two
// sinks are offered: text for a terminal, JSON for a log pipeline.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing to w.
//
// level is one of debug, info, warn, error. format is "text" or "json". The
// output is deliberately deterministic (no time truncation, no colour codes) so
// that log assertions in tests are stable.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("logging: unsupported level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch strings.ToLower(format) {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text", "":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: unsupported format %q", format)
	}
	return slog.New(h), nil
}
