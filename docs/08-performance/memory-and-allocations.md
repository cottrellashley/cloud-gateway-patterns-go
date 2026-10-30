# Memory and allocations

**Pattern:** stream instead of buffering; reuse buffers through `sync.Pool`; bound everything user-controlled (bodies, headers, cursors, keys); measure with `pprof` allocs and benchmarks before optimising; let the GC see the container limit.

## What the proxies do

**Caddy** pools the copy buffers for streaming (`modules/caddyhttp/reverseproxy/streaming.go` @eb04ed5):

```go
var streamingBufPool = sync.Pool{
    New: func() any {
        // The Pool's New function should generally only return pointer
        // types, since a pointer can be put into the return interface
        // value without an allocation
        // - (from the package docs)
        b := make([]byte, defaultBufferSize)
        return &b
    },
}
// copyBuffer: buf := streamingBufPool.Get().(*[]byte); defer streamingBufPool.Put(buf)
```

The comment is the rule for `sync.Pool`: pool pointers to slices, not slices. `net/http/httputil.ReverseProxy` exposes `BufferPool` for the same purpose; chi pools its routing context (`mx.pool.Get().(*Context)`) and notes the allocation costs of `r.WithContext` (2) vs `context.WithValue` (1).

**Traefik** buffers request bodies for retries only up to `maxRequestBodyBytes` (`mirror.NewReusableRequest(req, r.maxRequestBodyBytes)`), falling back to a `peekedBody` that replays what was read and streams the rest — bounded memory per request regardless of client behaviour. **oxy**'s `buffer` middleware spills bodies to disk past `MemBytes` with a hard `MaxBytes` (`multibuf.New(req.Body, multibuf.MaxBytes(...), multibuf.MemBytes(...))`).

## Rules

1. **Bound inputs**: `http.MaxBytesReader` on bodies (`middleware.MaxBytes`), `MaxHeaderBytes` 16 KiB, cursor ≤ 4 KiB, JWKS ≤ 1 MiB, SSE buffer depth per client, keyed-limiter capacity. Every unbounded input is a memory DoS.
2. **Stream**: `io.Copy` with pooled buffers; never `io.ReadAll` a response you are proxying; Parquet/Arrow results never enter the gateway (`docs/06-data-and-async/arrow-flight-data-plane.md`).
3. **Pool hot, fixed-size buffers** (`sync.Pool` of `*[]byte`); do not pool objects with variable large capacity (the pool keeps the largest ever allocated).
4. **Pre-size** slices and maps when the length is known (Uber guide "Prefer Specifying Container Capacity"); `strings.Builder`/`bytes.Buffer` with `Grow`.
5. **Avoid `fmt` in hot paths** (Uber guide "Prefer strconv over fmt"); use `strconv.AppendInt` family and `slog.LogAttrs`.
6. **Watch escape analysis**: `go build -gcflags=-m` on the proxy and middleware packages; returning pointers to locals and capturing variables in closures move allocations to the heap.
7. **GC configuration**: `GOMEMLIMIT` at ~90 % of the container limit (`docs/05-observability/runtime-tuning-and-profiling.md`); `GOGC` default; measure GC CPU in `runtime/metrics` before touching it.
8. **HTTP/2 flow control buffers**: gRPC/HTTP2 window sizes (`grpc.InitialWindowSize`, `InitialConnWindowSize`, `ReadBufferSize`/`WriteBufferSize` default 32 KiB) trade memory per stream for throughput; raise them only for high-bandwidth streams (Flight).

## Measuring

```
go test -run=^$ -bench=BenchmarkChain -benchmem -memprofile=mem.out ./patterns/middleware/
go tool pprof -alloc_space mem.out
```

`-benchmem` reports allocs/op; a middleware chain should add zero or one allocation per request (the context value). Compare against `go-perfbook` ("Concrete optimization tips", "Data Changes", "Garbage Collection") for the escalation ladder: algorithm → data layout → allocation → GC tuning, in that order.
