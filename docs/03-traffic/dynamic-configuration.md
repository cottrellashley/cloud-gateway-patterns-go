# Dynamic configuration (hot reload)

**Pattern:** the live configuration is an immutable value behind an atomic pointer. A new version is loaded, validated, compared with the current one, swapped in a single step, and listeners rebuild what they must. Readers never lock and read the value once per request. Implemented in `patterns/config`.

## Evidence

**Traefik** (`pkg/server/configurationwatcher.go` @38d53dd) receives configurations from providers on a channel, drops duplicates, merges, and calls every listener:

```go
case newConfigs, ok := <-c.newConfigs:
    ...
    if reflect.DeepEqual(newConfigs, lastConfigurations) { continue }
    conf := mergeConfiguration(newConfigs.DeepCopy(), c.defaultEntryPoints)
    conf = applyModel(conf)
    ...
    for _, listener := range c.configurationListeners { listener(conf) }
    lastConfigurations = newConfigs
```

Listeners build a complete new router and switch the entrypoint's handler to it; in-flight requests keep the old handler because they already hold it.

**Caddy** (`caddy.go` @eb04ed5, `changeConfig`) exposes the config through an admin API with optimistic concurrency:

```go
if ifMatchHeader != "" {
    // expect the first and last character to be quotes ... format "<path> <hash>"
    ...
    if hex.EncodeToString(hash.Sum(nil)) != parts[1] {
        return APIError{HTTPStatus: http.StatusPreconditionFailed, Err: fmt.Errorf("If-Match header did not match current config hash")}
    }
}
...
// if nothing changed, no need to do a whole reload unless the client forces it
if !forceReload && bytes.Equal(rawCfgJSON, newCfg) { Log().Info("config is unchanged"); return errSameConfig }
...
// load this new config; if it fails, we need to revert to our old representation of caddy's actual config
err = unsyncedDecodeAndRun(newCfg, true)
```

and `unsyncedDecodeAndRun` swaps `currentCtx` under a mutex and only then stops the old context (`oldCtx := currentCtx; currentCtx = ctx; ... unsyncedStop(oldCtx)`), i.e. the new config is serving before the old one is torn down.

**Envoy go-control-plane** (`pkg/cache/v3/simple.go` @370bc89) stores versioned immutable snapshots per node and answers open watches on set:

```go
func (cache *snapshotCache) SetSnapshot(ctx context.Context, node string, snapshot ResourceSnapshot) error {
    cache.mu.Lock(); defer cache.mu.Unlock()
    cache.snapshots[node] = snapshot
    if info, ok := cache.status[node]; ok {
        ... cache.respondSOTWWatches(ctx, info, snapshot) ... cache.respondDeltaWatches(ctx, info, snapshot)
    }
```

**Kubernetes** does the same with informers and the aggregator's `proxyHandler.handlingInfo.Load()` (an `atomic.Value` holding the current `proxyHandlingInfo`), swapped by `updateAPIService` when the `APIService` object changes.

## The pattern

```go
type Routes struct{ Upstreams []string }
routes, err := config.New(initial, func(r Routes) error { ... })   // validated initial value
routes.OnChange(func(r Routes) { rebuildBalancer(r) })             // listeners rebuild derived state
changed, err := routes.Swap(next)                                   // validate → DeepEqual → atomic store → notify
r := routes.Load()                                                  // once per request
etag := routes.CurrentVersion()                                     // sha256 of the JSON form
```

Admin updates carry the version they were based on and are rejected with `412` otherwise (`config.CheckIfMatch`). `config.Watch` polls a loader on an interval and keeps the last good value when loading or validation fails — Traefik keeps serving the previous configuration when a provider errors, and Caddy restores the old one on failure.

## Where configuration comes from

* Static (flags/env) for process shape: addresses, timeouts, identities. See `docs/07-services/configuration.md`.
* Dynamic for routes, upstreams, policies, limits, provider manifests: a store (Cosmos DB, Git, App Configuration) polled or watched, or pushed over a change feed (SSE) — the Atlas catalog change feed is an instance of this.
* Push beats poll for latency; poll is the fallback that survives a dead push channel (Traefik providers push; `Watch` here polls; run both).

## Rules

1. Validate before swap; never let an invalid config become live.
2. Dedupe with DeepEqual or a hash; listeners are expensive.
3. Readers hold one snapshot per request.
4. Version every snapshot; expose it in `/readyz?verbose` and in admin responses as an ETag.
5. Listeners must be fast and must not block the swapper; derive heavy state (balancers, compiled matchers) in the listener and swap that too.
6. Keep the last good config on any failure and alert; never fail closed on a config-source outage.

## Tests

`patterns/config/config_test.go`: identical config does not notify; invalid config leaves the old live; `If-Match` round-trip; watcher keeps the last good value across a loader error.
