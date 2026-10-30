# 13. Failure isolation and blast radius

**Principle:** every dependency, tenant and change is contained so that its failure degrades one thing and not the platform. Dependencies get bulkheads (connection, concurrency and queue limits per upstream), passive and active health detection, and circuit breakers that fail fast; the process protects itself with admission control and overload actions keyed to real resource pressure; every configuration or deployment change is make-before-break with a kill switch; retries are budgeted so they cannot amplify an outage; nothing unbounded exists.

**Why it matters at scale:** with fifty upstreams, one of them is always unhealthy. Without isolation, a slow vendor API consumes every worker, the catalog endpoint times out, and agents that only wanted owned data see a global outage. Envoy's entire upstream model (circuit breakers, outlier detection, overload manager, draining) exists to make failures local; Kubernetes bounds every controller with rate-limited queues and every apiserver with flow control; Caddy's atomic reloads mean a bad config never half-applies.

## Evidence

**Envoy circuit breaking** ([circuit_breaking.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/upstream/circuit_breaking.rst)): "It's nearly always better to fail quickly and apply back pressure downstream as soon as possible … Envoy enforces circuit breaking limits at the network level as opposed to having to configure and code each application independently. Envoy supports various types of fully distributed (not coordinated) circuit breaking: cluster maximum connections, cluster maximum pending requests, cluster maximum requests, cluster maximum active retries, cluster maximum concurrent connection pools." Each limit is *per upstream cluster*: that is the bulkhead.

**Outlier detection** ([outlier.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/upstream/outlier.rst)): "dynamically determining whether some number of hosts in an upstream cluster are performing unlike the others and removing them from the healthy load balancing set … consecutive failures, temporal success rate, temporal latency … a form of *passive* health checking", distinguishing externally originated errors (5xx from the host) from locally originated ones (timeouts, resets), with ejection and a maximum ejection percentage so detection cannot empty a cluster.

**Overload manager** ([overload_manager.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/overload_manager.rst)): "protecting the Envoy server from overload with respect to various system resources (such as memory, cpu or file descriptors) … This is distinct from circuit breaking which is primarily aimed at protecting upstream services. … works by periodically polling the *pressure* of a set of **resources** [0,1], feeding those through **triggers**, and taking **actions**" (stop accepting connections, shed load, reduce timeouts). Self-protection is a separate mechanism from dependency protection.

**Draining and hot restart** ([draining.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/draining.rst), [hot_restart.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/hot_restart.rst)): during drain the connection manager adds `Connection: close`, sends HTTP/2 GOAWAY, and the probability of draining a connection rises linearly over the drain time; on hot restart "the new process fully initializes itself … before it asks for copies of the listen sockets from the old process. The new process starts listening and then tells the old process to start draining." Make before break at the process level. Initialisation is bounded ([init.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/init.rst)): "an Envoy is always guaranteed to initialize within initial_fetch_timeout".

**Runtime guards as kill switches** ([runtime.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/runtime.rst)): "a mechanism to disable new behavior or risky changes … some deployments might find it useful to set up dynamic … runtime configuration as a safety measure to be able to quickly disable the new behavior without having to revert to an older version". Kubernetes feature gates ([feature-gates.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/feature-gates.md)): "A feature gate is intended to be a fast way to neutralize a new feature and mitigate damage caused by bugs."

**Kubernetes bounds its own loops and its own server.** Controllers retry per key with exponential backoff capped at 1000 s and an overall bucket of 10 qps ([default_rate_limiters.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/client-go/util/workqueue/default_rate_limiters.go#L50)), so one bad object cannot hot-loop a controller. The apiserver chain (principle 09) has `WithMaxInFlightLimit` (separate read and mutating limits, long-running exempt) or `WithPriorityAndFairness` (per-flow queues so one tenant's burst cannot starve others), `WithRequestDeadline`, `WithRetryAfter` during shutdown, and `WithProbabilisticGoaway` to rebalance HTTP/2 connections.

**Caddy** keeps failures from half-applying ([architecture.md §Managing configuration](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/architecture.md)): "either the whole thing is replaced, or nothing gets changed … A config reload works by provisioning the new modules, and if all succeed, the old ones are cleaned up"; and scopes state so a reload cannot leak it: "Each configuration is associated with a context which holds all the module state, so most state never escapes the scope of a config", with `UsagePool` for the little that must be global (upstream health).

**go-control-plane** refuses to publish an inconsistent snapshot ([`Snapshot.Consistent`](https://github.com/envoyproxy/go-control-plane/blob/370bc89/pkg/cache/v3/snapshot.go#L83)); **Traefik** drops no-op configurations ([configurationwatcher.go](https://github.com/traefik/traefik/blob/38d53dd/pkg/server/configurationwatcher.go#L182)) so churn cannot cause reload storms.

**gRPC retry throttling** and **gobreaker** (`docs/03-traffic/retries-backoff-circuit-breakers.md`): retries draw from a token budget refilled by successes; a breaker opens after a failure threshold, half-opens after a timeout, and closes on success — per dependency.

## The rules

1. **A bulkhead per dependency:** its own connection pool, its own in-flight cap, its own queue with a bound and a timeout, its own breaker, its own health. Pools and limits are never shared between an owned-data warehouse and a vendor API.
2. **Fail fast at the bulkhead.** When the cap is reached, reject immediately with 503/429 + `Retry-After` and the dependency named in the error detail; never queue without a bound; never wait for a dead upstream to time out.
3. **Passive detection everywhere, active where cheap.** Record every upstream outcome (5xx, timeout, reset) and eject hosts that misbehave; probe health actively where a probe is meaningful; cap ejection so detection cannot empty a pool.
4. **Protect the process separately from the dependencies:** admission by in-flight count (reads vs writes vs long-running), shedding by resource pressure (memory, goroutines, queue depth), per-tenant fairness so one principal's burst cannot starve the rest.
5. **Budget retries.** Retry only idempotent work, only on retryable errors, never after the deadline, with jitter, from a budget that shrinks during an outage. A retry storm is self-inflicted.
6. **Every change is make-before-break with a kill switch:** new snapshot validated for consistency → new generation provisioned → switch → old cleaned up; a new behaviour ships behind a gate that can be turned off without a deploy; deploys drain (readiness off → delay → drain → close) and the new replica is ready before the old one stops.
7. **Scope state to a generation.** Everything built from a configuration lives and dies with it; only explicitly global state (upstream health) survives, through a managed pool.
8. **Bound every loop and every input:** bodies, headers, cursors, keys, buffers, goroutines, map sizes, queue lengths, controller requeues.
9. **Degrade by capability, never by platform.** The catalog, discovery, health and operations endpoints depend on nothing a provider depends on; a provider failure shows as `Ready=False` on its capabilities and 503 on its actions, with everything else serving.
10. **Measure the blast radius before shipping:** for each dependency, write down what fails when it is down for an hour and which users notice; if the answer is "everyone", redesign.

## In this repository

| Mechanism | Where |
| --- | --- |
| Per-dependency backoff, retry budget, breaker state machine | `patterns/resilience` (`Backoff`, `Budget`, `Breaker`, `Do`), `docs/03-traffic/retries-backoff-circuit-breakers.md` |
| Admission with bounded backlog and timeout; read/write in-flight limits with long-running and operator exemptions; drain rejections | `patterns/shedding`, `docs/03-traffic/load-shedding-and-admission.md` |
| Passive health (`RecordFailure`, `MaxFails`, `FailDuration`), balancers that skip unavailable upstreams | `patterns/lb`, `patterns/proxy`, `docs/03-traffic/health-checks.md`, `docs/03-traffic/load-balancing.md` |
| Per-principal and per-group rate limits with IETF headers and fail-open/closed choice | `patterns/ratelimit`, `docs/03-traffic/rate-limiting-and-quotas.md` |
| Make-before-break at process level | `patterns/httpserver.Run`, `patterns/lifecycle`, `docs/02-server/http-server-lifecycle.md` |
| Validated, deduplicated, atomic configuration snapshots | `patterns/config.Hot` |
| Generations of modules with reverse-order cleanup | `patterns/registry.Context` |
| Bounded controller retries | `patterns/reconcile.Exponential`, `Queue.AddRateLimited` |
| Bounded concurrency and singleflight | `patterns/concurrency`, `patterns/cache.Flight` |
| Bounded inputs | `patterns/middleware.MaxBytes`, `httpserver` header limits, `pagination.ClampPageSize`, `sse.Sink` |

```go
// One bulkhead per provider: the gateway kernel builds these from the snapshot; nothing is shared across providers.
type Bulkhead struct {
    Name     string
    Sem      *concurrency.Semaphore      // in-flight cap for this dependency
    Breaker  *resilience.Breaker         // opens on consecutive failures, half-opens after Timeout
    Budget   *resilience.Budget          // retry tokens refilled by successes
    Backoff  resilience.Backoff
    Timeout  time.Duration               // per-call, always <= remaining request deadline
}

func (b *Bulkhead) Call(ctx context.Context, fn func(ctx context.Context) error) error {
    if !b.Sem.TryAcquire() {                                           // fail fast, never queue unbounded
        return apierror.New(http.StatusServiceUnavailable, apierror.CodeOverloaded, b.Name+" is at capacity").WithRetryAfter(1).WithDetail("dependency", b.Name, "in-flight cap reached")
    }
    defer b.Sem.Release()
    _, err := resilience.Do(b.Breaker, func() (struct{}, error) {       // breaker open → immediate error
        cctx, cancel := context.WithTimeout(ctx, b.Timeout)
        defer cancel()
        return struct{}{}, fn(cctx)
    })
    return err
}
```

## Applying it to a capability gateway

* **Bulkheads by group and provider:** the warehouse session pool for `owned.*` (sized to the warehouse's concurrency, with a bounded wait queue), one pool and breaker per vendor API, one per platform source. A vendor outage opens its breaker; `vendor.*` capabilities report `Ready=False, reason=UpstreamUnavailable` and their actions return 503 with the vendor named; `owned.*` and the catalog are untouched.
* **Per-principal fairness:** in-flight and bytes/second limits per principal and per group, so one agent fanning out hundreds of `execute` calls cannot exhaust the shared session pool for everyone. The handoff mode is the pressure valve: when the session pool is saturated, policy may require `handoff` for large queries so the bytes move on the client's connection.
* **Self-protection:** admission by in-flight count on the gateway, memory-pressure shedding (reject new large `execute` requests when heap is near the limit, keep serving catalog and small requests), and queue-depth triggers on the operation worker pool.
* **Change safety:** a new provider version is a new registry generation, provisioned and health-checked before the snapshot flips; a schema change is a new generation behind `Compatible=True`; new modes ship behind gates; every deploy drains with readiness off first. A bad manifest fails `Consistent()` and never reaches a replica.
* **Blast-radius table** kept in the repository: dependency → what degrades → who notices → kill switch → owner. Reviewed when a dependency is added.

## Anti-patterns

* One HTTP client, one connection pool and one worker pool for every upstream.
* Unbounded `chan` or slice queues "so we don't drop anything".
* Retrying on every error with no budget and no jitter.
* A catalog endpoint that calls each provider's `/describe` live.
* Deleting the old route, then adding the new one.
* A new behaviour that can only be disabled by rolling back a deploy.
