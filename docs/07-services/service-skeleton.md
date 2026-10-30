# Service skeleton and layout

**Pattern:** every service is a `main` that parses config, builds dependencies, registers routes, runs under a lifecycle group, and exposes `/livez`, `/readyz`, `/metrics` and `pprof` on a debug port; business logic lives in packages with no HTTP imports; handlers return values/errors and a thin web layer encodes them. Reference layouts: ardanlabs/service and go-kratos/kratos.

## ardanlabs/service (`@9ea1316`)

Layout: `api/` (services and tooling binaries), `app/` (request/response layer: `sdk/mid` middlewares, `sdk/errs`), `business/` (domain and data access), `foundation/` (web, logger, otel — no business knowledge), `zarf/` (deployment).

The web layer makes handlers return an encoder, so middleware can inspect results (`foundation/web/web.go`):

```go
type HandlerFunc func(ctx context.Context, r *http.Request) Encoder

func (a *App) HandlerFunc(method string, group string, path string, handlerFunc HandlerFunc, mw ...MidFunc) {
    handlerFunc = wrapMiddleware(mw, handlerFunc)
    handlerFunc = wrapMiddleware(a.mw, handlerFunc)
    h := func(w http.ResponseWriter, r *http.Request) {
        ctx := setTracer(r.Context(), a.tracer)
        ctx = setWriter(ctx, w)
        otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(w.Header()))
        resp := handlerFunc(ctx, r)
        if err := Respond(ctx, w, resp); err != nil { a.log(ctx, "web-respond", "ERROR", err); return }
    }
    finalPath := fmt.Sprintf("%s %s", method, "/"+group+path)
    a.mux.HandleFunc(finalPath, h)   // Go 1.22 ServeMux patterns
}
```

Its error middleware converts any error to the app error type, logs once with the origin, and hides internals (`app/sdk/mid/errors.go`):

```go
var appErr *errs.Error
if !errors.As(err, &appErr) { appErr = errs.Errorf(errs.Internal, "Internal Server Error") }
log.Error(ctx, "handled error during request", "err", err, "source_err_file", path.Base(appErr.FileName), "source_err_func", path.Base(appErr.FuncName))
if appErr.Code == errs.InternalOnlyLog { appErr = errs.Errorf(errs.Internal, "Internal Server Error") }
return appErr
```

and `mid.Panics` recovers into an `InternalOnlyLog` error and bumps a metric. The `main.go` (`api/services/auth/main.go`) is the run shape: `conf.Parse` with defaults in struct tags (`ReadTimeout time.Duration \`conf:"default:5s"\``, `Password string \`conf:"default:postgres,mask"\``), `GOMAXPROCS` logged, tracing initialised, a debug mux on `DebugHost`, the API server in a goroutine sending to `serverErrors`, `signal.Notify(shutdown, SIGINT, SIGTERM)`, and `api.Shutdown(ctx)` with `ShutdownTimeout`.

## kratos (`@dd0fb7d`)

Kratos abstracts transports behind `transport.Server{Start(ctx); Stop(ctx)}` and a transport-agnostic middleware type:

```go
type Handler func(ctx context.Context, req any) (any, error)
type Middleware func(Handler) Handler
func Chain(m ...Middleware) Middleware { return func(next Handler) Handler { for i := len(m)-1; i >= 0; i-- { next = m[i](next) }; return next } }
```

so the same `recovery`, `logging`, `tracing`, `ratelimit` middlewares apply to HTTP and gRPC servers. `App.Run` starts every server, registers with a service registry (`registrar.Register(rctx, instance)` with a timeout), and stops them all on the first error or signal with `stopTimeout` (`app.go`).

## The layout this repo recommends

```
cmd/<service>/main.go        wiring only (flags/env → deps → routes → lifecycle.Group)
internal/app/                 HTTP/gRPC handlers; decode → call domain → encode (thin)
internal/domain/<ctx>/        business types and logic; no net/http, no SQL
internal/store/<backend>/     repositories (Cosmos, Postgres, Snowflake); return domain types
internal/platform/            shared infra adapters (telemetry, config, clients)
patterns/...                  this repository, vendored or imported
```

Rules:

1. `main` is the only place that knows every concrete type; everything else takes interfaces. Dependency injection by constructor arguments; `uber-go/fx` or `google/wire` only when the graph is genuinely large.
2. Handlers do three things: decode and validate input, call one domain function with `r.Context()`, encode the result or write the envelope. No business branching in handlers.
3. Domain packages return typed errors (`docs/07-services/errors.md`); the web layer maps them.
4. Every outbound call takes a context and is wrapped with `patterns/resilience`.
5. Startup order: config → logger → telemetry → stores (with connectivity check) → routes → readiness true. Shutdown is the reverse, driven by `lifecycle.Group`.
6. One debug listener per process (`pprof`, `expvar`, `/metrics`, `/readyz?verbose`), bound to the internal network.

## Minimal main (shape)

```go
func main() { if err := run(context.Background()); err != nil && !lifecycle.IsSignal(err) { log.Error("run", "err", err); os.Exit(1) } }
func run(ctx context.Context) error {
    cfg := loadConfig()                                   // docs/07-services/configuration.md
    log := telemetry.NewLogger(os.Stdout, cfg.LogLevel, "orders", version, cfg.Env)
    store, err := cosmos.New(ctx, cfg.Cosmos, cred); if err != nil { return err }
    svc := orders.NewService(store)
    mux := routes.New(svc)                                // thin handlers
    h := middleware.Chain(mux, middleware.Recover(log), middleware.RequestID(""), telemetry.Propagate(true), telemetry.AccessLog(log, telemetry.AccessLogOptions{Skip: isProbe}), middleware.Deadline("", 30*time.Second, 5*time.Minute))
    var ready lifecycle.Readiness
    srv := httpserver.New(ctx, cfg.HTTP, h, log)
    var g lifecycle.Group
    g.Add(lifecycle.SignalActor(ctx))
    g.Add(lifecycle.ContextActor(ctx, func(ctx context.Context) error { return httpserver.Run(ctx, srv, cfg.HTTP, &ready, log) }))
    return g.Run()
}
```
