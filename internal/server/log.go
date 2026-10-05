package server

import (
	"context"
	"log/slog"
)

// Options.Logger is a *slog.Logger, but the server deliberately depends only on
// the three-method logAdapter interface, which keeps the server testable with a
// stub. loggerFrom bridges the two: a real *slog.Logger is passed straight
// through (its Attr values, not fmt strings, so JSON logs keep their keys),
// anything else is wrapped in a minimal slog.Handler that folds attributes into
// the message as key=value pairs.
func loggerFrom(l logAdapter) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	if s, ok := l.(*slog.Logger); ok {
		return s
	}
	return slog.New(&adapterHandler{target: l})
}

type adapterHandler struct {
	target logAdapter
	attrs  []any
	group  string
}

func (h *adapterHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *adapterHandler) Handle(_ context.Context, r slog.Record) error {
	args := make([]any, 0, len(h.attrs)+2*r.NumAttrs()+2)
	if h.group != "" {
		args = append(args, "logger", h.group)
	}
	args = append(args, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		args = append(args, a.Key, a.Value.Any())
		return true
	})
	switch {
	case r.Level >= slog.LevelError:
		h.target.Error(r.Message, args...)
	case r.Level >= slog.LevelWarn:
		h.target.Warn(r.Message, args...)
	default:
		h.target.Info(r.Message, args...)
	}
	return nil
}

func (h *adapterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]any{}, h.attrs...), flatten(attrs)...)
	return &next
}

func (h *adapterHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.group = name
	return &next
}

func flatten(attrs []slog.Attr) []any {
	out := make([]any, 0, 2*len(attrs))
	for _, a := range attrs {
		out = append(out, a.Key, a.Value.Any())
	}
	return out
}
