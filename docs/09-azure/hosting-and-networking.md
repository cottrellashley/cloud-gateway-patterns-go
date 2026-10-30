# Azure hosting and networking

**Pattern:** the gateway runs as a container on Azure Container Apps (or App Service for Linux containers when that is what you have), behind Azure Front Door (or Application Gateway for regional/WAF-in-VNet needs), with managed identity, Key Vault references, revisions for canaries, health probes wired to `/livez` and `/readyz`, and a termination grace that exceeds the gateway's `GracePeriod`.

## Choosing the compute

| | Container Apps | App Service (Linux container) |
| --- | --- | --- |
| Ingress | HTTPS/TCP, h2c to the container, internal ingress for services | HTTPS; HTTP/2 and WebSockets supported |
| Scaling | KEDA scalers (HTTP concurrency, queue depth), scale to zero | per-plan instances, autoscale rules |
| Revisions / traffic split | yes (blue/green, canary) | deployment slots with traffic routing |
| Dapr | built in | no |
| Identity / secrets | managed identity, Key Vault references | managed identity, Key Vault references |
| Fit | new gateway + services | an existing Web App runtime you already own |

Both expose cgroup limits, so `automaxprocs`/`automemlimit` work; both deliver `SIGTERM` and then kill after a grace period — set `httpserver.Config.GracePeriod` below that window and `ShutdownDelay` to cover probe intervals.

## Edge

* **Front Door**: global anycast, WAF, TLS termination, origin health probes. It adds `X-Azure-ClientIP`, `X-Azure-SocketIP`, `X-Forwarded-For/Host/Proto`; the gateway trusts those headers only from Front Door's backend IP ranges (service tag `AzureFrontDoor.Backend`) — `middleware.RealIP(frontDoorPrefixes, "X-Azure-ClientIP")`. Lock the origin to Front Door with the `X-Azure-FDID` header check or Private Link.
* **Application Gateway**: regional, VNet-resident, WebSocket-capable; the same header rules with `X-Forwarded-*`.
* Origin timeouts are configurable but short (seconds to a few minutes); everything longer is an LRO; streams heartbeat.

## Networking

* Services on internal ingress only; the gateway is the sole public origin.
* Private endpoints for Cosmos DB, Storage, Key Vault, Service Bus; the gateway's outbound traffic stays on the VNet.
* Data-plane endpoints (Flight, Blob SAS) are separately exposed and separately rate-limited; the gateway only mints tickets.
* DNS-based service discovery inside the environment (`<app>.internal.<env>.azurecontainerapps.io`); no client-side service registry needed until you run outside one environment.

## Probes and rollout

* Liveness → `/livez`; readiness → `/readyz`; startup → `/readyz` with a longer failure threshold.
* Canary with revision traffic split (e.g. 5 % → 50 % → 100 %) gated on the SLO metrics (`docs/05-observability/metrics.md`); rollback = traffic back to the previous revision.
* Replica count ≥ 3 across zones for the gateway; min replicas > 0 (no scale-to-zero on the front door).

## Secrets and identity

User-assigned managed identity per app; Key Vault references in settings; `AZURE_TOKEN_CREDENTIALS=prod`; certificates for mTLS to services issued from Key Vault and rotated by version.

## Checklist

- [ ] Front Door (or App Gateway) → gateway only; services internal.
- [ ] Trusted proxy ranges configured; `X-Azure-ClientIP` preferred.
- [ ] Probes wired; grace periods aligned; min replicas 3.
- [ ] Revisions for canary; SLO-gated promotion.
- [ ] Private endpoints for every store; managed identity everywhere.
