# Request lifecycle and middleware order

**Pattern:** a fixed handler chain, outermost first, where every layer can rely on the layers before it. The order is not a matter of taste; each position is forced by a dependency or a failure mode.

## The order the gateway uses

```
Recover            → nothing below can crash the process; 500 envelope if nothing was written
RequestID          → every later log line and error carries it
RealIP             → principal-less limits and logs need the real client
Propagate (trace)  → logs and upstream calls correlate from here on
AccessLog          → wraps the writer; logs status/bytes/latency when the handler returns
RequestMetrics     → RED metrics with the route label
ShutdownRetryAfter → draining replicas answer 429 + Connection: close before doing any work
MaxBytes           → bodies are bounded before anything reads them
Deadline           → the request context has a deadline before any I/O
Authn              → principal established before anything that keys on it
InFlight           → concurrency cap, after authn so operators can be exempt
RateLimit          → per-principal, needs the principal
Idempotency        → needs the principal (scoped keys) and a bounded body
Authz / policy     → needs principal and route
Handler / proxy
```

`patterns/middleware.Chain(h, a, b, c)` applies `a(b(c(h)))` — the same as chi's `chain()`:

```go
// go-chi/chi chain.go @9f8adb1
h := middlewares[len(middlewares)-1](endpoint)
for i := len(middlewares) - 2; i >= 0; i-- {
    h = middlewares[i](h)
}
```

## Evidence: Kubernetes `DefaultBuildHandlerChain`

The apiserver builds its chain inside-out; reading it bottom-up gives the outermost-first order ([`config.go`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/server/config.go)):

```go
handler = genericapifilters.WithAuthorization(handler, ...)
handler = genericfilters.WithMaxInFlightLimit(handler, ...)       // or WithPriorityAndFairness
handler = impersonation.WithImpersonation(handler, ...)
handler = genericapifilters.WithAudit(handler, ...)
handler = genericapifilters.WithTracing(handler, c.TracerProvider) // "comes after authentication so we can allow authenticated clients to influence sampling"
handler = genericapifilters.WithAuthentication(handler, ...)
handler = genericfilters.WithCORS(handler, ...)
handler = genericapifilters.WithWarningRecorder(handler)
handler = genericfilters.WithTimeoutForNonLongRunningRequests(handler, c.LongRunningFunc)
handler = genericapifilters.WithRequestDeadline(handler, ...)
handler = genericfilters.WithWaitGroup(handler, ...)
handler = genericfilters.WithRetryAfter(handler, c.lifecycleSignals.NotAcceptingNewRequest.Signaled())
handler = genericfilters.WithHTTPLogging(handler)
handler = genericapifilters.WithRequestInfo(handler, c.RequestInfoResolver)
handler = genericapifilters.WithRequestReceivedTimestamp(handler)
handler = genericfilters.WithPanicRecovery(handler, c.RequestInfoResolver)
handler = genericapifilters.WithAuditInit(handler)
return handler
```

So, outermost first: audit-ID init → **panic recovery** → received timestamp → request info (route) → HTTP logging → retry-after-when-draining → wait group (drain tracking) → **request deadline** → timeout → CORS → **authentication** → tracing → audit → impersonation → **max-in-flight** → **authorization** → handler. That is the order above, with the gateway's additions (RealIP, rate limit, idempotency) slotted by the same logic.

Three things the comments in that file say explicitly:

* "WithTimeoutForNonLongRunningRequests will call the rest of the request handling in a go-routine with the context with deadline. The go-routine can keep running, while the timeout logic will return a timeout to the client." — timeouts that *write on behalf of a running handler* need a goroutine and a race-safe writer. `patterns/middleware.Deadline` deliberately does not do that (see the timeout note below).
* "WithWarningRecorder must be wrapped by the timeout handler to make the addition of warning headers threadsafe."
* "WithRoutine ... must be after WithPanicRecover() to be protected from panics."

## Evidence: Traefik and Caddy

Traefik attaches middlewares per router with `alice` chains and always puts `recovery` outermost and `retry` innermost (closest to the service) so a retry re-enters only the service, not the auth/limit layers (`pkg/server/middleware`, `pkg/middlewares/retry`). Caddy's `reverse_proxy` handler is last in the route and its `handle_errors` route is the recovery boundary.

## Rules and why

| Rule | Why |
| --- | --- |
| Recover outermost, and re-panic `http.ErrAbortHandler` | chi: "we don't recover http.ErrAbortHandler so the response to the client is aborted" — net/http uses it to abort responses silently |
| RequestID before logging | the access log and every error must carry it; reuse the edge's ID when it is well-formed |
| RealIP before any limit or log | limits keyed by IP must use the true client; logs must not record the edge's IP |
| Trace before authn **unless** sampling should depend on the principal | Kubernetes puts tracing after authn to let authenticated clients influence sampling; for a public gateway start a new root span and link the caller's |
| Deadline before authn | token verification, JWKS fetches and policy calls are I/O and must be bounded |
| MaxBytes before idempotency and handlers | the fingerprint and the handler both read the body |
| Authn before in-flight limits | so exempt principals (operators) can be let through when the limiter is full, as `WithMaxInFlightLimit` does for `system:masters` |
| Rate limit after authn, before authz | per-principal keys exist; authz may be expensive and should not run for callers already over quota |
| Idempotency after authn | keys are scoped per principal so tenants cannot collide or probe each other's keys |
| Retry closest to the upstream | a retry must not re-run auth, limits or logging |

## Timeouts: context deadline vs. writing on behalf of the handler

Two valid designs exist; pick deliberately.

1. **Context deadline only** (`patterns/middleware.Deadline`, chi's `Timeout`): set `ctx` deadline, let the handler observe it, write 504 after the handler returns if it wrote nothing. No goroutine, no race, but a handler that ignores `ctx` runs long.
2. **Supervising goroutine** (`http.TimeoutHandler`, Kubernetes `timeoutHandler`): run the handler in a goroutine with a buffered/locked writer; on timeout write 504 immediately and let the goroutine finish in the background. Preempts bad handlers; costs a goroutine and a response buffer per request and needs the `timeoutWriter` dance to avoid concurrent writes.

The gateway uses (1) and makes every upstream call take `r.Context()`, which is what actually stops the work.
