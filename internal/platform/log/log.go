// Package log builds the service's slog JSON logger.
//
// Request-scoped attributes (request_id today; user_id, staff_id and
// session_id once authn exists, plan §3.10) are stored in the context with
// WithAttrs and added to every record logged with a *Context method.
package log

import (
	"context"
	"io"
	"log/slog"
)

// New returns a JSON logger writing to w at the given level.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(contextHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
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
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r) //nolint:wrapcheck // decorator: the inner handler's error is returned as is
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
