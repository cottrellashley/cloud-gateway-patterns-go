// Package telemetry is the gateway's observability plumbing with the standard
// library only: structured logs (log/slog), W3C trace-context propagation
// (traceparent), an access-log middleware whose fields match what the
// Kubernetes apiserver httplog and Traefik/Caddy access logs record, and a
// metrics Recorder interface with the RED labels Prometheus users expect.
//
// Production wiring replaces pieces, not shapes: otelhttp.NewHandler for
// spans (it propagates traceparent the same way), promhttp.InstrumentHandler*
// for metrics. See docs/05-observability/*.md for the upstream code.
package telemetry

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// NewLogger returns a JSON logger with stable field names: ts (RFC 3339 with
// nanoseconds, UTC), level, msg, plus service/version/env on every line.
func NewLogger(w io.Writer, level slog.Leveler, service, version, env string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.String("ts", a.Value.Time().UTC().Format(time.RFC3339Nano))
			}
			return a
		},
	})
	return slog.New(h).With("service", service, "version", version, "env", env)
}

type loggerKey struct{}

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// FromContext returns the request-scoped logger or slog.Default().
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// With returns a context whose logger carries the extra attributes. Call it
// once per request with request_id and trace_id so that every log line in the
// request, in any package, is correlated without passing IDs around.
func With(ctx context.Context, args ...any) context.Context {
	return WithLogger(ctx, FromContext(ctx).With(args...))
}
