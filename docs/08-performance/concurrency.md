# Concurrency

**Pattern:** every goroutine has an owner, a bound and a way to stop; cancellation flows through `context`; shared state is behind a mutex or an atomic snapshot; fan-out is bounded; panics in background goroutines are recovered. Implemented in `patterns/concurrency`; primitives from `golang.org/x/sync`.

## Rules (Uber Go style guide @1d60a91, with the sections to read)

* **"Don't fire-and-forget goroutines"**: a goroutine without a `WaitGroup`/group and a stop signal is a leak and a shutdown hazard. `lifecycle.ContextActor` and `concurrency.Group` are the two sanctioned ways to start one.
* **"Channel Size is One or None"**: buffered channels are a design decision, not a performance knob; unbounded buffering hides backpressure problems until memory runs out.
* **"Zero-value Mutexes are Valid"**, **"Avoid Mutable Globals"**, **"Copy Slices and Maps at Boundaries"**.
* **"Defer to Clean Up"** and **"Exit in Main"** (only `main` calls `os.Exit`; `log.Fatal` in libraries skips defers).

## Primitives

`golang.org/x/sync/errgroup` (`errgroup.go` @1f920df) with `SetLimit` is bounded fan-out with first-error cancellation:

```go
func (g *Group) SetLimit(n int) { ... g.sem = make(chan token, n) }
func (g *Group) Go(f func() error) {
    if g.sem != nil { g.sem <- token{} }
    g.wg.Add(1)
    go func() {
        defer g.done()
        // It is tempting to propagate panics from f() up to the goroutine that calls Wait, but it creates more problems than it solves ...
        if err := f(); err != nil { g.errOnce.Do(func() { g.err = err; if g.cancel != nil { g.cancel(g.err) } }) }
    }()
}
```

`patterns/concurrency.Group` is the same with `context.WithCancelCause` and panic→error conversion (the upstream deliberately does not recover; a gateway's background work should, with the stack logged). `ForEach` is the bounded parallel map most call sites want; `Semaphore` is the counting semaphore (`x/sync/semaphore` is the weighted version). `singleflight` is in `patterns/cache`.

Traefik's `safe.Pool`/`safe.Go` recover panics in every background goroutine and `OperationWithRecover` wraps retried operations the same way; `concurrency.Go(fn, onPanic)` is that helper.

## Context

* Every blocking operation takes a `context.Context` and returns when it is done. The request context is cancelled when the client disconnects (net/http does this), on the per-request deadline (`middleware.Deadline`) and on process shutdown (`BaseContext`).
* Detach with `context.WithoutCancel` only when work must outlive the request (LRO workers, audit writes); never with `context.Background()` — values (trace, principal) would be lost.
* `context.WithTimeoutCause`/`WithCancelCause` + `context.Cause` tell the difference between "client left", "deadline" and "shutdown" in logs.
* Pipelines (Go blog "Pipelines and cancellation"): close channels from the sender side only; every stage selects on `ctx.Done()`.

## Shared state

| State | Mechanism |
| --- | --- |
| Config, route tables, policy sets | immutable snapshot behind `atomic.Pointer` (`patterns/config`) |
| Counters, flags | `sync/atomic` types (`atomic.Int64`, `atomic.Bool`) |
| Maps, caches, pools | `sync.Mutex`; `sync.RWMutex` only when reads vastly outnumber writes and critical sections are tiny |
| Per-request state | the request context; never a global map keyed by request ID |

## Leaks and deadlocks

* `goleak.VerifyTestMain` in packages that spawn goroutines.
* `go test -race` always; `GOFLAGS=-race` in CI.
* Timers: `time.NewTimer` + `Stop` (and drain) rather than `time.After` in loops (each `time.After` allocates a timer that lives until it fires).
* `select` with `ctx.Done()` in every wait; never a bare channel receive in a server goroutine.
