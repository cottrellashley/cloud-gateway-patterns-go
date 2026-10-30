# 01. Uniform resource model

**Principle:** everything the platform manages (a capability, a provider, a route, a policy, a dataset, an operation, a ticket) is a *resource* with one envelope (type identity, metadata, desired state, observed state), addressed by a hierarchical name, manipulated through a small fixed set of standard verbs plus explicitly-named custom actions, listed with one pagination shape, and failed with one error shape.

**Why it matters at scale:** clients, SDKs, tooling, authorization, audit and documentation are written once against the envelope and the verbs, not once per type. Kubernetes serves hundreds of kinds through one client library; ARM serves every Azure service through one CLI and one RBAC model because actions are named `provider/type/verb`. Without this, every new type is a new client, a new permission model and a new set of surprises.

## Evidence

**Kubernetes API conventions** ([api-conventions.md §Objects](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L227)): every kind MUST carry `metadata` with `name`, `uid`, `resourceVersion`, `generation`, `creationTimestamp`, `deletionTimestamp`, `labels`, `annotations`, and SHOULD split the body into `spec` (desired) and `status` (observed). The Go types are `TypeMeta` and `ObjectMeta` in [meta/v1/types.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go#L42):

```go
type TypeMeta struct {
    Kind       string `json:"kind,omitempty"`
    APIVersion string `json:"apiVersion,omitempty"`
}
type ObjectMeta struct {
    Name string; GenerateName string; Namespace string; UID types.UID
    ResourceVersion string; Generation int64
    CreationTimestamp Time; DeletionTimestamp *Time; DeletionGracePeriodSeconds *int64
    Labels map[string]string; Annotations map[string]string
    OwnerReferences []OwnerReference; Finalizers []string; ManagedFields []ManagedFieldsEntry
}
```

The verbs are not declared per type; they are **discovered from the interfaces the storage implements**. [`registry/rest/rest.go`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/registry/rest/rest.go#L58) defines `Getter`, `Lister`, `Creater`, `Updater`, `GracefulDeleter`, `Watcher`, `Patcher`, `Connecter`; [`installer.go`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/endpoints/installer.go#L527) turns them into routes:

```go
actions = appendIf(actions, action{"LIST", resourcePath, resourceParams, namer, false}, isLister)
actions = appendIf(actions, action{"POST", resourcePath, resourceParams, namer, false}, isCreater)
actions = appendIf(actions, action{"GET", itemPath, nameParams, namer, false}, isGetter)
actions = appendIf(actions, action{"PUT", itemPath, nameParams, namer, false}, isUpdater)
actions = appendIf(actions, action{"PATCH", itemPath, nameParams, namer, false}, isPatcher)
actions = appendIf(actions, action{"DELETE", itemPath, nameParams, namer, false}, isGracefulDeleter)
actions = appendIf(actions, action{"WATCH", "watch/" + itemPath, nameParams, namer, false}, isWatcher)
actions = appendIf(actions, action{"CONNECT", itemPath, nameParams, namer, false}, isConnecter)
```

A type cannot invent a verb. Non-CRUD behaviour is a **subresource** (`/status`, `/scale`, `/log`, `/exec`) with its own storage. Idempotency and concurrency are uniform too: name clashes on POST are 409 ([§Idempotency](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L804)); every write may carry `resourceVersion` and a mismatch is 409 ([§Concurrency Control](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L1146)). Errors are one kind, `Status`, with `reason`, `code`, `details`.

**Azure Resource Manager** (resource provider contract, mirrored at [resource-api-reference.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/resource-api-reference.md#L26)): every resource is `PUT/GET/PATCH/DELETE .../providers/{namespace}/{type}/{name}?api-version=…`, the body is always `{id, name, type, location, tags, properties, sku, kind, plan}`, "ARM does not distinguish between creation and update … a PUT should always be allowed to overwrite an existing resource", names come from the URL never the body, and names are matched case-insensitively but returned with the caller's casing. Actions are `POST .../{name}/{actionName}` ([proxy-api-reference.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/proxy-api-reference.md#L15)) "so that resource providers will be able to light-up authorization / RBAC scenarios without changing their API surface"; the canonical operation name is `{namespace}/{type}/{read|write|delete|action}` and every provider must list them at `GET /providers/{namespace}/operations` ([§Exposing Available Operations](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/proxy-api-reference.md#L119)). Errors are one OData-shaped object `{error:{code,message,target,details,innererror}}` ([common-api-details.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/common-api-details.md#L83)).

**Google AIPs:** [AIP-121](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0121.md) "a resource-oriented API exposes a large number of resources with a small number of methods on each", and warns "having an API that is identical to the underlying database schema is actually an anti-pattern". [AIP-122](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0122.md): names are `collection/id/collection/id`, lowercase, DNS-safe, exposed as a `name` field. [AIP-131](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0131.md)…135 fix Get/List/Create/Update/Delete; [AIP-136](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0136.md) custom methods are `POST …/{name}:verb`, "verb followed by a noun … must not contain prepositions", used "only for functionality that can not be easily expressed via standard methods". [AIP-158](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0158.md): collections "must provide pagination at the outset, as it is a backwards-incompatible change to add pagination". [AIP-193](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0193.md): one `Status` error with canonical codes.

**Azure REST guidelines** ([Guidelines.md §Performing an Action](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L507)): `POST https://.../<collection>/<id>:<action>` with the colon chosen because it cannot appear in a resource id, "DO NOT use an action operation when the operation behavior could reasonably be defined as one of the standard REST Create, Read, Update, Delete, or List operations".

## The rules

1. **One envelope.** `apiVersion`, `kind`, `metadata{name, uid, resourceVersion, generation, createdAt, deletionAt, finalizers, labels, annotations}`, `spec`, `status`. No type adds top-level fields. Server-owned metadata is ignored on input.
2. **Hierarchical names from the URL.** `/{group}/{version}/{collection}/{name}[/{subcollection}/{name}]`. Lowercase, DNS-safe, unique within the collection, never taken from the body, never containing `:` (reserved for actions).
3. **Standard verbs only.** `GET` item, `GET` collection (paged, filterable), `POST` collection (create, 409 on name clash), `PUT` item (full replace, idempotent), `PATCH` item (merge patch), `DELETE` item (graceful if finalizers), plus `/status` as a separate write path. A type that cannot support a verb simply does not implement its interface; the route does not exist and discovery says so.
4. **Custom actions are explicit.** `POST …/{name}:{verb}` (or `…/{collection}:{verb}`). The action name is the authorization and audit name. Parameters go in the body. Actions that may be slow return an operation (principle 12).
5. **One list shape.** `{items: [...], nextLink}` or `{value, continue}`; opaque continuation tokens bound to the original query; `page_size` clamped; paging present from day one.
6. **One error shape.** `patterns/apierror` for every error on every route including proxied ones; stable machine codes; human message; `target`; `details`; never a stack or an internal hostname.
7. **One status vocabulary.** Terminal states are `Succeeded`, `Failed`, `Canceled`; in-progress states end in `-ing`; conditions are `{type, status, observedGeneration, lastTransitionTime, reason, message}`.
8. **Resources are not tables.** The envelope is designed for the client's mental model and for compatibility, then mapped to storage. A storage column change must never force an API change.

## In this repository

| Mechanism | Where |
| --- | --- |
| Envelope, metadata, conditions, finalizers, generation vs resourceVersion | `patterns/resource` (`Object[S,T]`, `Meta`, `Condition`, `SetCondition`) |
| Store semantics: name idempotency, optimistic concurrency, status subresource, graceful delete | `patterns/resource.MemoryStore` (reference for a Cosmos DB implementation: ETag ⇄ `ResourceVersion`) |
| Conditional requests (`If-Match`, `If-None-Match`, `ETag`) | `patterns/resource.CheckPreconditions`, `SetETag` |
| Error envelope and status mapping | `patterns/apierror`, `patterns/resource.APIError`, `docs/07-services/errors.md` |
| Paged lists with input-bound cursors | `patterns/pagination`, `docs/06-data-and-async/pagination.md` |
| Operations as resources | `patterns/lro`, `docs/06-data-and-async/long-running-operations.md` |
| Route shapes and `:action` with `net/http` patterns | `docs/02-server/routing.md` (note: a `ServeMux` wildcard must be a whole segment, so actions are `POST /v1/x/{name}/{action}` or matched by prefix) |

```go
type CapabilitySpec struct {
    Provider string            `json:"provider"`          // registry ID, e.g. "provider.snowflake_sql"
    Modes    []string          `json:"modes"`             // "execute", "handoff"
    Schema   json.RawMessage   `json:"schema"`            // JSON Schema of parameters
    Result   json.RawMessage   `json:"result,omitempty"`  // writer schema of the result set
}
type CapabilityStatus struct {
    ObservedGeneration int64                `json:"observedGeneration,omitempty"`
    Conditions         []resource.Condition `json:"conditions,omitempty"` // Accepted, Compatible, Ready
    Route              string               `json:"route,omitempty"`
}
type Capability = resource.Object[CapabilitySpec, CapabilityStatus]
```

## Applying it to a capability gateway

* The catalog is a set of collections: `capabilities`, `providers`, `datasets`, `policies`, `operations`, `tickets`, `subscriptions`. Each is `resource.Object` with its own spec/status and lives behind the same handlers (`GET`/`PUT`/`PATCH`/`DELETE`/`/status`).
* Invocation is a custom action on the capability resource: `POST /v1/capabilities/{name}:execute` and `POST /v1/capabilities/{name}:handoff`. The action name is what policy and audit see (`capabilities/execute`, `capabilities/handoff`), so entitlement rules for vendor data attach to the action, not to a path pattern.
* Modes are not separate endpoints with separate shapes; they are the same action family with the same request envelope (`parameters`, `view`, `readerSchema`) and a result envelope that differs only in `kind` (`ResultSet` versus `Handoff`).
* Datasets owned by the platform, vendor datasets and platform-telemetry datasets are all `datasets/{name}` with a `spec.source` that differs; clients page them identically and discover their schema identically (principle 06).
* An agent that has learned the envelope once can operate every collection: list, describe, execute, poll the operation, read the result. That is what makes progressive disclosure cheap.

## Anti-patterns

* `POST /runQuery`, `POST /getPortfolio` — verbs in paths, no resource, no name, nothing to authorize or audit by.
* A type with its own `{ok: false, errorMessage}` error body.
* Lists that return everything today and "will add paging later".
* A `status` written by the same `PUT` as `spec`, so a stale client overwrites the controller's observations.
* Names in bodies (`{"name": "x"}` on `PUT /things/y`), or names that change case or format between write and read.
