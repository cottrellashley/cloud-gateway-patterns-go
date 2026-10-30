# Routing

**Pattern:** the standard library `http.ServeMux` (Go 1.22+) for method- and path-pattern routing, with route patterns surfaced to telemetry; chi when you need nested routers, groups and inline middleware; a dynamic router only when routes come from configuration at runtime (Traefik/Caddy style).

## Standard library patterns

Go 1.22 added methods, wildcards and precedence rules to `ServeMux` (`net/http/pattern.go`: "A pattern ... has an optional method, an optional host, and a path"; paths ending in `/` are "represented with an anonymous '...' wildcard"; `{$}` matches the exact path).

```go
mux := http.NewServeMux()
mux.Handle("GET /v1/operations/{id}", lro.GetHandler(store))
mux.Handle("POST /v1/operations/{id}/cancel", lro.CancelHandler(mgr))
mux.Handle("GET /v1/capabilities/{name}", describe)
mux.Handle("/", upstream)                                   // catch-all
id := r.PathValue("id")                                     // in handlers
route := r.Pattern                                          // "GET /v1/operations/{id}" — bounded-cardinality label
```

Rules the mux enforces that you should know:

* Two patterns that overlap without one being more specific conflict at registration (panic). A wildcard must be a whole segment: `{id}:cancel` is invalid (we use `/{id}/cancel`).
* `r.Pattern` is populated on match; `patterns/telemetry.RequestMetrics` uses it as the route label so metrics never see `/users/42`.

## chi, when you need composition

chi's value is composition, not speed. `Mux.With` copies the parent's middlewares and appends (`mux.go` @9f8adb1):

```go
func (mx *Mux) With(middlewares ...func(http.Handler) http.Handler) Router {
    if !mx.inline && mx.handler == nil { mx.updateRouteHandler() }
    var mws Middlewares
    if mx.inline { mws = make(Middlewares, len(mx.middlewares)); copy(mws, mx.middlewares) }
    mws = append(mws, middlewares...)
    im := &Mux{ pool: mx.pool, inline: true, parent: mx, tree: mx.tree, middlewares: mws, ... }
    return im
}
```

and `ServeHTTP` takes the routing context from a `sync.Pool` ("r.WithContext() causes 2 allocations and context.WithValue() causes 1 allocation") — the allocation-conscious shape to copy if you write your own router.

Use chi for `Route("/v1", func(r chi.Router){ r.Use(authn); r.Get(...) })` style grouping; keep `middleware.Chain` for the global chain so the order is visible in one place.

## Dynamic routing (routes from configuration)

Traefik rebuilds the entire router on every configuration change and swaps it atomically (`pkg/server/configurationwatcher.go`: dedupe with `reflect.DeepEqual`, then `for _, listener := range c.configurationListeners { listener(conf) }`; the HTTP switcher then `UpdateHandler`s). The equivalent here is `patterns/config.Hot[T]` holding an immutable route table and a handler that does `h.Load()` once per request:

```go
routes, _ := config.New(initialRoutes, validate)
mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    table := routes.Load()              // one snapshot per request
    table.Match(r).ServeHTTP(w, r)
})
```

Never read the hot value twice in one request; a swap between reads mixes two configurations.

## Versioning and compatibility routes

* Version in the path for the gateway's own API (`/v1/...`); Azure's guideline is a date-based `api-version` query parameter for ARM-style resource APIs — pick one and apply it everywhere.
* Deprecation is a header, not a path: `Deprecation: true` and `Sunset: <http-date>` (RFC 8594) with the envelope's `warnings`.

## Checklist

- [ ] Method in the pattern (`GET /x`), never `if r.Method != ...` in handlers.
- [ ] `r.Pattern` or `telemetry.WithRoute` feeds metrics and logs.
- [ ] Catch-all `/` is the proxy, registered last in your head even though the mux does not care.
- [ ] Hot route tables are read once per request.
