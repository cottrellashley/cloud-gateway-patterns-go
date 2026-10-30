# Health checks

**Pattern:** three probes with different meanings (`/livez`, `/readyz`, `/startupz` if the platform has it), a registry of named checks each bounded by a timeout, the Kubernetes report format, and active plus passive checks for upstreams. Implemented in `patterns/health` (gateway probes) and `patterns/lb` (upstream health).

## Probes of the gateway itself

The Kubernetes apiserver's `healthz.go` @ba832c1 is the reference implementation of a probe handler: checks are `HealthChecker{Name(); Check(*http.Request) error}`, excluded by `?exclude=`, reported one line each, and **the reason is withheld from the public body**:

```go
if err := check.Check(r); err != nil {
    // don't include the error since this endpoint is public.  If someone wants more detail
    // they should have explicit permission to the detailed checks.
    fmt.Fprintf(&individualCheckOutput, "[-]%s failed: reason withheld\n", check.Name())
    // but we do want detailed information for our log
    fmt.Fprintf(&failedVerboseLogOutput, "[-]%s failed: %v\n", check.Name(), err)
    failedChecks = append(failedChecks, check.Name())
} else {
    fmt.Fprintf(&individualCheckOutput, "[+]%s ok\n", check.Name())
}
...
if _, found := r.URL.Query()["verbose"]; !found { fmt.Fprint(w, "ok"); return }
```

It installs the same handler three times — `InstallHandler` (`/healthz`), `InstallReadyzHandler`, `InstallLivezHandler` — with different check sets, and `NewShutdownHealthz(stopCh)` is the readiness check that turns red at shutdown.

`patterns/health.Handler(name, log, checks...)` reproduces this (status `503` on failure; checks run concurrently; `WithTimeout` bounds each one). Wire it:

```go
mux.Handle("GET /livez",  health.Handler("livez",  log, health.Ping))
mux.Handle("GET /readyz", health.Handler("readyz", log,
    health.Ping,
    health.ReadinessCheck(&ready),                       // the lifecycle flag
    health.WithTimeout(health.NamedCheck("policy-bundle", bundleFresh), 500*time.Millisecond),
    health.WithTimeout(health.NamedCheck("state-store", cosmosPing), time.Second),
))
```

Semantics:

| Probe | Answers | May depend on |
| --- | --- | --- |
| `/livez` | is the process wedged? | nothing external; if it fails the platform restarts you |
| `/readyz` | should the edge send me traffic? | the readiness flag, config loaded, policy bundle fresh, critical stores reachable |
| `/startupz` | has startup finished? | same as readyz, used once by the platform |

Never make `/livez` depend on a downstream: a Snowflake outage must not restart the gateway fleet.

## Active health checks of upstreams

Traefik's `ServiceHealthChecker` (`pkg/healthcheck/healthcheck.go` @38d53dd) runs two loops — healthy targets on `interval`, unhealthy ones on `unhealthyInterval` — each check bounded by a deadline and accepting either a configured status or any `2xx/3xx`:

```go
func (shc *ServiceHealthChecker) Launch(ctx context.Context) {
    go shc.healthcheck(ctx, shc.unhealthyTargets, shc.unhealthyInterval)
    shc.healthcheck(ctx, shc.healthyTargets, shc.interval)
}
func (shc *ServiceHealthChecker) executeHealthCheck(ctx context.Context, config *dynamic.ServerHealthCheck, target *url.URL) error {
    ctx, cancel := context.WithDeadline(ctx, time.Now().Add(shc.timeout))
    defer cancel()
    switch config.Mode {
    case modeGRPC: ... checkHealthGRPC(ctx, target)
    default:       ... checkHealthHTTP(ctx, target)
    }
}
// checkHealthHTTP: if shc.config.Status == 0 && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest) → error
```

Caddy's `ActiveHealthChecks` adds `Passes`/`Fails` thresholds (hysteresis), `ExpectBody` regexp, `MaxSize` for the body read, and a dedicated `Port`/`URI` so the probe can hit `/readyz` on a different listener. Both set `Upstream.SetHealthy(bool)` which the balancer reads (`patterns/lb.Upstream.SetHealthy`).

## Passive health checks

Caddy (`healthchecks.go` @eb04ed5) counts failures observed on real traffic and forgets each one after `fail_duration`:

```go
err := upstream.Host.countFail(1)
...
go func(host *Host, failDuration time.Duration) {
    timer := time.NewTimer(failDuration)
    select { case <-h.ctx.Done(): ...; case <-timer.C: }
    err := host.countFail(-1)
}(upstream.Host, failDuration)
```

with `MaxFails`, `UnhealthyStatus`, `UnhealthyLatency` deciding what counts. `patterns/lb.Upstream.RecordFailure(d)` plus `Balancer.MaxFails` reproduce the mechanism; `patterns/proxy` calls it on transport errors. Passive checks react in one request; active checks bring a backend back without waiting for a real request to fail.

## Rules

1. Both active and passive on every upstream pool; active probes hit the service's `/readyz`.
2. Probe handlers are cheap, unauthenticated, uncached (`Cache-Control: no-store`), and never leak reasons.
3. Every check has a timeout shorter than the platform's probe timeout.
4. The readiness flag is the first `/readyz` check and the last thing set at startup.
5. Log check failures with the real reason at `warn`; alert on consecutive failures, not single ones.
