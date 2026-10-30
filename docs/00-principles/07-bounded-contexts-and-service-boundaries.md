# 07. Bounded contexts and service boundaries

**Principle:** resources are organised into named *groups* by ownership and lifetime, not by technology or team org chart. A group owns its data and its invariants, exposes them only through its contract (resources, actions, events), and never shares a table or a cache with another group. The size of a group is set by what must change together and be consistent together; everything else is a separate group that talks through contracts.

**Why it matters at scale:** a system with one schema everyone writes to cannot be changed by anyone. Kubernetes partitions its API into groups (`apps`, `batch`, `networking.k8s.io`) that version independently and can be served by different processes; ARM partitions Azure into provider namespaces (`Microsoft.Compute`, `Microsoft.Storage`) each with its own team, release cadence and RBAC operations; Dapr partitions building blocks into categories. Boundaries are what let hundreds of teams ship without a release train.

## Evidence

**Kubernetes API groups.** Every kind lives in a group and version ([`TypeMeta.APIVersion`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go#L42) is `group/version`). A group can be served by a different process entirely through an [`APIService`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/types.go#L51) (`group`, `version`, `service`, `groupPriorityMinimum`, `versionPriority`); the aggregator routes `/apis/{group}/{version}/…` to it and the core never links its code. Groups version at their own pace (`apps/v1` has been stable for years while `gateway.networking.k8s.io` iterated through betas). Cross-group references are by name ([api-conventions.md §Object references](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md): "Do not modify the referred object", "Minimize copying or printing values to the referrer object"), never by foreign key, and controllers of one group react to objects of another only through the API.

**ARM provider namespaces and "designing resources"** ([Addendum.md §Designing Resources](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L259)). Every resource type is `{namespace}/{type}`; the RPC spells out *when something deserves to be a resource*:

> Objects should be Tracked resources if: they are billable entities …; need to be orchestrated by the template execution engine; … they have dependencies on other resources.
> Objects should be top-level resources if: … their lifetime is independent of other resources.
> Objects should be nested resources if: … their lifetime is dependent on a containing resource.
> Objects should be proxy only resources if: they don't need to be Azure resources for one of the reasons above; if the objects/properties are required for the parent resource to be created, then they should *not* be Azure resources; … the lifecycle of the resources are managed outside ARM; they are very common and would incur a performance overhead.

That is a boundary rule set: lifetime and ownership decide nesting; cost decides whether something is a resource at all.

**Google AIP-121** ([0121.md](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0121.md)): model "the resources (nouns) the API will provide, the relationships and hierarchies between those resources" first; resource names encode the hierarchy (`publishers/123/books/les-miserables`); "API designers should not expect that their API will be reflective of their database schema".

**Dapr categories** ([category.go](https://github.com/dapr/dapr/blob/3342040/pkg/components/category.go)): `bindings`, `pubsub`, `secretstores`, `state`, `workflow`, `middleware`, `configuration`, `crypto`, `lock`, `nameresolution`, `conversation`, … — each a building block with its own API, own component interface and own compatibility story. [API-005](https://github.com/dapr/dapr/blob/3342040/docs/decision_records/api/API-005-state-store-behavior.md) shows a boundary being made explicit: concurrency and consistency models are stated per building block, not left to each backend.

**wild-workouts (DDD in Go)** ([internal/](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/tree/8ecfcdf/internal)): `trainer`, `trainings` and `users` are separate services with separate `domain/`, `app/`, `adapters/`, `ports/` and separate databases; `trainings` calls `trainer` through a generated client (`TrainerService.MoveTraining` in [approve_training_reschedule.go](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/blob/8ecfcdf/internal/trainings/app/command/approve_training_reschedule.go)), never through its tables. Invariants (`tr.ApproveReschedule(cmd.User.Type())`) live in the owning domain.

**Traefik** scopes providers by namespace ([`NamespacedProvider`](https://github.com/traefik/traefik/blob/38d53dd/pkg/provider/provider.go#L20): "each configured namespace results in a dedicated provider instance") and prefixes every router, service and middleware name with its provider (`name@provider`), so two teams cannot collide.

## The rules

1. **Name the groups.** For a data gateway the first cut is by *ownership*: data the platform ingests and owns (`owned.*`: portfolios, index data, security master, securitised assets), data licensed from vendors and not owned (`vendor.*`: each vendor its own group, with the vendor's entitlement model), and platform/operational data (`platform.*`: cloud cost and usage, source control, request telemetry). Within `owned.*`, split by lifetime and consistency: a portfolio and its positions change together; an index and its constituents change together; they do not change with each other.
2. **A group owns its store.** Nothing outside the group reads or writes its tables, containers or caches. Access is through the group's resources, actions and events.
3. **Boundaries are visible in names:** `/v1/{group}/{collection}/{name}`, registry IDs `provider.{group}_*`, event types `{group}.{collection}.{verb}`, operation names `{group}/{collection}/{verb}`.
4. **Reference across groups by name, never by join.** A capability in `vendor.x` that needs a security identifier from `owned.securitymaster` resolves it through that group's API (or a published, versioned mapping dataset), not through a shared table.
5. **Nest by lifetime.** A schema version belongs to its capability and dies with it (nested); a dataset is top-level because it outlives any capability that reads it; a cached query plan is "proxy only": addressable, not tracked.
6. **One consistency model per group, stated.** `owned.*` is strongly consistent within a collection and eventually consistent across; `vendor.*` is as consistent as the vendor; `platform.*` is eventually consistent with a stated freshness.
7. **Groups version and deploy independently** (principle 05): each has its own `v1`, its own maturity ladder, its own conformance tests, and can be served by a different process behind the gateway.
8. **Cross-group workflows are sagas or events, not transactions.** A workflow that touches two groups is an operation (principle 12) whose steps are idempotent and whose failure leaves each group consistent.

## In this repository

| Mechanism | Where |
| --- | --- |
| Routing by group prefix and host; one proxy target per group | `docs/02-server/routing.md`, `patterns/proxy`, `cmd/gateway/main.go` |
| Group-scoped names in the registry (`provider.owned_sql`, `provider.vendor_rest`) | `patterns/registry` |
| Events as the cross-group channel (CloudEvents, typed, versioned) | `docs/06-data-and-async/events-and-cloudevents.md` |
| Idempotent writes so sagas can retry steps | `patterns/idempotency`, `docs/04-identity/idempotency.md` |
| Operations for cross-group workflows | `patterns/lro` |
| Per-group rate limits and quotas | `patterns/ratelimit` keyed by `(principal, group)` |

```go
// Group is a first-class object in the gateway's model, not a naming convention.
type Group struct {
    Name        string   // "owned.portfolios", "vendor.marketdata_a", "platform.azure_cost"
    Owner       string   // team / distribution list
    Store       string   // the one store this group writes; nobody else may
    Consistency string   // "strong-within-collection" | "eventual(5m)" | "vendor"
    Providers   []registry.ID
    Events      []string // event types this group publishes
    Maturity    string
}
```

## Applying it to a capability gateway

* **Owned data groups** (`owned.portfolios`, `owned.index`, `owned.securitymaster`, `owned.securitised`) each have their own schemas in the warehouse, their own capability manifests, their own result-schema compatibility history, and publish `*.changed` events when a load completes. A query that joins portfolios and index data is a capability *in one group* that reads the other through a published, versioned dataset — or an explicit cross-group capability owned by whichever group owns the join's meaning.
* **Vendor groups** (`vendor.*`) wrap each licensed API: the group owns the entitlement model (per-user licences, display vs derived data, redistribution limits), its cache policy, its rate limits against the vendor, and its own failure isolation (principle 13). Vendor terms never leak into `owned.*` code.
* **Platform groups** (`platform.azure_cost`, `platform.github`, `platform.telemetry`) are read-mostly, eventually consistent, and are where the gateway's own request records are exposed as a dataset like any other.
* **The gateway kernel is its own group** (`gateway.*`: capabilities, providers, operations, tickets, policies) with the catalog store as its single store.
* **Boundary test:** if you cannot name the group that owns a column, you have not finished the design.

## Anti-patterns

* One warehouse schema every capability reads and writes freely.
* A `common` library that contains business rules for three groups.
* A vendor's entitlement check implemented inside an owned-data provider "because it was convenient".
* Cross-group joins in the gateway process.
* Groups named after teams (`team-alpha-api`) or technologies (`snowflake-api`) rather than what they own.
