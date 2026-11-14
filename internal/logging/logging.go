// Package logging adds the correlation ID from the context to every log record.
package logging

import (
	"context"
	"log/slog"
)

type correlationKey struct{}

// WithCorrelationID returns a copy of ctx that carries id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationID returns the ID set by WithCorrelationID, or "" if ctx has none.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// NewHandler wraps h so that records logged with a context carrying a correlation ID
// get a correlation_id attribute.
func NewHandler(h slog.Handler) slog.Handler {
	return handler{h}
}

type handler struct {
	slog.Handler
}

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup rewrap the result. The embedded methods return the inner handler,
// so a logger made with logger.With would silently stop adding the ID.
func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return handler{h.Handler.WithAttrs(attrs)}
}

func (h handler) WithGroup(name string) slog.Handler {
	return handler{h.Handler.WithGroup(name)}
}
