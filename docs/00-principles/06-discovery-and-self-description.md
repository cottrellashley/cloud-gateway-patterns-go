# 06. Discovery and self-description

**Principle:** the platform can enumerate what it has (groups, kinds, capabilities, verbs, actions, modes) and describe each thing in a machine-readable way (schema, field behaviour, maturity, limits, examples) at run time, from the same source that serves it. Schemas are *served*, not compiled into clients. Disclosure is progressive: list → describe → schema → example, so a client (or an agent) learns only what it needs for the task in front of it.

**Why it matters at scale:** hard-coded clients are the reason schema changes require coordinated releases. Kubernetes' `kubectl` and every controller work against kinds they have never seen because discovery and OpenAPI tell them the shape; ARM tooling builds RBAC and audit UIs from the operations list; Terraform core understands any provider from `GetProviderSchema`. For agents the point is sharper: an agent cannot be shipped with a thousand capability schemas in its prompt; it must be able to find, describe and then use them.

## Evidence

**Kubernetes discovery:** `GET /apis` lists groups and versions; `GET /apis/{group}/{version}` returns an `APIResourceList` of [`APIResource`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apimachinery/pkg/apis/meta/v1/types.go#L1249):

```go
type APIResource struct {
    Name string; SingularName string; Namespaced bool
    Group string; Version string; Kind string
    Verbs Verbs                 // discovered from the storage interfaces, not declared
    ShortNames []string; Categories []string
    StorageVersionHash string
}
```

The verbs are the ones [`installer.go`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/endpoints/installer.go#L527) derived from the implemented interfaces, so discovery can never disagree with behaviour. Aggregated discovery ([aggregated/handler.go `AddGroupVersion`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/endpoints/discovery/aggregated/handler.go#L252)) merges every extension server into one document, ordered by priority, served with an ETag. `/openapi/v3` serves the schemas with `x-kubernetes-*` extensions for field behaviour. `kubectl explain` is just a reader of those documents.

**Azure Resource Manager** requires every provider to publish `GET /providers/{namespace}/operations` ([proxy-api-reference.md §Exposing Available Operations](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/proxy-api-reference.md#L119)): `{name: "{namespace}/{type}/{read|write|delete|action}", display: {provider, resource, operation, description}, origin: "user|system"}`, "used by several clients for (1) custom role definitions for RBAC; (2) complex query filters for the event service; and (3) audit history". The same list drives permissions, filtering and audit UIs; it "should not vary based on particular subscriptions".

**Terraform** ([`GetProviderSchemaResponse`](https://github.com/hashicorp/terraform-plugin-go/blob/5376f1b/tfprotov6/provider.go#L180)): `ServerCapabilities`, `Provider *Schema`, `ResourceSchemas map[string]*Schema`, `DataSourceSchemas`, `Functions`. Core validates configuration against the provider's own schema before any plan.

**Dapr** components describe themselves through `Features()` ([feature.go](https://github.com/dapr/components-contrib/blob/47da7bd/state/feature.go): `ETAG`, `TRANSACTIONAL`, `QUERY_API`, `TTL`, `DELETE_WITH_PREFIX`, …) and a metadata schema (`metadata.ComponentWithMetadata`), so the runtime can answer "can this store do a transaction" without trying.

**Caddy** ([namespaces.md](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/extending-caddy/namespaces.md)) publishes the namespace → interface table, `caddy list-modules` lists what is compiled in, and the JSON docs are generated from module structs; configuration is self-describing because every module is.

**Google AIPs:** [AIP-203](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0203.md) field behaviour (`REQUIRED`, `OPTIONAL`, `OUTPUT_ONLY`, `IMMUTABLE`, `INPUT_ONLY`, `IDENTIFIER`) "must" be on every request field; [AIP-157](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0157.md) partial responses via `view` enums (`BASIC`, `FULL`) as the primary pattern, field masks as the alternative; [AIP-216](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0216.md) state enums with consistent vocabulary (`ACTIVE`, `CREATING`, `DELETING`, `SUCCEEDED`, `FAILED`).

**Self-reflective data contracts** (covered in the research report and `docs/06-data-and-async/arrow-flight-data-plane.md`): Arrow carries its schema in the stream; Avro resolves a writer schema against a reader schema; GraphQL exposes `__schema`; Flight SQL answers `GetSqlInfo`/`GetTables`. The data describes itself to whoever reads it.

## The rules

1. **Four levels, in order:** (1) *list* — names, one-line descriptions, kinds, maturity, modes; (2) *describe* — the full manifest of one thing: parameters schema, result schema, limits, actions, examples, policies that apply; (3) *schema* — the JSON Schema / Arrow schema documents by version, cacheable, ETagged; (4) *try* — a dry-run or example invocation. A client never needs level 3 to decide at level 1.
2. **Discovery is derived, never hand-written.** Verbs come from implemented interfaces; modes come from advertised features; schemas come from the registered manifest. If a thing is routable it is discoverable, and vice versa.
3. **Every field carries behaviour** (required, optional, output-only, immutable, default, deprecated, since). Every enum says whether it is extensible.
4. **Results carry their schema.** A result set includes (or links by version) the writer schema it was produced with, and honours a reader schema the client asks for.
5. **Discovery documents are versioned, ETagged, cacheable and cheap**, served from the data-plane snapshot (principle 03), not from the catalog store per request.
6. **Operation names are the authorization and audit names** (`capabilities/execute`), and the operations list is what the policy editor and the audit viewer read.
7. **Documentation is generated from the same manifests**, so it cannot drift.
8. **Views keep responses small**: `?view=basic|full`, and list endpoints default to `basic`.

## In this repository

| Mechanism | Where |
| --- | --- |
| Listing a namespace of extensions (the "what is compiled in" list) and asserting conformance | `patterns/registry.Namespace`, `registry.Conforms[T]` |
| Resource kinds with `apiVersion`/`kind` so every document identifies its own schema | `patterns/resource.Object` |
| Health reporting that enumerates named checks (`?verbose`) — the same shape for operational discovery | `patterns/health` |
| ETagged, conditional GETs for discovery documents | `patterns/resource.CheckPreconditions`, `patterns/lro.GetHandler` (weak ETag / 304) |
| Self-describing result sets and tickets | `docs/06-data-and-async/arrow-flight-data-plane.md` |
| Route labels (`r.Pattern`) as the stable names metrics and docs share | `patterns/telemetry.RouteFrom` |

```go
// Level 1: list. Cheap, cacheable, served from the snapshot.
type CatalogEntry struct {
    Name        string   `json:"name"`          // "owned.portfolios.positions"
    Kind        string   `json:"kind"`          // "Capability"
    Summary     string   `json:"summary"`
    Group       string   `json:"group"`         // "owned", "vendor", "platform"
    Modes       []string `json:"modes"`         // derived from provider features
    Maturity    string   `json:"maturity"`      // alpha | beta | ga | deprecated
    Describe    string   `json:"describe"`      // link to level 2
}
// Level 2: describe. The manifest itself plus the actions and policies that apply.
// Level 3: GET /v1/schemas/{name}@{version}  → JSON Schema, ETag, Cache-Control: max-age=86400
// Level 4: POST /v1/capabilities/{name}:execute?dryRun=true → validation result + plan, no execution
```

## Applying it to a capability gateway

* **Agents start at level 1.** `GET /v1/catalog?view=basic&group=owned` returns a few hundred bytes per capability. The agent picks one and calls `GET /v1/capabilities/{name}` for the manifest (parameters, result schema summary, modes, limits, examples, entitlement notes). Only when it is about to call does it fetch the full schema version it will use. Nothing about a capability lives in the agent's prompt.
* **Modes are described, not guessed.** The manifest lists `execute` (service runs the query in its live session and streams back) and `handoff` (compiled query + connection descriptor + expected schema for the client's own driver) with, for each, the request/response kinds and the limits that apply. A mode absent from discovery is absent from routing.
* **Result sets are self-describing**: every `execute` response carries `schema: {version, fields: [{name, type, nullable, since, deprecated, alias}]}` and honours `readerSchema` so a client built against version 3 keeps receiving version-3 columns from a version-7 server.
* **Owned, vendor and platform data are described uniformly** but the manifest says what differs: `source.kind`, licence/entitlement requirements for vendor data, freshness and cost hints for platform data.
* **The operations list** (`capabilities/read`, `capabilities/execute`, `capabilities/handoff`, `datasets/read`, `operations/cancel`, …) is served at `/v1/operations-catalog` and is what the policy bundle and the audit schema reference.

## Anti-patterns

* A client SDK with the result columns compiled in.
* A README that lists capabilities, maintained by hand.
* Discovery that says `modes: [execute, handoff]` because the manifest author typed it, while the provider cannot hand off.
* One giant `/describe-everything` endpoint agents must read before doing anything.
* Enums with no statement of extensibility, fields with no statement of requiredness.
