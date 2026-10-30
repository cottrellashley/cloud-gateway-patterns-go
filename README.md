# cloud-gateway-patterns-go

Battle-tested patterns for building a cloud API gateway ("front door") and the services behind it in Go — every pattern traced to the real source code of a production system (Kubernetes apiserver, Traefik, Caddy, grpc-go, chi, Pomerium, Oathkeeper, Envoy ratelimit, Azure SDK, Arrow Flight, River, and the Go standard library), with a compiled, tested Go implementation you can import or copy.

Read it like this:

1. `AGENTS.md` — how to navigate and the decision table ("I need X → use Y").
2. `docs/01-architecture/` — the topology and the handler-chain order.
3. `cmd/gateway/main.go` — the whole front door wired in ~150 lines.
4. The doc for the thing you are building; each doc links to the upstream file at a pinned commit and to the package here.

## Packages (`patterns/`)

| Package | What it gives you | Upstream evidence |
| --- | --- | --- |
| `apierror` | one error envelope (code, message, retryable, retryAfter, details), status mapping, RPC code mapping | Azure REST guidelines, connect-go |
| `lifecycle` | run group (first actor to stop interrupts all), signal actor, readiness flag | oklog/run, kratos, Traefik safe.Pool |
| `httpserver` | server timeouts, HTTP/2 + h2c, readiness-off → delay → drain → close | net/http, Caddy, Traefik, Kubernetes |
| `middleware` | chain, recover, request ID, trusted-proxy real IP, body limit, deadline, recording writer | chi, Caddy trusted_proxies, Kubernetes |
| `proxy` | reverse proxy with production transport, X-Forwarded policy, safe retries, passive health, envelope errors | net/http/httputil, Caddy, Traefik, Skipper |
| `lb` | weighted round robin (EDF heap), power-of-two-choices, upstream health/in-flight | Traefik wrr/p2c, Caddy hosts |
| `health` | `/livez` `/readyz` with named, timed checks and the Kubernetes report format | Kubernetes healthz, Traefik, Caddy |
| `resilience` | gRPC backoff, retry with budget and Retry-After, gobreaker state machine | grpc-go, go-retryablehttp, Azure SDK, gobreaker |
| `ratelimit` | token bucket (x/time/rate semantics), keyed TTL map, fixed window on a counter, IETF headers | x/time/rate, Traefik, Envoy ratelimit |
| `shedding` | admission with backlog, read/write in-flight limits with exemptions, drain rejections | chi Throttle, Kubernetes maxinflight/WithRetryAfter |
| `auth` | bearer JWT verification (RS/PS/ES), JWKS cache with throttled refresh, principal, scope guard | go-oidc, jwx, Oathkeeper, Kubernetes |
| `session` | AES-GCM cookie handle + server-side session store, rotation, clear | Pomerium |
| `telemetry` | slog setup, context logger, W3C traceparent, access log, metrics recorder | otelhttp, promhttp, Kubernetes httplog |
| `lro` | 202 + Operation-Location, status monitor with ETag/304, cooperative cancel, idempotent start, retention | Azure REST guidelines, ARM, MCP Tasks |
| `pagination` | signed, input-bound, expiring cursors; page shape | Azure guidelines, Kubernetes continue tokens |
| `sse` | SSE upgrade, events, heartbeat, Last-Event-ID replay ring, bounded sink | go-sse, httputil, MCP SDK |
| `idempotency` | Idempotency-Key replay / reuse / in-progress semantics | IETF draft, Stripe, Azure repeatability |
| `config` | atomic hot config with validation, versions, If-Match, watcher | Traefik, Caddy, go-control-plane |
| `cache` | generic singleflight, bounded TTL+LRU, stampede-safe GetOrLoad | x/sync/singleflight, go-oidc |
| `concurrency` | bounded error group, ForEach, semaphore, panic-safe goroutines | x/sync/errgroup, Traefik safe |

Zero third-party dependencies; `go test -race ./...` passes.

## Docs (`docs/`)

```
01-architecture/   front-door-and-topology, request-lifecycle-and-middleware-order
02-server/         http-server-lifecycle, routing, reverse-proxy, grpc-and-connect, streaming-sse-and-websockets
03-traffic/        dynamic-configuration, health-checks, load-balancing, retries-backoff-circuit-breakers,
                   load-shedding-and-admission, rate-limiting-and-quotas
04-identity/       bearer-tokens-and-jwks, azure-identity, sessions-and-user-state, authorization-policy, idempotency
05-observability/  structured-logging, tracing-and-propagation, metrics, request-telemetry-and-audit, runtime-tuning-and-profiling
06-data-and-async/ long-running-operations, pagination, events-and-cloudevents, arrow-flight-data-plane, background-jobs, caching
07-services/       service-skeleton, configuration, errors, testing, out-of-process-plugins
08-performance/    memory-and-allocations, concurrency, serialization
09-azure/          hosting-and-networking, state-stores
SOURCES.md         every upstream repository, the commit read, its licence
```

## Use

```sh
go get github.com/cottrellashley/cloud-gateway-patterns-go
go run ./cmd/gateway -upstream http://127.0.0.1:9000
```

or copy a package: each is self-contained with the standard library and the sibling `apierror`.

## Verify

```sh
make check   # gofmt, vet, race tests
```

## Licence

MIT for this repository. Excerpts quoted in `docs/` are from the projects listed in `NOTICE.md` under their own licences.
