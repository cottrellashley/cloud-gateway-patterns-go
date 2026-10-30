# 14. Observability as a contract

**Principle:** correlation identifiers, the per-request record, audit events and metrics are part of the API contract, not an implementation detail. Clients may send a request ID and will get one back; every hop logs the same IDs; the request record has a versioned schema and is emitted by the chain (not by handlers); audit is append-only and complete; metrics use stable names and labels (route patterns, not raw paths); and the platform's own telemetry is itself exposed as data through the same resource model.

**Why it matters at scale:** when the gateway, twelve providers and a warehouse each log differently, a user's "my query failed" takes a day to trace. ARM makes `x-ms-request-id` and `x-ms-correlation-request-id` mandatory and tells providers they *must* log them; the Azure guidelines forbid rejecting unknown tracing headers; Kubernetes puts audit, latency trackers and HTTP logging in the chain so no handler can forget. Treating telemetry as a contract is what makes a platform supportable by people who did not write it.

## Evidence

**ARM required headers** ([common-api-details.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/common-api-details.md#L12)): the proxy always adds `x-ms-correlation-request-id` — "the resource provider *must* log this so that end-to-end requests can be correlated across Azure" — and `x-ms-client-ip-address`, principal headers, and `referer` (the public URI, so providers build `Location` headers correctly). Clients may send `x-ms-client-request-id` — "If the caller provides this header – the resource provider *must* log this with their traces". Every response must carry `x-ms-request-id`, "a unique identifier for the current operation, service generated … to facilitate debugging", plus `Date` and `Content-Type`.

**Azure REST guidelines** ([Guidelines.md §Distributed Tracing & Telemetry](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L1283)): "DO follow the Azure SDK client guidelines for supporting telemetry headers and Open Telemetry. DO NOT reject a call if you have custom headers you don't understand, and specifically, distributed tracing headers." `x-ms-client-request-id` (both directions, logged), `x-ms-request-id` (response, "the service must ensure that this value is globally unique"). The error envelope's `code` is designed for aggregation ("an invariant error code used for error troubleshooting, aggregation, and analysis" — [Addendum.md §Operation Resource](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L205)).

**Kubernetes chain** ([config.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/server/config.go#L1073)): `WithAuditInit` (outermost: an audit ID exists before anything else), `WithRequestReceivedTimestamp`, `WithLatencyTrackers` (per-filter latency), `WithHTTPLogging`, `WithTracing`, `WithAudit` (policy-driven audit events with user, verb, resource, response status, stages). None of these is a handler decision. The operations list in ARM ([proxy-api-reference.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/proxy-api-reference.md#L119)) exists partly for "audit history / records for management operations": the audit vocabulary is the operation vocabulary.

**Envoy** treats statistics as a first-class output: every cluster has `upstream_cx_overflow`, `upstream_rq_pending_overflow`, ejection counters, per-filter stats; access logs are configurable per listener with a fixed format vocabulary; the admin interface exposes config dumps and stats. Operators debug Envoy from its telemetry, not from its code.

**Caddy** wraps every handler module uniformly ([`wrapMiddleware`](https://github.com/caddyserver/caddy/blob/eb04ed5/modules/caddyhttp/routes.go#L334)) so "which module handled the request" is traced by the host; logging is structured (`zap`) with a `logging` app of its own.

**wild-workouts** logs and counts every command through decorators ([decorator](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/blob/8ecfcdf/internal/common/decorator/command.go)) with the command name derived from the type — one place, every command, the same fields.

## The rules

1. **Three identifiers, always:** a *request ID* (server-generated, globally unique, returned as a header on every response, including errors and 202s), a *client request ID* (if the client sent one, echoed and logged), and *trace context* (W3C `traceparent`, propagated to every upstream call and logged as trace/span IDs). Operations and tickets record the IDs of the request that created them.
2. **Never reject unknown headers**, especially tracing headers; never strip trace context on the way upstream.
3. **The request record is a schema**, versioned like any resource: `{v, ts, requestId, clientRequestId, traceId, spanId, principal{sub, tid, appid}, route (pattern), method, status, errorCode, durationMs, upstream{name, status, durationMs}, bytesIn, bytesOut, group, capability, mode, operationId, costUnits, policyDecision{decider, allow, reason, policyVersion}}`. Fields are added, never removed or retyped (principle 05).
4. **Emitted by the chain, not the handler.** Access log, metrics, audit and metering are chain elements that observe the request and response; handlers add structured attributes to the context (`capability`, `operationId`, `rows`) and nothing else.
5. **Audit is complete and append-only:** every authorization decision, every write, every action start/finish, every ticket mint/redeem, with the policy version that decided. Audit goes to a store with retention and immutability, separately from debug logs.
6. **Metrics have stable names and bounded labels:** route *pattern* not path, error *code* not message, group and capability names (bounded set), status class. RED (rate, errors, duration) per route and per upstream; saturation per bulkhead; queue depth per controller.
7. **No secrets, no bodies, no query strings by default** in logs; redaction is a function applied in one place.
8. **Health and readiness report named checks** so an operator can see which dependency failed, with reasons withheld from unauthenticated callers.
9. **Telemetry is data in the platform:** the request records are a dataset (`platform.telemetry.requests`) queryable through the same capabilities as everything else, with the same schema discipline.
10. **Error codes are for aggregation.** The same condition always yields the same code; dashboards group by code; messages are for humans and may change.

## In this repository

| Mechanism | Where |
| --- | --- |
| JSON logger with service/version/env, durations as strings, context logger | `patterns/telemetry.NewLogger`, `WithLogger`, `FromContext` |
| W3C `traceparent` parse/new/child, propagation with trust policy, `X-Trace-Id` | `patterns/telemetry.TraceParent`, `Propagate`, `Inject` |
| Request ID generation and echo | `patterns/middleware.RequestID` |
| Access log wrapping the writer (status, bytes, latency, route, IDs), probe skipping | `patterns/telemetry.AccessLog` |
| RED metrics by route pattern | `patterns/telemetry.RequestMetrics`, `RouteFrom`, `docs/05-observability/metrics.md` |
| Request record and audit schema | `docs/05-observability/request-telemetry-and-audit.md` |
| Named health checks with withheld reasons | `patterns/health` |
| Error codes as the aggregation key | `patterns/apierror` |
| Runtime tuning and profiling endpoints | `docs/05-observability/runtime-tuning-and-profiling.md` |

```go
// Handlers enrich; the chain emits. `telemetry.AccessLog` emits the standard fields today (request ID, trace,
// route pattern, status, bytes, latency). Domain attributes travel on the context logger and on a request
// record created by the chain at the same position, in this shape:
type RequestRecord struct {                       // v1: fields are only ever added
    V int; RequestID, ClientRequestID, TraceID string
    Principal struct{ Sub, Tid, AppID string }
    Route, Method string; Status int; ErrorCode string; DurationMs int64
    Group, Capability, Mode, OperationID string; Rows, BytesOut int64; CostUnits float64
    Policy struct{ Decider, Reason, Version string; Allow bool }
}
func (h *CapabilityHandler) Execute(w http.ResponseWriter, r *http.Request) {
    ctx := telemetry.With(r.Context(), "group", cap.Spec.Group, "capability", cap.Metadata.Name, "mode", mode)
    rec := recordFrom(ctx)                        // created by the chain; emitted when the handler returns
    rec.Group, rec.Capability, rec.Mode = cap.Spec.Group, cap.Metadata.Name, mode
    ...
    rec.OperationID, rec.Rows, rec.CostUnits = op.ID, rs.Rows, plan.Cost
}
// Audit is a separate, append-only stream keyed by the same request ID.
audit.Emit(ctx, audit.Event{V: 1, Action: "capabilities/execute", Decision: dec, Decider: name, PolicyVersion: pv,
    RequestID: middleware.RequestIDFrom(ctx), Principal: p.Ref(), Resource: capName})
```

## Applying it to a capability gateway

* **The request record in Cosmos DB is the metering and audit source of truth** the current runtime already keeps; the work is to make it a *schema* (`v`, additive changes only), to emit it from the chain rather than from handlers, and to add the IDs that tie it to the warehouse (operation ID = query tag) and to tickets (ticket ID, redemption record).
* **Correlation across the handoff seam:** the ticket carries the request ID and operation ID; the client's driver sets them as the query tag when it runs the statement; completion callbacks (or warehouse query history polled by a controller) fill in rows, bytes and cost on the operation. The record is complete even though the bytes never touched the gateway.
* **Per-group and per-vendor metrics** (`gateway_upstream_requests_total{upstream="vendor.marketdata_a"}`, breaker state, pool saturation) are what the blast-radius table (principle 13) is monitored against.
* **Agents get telemetry too:** every response carries `X-Request-Id`; errors carry `requestId` in the envelope; an agent can quote it, and a human can find the full record, the audit decision and the warehouse query in one search.
* **Telemetry as a dataset:** `platform.telemetry.requests` and `platform.telemetry.operations` are capabilities like any other, so usage, cost and error analysis are queries, not log spelunking.

## Anti-patterns

* Request IDs that exist only in the gateway's logs and not in the provider's.
* A middleware that rejects requests with "unknown" headers.
* `log.Printf("query failed: %v", err)` in a handler with no IDs.
* Metrics labelled with raw paths or user IDs (unbounded cardinality).
* Audit written by handlers, so the one that forgot is the one you need.
* Changing the meaning of a record field without a version bump.
