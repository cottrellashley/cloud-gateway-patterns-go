# Runtime tuning and profiling

**Pattern:** let the Go runtime see the container's real limits (`GOMAXPROCS` from the CPU quota, `GOMEMLIMIT` from the memory limit), expose `pprof` on an internal listener, enable continuous profiling in production, and use profile-guided optimisation for the gateway binary.

## GOMAXPROCS and the CPU quota

uber-go/automaxprocs (`maxprocs/maxprocs.go` @1ea14c3):

```go
// Honor the GOMAXPROCS environment variable if present. Otherwise, amend
// `runtime.GOMAXPROCS()` with the current process' CPU quota if the OS is
// Linux, and guarantee a minimum value of 1.
if max, exists := os.LookupEnv(_maxProcsKey); exists { ... return undoNoop, nil }
maxProcs, status, err := cfg.procs(cfg.minGOMAXPROCS, cfg.roundQuotaFunc)
...
runtime.GOMAXPROCS(maxProcs)
```

Without it, a container with a 2-CPU quota on a 64-core node runs with `GOMAXPROCS=64`, the scheduler thrashes and the GC steals CPU from the request path. Go 1.25 made the runtime cgroup-aware for `GOMAXPROCS` on Linux by default (check your version's release notes); `automaxprocs` remains the explicit, logged choice and is harmless when the runtime already does it.

## GOMEMLIMIT and the memory limit

KimMachineGun/automemlimit (`memlimit/memlimit.go` @713a988) sets `GOMEMLIMIT` to a ratio (default `0.9`) of the cgroup memory limit, with `AUTOMEMLIMIT` to override and `WithRefreshInterval` to track limit changes. The Go GC guide explains the model: `GOMEMLIMIT` is a soft limit the GC works to stay under; combine it with `GOGC` to trade CPU for memory. Set the limit to ~90 % of the container limit so the GC becomes aggressive before the kernel OOM-kills the process. This replaces the "memory ballast" trick Twitch popularised in 2019.

```go
import ( _ "go.uber.org/automaxprocs"; "github.com/KimMachineGun/automemlimit/memlimit" )
func init() { memlimit.SetGoMemLimitWithOpts(memlimit.WithRatio(0.9), memlimit.WithProvider(memlimit.FromCgroupHybrid)) }
```

On App Service and Container Apps the limits are cgroups, so both work.

## pprof and continuous profiling

* `net/http/pprof` on the **debug** listener only (ardanlabs/service runs a separate `DebugHost` with `expvar` and `pprof`; the Kubernetes apiserver can serve a debug socket over UDS). Never on the public port.
* Continuous profiling (Grafana Pyroscope's Go SDK, or Azure Monitor Profiler for App Service) is the only way to see allocation and lock contention in production at the moment of an incident.
* `felixge/fgprof` complements the CPU profile with off-CPU time (blocked on I/O, locks).

## Profile-guided optimisation

Build the gateway with a representative CPU profile: `go build -pgo=default.pgo` (Go ≥ 1.21; `default.pgo` in the main package directory is picked up automatically). Go's PGO documentation reports typical 2–7 % CPU improvements from inlining and devirtualisation of hot paths — cheap wins for a proxy.

## Execution traces

`go tool trace` (and the Go 1.22+ flight recorder / `runtime/trace` improvements described in the "More powerful Go execution traces" post) show scheduler latency, GC pauses and goroutine blocking; use them when p99 spikes do not show up in CPU profiles.

## Checklist

- [ ] `automaxprocs` and `automemlimit` imported in `main`; limits logged at startup.
- [ ] Debug listener with `pprof`, `expvar`, `/metrics`, bound to localhost or the internal network.
- [ ] Continuous profiler in production.
- [ ] `default.pgo` refreshed each release from production profiles.
- [ ] `GOGC` left default unless profiling shows GC CPU > 10 %.
