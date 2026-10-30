# Front door and topology

**Pattern:** one cloud gateway process (the "front door" API) in front of many services, with a managed edge (Azure Front Door or Application Gateway) in front of the gateway. The gateway owns identity, routing, limits, telemetry and the error contract; services own business logic and data. Nothing else owns those things.

```
Internet / agents / SDKs
        │  TLS, WAF, DDoS, global anycast
        ▼
Azure Front Door (or Application Gateway)         ← terminates TLS, adds X-Forwarded-*, X-Azure-ClientIP
        │  HTTP/1.1 or h2c to origin, private link
        ▼
Gateway (this repo's patterns, Go)                ← authn, authz, limits, routing, LRO, telemetry, error envelope
   ├── service A  (HTTP/JSON, Connect/gRPC)
   ├── service B  (SSE / streaming)
   ├── data plane (Arrow Flight, Blob SAS)        ← ticketed, bytes never traverse the gateway
   └── jobs / events (River, Event Grid)
```

## What each layer owns

| Layer | Owns | Must not do |
| --- | --- | --- |
| Edge (Front Door / App Gateway) | TLS, WAF, DDoS, geo-routing, health probing of the gateway, origin response timeout | Business routing, auth decisions, per-user limits (it has no principal) |
| Gateway | Bearer validation, principal, policy, quotas, request IDs, trace context, retries to services, load balancing, LRO status monitors, access logs, error envelope | Business logic, data access, long computations |
| Services | Domain logic, data, idempotent writes keyed by the gateway's key, their own health | Re-validating tokens from scratch (trust the gateway's signed context), per-user rate limits |
| Data plane | Bulk bytes (Arrow Flight, Parquet, Blob) with a gateway-minted ticket | Policy decisions |

## Evidence: this is how the mature gateways are shaped

* **Kubernetes** puts one `GenericAPIServer` in front of many "extension API servers" through `kube-aggregator`; the aggregator proxies by path and forwards the authenticated user as headers, never the client's credentials ([`handler_proxy.go`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go)):

  ```go
  proxyRoundTripper = transport.NewAuthProxyRoundTripper(user.GetName(), userUID, user.GetGroups(), user.GetExtra(), proxyRoundTripper)
  ...
  handler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, upgrade, &responder{w: w})
  ```
  and before that it checks `handlingInfo.serviceAvailable` and returns `503 service unavailable` when the backing service is down.

* **Traefik** models the same three layers as *entrypoint → router → service* (`pkg/server/`), with middlewares attached to routers; **Caddy** as *server → routes → handlers* (`modules/caddyhttp/`). Both terminate TLS themselves because they are the edge; behind Azure Front Door your gateway is an origin and should serve plain HTTP/1.1 and h2c.

* **Azure Resource Manager** is the canonical "control plane in front of resource providers" and separates control-plane operations (RBAC, policy, locks, activity log) from data-plane endpoints that stay reachable when ARM is down. The gateway follows the same split: control calls go through it; bulk data gets a ticket and goes direct.

## Rules

1. **Trust the edge only by IP.** The gateway must trust `X-Forwarded-For` / `X-Azure-ClientIP` only when the TCP peer is inside the edge's address ranges. See `patterns/middleware.RealIP` — chi's `RealIP` trusts the leftmost XFF entry and is therefore spoofable; Caddy's `trusted_proxies` is the correct model.
2. **Terminate TLS at the edge, serve h2c at the gateway.** Go 1.24+ `http.Server.Protocols.SetUnencryptedHTTP2(true)` replaces the old `h2c.NewHandler` dance (Traefik's comment in `server_entrypoint_tcp.go`: "With the addition of UnencryptedHTTP2 in http.Server#Protocols in go1.24 setting the h2c handler is not necessary anymore").
3. **Respect the edge's timeouts.** Front Door and App Gateway enforce origin response timeouts measured in seconds; anything longer is a long-running operation (`patterns/lro`) and anything streaming needs heartbeats (`patterns/sse`, 15 s).
4. **Readiness is a contract with the edge.** `/readyz` goes red before the gateway drains and stays red for `ShutdownDelay` so the edge's health probes notice (`patterns/httpserver.Run`).
5. **One error envelope everywhere.** The edge may add its own 5xx pages; the gateway and services only ever emit `patterns/apierror`.
6. **Bytes bypass the gateway.** A result over a few megabytes is planned at the gateway and fetched from the data plane with a ticket; see `docs/06-data-and-async/arrow-flight-data-plane.md`.

## Reference wiring

`cmd/gateway/main.go` builds the whole chain in ~150 lines. Read it next, then `docs/01-architecture/request-lifecycle-and-middleware-order.md` for why the chain is ordered the way it is.
