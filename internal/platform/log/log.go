// Package log builds the service's slog JSON logger.
//
// Request-scoped attributes (request_id today; user_id, staff_id and
// session_id once authn exists, plan §3.10) are stored in the context with
// WithAttrs and added at the top level of every record logged with a
// *Context method, also under logger.WithGroup.
package log

import (
	"context"
	"io"
	"log/slog"
)

// New returns a JSON logger writing to w at the given level.
func New(w io.Writer, level slog.Level) *slog.Logger {
	root := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(&contextHandler{root: root, current: root})
}

type ctxKey struct{}

// WithAttrs returns a context whose log records carry attrs in addition to
// any attributes already in ctx.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(prev)+len(attrs))
	merged = append(merged, prev...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// contextHandler adds the attributes stored by WithAttrs to each record.
//
// Attributes added to a record land inside the handler's open groups, so
// once WithGroup has been called the context attributes are applied to the
// root handler first and the logger's own WithAttrs/WithGroup calls are
// replayed on top. Without groups the cheap path (Record.AddAttrs) is used.
type contextHandler struct {
	root    slog.Handler
	current slog.Handler
	ops     []func(slog.Handler) slog.Handler
	grouped bool
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.current.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	if len(attrs) == 0 {
		return h.current.Handle(ctx, r) //nolint:wrapcheck // decorator: the inner handler's error is returned as is
	}
	if !h.grouped {
		r.AddAttrs(attrs...)
		return h.current.Handle(ctx, r) //nolint:wrapcheck // decorator: the inner handler's error is returned as is
	}
	inner := h.root.WithAttrs(attrs)
	for _, op := range h.ops {
		inner = op(inner)
	}
	return inner.Handle(ctx, r) //nolint:wrapcheck // decorator: the inner handler's error is returned as is
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(func(inner slog.Handler) slog.Handler { return inner.WithAttrs(attrs) }, false)
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(func(inner slog.Handler) slog.Handler { return inner.WithGroup(name) }, true)
}

func (h *contextHandler) with(op func(slog.Handler) slog.Handler, group bool) *contextHandler {
	ops := make([]func(slog.Handler) slog.Handler, 0, len(h.ops)+1)
	ops = append(ops, h.ops...)
	ops = append(ops, op)
	return &contextHandler{
		root:    h.root,
		current: op(h.current),
		ops:     ops,
		grouped: h.grouped || group,
	}
}
