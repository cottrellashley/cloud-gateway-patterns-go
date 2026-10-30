# Metrics

**Pattern:** RED metrics (rate, errors, duration) per route from one middleware; in-flight gauges; dependency metrics per upstream; bounded label cardinality; Prometheus exposition behind an internal listener. The gateway code depends on `patterns/telemetry.Recorder`; the Prometheus implementation is a thin adapter shown below.

## The reference middleware: promhttp

`client_golang/prometheus/promhttp/instrument_server.go` @315e020 is the canonical instrumentation; the shapes to copy:

```go
func InstrumentHandlerDuration(obs prometheus.ObserverVec, next http.Handler, opts ...Option) http.HandlerFunc {
    ...
    code, method := checkLabels(obs.MustCurryWith(hOpts.emptyDynamicLabels()))
    if code {
        return func(w http.ResponseWriter, r *http.Request) {
            now := time.Now()
            d := newDelegator(w, nil)          // response writer delegator that records status
            next.ServeHTTP(d, r)
            l := labels(code, method, r.Method, d.Status(), hOpts.extraMethods...)
            ...
            observeWithExemplar(obs.With(l), time.Since(now).Seconds(), hOpts.getExemplarFn(r))
        }
    }
    ...
}
func InstrumentHandlerInFlight(g prometheus.Gauge, next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.Inc(); defer g.Dec(); next.ServeHTTP(w, r) })
}
```

`WithExemplarFromContext` attaches the trace ID as an exemplar so a latency spike in Grafana links to a trace. The exposition handler (`http.go`) supports `MaxRequestsInFlight` and `Timeout` on `/metrics` itself — scrapers can overload a server too.

## Metric set for the gateway

| Metric | Type | Labels |
| --- | --- | --- |
| `http_server_request_duration_seconds` | histogram (OTel semconv name `http.server.request.duration`) | `method`, `route`, `status` |
| `http_server_requests_in_flight` | gauge | — |
| `http_server_request_body_bytes`, `response_body_bytes` | histogram | `route` |
| `gateway_upstream_request_duration_seconds` | histogram | `upstream`, `status` |
| `gateway_upstream_inflight` | gauge | `upstream` |
| `gateway_upstream_failures_total` | counter | `upstream`, `reason` (dial, timeout, 5xx) |
| `gateway_breaker_state` | gauge (0 closed,1 half,2 open) | `dependency` |
| `gateway_ratelimit_decisions_total` | counter | `policy`, `allowed` |
| `gateway_shed_total` | counter | `class`, `reason` |
| `gateway_lro_operations` | gauge | `status` |
| `gateway_auth_failures_total` | counter | `reason` |
| Go runtime | `go_*`, `process_*` from the default collectors | — |

Caddy (`modules/caddyhttp/metrics.go`) and Traefik (`pkg/middlewares/metrics`) ship essentially this set; Traefik additionally emits per-service health (`traefik_service_server_up`) from the health checker (`metricsHealthCheck` interface).

## Cardinality

* `route` is the matched pattern (`r.Pattern`), never the raw path. One wrong label and the time series count explodes.
* No `principal`, `tenant`, `request_id` labels on metrics — those belong in logs and traces. Tenant-level metrics use a bounded tenant ID set and an explicit allowlist.
* Status as a label is fine (bounded); so is `method` restricted to known methods (promhttp's `extraMethods` exists because unknown methods must be collapsed).

## Prometheus adapter for `telemetry.Recorder`

```go
type promRecorder struct {
    dur      *prometheus.HistogramVec
    inflight prometheus.Gauge
}
func NewPromRecorder(reg prometheus.Registerer) telemetry.Recorder {
    r := &promRecorder{
        dur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
            Name: "http_server_request_duration_seconds", Help: "Request duration by route and status.",
            Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
        }, []string{"method", "route", "status"}),
        inflight: prometheus.NewGauge(prometheus.GaugeOpts{Name: "http_server_requests_in_flight"}),
    }
    reg.MustRegister(r.dur, r.inflight)
    return r
}
func (r *promRecorder) ObserveRequest(method, route string, status int, d time.Duration, _, _ int64) {
    r.dur.WithLabelValues(method, route, strconv.Itoa(status)).Observe(d.Seconds())
}
func (r *promRecorder) InFlight(delta int) { r.inflight.Add(float64(delta)) }
```

Expose with `promhttp.HandlerFor(reg, promhttp.HandlerOpts{MaxRequestsInFlight: 2, Timeout: 5 * time.Second})` on the debug listener, not the public one.

## SLOs

Define three and alert on burn rate: availability (non-5xx / all, excluding 499), latency (p99 of `http_server_request_duration_seconds` per route class), and dependency success (upstream failures / upstream requests). Dashboards: RED per route, per upstream, breaker state, shed counts, in-flight vs limit.

## Azure

Azure Monitor managed Prometheus scrapes Container Apps; for App Service, push via the OpenTelemetry collector or expose on an internal port and scrape from a sidecar. Application Insights receives the same metrics via OTLP if you export through OpenTelemetry instead of Prometheus.
