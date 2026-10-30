# AGENTS.md — how to use this repository

You are building or reviewing a Go cloud gateway or a service behind it. This repository is the pattern library. Follow it unless a documented reason says otherwise.

## Navigation

1. Start with `docs/01-architecture/front-door-and-topology.md` and `docs/01-architecture/request-lifecycle-and-middleware-order.md`.
2. Open `cmd/gateway/main.go` to see every pattern wired together; copy its shape for a new process.
3. For a specific concern, open the doc from the table below. Each doc has: the pattern in one sentence, the upstream evidence (file + pinned commit + excerpt), the implementation in `patterns/`, rules, and a checklist.
4. Import the package or copy it. Every package compiles with the standard library only and has tests you can keep.

## Decision table

| I need to… | Use | Doc |
| --- | --- | --- |
| start/stop a process cleanly | `lifecycle.Group`, `httpserver.Run` | 02-server/http-server-lifecycle |
| order middleware | `middleware.Chain` in the documented order | 01-architecture/request-lifecycle-and-middleware-order |
| return an error to a client | `apierror.Write(w, err)`; never `http.Error` | 07-services/errors |
| forward to a service | `proxy.New` + `lb.PowerOfTwoChoices`/`WeightedRoundRobin` | 02-server/reverse-proxy, 03-traffic/load-balancing |
| know the real client IP | `middleware.RealIP(trustedCIDRs, "X-Azure-ClientIP")` | 01-architecture/front-door-and-topology |
| bound a request in time | `middleware.Deadline` and pass `r.Context()` everywhere | 01-architecture/request-lifecycle-and-middleware-order |
| protect from overload | `shedding.InFlight`, `shedding.NewAdmission` | 03-traffic/load-shedding-and-admission |
| limit a client | `ratelimit.Keyed` (per replica) or `ratelimit.FixedWindow` on Redis (cross replica) | 03-traffic/rate-limiting-and-quotas |
| call a dependency safely | `resilience.Retry` + `resilience.Breaker` + `Budget` | 03-traffic/retries-backoff-circuit-breakers |
| validate a bearer token | `auth.NewKeySet`, `auth.Verifier`, `auth.Middleware` | 04-identity/bearer-tokens-and-jwks |
| authorize | PEP middleware shape + Cedar/OPA | 04-identity/authorization-policy |
| keep browser user state | `session.Manager` + `http.CrossOriginProtection` | 04-identity/sessions-and-user-state |
| make a write safe to retry | `idempotency.Middleware` + Idempotency-Key | 04-identity/idempotency |
| run something longer than a request | `lro.Manager` + `WriteAccepted`/`GetHandler`/`CancelHandler` | 06-data-and-async/long-running-operations |
| page a list | `pagination.Codec` | 06-data-and-async/pagination |
| stream to a client | `sse.Serve` with `sse.Ring` and `sse.Sink` | 02-server/streaming-sse-and-websockets |
| hot-reload routes/policies | `config.Hot` + `config.Watch` | 03-traffic/dynamic-configuration |
| expose health | `health.Handler` for `/livez` and `/readyz` | 03-traffic/health-checks |
| log/trace/measure | `telemetry.NewLogger`, `Propagate`, `AccessLog`, `RequestMetrics` | 05-observability/* |
| move big data | plan + ticket, Arrow Flight / Parquet, never through the gateway | 06-data-and-async/arrow-flight-data-plane |
| run background work | durable queue (River shape) + LRO record | 06-data-and-async/background-jobs |
| deploy on Azure | Container Apps/App Service + Front Door, managed identity | 09-azure/* |

## Non-negotiable rules

1. One error envelope (`apierror`). No `http.Error`, no ad-hoc JSON errors, no internal details in messages.
2. Every outbound call takes `ctx` and is bounded by the request deadline.
3. No unbounded inputs: bodies, headers, cursors, keys, buffers, goroutines, map sizes.
4. Retry only idempotent work, only on retryable errors, never after `ctx` is done, always with a budget.
5. Readiness goes false before draining; shutdown is delay → drain → close.
6. Inbound `Authorization` is never forwarded; services receive a gateway-signed context.
7. Trust forwarding headers only from configured proxy ranges.
8. Logs: no secrets, no bodies, no query strings by default; request ID and trace ID on every line.
9. Route labels (`r.Pattern`) for metrics, never raw paths.
10. Bulk data bypasses the gateway with a ticket.

## When you extend this repository

* Add a package under `patterns/` with tests and zero third-party dependencies, or document why a dependency is required.
* Add a doc with the same five sections and cite the upstream file at a pinned commit (`docs/SOURCES.md` lists the commits read).
* Run `make check` (gofmt, vet, race tests) before committing.
* Quote upstream code only in short excerpts with attribution; licences are listed in `NOTICE.md`.
