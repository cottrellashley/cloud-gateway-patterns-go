# 09. Cross-cutting concerns as ordered chains

**Principle:** every behaviour that applies to all (or a class of) requests — recovery, request identity, real client IP, tracing, logging, metrics, drain handling, body limits, deadlines, authentication, admission/in-flight limits, rate limits, idempotency, authorization, mutation then validation — is a *filter* in a declared chain with a fixed, documented position. The chain is built from a list (configuration or code) by one builder; handlers and providers never re-implement a concern; the order is derived from dependencies between concerns and from failure modes, and is the same in every process.

**Why it matters at scale:** with hundreds of routes and dozens of providers, any concern implemented "in the handler" is implemented inconsistently or forgotten. Kubernetes' apiserver puts twenty-odd filters in one function; Envoy makes the filter list the unit of configuration and derives encoder order from decoder order; Caddy compiles routes into a middleware stack. All three let the position of a concern be reasoned about once.

## Evidence

**Kubernetes `DefaultBuildHandlerChain`** ([config.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/server/config.go#L1073)) wraps the API handler from the inside out; reading the function top to bottom gives inner → outer, so the request sees (outer → inner):

```
WithAuditInit → WithPanicRecovery → WithMuxAndDiscoveryComplete → WithRequestReceivedTimestamp
→ WithRoutine → WithLatencyTrackers → WithHTTPLogging → WithRetryAfter (draining) → WithHSTS → WithCacheControl
→ WithProbabilisticGoaway → WithWatchTerminationDuringShutdown → WithWaitGroup → WithRequestDeadline
→ WithTimeoutForNonLongRunningRequests → WithWarningRecorder → WithCORS → WithAuthentication (failed → Unauthorized + audit)
→ WithTracing → WithAudit → WithImpersonation → WithPriorityAndFairness | WithMaxInFlightLimit
→ WithAuthorization → apiHandler (→ admission: mutating then validating → storage)
```

Each position is forced: recovery is outermost so nothing crashes the process; the received timestamp precedes latency tracking; `WithRetryAfter` answers draining replicas before any work; the deadline exists before anything does I/O; authentication precedes audit (audit records the user), in-flight limits (so exempt users can bypass) and authorization; priority-and-fairness sits after authentication because it classifies by user. Admission ([chain.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/admission/chain.go#L31)) runs all `Admit` (mutating) before all `Validate`, so validators see the final object.

**Envoy filter ordering** ([http_filters.rst §Filter ordering](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/http/http_filters.rst#L49)):

> If filters are configured in the following order (and assuming all three filters are decoder/encoder filters): A, B, C — the connection manager will invoke decoder filters in the order: A, B, C. On the other hand, the connection manager will invoke encoder filters in the **reverse** order: C, B, A. … The last configured filter has to be a terminal filter … This is most likely the router filter.

The list *is* the configuration; a filter's position is a deployment decision, not a code change; response processing mirrors request processing automatically.

**Caddy** compiles a route list into a middleware stack ([routes.go `Compile`](https://github.com/caddyserver/caddy/blob/eb04ed5/modules/caddyhttp/routes.go#L239)):

```go
func (routes RouteList) Compile(next Handler) Handler {
    mid := make([]Middleware, 0, len(routes))
    for _, route := range routes { mid = append(mid, wrapRoute(route)) }
    stack := next
    for _, middleware := range slices.Backward(mid) { stack = middleware(stack) }
    return stack
}
```

and every handler module is wrapped uniformly ([`wrapMiddleware`](https://github.com/caddyserver/caddy/blob/eb04ed5/modules/caddyhttp/routes.go#L334)) so tracing of "which module handled this" is a property of the chain, not of each module.

**Traefik** builds a chain from names ([middlewares.go `buildConstructor`](https://github.com/traefik/traefik/blob/38d53dd/pkg/server/middleware/middlewares.go#L105)): a router references `middlewares: [auth@file, ratelimit@file]` and the builder resolves each name to a constructor, detects cycles, and composes with `alice`. Position is configuration.

**wild-workouts** applies the same idea one layer down: command handlers are wrapped by logging and metrics decorators in a fixed order ([decorator/command.go](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/blob/8ecfcdf/internal/common/decorator/command.go)), so no handler logs or counts itself.

**gRPC interceptors** (`docs/02-server/grpc-and-connect.md`) are the same chain for RPC: recovery, tracing, auth, rate limit, timeout in a declared order.

## The rules

1. **One chain definition per process class**, in one file, as a list. The gateway's order is in `docs/01-architecture/request-lifecycle-and-middleware-order.md` and `cmd/gateway/main.go`; services use the same order minus what the gateway already did.
2. **Position is derived from dependencies, written down next to the list.** "Authn before rate limit because the limit is per principal", "Deadline before any I/O", "Access log wraps the writer so it sees status and bytes". A reviewer checks the reason, not the taste.
3. **Handlers and providers never re-implement a chain concern.** No `recover()`, no `X-Request-ID` parsing, no bearer parsing, no `time.After` deadlines, no ad-hoc logging of request/response in a handler. If a handler needs it, it is missing from the chain.
4. **Admission is two phases:** mutate (defaults, normalisation, injected fields) then validate (schema, invariants, policy). Validators see the final object; mutators never run after a validator.
5. **Response-side concerns mirror request-side order** (Envoy encoder rule): the filter that first sees the request is the last to see the response.
6. **Exemptions are properties of the filter, not bypasses around it.** Health probes skip the access log via an option; operators are exempt from in-flight limits via a predicate. Nothing is mounted "outside the chain".
7. **The chain is observable:** each filter has a name, a latency tracker and a metric; failures carry the filter name in the error `Hop`/detail so "where did this 429 come from" is answerable.
8. **Long-running requests are classified once** (a `LongRunningFunc`) and every filter that must treat them differently (timeouts, in-flight limits, drain) consults that single classification.

## In this repository

| Mechanism | Where |
| --- | --- |
| Chain composition identical to chi/Caddy (`a(b(c(h)))`) | `patterns/middleware.Chain` |
| The documented gateway order with reasons | `docs/01-architecture/request-lifecycle-and-middleware-order.md`, `cmd/gateway/main.go` |
| Outermost recovery, request ID, trusted real IP, body limit, deadline | `patterns/middleware` |
| Draining answer before work | `patterns/shedding.ShutdownRetryAfter` |
| Trace propagation, access log wrapping the writer, route-labelled metrics | `patterns/telemetry` |
| Authn → in-flight → rate limit → idempotency → authz | `patterns/auth`, `patterns/shedding`, `patterns/ratelimit`, `patterns/idempotency`, `docs/04-identity/authorization-policy.md` |
| Long-running classification shared by filters | `patterns/shedding.InFlight` (`LongRunning`, `Exempt` predicates) |
| gRPC/Connect interceptor chain | `docs/02-server/grpc-and-connect.md` |

```go
// The list is the contract (this is cmd/gateway/main.go). Build it once; mount everything under it.
inflight := shedding.NewInFlight(400, 100)
handler := middleware.Chain(mux,
    middleware.Recover(log),                                              // nothing below crashes the process
    middleware.RequestID("X-Request-Id"),                                 // every later line carries it
    middleware.RealIP(cidrs, "X-Azure-ClientIP"),                         // limits/logs need the real client
    telemetry.Propagate(false),                                           // trace context before logging
    telemetry.AccessLog(log, telemetry.AccessLogOptions{Skip: isProbe}),  // wraps the writer: sees status/bytes
    telemetry.RequestMetrics(recorder),
    shedding.ShutdownRetryAfter(func() bool { return !ready.Ready() }, 2, isProbe), // draining replicas answer first
    middleware.MaxBytes(8<<20),                                           // bodies bounded before anyone reads
    middleware.Deadline("X-Request-Deadline", 30*time.Second, 5*time.Minute),
    auth.Middleware(verifier, "gateway"),                                 // principal established
    inflight.Middleware,                                                  // after authn: exemptions by principal
    ratelimit.Middleware(perUser, userKey, "per-user", true),
    idempotency.Middleware(idempotency.Options{Store: idemStore, Scope: userKey}), // needs principal + bounded body
    authz.Middleware(deciders),                                           // PEP shape from docs/04-identity/authorization-policy.md
)
```

## Applying it to a capability gateway

* **Capability-specific admission is a chain too**, run by the catalog controller on every manifest generation: `defaults` (fill `maturity: alpha`, normalise names) → `schema` (JSON Schema validity) → `provider-features` (modes ⊆ provider features) → `compatibility` (diff against previous generation) → `policy` (vendor groups must declare entitlements) → `naming` (group prefix). Mutators first, validators second, each named in the resulting condition's `reason`.
* **Request-side additions sit at documented positions:** `entitlement` (vendor data per-user licence check) is an authorization-phase filter that runs after `policy`; `metering` (count rows/bytes/cost per principal and capability) is a response-side concern that mirrors `RequestMetrics`; `reader-schema` projection is a terminal-side encoder step, never inside a provider.
* **Both modes go through the same chain.** `handoff` is not "lighter"; it passes the same authn, authz, entitlement and metering filters and differs only at the terminal handler (mint ticket vs run query). A ticket minted outside the chain is a policy bypass.
* **Providers behind the gateway run a reduced chain**: recovery, request ID (propagated), trace, access log, deadline from the propagated header, gateway-signature verification instead of bearer validation, in-flight limit. No per-user rate limiting (the gateway did it), no token parsing.

## Anti-patterns

* A provider handler that calls `jwt.Parse` on the incoming header.
* `defer recover()` in forty handlers.
* Rate limiting mounted before authentication so limits are per IP for authenticated agents.
* A `/health` route registered outside the chain so it has no request ID in its logs.
* Validation that runs before defaulting, so the validator rejects what the defaulter would have fixed.
* "This one route is special" exceptions mounted around the chain rather than expressed as a filter option.
