# 04. Extension points, not forks

**Principle:** the kernel is small and stable; everything variable (an authentication method, a provider kind, a policy engine, a storage backend, a load-balancing policy, a result encoder) is an *extension* that implements a *namespace interface* and registers itself under `<namespace>.<name>`. Configuration refers to extensions by name; the kernel instantiates them through a registry, runs a fixed lifecycle (load → provision → validate → use → cleanup), type-asserts them to the namespace interface, and can list what is available. Adding behaviour never means editing the kernel.

**Why it matters at scale:** the alternative is a growing `switch provider.Kind` in the core, a kernel every team has to modify (and therefore break), and a release train everyone waits for. Caddy's entire HTTP server is modules in namespaces; Kubernetes grows through admission plugins, storage interfaces and aggregated API servers without touching the apiserver; Dapr ships hundreds of components behind nine interfaces; Terraform supports thousands of providers through one gRPC protocol. Each kernel stays reviewable.

## Evidence

**Caddy modules** ([modules.go](https://github.com/caddyserver/caddy/blob/eb04ed5/modules.go#L54)):

```go
type Module interface { CaddyModule() ModuleInfo }   // "must not have any side-effects"

type ModuleInfo struct {
    ID  ModuleID        // "full name" … must be unique and properly namespaced
    New func() Module   // returns a pointer to a new, empty instance … no other initialization should occur within it
}

// Thus, a module ID has the form: <namespace>.<name>
// - http
// - http.handlers.file_server
// - caddy.logging.encoders.json
```

[`RegisterModule`](https://github.com/caddyserver/caddy/blob/eb04ed5/modules.go#L138) panics on a missing ID, a reserved ID (`caddy`, `admin`), a nil constructor, a constructor returning nil, or a duplicate: wiring mistakes fail at startup. Lifecycle interfaces are optional and discovered by assertion ([`Provisioner`, `Validator`, `CleanerUpper`](https://github.com/caddyserver/caddy/blob/eb04ed5/modules.go#L296)); [`Context.LoadModuleByID`](https://github.com/caddyserver/caddy/blob/eb04ed5/context.go#L366) does `New()` → JSON decode → `Provision` → `Validate`, and on failure calls `Cleanup` "to allow cleaning up from partial provisionings". The website states the design rule ([extending-caddy.md](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/extending-caddy.md), [namespaces.md](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/extending-caddy/namespaces.md)): "If your module is in `<namespace>`, then it should compile as `<type>`" — `http.handlers` → `caddyhttp.MiddlewareHandler`, `http.matchers` → `caddyhttp.RequestMatcher`, `http.reverse_proxy.selection_policies` → `reverseproxy.Selector`, `tls.issuance` → `certmagic.Issuer`. Host modules know the interface; guest modules never import the host ([architecture.md §Module lifecycle](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/architecture.md): "Host modules (or parent modules) are those that load other modules. Guest modules (or child modules) are those that get loaded. All modules are guest modules -- even app modules.").

**Kubernetes admission** ([admission/interfaces.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/admission/interfaces.go#L124), [plugins.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/admission/plugins.go#L71), [chain.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/admission/chain.go#L31)): plugins register by name with a factory; `NewFromPlugins(pluginNames, …)` builds the chain from configuration; the chain calls every `MutationInterface.Admit` and then every `ValidationInterface.Validate`. Policy enforcement is extended by registering, not by editing the apiserver.

**Kubernetes storage and aggregation:** verbs are discovered from which `rest.*` interfaces a storage implements (principle 01); whole API groups are added by registering an [`APIService`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/types.go#L51) (`group`, `version`, `service`, `caBundle`, `groupPriorityMinimum`, `versionPriority`) that the aggregator proxies to. The core server is untouched.

**Dapr components** ([components-contrib `state.Store`](https://github.com/dapr/components-contrib/blob/47da7bd/state/store.go#L29)):

```go
type Store interface { metadata.ComponentWithMetadata; BaseStore; BulkStore }
type BaseStore interface {
    Init(ctx context.Context, metadata Metadata) error
    Features() []Feature                 // ETAG, TRANSACTIONAL, QUERY_API, TTL, …
    Delete / Get / Set
    io.Closer
}
```

Optional capabilities are separate interfaces (`TransactionalStore`, `Querier`) *and* advertised through `Features()` so the runtime can refuse a request a backend cannot honour. The runtime's [state registry](https://github.com/dapr/dapr/blob/3342040/pkg/components/state/registry.go) maps `name/version` → constructor, with `Preferred`, `Deprecated` and `Others` versions ([versioning.go](https://github.com/dapr/dapr/blob/3342040/pkg/components/versioning.go)) and a deprecation warning on use. Component categories are an enum ([category.go](https://github.com/dapr/dapr/blob/3342040/pkg/components/category.go): `bindings`, `pubsub`, `secretstores`, `state`, `workflow`, `middleware`, `configuration`, `crypto`, `lock`, `nameresolution`, …) — the namespaces of the Dapr kernel. [ARC-001](https://github.com/dapr/dapr/blob/3342040/docs/decision_records/architecture/ARC-001-refactor-for-modularity-and-testability.md) made this explicit: "Separate binding implementations to a separate repository … lays the foundation of opening up extensible points (such as Bindings) to the community."

**Terraform providers** ([tfprotov6 `ProviderServer`](https://github.com/hashicorp/terraform-plugin-go/blob/5376f1b/tfprotov6/provider.go#L12)): out-of-process extensions speaking one versioned gRPC protocol — `GetProviderSchema`, `ValidateProviderConfig`, `ConfigureProvider`, `StopProvider`, plus `ResourceServer` (`ValidateResourceConfig`, `UpgradeResourceState`, `ReadResource`, `PlanResourceChange`, `ApplyResourceChange`, `ImportResourceState`, `MoveResourceState`). The core never links a provider.

**Traefik providers** ([provider.go](https://github.com/traefik/traefik/blob/38d53dd/pkg/provider/provider.go#L9)): `Provide(configurationChan chan<- dynamic.Message, pool *safe.Pool) error; Init() error`. Docker, Kubernetes, Consul, file — one interface, one channel.

**Envoy** extends through typed extension configs: every filter, cluster type, access logger and tracer is a message identified by type URL, selected by name in configuration ([http_filters.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/http/http_filters.rst)).

## The rules

1. **Name the namespaces of your kernel** and write down the interface for each. For a gateway: `provider.*` (how a capability is executed), `authn.*`, `authz.*`, `limiter.*`, `encoder.*` (result encodings), `store.*` (catalog/operation storage), `ticket.*` (handoff kinds), `event.sink.*`.
2. **One registry, one ID scheme** `<namespace>.<name>`, lowercase, registered at init, duplicates and reserved names panic at startup.
3. **Constructors are pure; setup is `Provision`; checks are `Validate`; teardown is `Cleanup`.** Partial provisioning is cleaned up. Loading happens for a whole generation, then the generation is activated.
4. **Hosts type-assert, guests never import hosts.** The dependency arrow points from extension to kernel interface only.
5. **Optional behaviour is an optional interface plus an advertised feature list**, never a boolean the kernel has to know about.
6. **Everything in a namespace is listable** (discovery, docs, conformance tests): `registry.Namespace("provider")`.
7. **Extensions declare their configuration schema** (JSON Schema from the struct or `GetProviderSchema`) so configuration is validated before provisioning and documented without prose.
8. **Out-of-process when trust or language differs.** A provider written by another team or in Python runs behind the provider protocol (`docs/07-services/out-of-process-plugins.md`), not inside the gateway process.
9. **Versioned extension contracts.** `provider.snowflake_sql/v1` and `/v2` can coexist; a default version exists; deprecated versions warn.

## In this repository

| Mechanism | Where |
| --- | --- |
| Namespaced registry, panicking registration, lifecycle (`Provisioner`, `Validator`, `Cleaner`), generation `Context` with reverse-order cleanup, typed `LoadAs`, inline-key loading, `Conforms[T]` startup check | `patterns/registry` |
| Chain of middlewares built from a list (the `http.handlers` idea) | `patterns/middleware.Chain`, `docs/01-architecture/request-lifecycle-and-middleware-order.md` |
| Pluggable balancer and transport behind interfaces | `patterns/lb` (`Balancer`), `patterns/proxy.Options` |
| Pluggable stores | `patterns/lro.Store`, `patterns/idempotency.Store`, `patterns/session.Store`, `patterns/resource.Store`, `patterns/ratelimit.Counter` |
| Out-of-process extensions | `docs/07-services/out-of-process-plugins.md` (go-plugin handshake, Connect/gRPC) |

```go
// The kernel declares the namespace interface.
type Provider interface {
    Describe(ctx context.Context) (Manifest, error)
    Execute(ctx context.Context, req ExecuteRequest, sink ResultSink) error
}
type Handoffer interface { Handoff(ctx context.Context, req ExecuteRequest) (Ticket, error) } // optional capability

// An extension registers itself; the kernel never imports it.
type snowflakeSQL struct {
    Account string `json:"account"`
    Pool    int    `json:"pool"`
    sess    *sessionPool
}
func (*snowflakeSQL) ModuleInfo() registry.Info {
    return registry.Info{ID: "provider.snowflake_sql", New: func() registry.Module { return &snowflakeSQL{Pool: 8} }}
}
func (s *snowflakeSQL) Provision(c *registry.Context) error { s.sess = openPool(c.Context(), s.Account, s.Pool); return nil }
func (s *snowflakeSQL) Validate() error { if s.Account == "" { return errors.New("account required") }; return nil }
func (s *snowflakeSQL) Cleanup() error  { return s.sess.Close() }
func init() { registry.Register(&snowflakeSQL{}) }

// The kernel loads a generation from configuration.
gen := registry.NewContext(ctx, registry.Default)
for _, p := range cfg.Providers {                                  // [{"provider":"snowflake_sql","account":"…"}, …]
    prov, err := registry.LoadInlineAs[Provider](gen, "provider", "provider", p)
    ...
    if h, ok := prov.(Handoffer); ok { features = append(features, "handoff") } // optional interface → advertised feature
}
```

## Applying it to a capability gateway

* **Provider kinds are modules in `provider.*`:** `provider.snowflake_sql` (owned data; builds SQL from parameters, runs in the live session pool, streams results), `provider.vendor_rest` (data not owned: a licensed market-data or analytics API, with per-user entitlement checks), `provider.azure_cost`, `provider.github`, `provider.http_passthrough`. The gateway kernel knows only the `Provider` interface and the optional `Handoffer`, `Streamer`, `Subscriber` interfaces.
* **A new domain is a new module plus manifests, never a kernel change.** The checklist for "add YieldBook-style vendor data" is: implement `Provider` (and `Handoffer` if a client-side driver exists), register, add provider configuration, publish manifests. The chain, policy, metering and discovery come for free.
* **Features drive modes.** A capability advertises `modes: [execute, handoff]` only if its provider implements `Handoffer`; the catalog controller refuses a manifest that claims a mode the provider cannot honour (Dapr's `Features()` check).
* **Encoders are modules too** (`encoder.arrow_ipc`, `encoder.json_rows`, `encoder.parquet`), selected by `Accept`, so adding a format never touches a provider.
* **Startup is a conformance test:** `registry.Conforms[Provider](registry.Default, "provider")` runs in `main` and in CI; a module in the namespace that does not implement the interface fails the build, not a request.

## Anti-patterns

* `switch cap.Provider { case "snowflake": … case "bloomberg": … }` anywhere in the gateway.
* Extensions that import the gateway's internal packages to reach into its state.
* Boolean flags on the kernel (`supportsHandoff bool`) instead of optional interfaces and advertised features.
* Registration by editing a central list in `main.go` rather than `init()` in the extension's package (or configuration).
* "Just fork the gateway for this team."
