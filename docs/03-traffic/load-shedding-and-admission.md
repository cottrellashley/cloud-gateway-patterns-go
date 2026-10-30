# Load shedding and admission control

**Pattern:** bound concurrency explicitly, queue briefly, then reject with a clear status and `Retry-After`; exempt long-running requests and operators; reject early during shutdown. Implemented in `patterns/shedding`.

## Why explicit bounds

Go will happily accept 100 000 goroutines; the failure mode is not a crash but latency collapse and OOM. Every production server bounds in-flight work:

* Kubernetes `WithMaxInFlightLimit` (`server/filters/maxinflight.go` @ba832c1): two buffered channels, one for read-only and one for mutating verbs; long-running requests skip it; privileged groups bypass when full; everyone else gets `429 + Retry-After`:

  ```go
  select {
  case c <- true:
      ... handler.ServeHTTP(w, r)
  default:
      // at this point we're about to return a 429, BUT not all actors should be rate limited.  A system:master is so powerful
      // that they should always get an answer.  It's a super-admin or a loopback connection.
      if currUser, ok := apirequest.UserFrom(ctx); ok {
          for _, group := range currUser.GetGroups() {
              if group == user.SystemPrivilegedGroup { handler.ServeHTTP(w, r); return }
          }
      }
      metrics.RecordDroppedRequest(...)
      tooManyRequests(r, w, retryAfter)
  }
  ```
  The newer **API Priority and Fairness** filter replaces it with fair queuing across flows (`util/flowcontrol/fairqueuing/queueset/doc.go`: "a technique called 'fair queuing for server requests' ... multiple requests can be executing at once, and the service time is not known until the execution completes"). Reach for APF-style fairness only when one tenant can starve others; start with per-class limits plus per-principal rate limits.

* chi `Throttle` (`middleware/throttle.go` @9f8adb1): `Limit` processing tokens plus a `BacklogLimit` with `BacklogTimeout`; beyond the backlog, immediate rejection:

  ```go
  case btok := <-t.backlogTokens:
      defer func() { t.backlogTokens <- btok }()
      select { case tok := <-t.tokens: ... next.ServeHTTP(w, r); return; default: }
      timer := time.NewTimer(t.backlogTimeout)
      select {
      case <-timer.C: t.setRetryAfterHeaderIfNeeded(w, false); http.Error(w, errTimedOut, t.statusCode); return
      case <-ctx.Done(): ...
      case tok := <-t.tokens: ... next.ServeHTTP(w, r)
      }
  default:
      t.setRetryAfterHeaderIfNeeded(w, false); http.Error(w, errCapacityExceeded, t.statusCode)
  ```

* Traefik `inflightreq` middleware and Caddy `max_requests` per upstream do the same per route / per backend; grpc-go `MaxConcurrentStreams` does it per connection; Kratos ships a BBR-style adaptive limiter (`middleware/ratelimit`: `done, e := options.limiter.Allow(); ... done(DoneInfo{Err: err})`) that derives the limit from observed latency and throughput.

## Status codes

| Situation | Status | Who should react |
| --- | --- | --- |
| This replica is at capacity | `503` + `Retry-After` | the load balancer (try another replica) |
| This client exceeded its share | `429` + `Retry-After` + `RateLimit` headers | the client (slow down) |
| Replica is draining | `429`/`503` + `Retry-After` + `Connection: close` | both |

Kubernetes uses `429` for in-flight limits because there is no other replica to point the client at; a gateway behind an edge should use `503` for capacity so the edge's retry/failover logic engages, and keep `429` for per-principal quotas.

## The pattern

```go
admit := shedding.NewAdmission(limit, backlog, 200*time.Millisecond, http.StatusServiceUnavailable) // chi Throttle shape
inflight := shedding.NewInFlight(readLimit, writeLimit)                                              // apiserver shape
inflight.LongRunning = isStreamRequest
inflight.Exempt = isOperator
drain := shedding.ShutdownRetryAfter(func() bool { return !ready.Ready() }, 2, isProbe)              // apiserver WithRetryAfter
```

Sizing: `limit` ≈ (target p99 latency budget / mean service time) × cores, measured; `backlog` small (10–20 % of limit) and `backlogTimeout` well under the client's deadline so queued requests do not time out after being admitted.

## Shutdown rejections

Kubernetes `WithRetryAfter` (`server/filters/with_retry_after.go` @ba832c1) returns, once `NotAcceptingNewRequest` is signalled, "The apiserver is shutting down, please try again later." with `TearDownConnection: true`. `patterns/shedding.ShutdownRetryAfter` sets `Connection: close` and `Retry-After` so HTTP/1.1 keep-alive clients and HTTP/2 clients both move to another replica; probes are exempt so the platform can still read `/readyz`.

## Beyond limits: adaptive concurrency

When service time varies with load (databases, LLMs), fixed limits are either too tight or too loose. Options, in order of complexity: Kratos BBR limiter, failsafe-go `adaptivelimiter`/`adaptivethrottler`, Kubernetes APF. Measure first; a fixed limit plus P2C balancing solves most cases.

## Checklist

- [ ] Every listener has an in-flight cap; streams exempt and capped by connection count.
- [ ] Rejections carry `Retry-After` and the envelope; metrics count dropped requests per class.
- [ ] Operators/probes exempt from capacity rejections.
- [ ] Drain rejections include `Connection: close`.
