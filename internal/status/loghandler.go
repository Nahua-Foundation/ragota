package status

import (
	"context"
	"log/slog"
)

// RepoIDKey is the attribute key the indexing path logs a repository under.
// The tee reads it so that a warning about one repository can be shown beside
// that repository rather than only in a global list.
const RepoIDKey = "repo_id"

// LogHandler tees log records into a Bus and passes every one of them on to
// the handler it wraps.
//
// It exists so that "the recent warnings and errors" needs no second reporting
// path beside slog: the places that already log a soft failure — an unreadable
// file, a manifest that will not parse, a watch that could not be established
// — are exactly the places a front end has to show, and asking each of them to
// publish twice is how the two lists drift apart.
type LogHandler struct {
	inner slog.Handler
	bus   *Bus
	level slog.Level
	repo  string // repo_id fixed by WithAttrs, inherited by derived handlers
}

// NewLogHandler wraps inner so that records at level or above also reach bus.
// A nil bus yields inner unchanged, so installing the tee is one line at the
// call site whether or not a bus exists.
func NewLogHandler(inner slog.Handler, bus *Bus, level slog.Level) slog.Handler {
	if bus == nil {
		return inner
	}
	return &LogHandler{inner: inner, bus: bus, level: level}
}

// Enabled reports whether inner handles records at this level.
func (h *LogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle passes the record to the wrapped handler and, when it is important
// enough, appends it to the bus log.
func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.level {
		repo := h.repo
		if repo == "" {
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == RepoIDKey {
					repo = a.Value.String()
					return false
				}
				return true
			})
		}
		h.bus.Log(r.Level, repo, r.Message)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a handler whose records carry attrs.
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	repo := h.repo
	for _, a := range attrs {
		if a.Key == RepoIDKey {
			repo = a.Value.String()
		}
	}
	return &LogHandler{inner: h.inner.WithAttrs(attrs), bus: h.bus, level: h.level, repo: repo}
}

// WithGroup returns a handler that nests subsequent attributes under name.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{inner: h.inner.WithGroup(name), bus: h.bus, level: h.level, repo: h.repo}
}
