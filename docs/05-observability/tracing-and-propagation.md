# Tracing and context propagation

**Pattern:** W3C Trace Context (`traceparent`) on every request, extracted or started at the gateway, propagated to every upstream call and into tickets and audit records; OpenTelemetry (`otelhttp`, `otelgrpc`) for real spans when a backend exists; a trace ID in every log line regardless. Implemented in `patterns/telemetry` (`Propagate`, `Inject`, `TraceParent`).

## What otelhttp actually does

`otelhttp` (`instrumentation/net/http/otelhttp/handler.go` @71cf443) is the handler to use for spans; reading it shows the rules a gateway must follow even without OTel:

```go
ctx := h.propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
...
if h.publicEndpointFn != nil && h.publicEndpointFn(r.WithContext(ctx)) {
    opts = append(opts, trace.WithNewRoot())
    // Linking incoming span context if any for public endpoint.
    if s := trace.SpanContextFromContext(ctx); s.IsValid() && s.IsRemote() {
        opts = append(opts, trace.WithLinks(trace.Link{SpanContext: s}))
    }
}
...
ctx, span := tracer.Start(ctx, h.spanNameFormatter(h.operation, r), opts...)
defer span.End()
...
w = httpsnoop.Wrap(w, httpsnoop.Hooks{ Header, Write, ReadFrom, WriteHeader, Flush ... })   // keep Flusher/Hijacker working
...
span.SetStatus(h.semconv.Status(statusCode))
h.semconv.RecordMetrics(ctx, ...)   // http.server.request.duration etc.
```

* **Public endpoint**: for untrusted callers start a *new root* span and **link** the caller's span rather than becoming its child — otherwise any client can inject sampling decisions and bogus parents. Kubernetes passes `otelhttp.WithPublicEndpointFn(notSystemPrivilegedGroup)` for exactly this (`filters/traces.go` @ba832c1).
* **Span name from the route**, not the path: Kubernetes' formatter uses `RequestInfo` (`getSpanNameFromRequestInfo`) and falls back to `r.Method`; the gateway uses `r.Pattern`.
* **Writer wrapping** must preserve `Flush`/`Hijack`; `patterns/middleware.ResponseWriter` does.
* Kubernetes also stamps `audit-id` on the span so traces link to audit entries.

`otelhttp.NewTransport(base)` injects `traceparent` on outbound calls (`t.propagators.Inject(ctx, propagation.HeaderCarrier(r.Header))`); `otelgrpc.NewServerHandler()`/`NewClientHandler()` are the gRPC equivalents, installed as `grpc.StatsHandler`.

## Without a tracing backend

`patterns/telemetry.Propagate(trustIncoming)` extracts or creates a `traceparent`, stores it in the context, tags the context logger with `trace_id`/`span_id` and echoes `X-Trace-Id` so support can ask users for it. `Inject(ctx, req.Header)` writes a child `traceparent` on outbound calls. The format is the spec's version-00: `00-<16-byte trace id hex>-<8-byte span id hex>-<flags>`; all-zero IDs and version `ff` are invalid.

Trust rule mirrors otelhttp: for a public gateway, `Propagate(false)` starts a new trace and keeps the caller's ID only as a log field; for the internal hop between the Local Gateway and Cloud Gateway (authenticated), `Propagate(true)` continues the trace.

## Where the trace ID must appear

| Place | Why |
| --- | --- |
| Every log line (via the context logger) | correlate without a tracing backend |
| Access log and audit record | join across systems |
| Upstream requests (`traceparent`) and gRPC metadata | end-to-end trace |
| Data-plane tickets (`trace` claim) | DoGet shows up under the request's trace |
| Snowflake `QUERY_TAG`, Airflow run `conf`, GitHub `User-Agent` | find the external side of a request |
| Error envelope? **No** — the request ID is the user-facing handle; trace IDs are internal | |

## Sampling

Head-based sampling at the gateway (`sampled` flag) for volume; always sample errors and slow requests by recording spans locally and exporting on error (tail sampling in the collector). Kubernetes lets authenticated clients influence sampling by placing tracing after authentication — decide whether your clients may.

## Checklist

- [ ] `Propagate` right after `RealIP`; `Inject` in every HTTP client and `otelgrpc` on every gRPC client.
- [ ] Public endpoints: new root + link.
- [ ] Span names are route patterns.
- [ ] Trace ID in logs, audit, tickets and external query tags.
- [ ] OTLP exporter to Azure Monitor (Application Insights supports OTLP via the Azure Monitor OpenTelemetry distro) when a backend is available; the code above is unchanged either way.
