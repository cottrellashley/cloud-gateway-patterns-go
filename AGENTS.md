# AGENTS.md — how to use this repository

You are building or reviewing a Go cloud gateway or a service behind it. This repository is the architecture and pattern library. Follow it unless a documented reason says otherwise.

## Navigation

0. Read `docs/00-principles/README.md` first. It states the fifteen principles these systems follow to scale, the one-line constitution, and the review checklist. The pattern docs are mechanisms *for* those principles; when in doubt the principle wins.
1. Then `docs/01-architecture/front-door-and-topology.md` and `docs/01-architecture/request-lifecycle-and-middleware-order.md`.
2. Open `cmd/gateway/main.go` (data plane: the chain in front of services) and `cmd/kernel/main.go` (control plane: resources, provider registry, reconciliation into a snapshot, `:execute` action, discovery) to see the patterns wired together; copy their shape for a new process.
3. For a specific concern, open the doc from the table below. Each doc has: the pattern in one sentence, the upstream evidence (file + pinned commit + excerpt), the implementation in `patterns/`, rules, and a checklist.
4. Import the package or copy it. Every package compiles with the standard library only and has tests you can keep.

## The constitution (hold this in your head)

1. Model it as a resource before you model it as an endpoint. If it has a name and a lifecycle it gets the envelope. (01)
2. Clients express intent; the platform converges. No RPC whose meaning is "do step 3 of a workflow". (02)
3. Control plane and data plane are different processes with different SLOs. Bytes never ride the control plane. (03)
4. New behaviour arrives as a registered module in an existing namespace, not as an `if` in the kernel. (04)
5. Write the contract first, run the compatibility rules on every change, gate anything not GA. (05)
6. If a client has to hard-code it, the platform has failed to describe it. Serve the schema. (06)
7. One group owns one set of data. Cross-group access goes through that group's contract or its events. (07)
8. Dependencies point inward. Transport code never contains a business rule; business code never imports a transport. (08)
9. Cross-cutting behaviour is a chain element with a documented position. Handlers never re-implement one. (09)
10. The edge authenticates; everything behind it trusts a signed principal and decides on attributes. (10)
11. Every write is repeatable and every read is conditional. No operation exists that is unsafe to retry. (11)
12. Anything slower than a request is an operation with an ID, a terminal state and a retention period. (12)
13. A dependency failure degrades one capability, never the catalog. Every change is make-before-break. (13)
14. Every hop logs the same correlation IDs and the request record is a schema, not a log line. (14)
15. There is one template, one review, one conformance suite and one deprecation calendar. (15)

Numbers refer to `docs/00-principles/NN-*.md`. Before proposing a design, run the fifteen-question review checklist in `docs/00-principles/README.md`.

## Decision table

| I need to… | Use | Doc |
| --- | --- | --- |
| model something the platform manages (capability, provider, policy, dataset, ticket) | `resource.Object[Spec,Status]` + `resource.Store` | 00-principles/01, 02 |
| converge actual state onto desired state (routes from manifests, operations, cleanups) | `reconcile.Queue` + `reconcile.Controller` + `Resync` | 00-principles/02 |
| add a provider kind / auth method / encoder / store backend | implement the namespace interface, `registry.Register`, load with `registry.LoadAs` | 00-principles/04 |
| decide what is control plane vs data plane | `config.Hot` snapshot on the data plane; catalog store only behind controllers | 00-principles/03 |
| judge whether an API change is allowed | the six compatibility rules; maturity + gates | 00-principles/05 |
| let agents find and understand capabilities | list → describe → schema → try; derived discovery | 00-principles/06 |
| start/stop a process cleanly | `lifecycle.Group`, `httpserver.Run` | 02-server/http-server-lifecycle |
| order middleware | `middleware.Chain` in the documented order | 01-architecture/request-lifecycle-and-middleware-order, 00-principles/09 |
| return an error to a client | `apierror.Write(w, err)`; never `http.Error` | 07-services/errors |
| forward to a service | `proxy.New` + `lb.PowerOfTwoChoices`/`WeightedRoundRobin` | 02-server/reverse-proxy, 03-traffic/load-balancing |
| know the real client IP | `middleware.RealIP(trustedCIDRs, "X-Azure-ClientIP")` | 01-architecture/front-door-and-topology |
| bound a request in time | `middleware.Deadline` and pass `r.Context()` everywhere | 01-architecture/request-lifecycle-and-middleware-order |
| protect from overload | `shedding.InFlight`, `shedding.NewAdmission`; one bulkhead per dependency | 03-traffic/load-shedding-and-admission, 00-principles/13 |
| limit a client | `ratelimit.Keyed` (per replica) or `ratelimit.FixedWindow` on Redis (cross replica) | 03-traffic/rate-limiting-and-quotas |
| call a dependency safely | `resilience.Retry` + `resilience.Breaker` + `Budget` | 03-traffic/retries-backoff-circuit-breakers |
| validate a bearer token | `auth.NewKeySet`, `auth.Verifier`, `auth.Middleware` | 04-identity/bearer-tokens-and-jwks |
| authorize | attribute deciders (Allow/Deny/NoOpinion) + Cedar/OPA; authorize before validate | 04-identity/authorization-policy, 00-principles/10 |
| keep browser user state | `session.Manager` + `http.CrossOriginProtection` | 04-identity/sessions-and-user-state |
| make a write safe to retry | `idempotency.Middleware` + Idempotency-Key; `If-Match` on resources | 04-identity/idempotency, 00-principles/11 |
| run something longer than a request | `lro.Manager` + `WriteAccepted`/`GetHandler`/`CancelHandler` | 06-data-and-async/long-running-operations, 00-principles/12 |
| page a list | `pagination.Codec` | 06-data-and-async/pagination |
| stream to a client | `sse.Serve` with `sse.Ring` and `sse.Sink` | 02-server/streaming-sse-and-websockets |
| hot-reload routes/policies | `config.Hot` + `config.Watch`; make before break | 03-traffic/dynamic-configuration |
| expose health | `health.Handler` for `/livez` and `/readyz` | 03-traffic/health-checks |
| log/trace/measure | `telemetry.NewLogger`, `Propagate`, `AccessLog`, `RequestMetrics` | 05-observability/*, 00-principles/14 |
| move big data | plan + ticket, Arrow Flight / Parquet, never through the gateway | 06-data-and-async/arrow-flight-data-plane |
| run background work | durable queue (River shape) + LRO record; workers as controllers | 06-data-and-async/background-jobs |
| start a new service | the template shape; conformance suite; API review before beta | 07-services/service-skeleton, 00-principles/15 |
| deploy on Azure | Container Apps/App Service + Front Door, managed identity | 09-azure/* |

## Non-negotiable rules (mechanism level)

1. One error envelope (`apierror`). No `http.Error`, no ad-hoc JSON errors, no internal details in messages.
2. Every outbound call takes `ctx` and is bounded by the request deadline.
3. No unbounded inputs: bodies, headers, cursors, keys, buffers, goroutines, map sizes, queues.
4. Retry only idempotent work, only on retryable errors, never after `ctx` is done, always with a budget.
5. Readiness goes false before draining; shutdown is delay → drain → close.
6. Inbound `Authorization` is never forwarded; services receive a gateway-signed context.
7. Trust forwarding headers only from configured proxy ranges.
8. Logs: no secrets, no bodies, no query strings by default; request ID and trace ID on every line.
9. Route labels (`r.Pattern`) for metrics, never raw paths.
10. Bulk data bypasses the gateway with a ticket.
11. `spec` is written by clients, `status` by controllers, through different operations; every write carries the version it was based on.
12. Extensions register in a namespace and go through provision → validate → cleanup; the kernel never switches on a kind string.

## When you extend this repository

* Add a package under `patterns/` with tests and zero third-party dependencies, or document why a dependency is required.
* Add a doc with the same five sections and cite the upstream file at a pinned commit (`docs/SOURCES.md` lists the commits read). A principle doc has: principle, why it matters at scale, evidence, rules, in this repository, applying it, anti-patterns.
* Run `make check` (gofmt, vet, race tests) before committing.
* Quote upstream code only in short excerpts with attribution; licences are listed in `NOTICE.md`.
