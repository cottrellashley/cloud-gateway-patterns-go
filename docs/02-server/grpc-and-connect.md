# gRPC and Connect servers

**Pattern:** services expose RPCs with `connect-go` (plain `net/http`, same handler chain, gRPC-compatible on the wire) or `grpc-go` when gRPC-native features are required; the gateway speaks HTTP/JSON to clients and forwards to RPC services with the Connect protocol or gRPC. Interceptors are the middleware equivalent and are ordered by the same rules as `docs/01-architecture/request-lifecycle-and-middleware-order.md`.

## grpc-go server options that matter in production

From `grpc-go/server.go` and `keepalive/keepalive.go` @bc90736:

```go
grpc.NewServer(
    grpc.MaxConcurrentStreams(1024),         // per-connection stream cap
    grpc.MaxRecvMsgSize(16<<20),             // default 4 MiB
    grpc.ConnectionTimeout(10*time.Second),  // handshake/setup bound
    grpc.KeepaliveParams(keepalive.ServerParameters{
        MaxConnectionIdle:     5 * time.Minute,  // "closed by sending a GoAway" when idle
        MaxConnectionAge:      30 * time.Minute, // "A random jitter of +/-10% will be added ... to spread out connection storms"
        MaxConnectionAgeGrace: 30 * time.Second,
        Time:                  2 * time.Hour,    // server pings the client after this much inactivity (default 2h)
        Timeout:               20 * time.Second, // default
    }),
    grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
        MinTime:             5 * time.Minute, // default: clients pinging faster get GOAWAY
        PermitWithoutStream: false,
    }),
    grpc.ChainUnaryInterceptor(recovery, tracing, authn, ratelimit),
    grpc.ChainStreamInterceptor(...),
)
```

`MaxConnectionAge` with jitter is how a gateway keeps load balanced across a service that scales out: long-lived HTTP/2 connections otherwise pin all traffic to the replicas that existed at connect time.

Interceptor chaining is built once (`chainUnaryServerInterceptors`): the first interceptor passed is outermost, and `getChainUnaryHandler` builds the recursive closure — the same outermost-first order as HTTP middleware.

`GracefulStop()` stops accepting, sends GOAWAY and waits for in-flight RPCs; run it inside the lifecycle group with the same grace/close pair as HTTP.

## Health and reflection

`grpc-go/health/server.go` implements `grpc.health.v1.Health` with `Check`, `List` and `Watch`; `Watch` streams status changes and ends with `codes.Canceled` when the stream context is done. Register it and flip the service status with `SetServingStatus` from the same readiness flag the HTTP `/readyz` uses. Traefik's health checker supports `mode: grpc` and calls exactly this service (`healthcheck.go` `checkHealthGRPC`).

Reflection is off by default and is a disclosure surface; enable it only on internal listeners.

## Retries: service config, not code

grpc-go retries are configured per method in the service config (`examples/features/retry/client/main.go` @bc90736):

```json
"retryPolicy": {
  "MaxAttempts": 4,
  "InitialBackoff": ".01s",
  "MaxBackoff": ".01s",
  "BackoffMultiplier": 1.0,
  "RetryableStatusCodes": [ "UNAVAILABLE" ]
}
```

Only `UNAVAILABLE` (and sometimes `RESOURCE_EXHAUSTED` with a server push-back) is safely retryable; gRPC also applies **retry throttling** (a token bucket shared per server: `maxTokens`, `tokenRatio`) which `patterns/resilience.Budget` reproduces for HTTP calls.

go-grpc-middleware (`interceptors/` @e4b8359) gives the standard set: `recovery.UnaryServerInterceptor` (recover → error), `ratelimit.UnaryServerInterceptor(limiter)` returning `codes.ResourceExhausted`, `timeout.UnaryClientInterceptor(d)` (per-call `context.WithTimeout`), `retry` with `WithCodes`, `WithBackoff`, `WithPerRetryTimeout` ("You cannot automatically retry on Cancelled and Deadline").

## Connect

connect-go serves RPCs as ordinary `http.Handler`s: the handler negotiates the protocol from `Content-Type` (`connecthttp/handler.go` @5791e34 returns `405` with an `Allow` header for the wrong method and `415 Unsupported Media Type` for unknown content types), so it sits inside the exact same middleware chain as REST routes and works through any HTTP/1.1 or HTTP/2 proxy (Front Door included). Its error model is a code plus details; the HTTP mapping (`connectCodeToHTTP`) is what `patterns/apierror.HTTPStatusForRPCCode` reproduces so the gateway returns the same status a Connect client would compute:

| Code | HTTP |
| --- | --- |
| canceled | 499 |
| invalid_argument, out_of_range, failed_precondition | 400 |
| deadline_exceeded | 504 |
| not_found | 404 |
| already_exists, aborted | 409 |
| permission_denied | 403 |
| resource_exhausted | 429 |
| unimplemented | 501 |
| unavailable | 503 |
| unauthenticated | 401 |
| unknown, internal, data_loss | 500 |

Prefer Connect for new services: one port, one chain, browser-callable, gRPC-compatible when a client insists.

## Data plane note

Arrow Flight is gRPC; `arrow-go/arrow/flight/server.go` @56815b7 wraps `grpc.NewServer` and takes `ServerMiddleware{Stream, Unary}` interceptors plus a `CustomServerMiddleware` (`StartCall`/`CallCompleted`) so the ticket-verifying interceptor is a normal gRPC interceptor. See `docs/06-data-and-async/arrow-flight-data-plane.md`.

## Checklist

- [ ] Keepalive params with `MaxConnectionAge` set on every long-lived service.
- [ ] Health service registered and wired to readiness.
- [ ] Interceptors: recovery outermost, then tracing, authn (verify the gateway-signed context), limits.
- [ ] Retry policy in service config with `UNAVAILABLE` only; budget on.
- [ ] Connect for new services unless a feature forces gRPC-native.
