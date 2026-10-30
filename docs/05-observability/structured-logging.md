# Structured logging

**Pattern:** `log/slog` with a JSON handler, a request-scoped logger in the context carrying `request_id`, `trace_id` and `principal`, stable field names, and a strict rule about what never gets logged. Implemented in `patterns/telemetry` (`NewLogger`, `WithLogger`, `FromContext`, `With`).

## slog, used correctly

The `slog.Handler` interface (go1.25.14):

```go
type Handler interface {
    Enabled(context.Context, Level) bool
    Handle(context.Context, Record) error
    WithAttrs(attrs []Attr) Handler
    WithGroup(name string) Handler
}
```

`WithAttrs` is what makes request-scoped loggers cheap: `logger.With("request_id", id)` pre-formats the attribute once; every subsequent line in the request reuses it. The slog docs recommend `LogAttrs` for the hottest paths ("For the most efficient log output, use Logger.LogAttrs") and `LogValuer` for values that are expensive to compute or must be redacted (`Value.Resolve`).

`patterns/telemetry.NewLogger` sets the handler once with `ReplaceAttr` renaming the time key to `ts` in RFC 3339 nanoseconds UTC, and attaches `service`, `version`, `env` to every line:

```go
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
```

## Request scoping

```go
ctx = telemetry.With(ctx, "request_id", rid, "trace_id", tid)   // once, in middleware
telemetry.FromContext(ctx).Warn("upstream slow", "upstream", up.Name, "ms", d.Milliseconds())
```

Kratos' recovery middleware and Traefik's `middlewares.GetLogger(req.Context(), name, typeName)` do the same: the logger is in the context; packages never hold a global logger with request fields. ardanlabs/service passes a `logger` whose events hook (`logger.NewWithEvents(..., otel.GetTraceID, events)`) reads the trace ID from the context on every line.

## What the access log and the app log are for

* **Access log**: one line per request, written by middleware when the handler returns; fields in `docs/05-observability/request-telemetry-and-audit.md`.
* **Application log**: what happened inside, at `debug`/`info`/`warn`/`error`, always with the request-scoped logger.
* **Audit log**: a separate append-only stream for security-relevant decisions; never mixed into either.

The Kubernetes apiserver keeps three (`httplog`, `klog`, the audit backend) for exactly this reason.

## Rules

1. Levels: `error` = someone must look (5xx, panics, policy bundle stale); `warn` = degraded but handled (retry succeeded, limiter fail-open); `info` = lifecycle and access log; `debug` = off in production, enabled per request via a header only for operators.
2. Never log: `Authorization`, cookies, tokens (log `TokenHash`), request/response bodies, query strings by default (identifiers and filters are PII), stack traces at `info`.
3. Stable keys, dotted namespaces where OpenTelemetry semantic conventions exist (`http.method`, `http.status`, `url.path`, `client.ip`), snake_case otherwise.
4. Errors are logged **once**, at the boundary that handles them (the access log for 5xx, `Recover` for panics); inner layers wrap with `%w` and return. ardanlabs/service's `mid.Errors` logs once with the source file/function captured in the error and converts to the envelope.
5. Panics log the stack: chi, Traefik and Kratos all capture `runtime.Stack` with a 64 KiB buffer (`const size = 64 << 10`).
6. Log volume is a cost and a DoS vector: sample `debug`, never log per-byte of streams, and cap the length of any user-controlled field.

## Azure

Container Apps and App Service collect stdout; ship JSON lines to Log Analytics (ContainerAppConsoleLogs / AppServiceConsoleLogs) and query with KQL on the parsed fields. Keep one line per event; multi-line stack traces go in a single `stack` field.
