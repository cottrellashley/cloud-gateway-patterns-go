# 03. Control plane and data plane

**Principle:** the platform has two planes with different jobs, SLOs and scaling. The **control plane** stores and distributes desired state: catalog, routes, policies, schemas, limits, credentials metadata. It is eventually consistent, changes rarely relative to traffic, and is optimised for correctness and auditability. The **data plane** serves requests and moves bytes using the last configuration it received. It must keep working when the control plane is unavailable, must never block a request on a control-plane round trip, and must never carry bulk payloads the control plane would have to buffer.

**Why it matters at scale:** when one process both decides and serves, every configuration store hiccup is an outage, every large payload competes with policy decisions for memory, and the two cannot be scaled or secured independently. Envoy serves millions of requests per second from a snapshot while its management server can be restarted at will; ARM makes resource-level decisions and never sees the bytes a storage account serves; Arrow Flight returns a plan and tickets from a coordinator and lets clients fetch data from the nodes that hold it.

## Evidence

**Envoy xDS** ([xds_protocol.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/api-docs/xds_protocol.rst)) is the canonical control-plane protocol. Resources are typed by URL (`type.googleapis.com/envoy.config.cluster.v3.Cluster`, …), delivered as versioned snapshots over a stream, and acknowledged: "the client's ACK/NACK … `version_info` … `nonce`" ([§ACK/NACK](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/api-docs/xds_protocol.rst#L319)). A rejected update leaves the previous one serving. Updates are eventually consistent and must be sequenced to avoid dropping traffic ([§Eventual consistency considerations](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/api-docs/xds_protocol.rst#L742)):

> In general, to avoid traffic drop, sequencing of updates should follow a make before break model, wherein: CDS updates (if any) must always be pushed first. EDS updates (if any) must arrive after CDS updates for the respective clusters. LDS updates must arrive after corresponding CDS/EDS updates. RDS updates related to the newly added listeners must arrive after CDS/EDS/LDS updates. … Stale CDS clusters and related EDS endpoints (ones no longer being referenced) can then be removed.

New listeners and clusters are *warmed* before receiving traffic ([§Resource warming](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/api-docs/xds_protocol.rst#L709)). On start, Envoy waits a bounded `initial_fetch_timeout` for configuration and then serves regardless ([init.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/init.rst)): "A key design principle of initialization is that an Envoy is always guaranteed to initialize within initial_fetch_timeout". The management side, [go-control-plane `Snapshot.Consistent`](https://github.com/envoyproxy/go-control-plane/blob/370bc89/pkg/cache/v3/snapshot.go#L83), refuses to publish a snapshot whose routes reference clusters it does not contain: "all EDS resources are listed by name in CDS resources; all SRDS/RDS resources are listed by name in LDS resources".

**Caddy** keeps the admin API (control) separate from the servers (data) and swaps whole configurations ([architecture.md §Managing configuration](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/architecture.md)): "configs are treated as immutable, atomic units: either the whole thing is replaced, or nothing gets changed … A config reload works by provisioning the new modules, and if all succeed, the old ones are cleaned up. For a brief period, two configs are operational at the same time." Request handling never takes the config lock.

**Traefik** is the same shape: providers push `dynamic.Message` on a channel; the [`ConfigurationWatcher`](https://github.com/traefik/traefik/blob/38d53dd/pkg/server/configurationwatcher.go#L182) drops unchanged configurations with `reflect.DeepEqual`, merges, and calls listeners that build a complete new router and swap it in. Requests in flight keep the router they started with.

**Kubernetes:** the apiserver is the control plane for the cluster, kubelets and kube-proxy are the data plane; kube-aggregator ([handler_proxy.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go#L107)) reads its routing decision from an `atomic.Value` (`r.handlingInfo.Load()`) that `updateAPIService` swaps when the `APIService` object changes, so proxying never waits on the informer.

**Azure** ([ConsiderationsForServiceDesign.md §Azure Management Plane vs Data Plane](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/ConsiderationsForServiceDesign.md#L35)): "A management plane API is implemented through the Azure Resource Manager (ARM) and is used to provision and control the operational state of resources. A data plane API is used by developers to implement applications." They have different hosts, different auth audiences, different throttling and different guidelines.

**Arrow Flight** (see `docs/06-data-and-async/arrow-flight-data-plane.md`): `GetFlightInfo` returns a plan with endpoints and opaque tickets; `DoGet(ticket)` streams record batches from whichever node holds them. The coordinator never carries the data.

## The rules

1. **Two processes or two clearly separated subsystems, two SLOs.** Control plane: availability measured in minutes of staleness tolerated. Data plane: availability measured per request. Scale them independently.
2. **The data plane holds a complete, immutable snapshot** and reads it with one atomic load per request. It never queries the configuration store on the request path.
3. **Snapshots are versioned, validated and consistent before publication.** A snapshot that references something it does not contain is rejected; a rejected snapshot leaves the previous one serving.
4. **Make before break.** Add the new cluster/route/provider, warm it, switch, then remove the old. Never remove then add.
5. **Bounded startup, last-known-good.** A data-plane replica starts serving after a bounded wait for configuration, from a persisted last-known-good snapshot if the control plane is unreachable.
6. **Bytes never ride the control plane.** Bulk results, uploads and streams go through a data path (Flight, Blob SAS, a direct database connection) with a ticket minted by the control plane. The control plane carries plans, tickets, schemas and status.
7. **Control-plane changes are resources** (principle 01) with generations and status, so "what is live right now" is a query, not a guess.
8. **Different trust boundaries.** The control plane has write access to the catalog store; data-plane replicas have read access to snapshots and mint-only access to tickets.

## In this repository

| Mechanism | Where |
| --- | --- |
| Immutable snapshot behind an atomic pointer, validated swap, `DeepEqual` dedupe, listeners, `Watch` that keeps the last good value | `patterns/config.Hot`, `config.Watch`, `docs/03-traffic/dynamic-configuration.md` |
| Snapshot version and `If-Match` for control-plane writes | `config.Version`, `config.CheckIfMatch` |
| Data-plane routing from a snapshot: balancer and proxy built per snapshot, swapped atomically | `patterns/lb`, `patterns/proxy`, `cmd/gateway/main.go` |
| Make-before-break at the process level: readiness off → delay → drain → close; new replica ready before old drains | `patterns/httpserver.Run`, `patterns/lifecycle`, `docs/02-server/http-server-lifecycle.md` |
| Two generations of modules alive during a reload | `patterns/registry.Context` (load new generation fully, switch, `Close` old) |
| Ticketed data plane | `docs/06-data-and-async/arrow-flight-data-plane.md`, `patterns/pagination` (signed, input-bound tokens are the same idea) |

```go
// Control plane: a controller converges Capability resources into a RoutingSnapshot.
snap, _ := config.New(RoutingSnapshot{}, func(s RoutingSnapshot) error { return s.Consistent() }) // routes → providers must resolve
snap.OnChange(func(s RoutingSnapshot) { router.Store(buildRouter(s)) })                           // rebuild derived state once per change
go config.Watch(ctx, snap, 15*time.Second, loadSnapshotFromStore, log)                              // last good kept on load errors

// Data plane: one atomic load per request, never the store.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.router.Load().ServeHTTP(w, r) }
```

## Applying it to a capability gateway

* **Control plane** = the catalog (capabilities, providers, datasets, policies, schema versions) in Cosmos DB plus the controllers that validate manifests, diff schemas, and publish a `RoutingSnapshot` (routes, provider endpoints, policy bundles, limits, schema versions) with a version. Change feed or polling, not per-request reads.
* **Data plane** = gateway replicas that load the snapshot, authenticate, authorize, meter, and either proxy to a provider, run the query in the live session pool (`execute`) or mint a handoff ticket (`handoff`). A snapshot store outage degrades to "no new capabilities for a while", never to "no traffic".
* **The ticket is the seam.** `handoff` returns compiled SQL, a connection descriptor, the expected schema and a short-lived signed ticket; the client's own driver moves the bytes. `execute` streams Arrow or JSON from the service's session, but the result still bypasses the control-plane store (the operation record holds a pointer to the result location, not the result).
* **Sequencing of changes follows make-before-break:** add provider endpoint → add schema version → add route → flip default version → remove old route → retire old schema version. Each step is a generation with status; a failed step leaves the previous generation serving.
* **Snapshot consistency is checked before publication:** every route references a provider that exists and is `Ready`; every capability references a schema version that exists; every policy references known actions. The check is the Go equivalent of `Snapshot.Consistent`.

## Anti-patterns

* Reading the catalog database inside the request handler "to be sure it's fresh".
* A gateway that buffers a 2 GB result because the response goes through it.
* Deleting the old route before the new one is warm.
* Startup that blocks forever until the configuration store answers.
* One process, one database, one set of credentials for both catalog writes and request serving.
