# Rate limiting and quotas

**Pattern:** a token bucket per principal for smoothing bursts at one replica; a fixed-window counter in shared storage for quotas that must hold across replicas; standard `RateLimit` headers on every response and `Retry-After` on `429`. Implemented in `patterns/ratelimit`.

## Token bucket: `golang.org/x/time/rate`

The reference implementation (`rate/rate.go` @fb013b3) refills lazily on each call and lets a reservation go negative to compute a wait:

```go
func (lim *Limiter) advance(t time.Time) (newTokens float64) {
    last := lim.last
    if t.Before(last) { last = t }
    elapsed := t.Sub(last)
    delta := lim.limit.tokensFromDuration(elapsed)
    tokens := lim.tokens + delta
    if burst := float64(lim.burst); tokens > burst { tokens = burst }
    return tokens
}
func (lim *Limiter) reserveN(t time.Time, n int, maxFutureReserve time.Duration) Reservation {
    ...
    tokens := lim.advance(t)
    tokens -= float64(n)
    var waitDuration time.Duration
    if tokens < 0 { waitDuration = lim.limit.durationFromTokens(-tokens) }
    ok := n <= lim.burst && waitDuration <= maxFutureReserve
    if ok { r.tokens = n; r.timeToAct = t.Add(waitDuration); lim.last = t; lim.tokens = tokens; lim.lastEvent = r.timeToAct }
    return r
}
```

`patterns/ratelimit.Bucket.Reserve(now, n, maxWait)` is this logic. Two numbers define a bucket: `rate` (sustained requests/second) and `burst` (the largest spike admitted at once).

## Per-key buckets with eviction: Traefik

Traefik keys buckets by source (IP, header, or request host) and keeps them in a bounded TTL map (`pkg/middlewares/ratelimiter/` @38d53dd): the TTL is computed from the rate so an idle key expires once its bucket would be full again:

```go
// Make the ttl inversely proportional to how often a rate limiter is supposed to see any activity (when maxed out),
ttl := 1
if rtl >= 1 { ttl++ } else if rtl > 0 { ttl += int(1 / rtl) }
```

and `ttlmap.Map` is a map plus an expiry heap (`expiryHeap`, `freeSpace()` evicts the soonest-expiring when at capacity). The middleware either rejects (`delay == nil` → `429 "No bursty traffic allowed"`), delays up to `maxDelay` (`time.After(*delay)`) or rejects when the wait is too long (`serveDelayError`). `patterns/ratelimit.Keyed` reproduces the TTL heap; its `Allow` rejects rather than delays (delaying holds a goroutine per waiting request, which is itself a load-shedding problem).

## Fixed window in shared storage: Envoy's ratelimit service

envoyproxy/ratelimit (`src/redis/fixed_cache_impl.go` @11a831b) is the global limiter used behind Envoy's `ratelimit` filter. Its design points:

* the cache key embeds the **window start** (`key_<unix time / unit>`), so every replica agrees on the window without coordination; `utils.UnitToDivider` maps SECOND/MINUTE/HOUR/DAY/WEEK/MONTH/YEAR to seconds;
* increments are pipelined `INCRBY` + `EXPIRE` with jittered TTL (`expirationSeconds += this.baseRateLimiter.JitterRand.Int63n(this.baseRateLimiter.ExpirationJitterMaxSeconds)`) to avoid synchronized expiry;
* an optional local cache remembers keys already over the limit so hot offenders never touch Redis ("Set the TTL of the local_cache to be the entire duration");
* `stopCacheKeyIncrementWhenOverlimit` reads before incrementing to stop counting hits that will be rejected anyway;
* **shadow mode**: a limit can be evaluated and reported without enforcing ("If the limit is in ShadowMode, it should be always return OK") — roll out new limits in shadow first;
* the response carries `limit`, `limit_remaining` and near-limit stats.

`patterns/ratelimit.FixedWindow` is the algorithm on a `Counter` interface; `MemoryCounter` is the single-replica implementation and Redis `INCR`/`EXPIRE` (or Cosmos DB with TTL) is the production one.

## Headers

IETF `draft-ietf-httpapi-ratelimit-headers` (structured fields):

```
RateLimit: "per-user";r=42;t=17          # remaining, seconds to reset
RateLimit-Policy: "per-user";q=100;w=60  # quota per window, window seconds
Retry-After: 17                          # only on 429
```

Older APIs use `X-RateLimit-Limit/Remaining/Reset`; ARM uses `x-ms-ratelimit-remaining-subscription-reads` with a token bucket of 250 reads refilling 25/s per principal per region. Pick the IETF form for new APIs; `patterns/ratelimit.SetHeaders` renders it.

## Keys

Order of preference for the limit key: principal subject → API key → client IP (only with `middleware.RealIP`). Prefix every key with the policy name so two limiters never share a bucket (Traefik: `rlSource := fmt.Sprintf("%s:%s", rl.name, source)`).

## Quotas and budgets

Quotas (per day/month) are fixed windows with long units in shared storage; budgets (credits, cost) are reservations: reserve the estimate before the call, settle the actual after. Both need the same store properties: atomic increment, TTL, and audit of denials. Rolling them out: shadow mode → `RateLimit` headers only → enforce.

## Checklist

- [ ] Per-principal token bucket at the gateway; shared fixed-window quotas for cross-replica limits.
- [ ] Policy name in every key; headers on every response; `Retry-After` on 429.
- [ ] Limiter outage: fail open with an alert for soft limits, fail closed for billing-grade quotas (`Middleware(..., failOpen)`).
- [ ] Shadow mode before enforcement for new limits.
- [ ] Bounded key map (`capacity`) so an attacker spraying keys cannot exhaust memory.
