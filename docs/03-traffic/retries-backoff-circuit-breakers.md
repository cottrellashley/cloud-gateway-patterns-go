# Retries, backoff, circuit breakers

**Pattern:** retry only what is safe, with exponential backoff and jitter, honouring `Retry-After`, bounded by the request deadline **and** by a shared retry budget; put a circuit breaker in front of each dependency so a dead one fails fast. Implemented in `patterns/resilience`.

## Backoff

grpc-go's algorithm and defaults (`backoff/backoff.go`, `internal/backoff/backoff.go` @bc90736):

```go
var DefaultConfig = Config{ BaseDelay: 1.0 * time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 120 * time.Second }

func (bc Exponential) Backoff(retries int) time.Duration {
    if retries == 0 { return bc.Config.BaseDelay }
    backoff, max := float64(bc.Config.BaseDelay), float64(bc.Config.MaxDelay)
    for backoff < max && retries > 0 { backoff *= bc.Config.Multiplier; retries-- }
    if backoff > max { backoff = max }
    // Randomize backoff delays so that if a cluster of requests start at the same time, they won't operate in lockstep.
    backoff *= 1 + bc.Config.Jitter*(rand.Float64()*2-1)
    ...
}
```

cenkalti/backoff uses the same proportional randomisation (`getRandomValueFromInterval`, defaults `InitialInterval 500ms, RandomizationFactor 0.5, Multiplier 1.5, MaxInterval 60s`). go-retryablehttp's `LinearJitterBackoff` is the alternative when you want bounded, evenly spread delays. `patterns/resilience.Backoff.Delay(attempt)` is the grpc-go formula.

## What to retry

go-retryablehttp (`client.go` @fd00458) is the clearest statement of the HTTP rule:

```go
func DefaultRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
    // do not retry on context.Canceled or context.DeadlineExceeded
    if ctx.Err() != nil { return false, ctx.Err() }
    ...
}
func baseRetryPolicy(resp *http.Response, err error) (bool, error) {
    if err != nil {
        if v, ok := err.(*url.Error); ok {
            // Don't retry if the error was due to too many redirects / invalid protocol scheme / invalid header / TLS cert verification failure
            ...
        }
        return true, nil   // The error is likely recoverable so retry.
    }
    // 429 Too Many Requests is recoverable.
    if resp.StatusCode == http.StatusTooManyRequests { return true, nil }
    // retry on 500-range responses ... except 501
    if resp.StatusCode == 0 || (resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented) { return true, ... }
    return false, nil
}
func DefaultBackoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
    if resp != nil && (resp.StatusCode == 429 || resp.StatusCode == 503) {
        if sleep, ok := parseRetryAfterHeader(resp.Header["Retry-After"]); ok { return sleep }
    }
    ...
}
```

The Azure SDK's retry policy (`sdk/azcore/runtime/policy_retry.go` @e948fd2) adds three rules worth copying:

* a **per-try timeout** (`options.TryTimeout`) distinct from the overall context;
* `Retry-After` is honoured but **capped**: `if delay > options.MaxRetryDelay { ... "Retry-After delay exceeds MaxRetryDelay" ... return }` — a huge Retry-After means "give up", not "sleep an hour";
* the body is wrapped so it can be rewound for each try (`req.RewindBody()`), and the response is drained before retrying "so nothing is leaked".

`patterns/resilience.Retry` implements: never retry after `ctx.Err()`, `Retryable(err)` predicate, `PerAttemptTimeout`, `RetryAfterError` honoured and capped by `MaxDelay`, and the budget below.

For proxied requests the rule is stricter (see `docs/02-server/reverse-proxy.md`): idempotent method, replayable body, nothing written yet.

## Retry budget (retry throttling)

gRPC's retry throttling is a token bucket per server: each failure costs 1 token, each success refunds `tokenRatio`, retries are allowed only while `tokens > maxTokens/2`. It is the only mechanism that prevents a **retry storm** — per-call `MaxAttempts` cannot, because 10 000 concurrent callers × 3 attempts is still 30 000 requests at a dying dependency. `patterns/resilience.Budget` is that bucket; its test shows retries stopping after the bucket drains.

## Circuit breaker

sony/gobreaker's state machine (`v2/gobreaker.go` @fed8e9e), reproduced in `patterns/resilience.Breaker`:

```go
func (cb *CircuitBreaker[T]) beforeRequest() (uint64, uint64, error) {
    ...
    if state == StateOpen { return generation, age, ErrOpenState }
    else if state == StateHalfOpen && cb.counts.validRequests() >= cb.maxRequests { return generation, age, ErrTooManyRequests }
    cb.counts.onRequest()
    ...
}
func (cb *CircuitBreaker[T]) afterRequest(previous uint64, age uint64, err error) {
    ...
    state, generation, _ := cb.currentState(now)
    if generation != previous { return }       // result from an old generation: ignore
    ...
}
func (cb *CircuitBreaker[T]) onFailure(state State, age uint64, now time.Time) {
    switch state {
    case StateClosed: cb.counts.onFailure(age); if cb.readyToTrip(cb.counts.Counts) { cb.setState(StateOpen, now) }
    case StateHalfOpen: cb.setState(StateOpen, now)
    }
}
func (cb *CircuitBreaker[T]) currentState(now time.Time) (State, uint64, uint64) {
    switch cb.state {
    case StateClosed: if !cb.expiry.IsZero() && cb.expiry.Before(now) { cb.toNewGeneration(now) } ...
    case StateOpen:   if cb.expiry.Before(now) { cb.setState(StateHalfOpen, now) }
    }
    ...
}
```

Closed → Open when `ReadyToTrip(counts)` (default: more than 5 consecutive failures); Open → Half-open after `Timeout` (default 60 s); Half-open admits `MaxRequests` trial calls and closes after that many consecutive successes, or re-opens on the first failure. The **generation** counter discards results from calls that started before a state change.

oxy's `cbreaker` adds a *recovering* state with a ramp-up: while recovering, a `ratioController` admits an increasing fraction of requests (`allowRequest`: "would we satisfy the target ratio if we allow this request?") instead of a fixed number of probes. Use that variant for very high-QPS dependencies where `MaxRequests` probes are too few to judge health.

Caddy's `Upstream.Healthy()` consults `u.cb.OK()`: a tripped breaker takes the upstream out of the balancer — breaker and load balancer compose through availability, not through errors.

## Composition order

failsafe-go's model (README @2942d1a: "wrapping functions with one or more resilience policies, which can be combined and composed") is the right mental model; the canonical order for an outbound call is:

```
timeout(overall deadline) → retry(budget, backoff) → breaker → per-attempt timeout → call
```

Retry outside the breaker so an open breaker fails every attempt immediately (the retry sees `ErrOpen` as non-retryable); per-attempt timeout inside so a hung attempt does not consume the whole deadline.

```go
res, err := resilience.Retry(ctx, policy, func(ctx context.Context) (Result, error) {
    return resilience.Do(breaker, func() (Result, error) { return client.Do(ctx, req) })
})
```

## Checklist

- [ ] Backoff: base 100ms–1s, multiplier 1.6–2, jitter ≥ 0.2, cap ≤ the deadline.
- [ ] Retry predicate: transport errors, 429, 502/503/504; never 4xx, never after ctx done, never non-idempotent.
- [ ] `Retry-After` honoured and capped; a cap breach aborts.
- [ ] One `Budget` per dependency, shared across all callers in the process.
- [ ] One `Breaker` per dependency; `OnStateChange` emits a metric and a log line.
- [ ] Retries are logged with `http.resend_count` (Traefik sets `semconv.HTTPRequestResendCount(attempts - 1)` on the retry span).
