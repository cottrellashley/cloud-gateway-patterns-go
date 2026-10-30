# Streaming: SSE and WebSockets

**Pattern:** Server-Sent Events for server-to-client streams (progress, logs, events, change feeds) because every HTTP client, proxy and load balancer handles it; WebSockets only for genuinely bidirectional sessions. Implemented in `patterns/sse`; WebSocket guidance uses `coder/websocket`.

## SSE: the exact server behaviour

From tmaxmax/go-sse `session.go` @fb578c3, the upgrade is nothing more than setting the content type and flushing, and reading the resume point:

```go
func (s *Session) doUpgrade() error {
    if !s.didUpgrade {
        s.res.Header()[headerContentType] = headerContentTypeValue  // "text/event-stream"
        if err := s.res.Flush(); err != nil { return err }
        s.didUpgrade = true
    }
    return nil
}
// Upgrade reads the Last-Event-ID header into Session.LastEventID
```

`patterns/sse.Upgrade` adds the headers that keep intermediaries from buffering and clears the write deadline:

```go
h.Set("Content-Type", "text/event-stream")
h.Set("Cache-Control", "no-store")
h.Set("X-Accel-Buffering", "no")       // nginx
_ = rc.SetWriteDeadline(time.Time{})  // streams outlive WriteTimeout
w.WriteHeader(http.StatusOK)
rc.Flush()
```

Flushing goes through `http.NewResponseController(w)`, which reaches the real writer through `Unwrap()` even when middleware wrapped it (`patterns/middleware.ResponseWriter` implements `Unwrap` and `Flush`).

The standard library proxy flushes SSE immediately (`httputil` `flushInterval` returns `-1` for `text/event-stream`), so a gateway that proxies a streaming service needs no special casing — but the gateway's **own** `WriteTimeout` must be 0 and the edge's idle timeout must be respected with heartbeats.

## Resume and replay

go-sse's `FiniteReplayer` keeps a bounded queue of messages and replays those after the client's `Last-Event-ID`. `patterns/sse.Ring` does the same with 20-digit zero-padded numeric IDs (string order == numeric order) and distinguishes "nothing missed" from "resume point older than the buffer":

```go
evs, err := ring.After(lastID)
if errors.Is(err, sse.ErrResumeExpired) { /* send a gap event, then continue */ }
```

The MCP Go SDK's Streamable HTTP transport exposes the same concept as an `EventStore` option ("If set, EventStore will be used to persist stream events and replay them") plus `StreamKeepAlive` — streaming for agents converged on the same two knobs.

## Heartbeats and backpressure

* **Heartbeat**: a comment line `: keepalive` every 15 s. Azure Front Door and Application Gateway drop idle connections after their idle timeout; a comment is invisible to clients and resets the timer.
* **Backpressure**: SSE has no flow control. `patterns/sse.Sink` bounds the per-client buffer and fails `Send` with `ErrClientTooSlow` after `slowTimeout` instead of growing memory; the producer then closes the stream and the client reconnects with `Last-Event-ID`. Kubernetes bounds watches with `ShutdownWatchTerminationGracePeriod` and the apiserver counts long-running requests separately from in-flight limits (`longRunningRequestCheck`) — do the same: exempt streams from `shedding.InFlight` and cap them by connection count.
* **Shutdown**: streams are long-running; on drain, send a terminal event and return so `Server.Shutdown` can finish before `GracePeriod` — otherwise `Close()` cuts them.

## WebSockets

coder/websocket (`accept.go` @bd31a8f):

```go
c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
    Subprotocols:   []string{"atlas.duplex.v1"},
    OriginPatterns: []string{"app.example.com"},   // default: same-origin only
})
c.SetReadLimit(1 << 20)   // default 32768 bytes per message (read.go: defaultReadLimit = 32768)
```

Rules:

* Origin checking is on by default; keep it. Cookie-authenticated WebSockets are the CSRF vector; bearer tokens cannot be set on browser upgrades, so mint a one-time connect token (`POST .../duplex-token`, 60 s, single use) and pass it in the query.
* Per-message read limit, per-connection ping (`c.Ping(ctx)`) on a timer, `CloseRead` when the server never reads.
* Through the proxy: `httputil.ReverseProxy` handles `Upgrade`; in `patterns/proxy` the recorder's `Flush`/`Hijack` pass-through is what keeps that working through middleware.
* Through the edge: Application Gateway supports WebSocket; verify Front Door's current support and timeouts before relying on it.

## Which one

| Need | Use |
| --- | --- |
| Progress, logs, catalog changes, event feeds | SSE (`patterns/sse.Serve`) |
| Agent tool streaming | SSE (MCP Streamable HTTP is SSE) |
| Interactive bidirectional session | WebSocket, or gRPC bidi for service-to-service |
| Bulk rows | not a stream: data plane (`docs/06-data-and-async/arrow-flight-data-plane.md`) |
