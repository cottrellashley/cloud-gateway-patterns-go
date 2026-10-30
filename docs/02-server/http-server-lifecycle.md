# HTTP server lifecycle

**Pattern:** explicit timeouts, HTTP/2 enabled, readiness flipped before drain, a delay so the load balancer notices, a bounded graceful shutdown, then a hard close. Implemented in `patterns/httpserver` and `patterns/lifecycle`.

## Timeouts

Go's `http.Server` fields, from `net/http/server.go` (go1.25.14):

```go
// ReadTimeout is the maximum duration for reading the entire request, including the body.
// Because ReadTimeout does not let Handlers make per-request decisions on each request body's
// acceptable deadline or upload rate, most users will prefer to use ReadHeaderTimeout.
ReadTimeout time.Duration
// ReadHeaderTimeout is the amount of time allowed to read request headers.
ReadHeaderTimeout time.Duration
// WriteTimeout is the maximum duration before timing out writes of the response.
WriteTimeout time.Duration
// IdleTimeout is the maximum amount of time to wait for the next request when keep-alives are enabled.
IdleTimeout time.Duration
// MaxHeaderBytes controls the maximum number of bytes the server will read parsing the request header's keys and values. If zero, DefaultMaxHeaderBytes is used.
MaxHeaderBytes int   // DefaultMaxHeaderBytes = 1 << 20
```

What the production servers set:

| Setting | Caddy (`modules/caddyhttp/app.go` @eb04ed5) | Traefik (`server_entrypoint_tcp.go` @38d53dd) | ardanlabs/service (`api/services/auth/main.go` @9ea1316) | This repo |
| --- | --- | --- | --- | --- |
| ReadHeaderTimeout | set by default (`srv.ReadHeaderTimeout = defaultReadHeaderTimeout // see #6663`) | from `RespondingTimeouts.ReadTimeout` | 5s (`ReadTimeout`) | 10s |
| ReadTimeout | 0 unless configured | configurable | 5s | 0 (per-route body limits + deadline instead) |
| WriteTimeout | 0 unless configured | configurable | 10s | 0 (streams; per-request deadline instead) |
| IdleTimeout | `defaultIdleTimeout = caddy.Duration(5 * time.Minute)` | configurable | 120s | 5m |
| MaxHeaderBytes | `if srv.MaxHeaderBytes <= 0 { srv.MaxHeaderBytes = 16 * 1024 }` | configurable | default (1 MiB) | 16 KiB |

Why `WriteTimeout = 0` for a gateway: it is a hard deadline on the whole response, so an SSE stream or a large download dies at the limit. Caddy clears it and uses per-connection idle timeouts (`ReadIdleTimeout`, `WriteIdleTimeout` for HTTP/2); `patterns/sse.Upgrade` calls `ResponseController.SetWriteDeadline(time.Time{})` for the same reason. Use `patterns/middleware.Deadline` for the per-request bound instead.

## HTTP/2 and h2c

```go
srv.Protocols = new(http.Protocols)
srv.Protocols.SetHTTP1(true)
srv.Protocols.SetHTTP2(true)
srv.Protocols.SetUnencryptedHTTP2(true) // behind a TLS-terminating edge
```

Caddy does exactly this and then still calls `http2.ConfigureServer` for listener-wrapper reasons (`app.go`: "TODO: Migrate to http.Server.Protocols once listener-wrapper behaviour is covered"). For a gateway with plain listeners, `Protocols` alone is enough.

Keep-alive abuse: the Kubernetes authentication filter degrades unauthenticated HTTP/2 connections to one request each (`req.ProtoMajor == 2 && isAnonymousUser(resp.User)` → `Connection: close`) citing CVE-2023-44487. Traefik's `keep_alive_middleware.go` closes connections after `KeepAliveMaxRequests` or `KeepAliveMaxTime` by setting `Connection: close`. Add the same when you expose the gateway to untrusted networks.

## Shutdown sequence

`net/http` `Server.Shutdown` (go1.25.14) closes listeners, runs `RegisterOnShutdown` hooks, then polls idle connections with exponential backoff capped at `shutdownPollIntervalMax = 500 * time.Millisecond` until the context expires:

```go
func (s *Server) Shutdown(ctx context.Context) error {
    s.inShutdown.Store(true)
    s.mu.Lock()
    lnerr := s.closeListenersLocked()
    for _, f := range s.onShutdown { go f() }
    s.mu.Unlock()
    s.listenerGroup.Wait()
    ...
    for {
        if s.closeIdleConns() { return lnerr }
        select {
        case <-ctx.Done(): return ctx.Err()
        case <-timer.C: timer.Reset(nextPollInterval())
        }
    }
}
```

Active connections (a request in flight, a stream) are **not** closed by `Shutdown`; when the context expires you must call `Close()`. Traefik does precisely this:

```go
// traefik pkg/server/server_entrypoint_tcp.go @38d53dd
reqAcceptGraceTimeOut := time.Duration(e.transportConfiguration.LifeCycle.RequestAcceptGraceTimeout)
if reqAcceptGraceTimeOut > 0 {
    logger.Info().Msgf("Waiting %s for incoming requests to cease", reqAcceptGraceTimeOut)
    time.Sleep(reqAcceptGraceTimeOut)
}
graceTimeOut := time.Duration(e.transportConfiguration.LifeCycle.GraceTimeOut)
ctx, cancel := context.WithTimeout(ctx, graceTimeOut)
...
shutdownServer := func(server stoppable) {
    err := server.Shutdown(ctx)
    if err == nil { return }
    if errors.Is(ctx.Err(), context.DeadlineExceeded) {
        logger.Debug().Err(err).Msg("Server failed to shutdown within deadline")
        if err = server.Close(); err != nil { logger.Error().Err(err).Send() }
        return
    }
    ...
}
```

The Kubernetes apiserver documents the full state machine in `genericapiserver.go` (`RunWithContext`): on stop, **`/readyz` starts failing immediately**, then it sleeps `ShutdownDelayDuration` "to give the load balancer a window ... to detect that /readyz is red and stop sending traffic", then stops accepting, drains the non-long-running wait group, and optionally sends `429 + Retry-After` to stragglers (`ShutdownSendRetryAfter`). Caddy has the same two knobs: `shutdown_delay` and `grace_period`.

`patterns/httpserver.Run` does the four steps in order:

```go
ready.Set(false)                                  // 1. /readyz red
time.Sleep(cfg.ShutdownDelay)                     // 2. LB observes it
shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.GracePeriod)
if err := srv.Shutdown(shutdownCtx); err != nil { // 3. drain
    _ = srv.Close()                               // 4. drop stragglers
}
```

Note `context.WithoutCancel(ctx)`: the drain context must not inherit the already-cancelled process context, otherwise `Shutdown` returns immediately. Kratos makes the same point in `app.go` (`stopCtx := context.WithoutCancel(octx)`).

## Process-level run loop

`patterns/lifecycle.Group` is `oklog/run` (`group.go` @c769e58):

```go
func (g *Group) Run() error {
    ...
    errors := make(chan error, len(g.actors))
    for _, a := range g.actors { go func(a actor) { errors <- a.execute() }(a) }
    err := <-errors                       // Wait for the first actor to stop.
    for _, a := range g.actors { a.interrupt(err) }  // Signal all actors to stop.
    for i := 1; i < cap(errors); i++ { <-errors }    // Wait for all actors to stop.
    return err
}
```

Kratos (`app.go` `Run`) does the same with `errgroup`: one goroutine per server to `Start`, one per server waiting on `ctx.Done()` to `Stop`, one for signals. Traefik's `safe.Pool` adds `GoCtx` + `Stop()` (cancel then `WaitGroup.Wait`). The property is identical: exactly one decision that the process is stopping, and every long-lived goroutine observes it.

## Checklist

- [ ] `ReadHeaderTimeout` set; `MaxHeaderBytes` 16 KiB; `IdleTimeout` minutes.
- [ ] `WriteTimeout` 0 **and** `middleware.Deadline` on every route.
- [ ] Readiness flips before drain; `ShutdownDelay` ≥ 2 × the edge's probe interval.
- [ ] `GracePeriod` ≤ the platform's termination grace (Container Apps / App Service give you a termination window — set this below it) and `Close()` after it.
- [ ] Shutdown context is `context.WithoutCancel(processCtx)`.
- [ ] One `lifecycle.Group`; signal actor registered first; background loops are `ContextActor`s.
