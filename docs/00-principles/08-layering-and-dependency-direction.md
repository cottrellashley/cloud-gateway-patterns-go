# 08. Layering and dependency direction

**Principle:** inside a service (and inside the gateway) code is arranged in layers with dependencies pointing one way: transport (HTTP, gRPC, SSE, Flight, queue consumers) → application (use cases: commands and queries) → domain (types, invariants) ← adapters (databases, vendor clients, caches). Each boundary is crossed through an explicit conversion; cross-cutting concerns wrap a layer as decorators rather than being written into it; the same application code is served by several transports.

**Why it matters at scale:** when a transport detail (an HTTP status, a JSON tag, a Snowflake type) leaks into business logic, every transport change becomes a business change and every business change becomes a transport change. Layering is what lets Dapr serve one API over HTTP and gRPC, lets Caddy swap any guest module without the host knowing, and lets a service grow new interfaces (an agent-facing one, a batch one) without rewriting its core.

## Evidence

**ardanlabs/service** ([AGENTS.md](https://github.com/ardanlabs/service/blob/9ea1316/AGENTS.md)) states the rules for its `app/`, `business/`, `foundation/` layout:

> Layer conversions (App ↔ Business ↔ Storage). Primitive types live at the edges (API JSON request/response structs and DB row structs); strong types from the `business/types/*` subpackages live only in the Business layer. Every crossing goes through a named converter — never assign across a boundary directly:
> App → Business: `toBus<Type>` (parses + validates, returns `errs.FieldErrors`)
> Business → App: `fromBus<Type>Response` (converts strong → primitive explicitly)
> Business → Storage: `toDB<Type>`
> Storage → Business: `toBus<Type>` (parses native → strong, returns error)
>
> Business-layer extensions (the `ExtBusiness`/`Extension` decorator pattern). Cross-cutting concerns (OTEL, logging, metrics, caching, auth) wrap the core `Business` without modifying it.

`foundation/` (web, logger, otel, keystore) has no business knowledge; `business/` has no HTTP imports; `app/` is the thin request/response layer.

**kratos-layout** ([tree](https://github.com/go-kratos/kratos-layout/tree/59ad406)): `api/` (protobuf contracts, generated), `internal/server/` (HTTP and gRPC construction), `internal/service/` ("transport-facing service methods"), `internal/biz/` ("usecases, entities, errors, repository interfaces"), `internal/data/` ("repository implementations"), `internal/conf/`. The repository interface is declared in `biz` and implemented in `data`: the dependency points inward.

**wild-workouts** ([internal/trainings](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/tree/8ecfcdf/internal/trainings)): `ports/` (HTTP and gRPC handlers), `app/command/` and `app/query/` (use cases, CQRS), `domain/training/` (entity with methods such as `ApproveReschedule` that enforce invariants), `adapters/` (Firestore repository, gRPC clients). The application is a struct of handlers ([app.go](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/blob/8ecfcdf/internal/trainings/app/app.go)):

```go
type Application struct { Commands Commands; Queries Queries }
type Commands struct {
    ApproveTrainingReschedule command.ApproveTrainingRescheduleHandler
    CancelTraining            command.CancelTrainingHandler
    ...
}
```

Each command handler is wrapped by decorators ([decorator/command.go](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/blob/8ecfcdf/internal/common/decorator/command.go)):

```go
func ApplyCommandDecorators[H any](handler CommandHandler[H], logger *logrus.Entry, metricsClient MetricsClient) CommandHandler[H] {
    return commandLoggingDecorator[H]{ base: commandMetricsDecorator[H]{ base: handler, client: metricsClient }, logger: logger }
}
```

and the repository exposes an *update closure* so the invariant runs inside the transaction without the domain knowing about the database ([approve_training_reschedule.go](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example/blob/8ecfcdf/internal/trainings/app/command/approve_training_reschedule.go)): `h.repo.UpdateTraining(ctx, cmd.TrainingUUID, cmd.User, func(ctx, tr *training.Training) (*training.Training, error) { if err := tr.ApproveReschedule(cmd.User.Type()); err != nil { return nil, err } … })`.

**Dapr ARC-001** ([decision record](https://github.com/dapr/dapr/blob/3342040/docs/decision_records/architecture/ARC-001-refactor-for-modularity-and-testability.md)): "Formally separate hosting and API implementations. Hosting provides communication protocols (HTTP/gRPC) as different access heads to the same Dapr API implementation. Ensure consistency between gRPC and HTTP interface."

**Caddy** encodes the direction in the host/guest rule ([architecture.md](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/architecture.md)): the host knows the guest's interface; the guest never imports the host; the `http.handlers` namespace interface is `MiddlewareHandler.ServeHTTP(w, r, next) error` ([caddyhttp.go](https://github.com/caddyserver/caddy/blob/eb04ed5/modules/caddyhttp/caddyhttp.go#L90)) — handlers return errors and the host maps them, the same inversion ardanlabs uses with `HandlerFunc … Encoder`.

**Envoy** separates decode (request) and encode (response) paths per filter ([http_filters.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/http/http_filters.rst)) and separates the *connection manager* (protocol) from *filters* (behaviour) from the *router* (upstream selection): protocol code never contains routing policy.

## The rules

1. **Four layers, one direction.** `ports`/`transport` → `app` → `domain`; `adapters` implement interfaces declared in `app`/`domain`. Enforce with import rules in CI (`go list -deps` or `depguard`): `domain` imports nothing of ours; `app` imports `domain`; `adapters` import `app`/`domain`; `ports` import `app`.
2. **Convert at every boundary with named functions.** Wire types (JSON structs, protobuf) and storage types (rows, documents, Arrow schemas) never cross into `app`/`domain`. `toDomainX`, `fromDomainX`, `toRowX` are the only places a field name appears twice.
3. **Handlers return values and errors; the transport layer encodes them.** One mapping from domain errors to the error envelope (`apierror.From` extended with domain sentinel errors), in one place.
4. **Commands and queries are separate types with separate handlers.** Queries may read optimised projections; commands go through the domain's invariants. Neither knows about HTTP.
5. **Cross-cutting concerns are decorators around handlers** (logging, metrics, tracing, caching, authorization at the use-case level), composed once in the application constructor, never written inside a handler.
6. **One implementation, many heads.** HTTP/JSON, Connect/gRPC, SSE, Flight and queue consumers all call the same `app` handlers. Behavioural parity between heads is tested.
7. **Invariants live with the entity** and run inside the storage transaction through a closure/repository pattern, so the domain never imports a database.
8. **Configuration enters at `main`**, is parsed once into typed values, and is passed down. No layer reads environment variables.

## In this repository

| Mechanism | Where |
| --- | --- |
| Reference layouts and the thin web layer (`HandlerFunc … Encoder`) | `docs/07-services/service-skeleton.md` |
| Domain error → envelope mapping in one place | `patterns/apierror.From`, `patterns/resource.APIError`, `docs/07-services/errors.md` |
| Decorator-style cross-cutting concerns at the transport layer | `patterns/middleware`, `patterns/telemetry.AccessLog`, `RequestMetrics` |
| Interfaces declared where they are consumed (stores, balancers, recorders) | `patterns/lro.Store`, `patterns/lb.Balancer`, `patterns/telemetry.Recorder` |
| Configuration parsed once, validated, typed | `docs/07-services/configuration.md`, `patterns/config` |
| Testing each layer alone and the heads for parity | `docs/07-services/testing.md` |

```
service/
  cmd/service/main.go          parse config, build adapters, build app, build ports, run lifecycle group
  internal/domain/capability/  Manifest, Schema, Plan, invariants (Validate, Compatible(prev)), sentinel errors
  internal/app/command/        ExecuteCapability, IssueHandoff, RegisterManifest     (write side)
  internal/app/query/          DescribeCapability, ListCatalog, GetOperation          (read side)
  internal/app/app.go          Application{Commands, Queries}; decorators applied here
  internal/adapters/           snowflake (session pool, SQL build via SQLGlot service), cosmos (catalog, operations), vendor clients
  internal/ports/http/         handlers: decode → app → encode; apierror; SSE
  internal/ports/flight/       DoGet(ticket) → app query → Arrow stream
  internal/ports/jobs/         queue consumer → app command
```

## Applying it to a capability gateway

* **The gateway kernel is layered too.** `domain`: Capability, Provider, Policy, Operation, Ticket, Snapshot and their invariants (a capability may not advertise a mode its provider lacks; a ticket may not outlive its operation). `app`: `RegisterCapability`, `PublishSnapshot`, `ExecuteCapability`, `IssueHandoff`, `CancelOperation`, `DescribeCapability`. `adapters`: Cosmos catalog store, Snowflake session pool, vendor HTTP clients, Key Vault, the registry of provider modules. `ports`: the HTTP chain, SSE, the Flight endpoint, the catalog change-feed consumer.
* **`execute` and `handoff` are two commands over one domain plan.** Both build the same `Plan` (compiled SQL, parameters, expected schema, cost estimate) from the manifest and parameters; `execute` hands it to the session-pool adapter and streams; `handoff` hands it to the ticket adapter. Mode selection is a transport-level choice; plan building is domain logic tested without any transport.
* **Vendor adapters hide vendor shapes.** A vendor response type never leaves `adapters/vendor_x`; it becomes a domain `ResultSet` with the writer schema at the boundary. Entitlement rules are domain logic in the vendor group, enforced in its command handler, not in the HTTP handler.
* **Agents and SDKs are just another head.** An MCP-style tool interface, a Python SDK and the HTTP API all call the same `app` handlers, so progressive disclosure behaves identically everywhere.

## Anti-patterns

* `json:"..."` tags on domain types, or `*http.Request` parameters on use-case functions.
* `w.WriteHeader(500)` inside business logic.
* A repository that returns rows and a handler that interprets them.
* Logging and metrics calls sprinkled through handlers instead of a decorator.
* Two transports with two slightly different behaviours because each re-implemented the use case.
