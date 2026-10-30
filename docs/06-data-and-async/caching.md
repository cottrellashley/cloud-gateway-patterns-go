# Caching

**Pattern:** cache the things that are read often and change rarely (JWKS, catalog documents, policy bundles, config snapshots, small reference data), bound them by size and TTL, coalesce concurrent misses with single-flight, and validate conditional requests with ETags. Implemented in `patterns/cache`; ETag serving pattern from the Kubernetes aggregated discovery handler.

## Single-flight

`golang.org/x/sync/singleflight` (`singleflight.go` @1f920df) collapses concurrent calls for one key into one execution; the others wait and share the result:

```go
func (g *Group) Do(key string, fn func() (any, error)) (v any, err error, shared bool) {
    g.mu.Lock()
    if c, ok := g.m[key]; ok { c.dups++; g.mu.Unlock(); c.wg.Wait(); ... return c.val, c.err, true }
    c := new(call); c.wg.Add(1); g.m[key] = c
    g.mu.Unlock()
    g.doCall(c, key, fn)
    return c.val, c.err, c.dups > 0
}
```

(The upstream version also distinguishes panics from `runtime.Goexit` with a double-defer; `patterns/cache.Flight` is the generic adaptation without that corner.) go-oidc's `RemoteKeySet` implements the same with a hand-rolled `inflight` struct for JWKS refreshes. Use it for every cache miss that triggers a network call — that is what prevents a cache stampede when a popular entry expires.

## Bounded TTL + LRU

`patterns/cache.TTL[K,V]` is a mutex-protected map plus `container/list` with lazy expiry on `Get` and LRU eviction at capacity, and `GetOrLoad` composed with `Flight`. It is deliberately simple. For a hot general-purpose cache use maypok86/otter (W-TinyLFU, lock-free reads) or dgraph-io/ristretto; Traefik uses a TTL heap (`ttlmap`) for limiter buckets, which `patterns/ratelimit.Keyed` copies.

## ETags: serve cached documents conditionally

The Kubernetes aggregated discovery handler (`endpoints/discovery/aggregated/etag.go` @ba832c1) computes the ETag from the serialised document and serves `304` on match:

```go
func calculateETag(resources interface{}) (string, error) {
    serialized, err := json.Marshal(resources)
    ...
    return fmt.Sprintf("%X", sha512.Sum512(serialized)), nil
}
func ServeHTTPWithETag(object runtime.Object, hash string, ...) {
    quotedHash := strconv.Quote(hash)   // ETag must be enclosed in double quotes
    w.Header().Set("ETag", quotedHash)
    w.Header().Set("Vary", "Accept")
    w.Header().Set("Cache-Control", "public")
    if clientCachedHash := req.Header.Get("If-None-Match"); quotedHash == clientCachedHash {
        w.WriteHeader(http.StatusNotModified)
        return
    }
    ...
}
```

and the document itself is cached (`fetchFromCache` → `cachedGroupList{cachedResponse, cachedResponseETag}`) and invalidated when a group changes. Catalogs, capability documents, policy bundles and config snapshots should be served exactly this way; `patterns/config.Version` is the same hash idea (SHA-256 of the JSON) for config.

## What to cache at a gateway, and for how long

| Object | TTL | Invalidation |
| --- | --- | --- |
| JWKS | `Cache-Control` max-age, 24 h default | unknown `kid` → refresh (throttled) |
| Catalog / capability documents | until change | change feed event + ETag |
| Policy bundle | minutes, grace period then fail closed | push + poll |
| Config snapshot | until change | watcher |
| Session records | request-local only | never cache across requests at the gateway |
| Upstream responses | only with explicit `Cache-Control` from the service and no `Authorization` | standard HTTP rules; Caddy and Traefik ship this as middleware if you need it |

Never cache per-principal API responses at the gateway unless the service marks them cacheable and the cache key includes the principal.

## Checklist

- [ ] Every network-backed miss goes through single-flight.
- [ ] Caches bounded by entries and TTL; metrics for hits/misses/size.
- [ ] ETags on every cacheable document endpoint; `304` handled.
- [ ] No cross-principal response caching.
