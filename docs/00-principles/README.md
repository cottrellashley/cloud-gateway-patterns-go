# Principles: how these systems scale to hundreds of services without losing control

The documents in `docs/01` to `docs/09` and the packages in `patterns/` are **mechanisms**: how to drain a server, retry a call, verify a token, page a list. Mechanisms are necessary and they are the small part. The systems this repository is distilled from (Kubernetes, Azure Resource Manager, Envoy, Caddy, Traefik, Dapr, Terraform, Google's and Azure's API programmes) each coordinate thousands of resource types, services or extensions written by people who never meet. They manage that with a short list of **architectural principles** that every component obeys, so that adding the two-hundredth service costs the same as adding the second.

This directory states those principles, shows where each one is enforced in the upstream source (pinned commits, same as the rest of the repository), turns each into rules, and maps each to the mechanisms here. Read it before the pattern docs. When a pattern doc and a principle appear to disagree, the principle wins and the pattern doc has a bug.

## The fifteen principles

| # | Principle | The rule in one line | Proven at scale by | Mechanisms in this repo |
| --- | --- | --- | --- | --- |
| [01](01-uniform-resource-model.md) | Uniform resource model | Everything the platform manages is a resource with one envelope, standard verbs, custom actions as `:verb`, one error shape, one list shape. | Kubernetes API conventions, ARM resource contract, Google AIP-121/131/136 | `patterns/resource`, `apierror`, `pagination`, `lro` |
| [02](02-declarative-intent-and-reconciliation.md) | Declarative intent, reconciliation | Clients write desired state (`spec`); controllers converge actual state and report it (`status`); loops are level-triggered, idempotent, rate-limited. | Kubernetes controllers, ARM `provisioningState`, Terraform plan/apply | `patterns/reconcile`, `resource`, `lro`, `config` |
| [03](03-control-plane-and-data-plane.md) | Control plane / data plane | Configuration, policy and catalog flow through a versioned, eventually-consistent control plane; bytes flow through a data plane that keeps working when the control plane is down. | Envoy xDS, ARM management vs data plane, Arrow Flight, Kubernetes | `config.Hot`, `lb`, `proxy`, `httpserver`, Flight doc |
| [04](04-extension-points-not-forks.md) | Extension points, not forks | A small kernel defines namespaces; each namespace is an interface; implementations register by name and go through provision → validate → use → cleanup. | Caddy modules, Kubernetes admission/storage/aggregation, Dapr components, Terraform providers, Traefik providers | `patterns/registry`, `middleware.Chain`, out-of-process plugins doc |
| [05](05-contracts-first-and-compatibility.md) | Contracts first, compatibility as a rule set | The API is designed before the code; changes are judged against written compatibility rules; features move alpha → beta → GA behind gates; deprecation is announced in-band. | Kubernetes API changes doc and feature gates, AIP-180/185, Azure `api-version` rules, Dapr component versioning | `apierror` codes, `pagination`, `config.Version`, `resource` |
| [06](06-discovery-and-self-description.md) | Discovery and self-description | The platform can enumerate what it has and describe each thing in machine-readable form; schemas are served, not shipped; disclosure is progressive. | Kubernetes discovery and OpenAPI, ARM operations list, Terraform `GetProviderSchema`, Dapr `Features()`, Caddy namespaces | `registry.Namespace`, `health` verbose, `resource` kinds |
| [07](07-bounded-contexts-and-service-boundaries.md) | Bounded contexts and service boundaries | Group resources by ownership and lifetime into named groups; a group owns its data and exposes contracts and events, never its tables. | Kubernetes API groups, ARM provider namespaces, ARM "designing resources", AIP hierarchy, wild-workouts | routing, events, idempotency docs |
| [08](08-layering-and-dependency-direction.md) | Layering and dependency direction | Dependencies point inward: transport → application → domain; adapters at the edges; convert at every boundary; one implementation behind many transports. | ardanlabs/service, kratos-layout, wild-workouts, Dapr ARC-001, Caddy host/guest rule | service skeleton, `apierror.From`, `telemetry` |
| [09](09-cross-cutting-concerns-as-ordered-chains.md) | Cross-cutting concerns as ordered chains | Every concern that applies to all requests is a filter in a declared chain with a fixed order; the handler never re-implements one. | Kubernetes `DefaultBuildHandlerChain`, Envoy filter ordering, Caddy route compilation, Traefik middleware builder, admission mutate-then-validate | `middleware.Chain`, `cmd/gateway/main.go` |
| [10](10-identity-and-policy-as-platform-services.md) | Identity and policy as platform services | Authenticate once at the edge, propagate a signed principal, authorize on attributes with chained deciders, audit every decision; services never see client credentials. | Kubernetes authn/authz interfaces and aggregator, ARM proxy headers, AIP-211 | `auth`, `session`, authorization doc |
| [11](11-idempotency-and-optimistic-concurrency.md) | Idempotency and optimistic concurrency | Every operation is safe to repeat: name idempotency on create, resource versions on write, request IDs on actions, server-defined retry policy. | Kubernetes name idempotency and `resourceVersion`, ARM PUT, AIP-154/155, Azure repeatability | `resource` store, `idempotency`, `resilience` |
| [12](12-asynchrony-is-explicit.md) | Asynchrony is explicit | Anything that may outlive a request is an operation resource with terminal states, a status monitor, cancellation and retention; streams and events are typed and resumable. | ARM async contract, AIP-151, Azure LRO guidelines, Kubernetes watch, Temporal determinism | `lro`, `sse`, events and background-jobs docs |
| [13](13-failure-isolation-and-blast-radius.md) | Failure isolation and blast radius | Bulkheads per dependency, fail fast, shed before collapse, make-before-break on every change, kill switches for new behaviour. | Envoy circuit breaking, outlier detection, overload manager, draining and hot restart; Kubernetes workqueue and flow control; Caddy atomic reloads | `resilience`, `shedding`, `ratelimit`, `lb`, `httpserver`, `lifecycle` |
| [14](14-observability-as-a-contract.md) | Observability as a contract | Correlation IDs, request records, audit and metrics are part of the API contract, versioned like any other schema, and emitted by the chain, not the handler. | ARM required headers, Azure tracing rules, Kubernetes audit and latency trackers, Envoy stats | `telemetry`, request-telemetry-and-audit doc |
| [15](15-operating-many-services.md) | Operating many services (the paved road) | Templates not tribal knowledge; API review before GA; decision records; conformance tests; one release and deprecation calendar. | kratos-layout, ardanlabs skeleton, Kubernetes API review and conformance, Azure API Stewardship Board, Dapr decision records | service skeleton, testing, `Makefile`, CI |

## The constitution (what an agent must hold in its head)

1. Model it as a resource before you model it as an endpoint. If it has a name and a lifecycle it gets the envelope.
2. Clients express intent; the platform converges. No RPC whose meaning is "do step 3 of a workflow".
3. Control plane and data plane are different processes with different SLOs. Bytes never ride the control plane.
4. New behaviour arrives as a registered module in an existing namespace, not as an `if` in the kernel.
5. Write the contract first, run the compatibility rules on every change, gate anything not GA.
6. If a client has to hard-code it, the platform has failed to describe it. Serve the schema.
7. One group owns one set of data. Cross-group access goes through that group's contract or its events.
8. Dependencies point inward. Transport code never contains a business rule; business code never imports a transport.
9. Cross-cutting behaviour is a chain element with a documented position. Handlers never re-implement one.
10. The edge authenticates; everything behind it trusts a signed principal and decides on attributes.
11. Every write is repeatable and every read is conditional. No operation exists that is unsafe to retry.
12. Anything slower than a request is an operation with an ID, a terminal state and a retention period.
13. A dependency failure degrades one capability, never the catalog. Every change is make-before-break.
14. Every hop logs the same correlation IDs and the request record is a schema, not a log line.
15. There is one template, one review, one conformance suite and one deprecation calendar.

## How the principles fit together

```
                         ┌────────────────────────── Organisation ──────────────────────────┐
                         │ 07 bounded contexts   08 layering   15 paved road / API review   │
                         └───────────────────────────────┬──────────────────────────────────┘
                                                         │ shapes what gets built
 ┌──────────────────── Control plane ─────────────────┐  │  ┌───────────────────── Data plane ─────────────────────┐
 │ 01 resources  02 reconcile  04 registry/extensions │◄─┴─►│ 09 ordered chain  10 identity  13 failure isolation │
 │ 05 contracts & versions   06 discovery             │ 03  │ 11 idempotency    12 operations/streams   14 telemetry│
 └────────────────────────────────────────────────────┘     └──────────────────────────────────────────────────────┘
        desired state, catalog, policy, schemas                   requests, bytes, tickets, events
        eventually consistent, versioned snapshots                 bounded, shed, retried, audited
```

The control plane is where the resource model, reconciliation, registry, contracts and discovery live. The data plane is where the chain, identity propagation, idempotency, operations and failure isolation live. Principle 03 is the seam between them, and principles 07, 08 and 15 decide how teams carve the whole into pieces that stay coherent.

## Reading order

* **Building the gateway kernel:** 01 → 02 → 04 → 03 → 09 → 10 → 13 → 14, then `cmd/kernel/main.go` (control plane) and `cmd/gateway/main.go` (data plane).
* **Adding a new service or provider behind the gateway:** 07 → 08 → 05 → 06 → 11 → 12 → 15, then `docs/07-services/service-skeleton.md`.
* **Reviewing a design:** the checklist below, then the principle each failing question points at.

## The review checklist

A design or pull request for the gateway or a service behind it is ready when every answer is yes.

1. Is every managed thing a resource with the standard envelope, and are non-CRUD operations expressed as `:action` on a resource or collection? (01)
2. Does the writer express desired state and does a controller converge, with `status` carrying `observedGeneration` and conditions? (02)
3. Can the data plane keep serving the last good configuration if the control plane store is unavailable for an hour? (03)
4. Is the new behaviour a module in a namespace with provision/validate/cleanup, discoverable by listing the namespace? (04)
5. Was the contract written first, were the compatibility rules applied, and is anything non-GA behind a gate with a documented removal? (05)
6. Can a client that has never seen this resource type discover it, fetch its schema and know which fields are required, output-only and immutable? (06)
7. Does the resource group own its data, and does nothing outside the group read its store directly? (07)
8. Are transport, application and domain in separate packages with converters at each boundary and no inward-to-outward imports? (08)
9. Is every cross-cutting behaviour implemented once, in the chain, at its documented position? (09)
10. Does the service receive a signed principal and never the client's token; does authorization run before validation; is every decision audited? (10)
11. Can every request be retried safely by a client that lost the response? (11)
12. Does anything that can exceed the request deadline return an operation resource with terminal states, cancel and retention? (12)
13. What is the blast radius of this dependency failing, and is it one capability rather than the catalog? Is the change make-before-break? (13)
14. Does every hop log the correlation IDs, and is the request record a versioned schema? (14)
15. Did it come from the template, pass the conformance suite and get an API review before GA? (15)
