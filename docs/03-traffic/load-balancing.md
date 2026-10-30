# Load balancing

**Pattern:** weighted round-robin for predictable spread across equal-ish backends; power-of-two-choices on in-flight count when request cost varies; consistent hashing only when affinity matters (sessions, caches). Implemented in `patterns/lb`.

## Weighted round robin: Traefik's EDF heap

Traefik's WRR is an earliest-deadline-first scheduler over a heap (`pkg/server/service/loadbalancer/wrr/wrr.go` @38d53dd):

```go
func (b *Balancer) nextServer() (*namedHandler, error) {
    ...
    for {
        // Pick handler with closest deadline.
        handler = heap.Pop(b).(*namedHandler)
        // curDeadline should be handler's deadline so that new added entry would have a fair competition environment with the old ones.
        b.curDeadline = handler.deadline
        handler.deadline += 1 / handler.weight
        heap.Push(b, handler)
        if _, ok := b.status[handler.name]; ok {
            if _, ok := b.fenced[handler.name]; !ok { break }   // do not select a fenced handler.
        }
    }
    return handler, nil
}
func (b *Balancer) Less(i, j int) bool { return b.handlers[i].deadline < b.handlers[j].deadline }
```

A server with weight 3 advances its deadline by 1/3 per pick and is therefore picked three times as often as a weight-1 server, with the picks interleaved rather than batched. New servers start at the current deadline so they do not get a burst. `patterns/lb.WeightedRoundRobin` is this algorithm; its test asserts a 300:100 split for weights 3:1 over 400 picks.

Caddy's `WeightedRoundRobinSelection` is a simpler counter walk (`currentWeight := int(r.index.Add(1)) % r.totalWeight`) that batches picks per weight; prefer EDF.

## Power of two choices: Traefik's P2C

```go
// pkg/server/service/loadbalancer/p2c/p2c.go @38d53dd
n1, n2 := b.rand.Intn(len(healthy)), b.rand.Intn(len(healthy)-1)
if n2 >= n1 { n2++ }            // In order to not get the same backend twice
h1, h2 := healthy[n1], healthy[n2]
if h2.inflight.Load() < h1.inflight.Load() { return h2, nil }   // Ensure h1 has fewer inflight requests than h2.
return h1, nil
```

Two random picks and the less-loaded wins; the classic result is near-optimal balance with O(1) work and no coordination. Skipper's `powerOfRandomNChoices` and Caddy's `RandomChoiceSelection` are the same idea. Use it when requests have very different costs (queries), where round-robin piles up on a slow replica.

Caddy's `LeastConnSelection` is the exhaustive version with reservoir sampling for ties:

```go
if numReqs == leastReqs {
    count++
    if count == 1 || weakrand.IntN(count) == 0 { bestHost = host }   // reservoir sample
}
```

## Availability gate

Every algorithm filters with Caddy's `Available()` (healthy && not full) — `patterns/lb.Upstream.Available(maxFails)`. In-flight counting (`Acquire`/`Release` around the proxied call) is what makes P2C and `MaxRequests` work; the proxy does it for you.

## Consistent hashing

Skipper's `consistentHash` builds a ring with `hashesPerEndpoint` virtual nodes and offers bounded-load search (`boundedLoadSearch(key, balanceFactor, ctx)`), which caps how far a hot key can overload one endpoint. Use only for affinity (sticky sessions when the state cannot be externalised, per-key caches). The gateway's default is **no affinity**: sessions live in a store (`patterns/session`) so any replica can serve any request.

## Choosing

| Workload | Algorithm |
| --- | --- |
| Uniform HTTP APIs, equal replicas | WRR (weights 1) |
| Heterogeneous replicas (different SKUs) | WRR with weights |
| Variable-cost requests (queries, LLM calls) | P2C on in-flight |
| Must hit the same backend per key | consistent hash with bounded load |

## Checklist

- [ ] `MaxFails` set on the balancer whenever passive health is on (a `MaxFails` of 0 disables it).
- [ ] `Acquire`/`Release` around every proxied call (done by `patterns/proxy`).
- [ ] Health flips come from active checks; never leave an upstream permanently marked down without an active probe to bring it back.
- [ ] Weights are integers ≥ 1 in config, floats internally.
