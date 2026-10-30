# 15. Operating many services: the paved road

**Principle:** the platform scales in *number of services and teams* only if there is one paved road: a service template that encodes the principles, an API review that gates GA, decision records that explain why, a conformance suite that every service passes, one release and deprecation calendar, and generated documentation. Variation is allowed off the road, but the road is the default and the default is good.

**Why it matters at scale:** every principle in this directory can be followed by one careful team and lost by the twentieth. Kubernetes keeps hundreds of API types coherent through API review, KEPs and conformance; Azure keeps thousands of operations coherent through the REST API Stewardship Board and mandatory previews; Dapr records architectural decisions so newcomers inherit the reasoning; kratos and ardanlabs ship templates so a new service starts correct. The paved road turns principles into defaults.

## Evidence

**Kubernetes API review and maturity process** ([api_changes.md §Alpha, Beta, and Stable Versions](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api_changes.md#L1087)): alpha "need not have had an *API review* (an intensive and targeted review of the API, on top of a normal code review)"; beta requires "all API operations, CLI commands, and UI support should be implemented; end-to-end tests complete; the API has had a thorough API review"; stable requires conformance. [conformance-tests.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/conformance-tests.md): "a subset of e2e tests that SIG Architecture has approved to define the core set of interoperable features that all conformant Kubernetes clusters must support … tests are proven to meet the conformance test requirements by review and by accumulation of data on flakiness and reliability", and they "test only GA, non-optional features or APIs". [feature-gates.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/feature-gates.md): gates "are expected to be deprecated and removed after a feature becomes GA". The conventions documents themselves ([api-conventions.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md), [controllers.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-api-machinery/controllers.md)) are the written road.

**Azure API Stewardship** ([ConsiderationsForServiceDesign.md](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/ConsiderationsForServiceDesign.md)): "Start with the Developer Experience … Focus on Hero Scenarios … Start with your API Definition", "YOU SHOULD release and evaluate a minimum of 2 preview versions prior to the first GA release", "YOU SHOULD write and test hypotheses about how your customers will use the API", "consider doing a *code with* exercise", and a review board with a typical session ([§Getting Help: The Azure REST API Stewardship Board](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/ConsiderationsForServiceDesign.md#L667)). The [Guidelines.md](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md) use `DO`/`YOU SHOULD`/`YOU MAY`/`DO NOT` so a review is a checklist, not a debate.

**Decision records.** Dapr keeps ADRs in the repository ([docs/decision_records](https://github.com/dapr/dapr/tree/3342040/docs/decision_records): `API-001-state-store-api-design`, `API-005-state-store-behavior`, `API-010-appcallback-versioning`, `ARC-001-refactor-for-modularity-and-testability`, `ARC-002-multitenancy`, `ENG-002-Dapr-Release`, …), each with Status / Context / Decisions / Consequences. [API-010](https://github.com/dapr/dapr/blob/3342040/docs/decision_records/api/API-010-appcallback-versioning.md) is a record of a decision *not* to build something, with the reason: "Introducing versioning to app callback APIs would require changes to the user applications which is not feasible".

**Templates.** [kratos-layout](https://github.com/go-kratos/kratos-layout/tree/59ad406): "Use this repository as a starting point for a new service … Layered `service`, `biz`, and `data` packages … Unit tests for the service layer … `make config`" — a new service starts with the layering, the config protobufs, the servers and the tests in place. [ardanlabs/service](https://github.com/ardanlabs/service/blob/9ea1316/AGENTS.md) goes further and ships the rules *as agent instructions*: layer conversions, the extension seam, `make test`/`lint`/`vuln-check`, skills for the layered architecture — the road is machine-readable.

**Generated documentation.** Caddy's module docs and JSON schema are generated from module structs ([architecture.md](https://github.com/caddyserver/website/blob/a49be19/src/docs/markdown/architecture.md)); Kubernetes' reference docs come from OpenAPI; ARM's operations list feeds portals and RBAC editors. Documentation that is generated cannot be stale.

**Release discipline.** Envoy names every runtime guard in release notes ([runtime.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/runtime.rst)); Kubernetes supports two consecutive beta versions for a cycle ([api_changes.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api_changes.md#L1087)); Azure communicates deprecations in-band with `azure-deprecating` ([Guidelines.md](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L820)).

## The rules

1. **One service template** (repository or generator) that produces: the layered layout (principle 08), `main` with the lifecycle group and the reduced chain (principle 09), health/metrics/pprof, the error envelope, configuration parsing, a manifest skeleton, a conformance test harness, CI with `gofmt`/`vet`/race tests/vulnerability check, and `AGENTS.md`. Starting any other way needs a written reason.
2. **API review before beta, conformance before GA.** Review is a checklist derived from the principles (the one in `README.md` here) applied to the contract, not to the code. Reviewers are a small named group; a review takes an hour; the outcome is recorded.
3. **Decision records for anything that changes a principle's application.** `docs/decisions/NNNN-title.md` with Status, Context, Decision, Consequences, and the alternatives rejected. Records are immutable; superseding records link back.
4. **Conformance suite = the spec made executable.** For each principle and each wire contract (envelope, errors, paging, operations, tickets, result-set schema, headers), a black-box test any service or provider must pass. Only GA behaviour is in the suite; the suite runs in every provider's CI against a local gateway and in the gateway's CI against reference providers.
5. **One calendar:** release cadence, support window per maturity level, deprecation lead time, sunset dates. Published; changes to it are themselves decision records.
6. **Generated docs only** for contracts: catalog, manifests, schemas, operations list, error codes, headers. Prose explains intent; it never lists fields.
7. **Golden paths are measured:** time from template to first GA capability, conformance pass rate, number of off-road exceptions, deprecations overdue. If the road is slow, fix the road.
8. **Agent-readable rules.** `AGENTS.md` in every repository states the rules an automated contributor must follow (layering, chain, error envelope, tests), as ardanlabs does; this repository is the shared source those files point at.
9. **Exceptions are explicit and expiring.** Off-road choices are recorded with an owner and a review date, not tolerated silently.

## In this repository

| Mechanism | Where |
| --- | --- |
| The road itself: principles, mechanisms, decision table, rules | `docs/00-principles/README.md`, `AGENTS.md` |
| Service skeleton and layouts | `docs/07-services/service-skeleton.md`, `cmd/gateway/main.go` |
| Testing strategy incl. conformance-style black-box tests | `docs/07-services/testing.md` |
| Configuration conventions | `docs/07-services/configuration.md` |
| Health, metrics, profiling as standard endpoints | `patterns/health`, `patterns/telemetry`, `docs/05-observability/*` |
| CI gate | `Makefile` (`make check`), `.github/workflows/ci.yml` |
| Startup conformance of extensions | `patterns/registry.Conforms[T]` |
| Sources pinned so rules can be re-verified | `docs/SOURCES.md`, `NOTICE.md` |

```
provider-template/
  AGENTS.md                      rules for humans and agents: layering, chain, envelope, tests, how to register a capability
  cmd/provider/main.go           lifecycle group, reduced chain, health/metrics/pprof, registry generation
  internal/{domain,app,adapters,ports}/
  manifests/*.json               capability manifests (contract first)
  conformance/                   runs the shared suite against this provider: envelope, errors, paging, operations, result schema, headers
  docs/decisions/0001-record-architecture-decisions.md
  Makefile                       check = fmt + vet + race tests + conformance; release = version + changelog + manifests publish
  .github/workflows/ci.yml
```

## Applying it to a capability gateway

* **Two templates:** the gateway kernel (one repository, this library as its dependency) and the *provider template* every data group uses (owned-data SQL providers, vendor wrappers, platform sources). A new group starts by cloning the template, writing manifests, implementing `Provider`, and passing conformance; the gateway never changes.
* **API review** for every capability promoted to `beta`, using the fifteen-question checklist against the manifest: resource names, modes vs features, schema compatibility mode, maturity, entitlement declaration for vendor data, operation class, limits, deprecation plan. Recorded as a status condition (`Reviewed=True, reason=APIReview-2026-11`) on the capability.
* **Conformance suite** derived from the specification's conformance checklist: envelope and metadata; error codes; `Idempotency-Key` semantics; 202/operation/cancel/retention; result-set schema and reader-schema projection; handoff ticket format and redemption; required headers; pagination tokens; discovery levels. Providers run it locally; the gateway runs it against reference providers on every build.
* **Decision records already due:** no-OBO consequences (service identity + platform policy), handoff ticket credential model (caller vs delegated), schema compatibility modes per group, versioning choice (`/v1/` + manifest versions), telemetry record schema v1.
* **Calendar:** monthly gateway releases; capability maturity windows (alpha unlimited, beta ≤ 2 releases per generation supported, GA until sunset + 6 months); deprecation announced in catalog + headers ≥ 90 days before sunset.

## Anti-patterns

* Each team's service with its own layout, its own error shape and its own health endpoint "because the template was too opinionated".
* GA without review, review without a checklist, checklist without the principles.
* Decisions remembered by the person who made them.
* A conformance suite that tests alpha behaviour and therefore blocks every change.
* Documentation that lists fields by hand.
* Deprecations with no date.
