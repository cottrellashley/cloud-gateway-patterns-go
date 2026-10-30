# Testing

**Pattern:** table-driven unit tests for logic, `httptest` for every handler and middleware, real network tests for the proxy and streaming, deterministic clocks injected as functions, goroutine-leak detection, load and chaos tests before production, and the conformance suite as a registration gate.

## What this repository's tests do (copy the shapes)

| Test | Technique |
| --- | --- |
| `middleware_test.go` chain order | record names in a slice through a fake chain |
| `middleware_test.go` RealIP | craft `RemoteAddr` and `X-Forwarded-For` to prove spoof resistance |
| `proxy_test.go` | two `httptest.Server`s, one closed to produce dial errors; assert retry only for idempotent methods and passive removal of the dead one |
| `auth_test.go` | generate RSA/EC keys, sign tokens in the test, serve JWKS from `httptest`, count fetches with an `atomic.Int32` to prove throttling and single refresh on rotation |
| `resilience_test.go` breaker | inject `now` to drive timeouts; record `OnStateChange` transitions |
| `ratelimit_test.go` | inject `now` into the keyed limiter and the memory counter |
| `sse_test.go` | real `httptest.Server`, `bufio.Scanner` over the response body, `Last-Event-ID` replay and heartbeat assertions |
| `lro_test.go` | poll the store until a status; blocking channel to hold a worker while testing cancel and 304 |
| `shedding_test.go` | goroutines holding slots, sleeps to order arrivals (keep these short and deterministic) |
| `config_test.go` watcher | loader that fails first then succeeds; assert last-good retention |

## Rules

1. **Inject the clock** (`now func() time.Time`) in anything time-dependent; never `time.Sleep` to wait for expiry. Go 1.25's `testing/synctest` can run goroutines in a fake-time bubble for the cases that still need waiting.
2. **`httptest.NewRecorder` for handlers, `httptest.NewServer` for anything with a transport** (proxy, SSE, WebSocket, JWKS) — the stdlib's own server is the most faithful mock of itself.
3. **Table tests** (Uber guide "Test Tables"): name every case; one assertion message with the inputs.
4. **Leak detection**: `go.uber.org/goleak` in `TestMain` for packages that start goroutines (proxy, lro, sse, lifecycle).
5. **Race detector always**: `go test -race ./...` in CI; the middleware chain and the hot config swap are the places races hide.
6. **Fuzz the parsers**: `traceparent`, cursors, JWS compact parsing, SSE `Last-Event-ID`, idempotency keys — `go test -fuzz` with corpus seeds from the tests.
7. **Contract tests against real backends** in a nightly job: Snowflake trial account, Airflow dev deployment; failures open incidents, not silent schema drift.
8. **Load tests** (`vegeta`, `k6`, `ghz` for gRPC, `bombardier`) with the SLO as the pass criterion; run them against a canary before every release.
9. **Chaos** (`Shopify/toxiproxy` between the gateway and upstreams: latency, timeouts, resets; Azure Chaos Studio for infrastructure) to prove retries, breakers and shedding behave.
10. **Golden files for the envelope and the access-log line** so contract changes are visible in diffs.

## Conformance as a gate

Providers/services are admitted only when the conformance suite passes (Atlas spec section 18): every declared error code produced, undeclared ones never, cancellation observed within the bound, deadlines cancel downstream work, outputs validate against their schema. Run it in the provider's CI and again at registration.

## CI

```
go vet ./... && go test -race -count=1 ./... && go test -run=^$ -bench=. -benchtime=1x ./... && gofmt -l . | tee /dev/stderr | wc -l | grep -q '^0$'
```

plus `govulncheck ./...` and a linter (`golangci-lint` with `errcheck`, `staticcheck`, `bodyclose`, `noctx`, `gosec`).
