# 05. Contracts first, compatibility as a rule set

**Principle:** the contract (resource schemas, verbs, error codes, headers, events) is designed and reviewed before the implementation, and every subsequent change is judged against a *written* list of compatible and incompatible changes. Features move through explicit maturity levels (alpha → beta → GA) behind gates that can be turned off; anything not GA can change or vanish; anything GA is supported until a published removal date announced in-band. Versions are coarse (`v1`, `v2`, or a date), never semver patch numbers on the wire.

**Why it matters at scale:** clients, SDKs and agents outlive server releases and cannot be forced to upgrade. Kubernetes has not broken a GA API since 2015; Azure services ship date-versioned APIs that old SDKs keep calling for years. The cost of that is not caution, it is a short rule set applied mechanically to every diff, plus a gate that lets risky things ship disabled.

## Evidence

**Kubernetes compatibility rules** ([api_changes.md §On compatibility](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api_changes.md#L107)). A change is compatible if it adds functionality that is not required for correct behaviour and does not change existing semantics, "including the semantic meaning of default values *and behavior*, interpretation of existing API types, fields, and values, which fields are required and which are not, mutable fields do not become immutable, valid values do not become invalid, explicitly invalid values do not become valid". Put another way:

> 1. Any API call … that succeeded before your change must succeed after your change.
> 2. Any API call that does not use your change must behave the same as it did before your change.
> 3. Any API call that uses your change must not cause problems … when issued against an API servers that do not include your change.
> 4. It must be possible to round-trip your change (convert to different API versions and back) with no loss of information.
> 5. Existing clients need not be aware of your change in order for them to continue to function as they did previously, even when your change is in use.
> 6. It must be possible to rollback to a previous version of API server that does not include your change and have no impact on API objects which do not use your change.

Maturity levels ([§Alpha, Beta, and Stable Versions](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api_changes.md#L1087)): alpha is `v1alpha1`, disabled by default, "the object schema and semantics may change in a later software release, without any provision for preserving objects"; beta is `v2beta3`, disabled by default since 1.24, "the project commits to complete the feature … releases should simultaneously support two consecutive versions … for at least one minor release cycle"; stable is `vX`, enabled by default, supported for many releases. New fields on a stable object must themselves meet the stable bar or be gated ([§Adding Unstable Features to Stable Versions](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api_changes.md#L1169)). Versions convert through an internal hub via [`runtime.Scheme`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apimachinery/pkg/runtime/scheme.go#L34): "a Version is a point-in-time identifier for a particular representation of that Type (typically backwards compatible) … a Group identifies a set of Versions, Kinds, and Types that evolve over time".

**Feature gates** ([feature-gates.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/feature-gates.md), [`FeatureSpec`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/component-base/featuregate/feature_gate.go#L73)): "A feature gate is intended to be a fast way to neutralize a new feature and mitigate damage caused by bugs … Feature gates are *not* intended to be long-term APIs." `FeatureSpec{Default bool; LockToDefault bool; PreRelease Alpha|Beta|GA|Deprecated; Version; MinCompatibilityVersion}`. Conformance tests ([conformance-tests.md](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/conformance-tests.md)) "test only GA, non-optional features or APIs (e.g., no alpha or beta endpoints, no feature flags required, no deprecated features)". Envoy does the same with runtime guards ([runtime.rst](https://github.com/envoyproxy/envoy/blob/4ab3213/docs/root/intro/arch_overview/operations/runtime.rst)): "Runtime guards are also used as a mechanism to disable new behavior or risky changes … The names of these runtime guards will be included in the release notes".

**Google AIP-180** ([0180.md](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0180.md)) names three compatibilities (source, wire, semantic) and lists the rules: components may be added, "new required fields must not be added", "any field previously populated by the server must continue to be populated, even if it introduces redundancy", "existing components … must not be removed … Renaming a component is semantically equivalent to remove and add", enum values in responses need care. [AIP-185](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0185.md): "Google APIs must not expose minor or patch version numbers … users receive new functionality without migration"; stability channels (`v1alpha`, `v1beta`) sit after the major version; "a new major version of an API must not depend on a previous major version"; old versions get "a reasonable, well-communicated deprecation period". [AIP-203](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0203.md): every request field is annotated `REQUIRED`, `OPTIONAL`, `OUTPUT_ONLY`, `IMMUTABLE`, … "many clients (e.g. CLIs and SDKs) rely on them to generate code".

**Azure REST guidelines** ([Guidelines.md §API Versioning](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L758)): "DO use a required query parameter named `api-version` on every operation", "DO use `YYYY-MM-DD` date values, with a `-preview` suffix", 400 `MissingApiVersionParameter` / `UnsupportedApiVersionValue` listing supported versions, "DO NOT introduce any breaking changes into the service", required fields only in v1, "it is a breaking change to remove a required field or make an optional field required or vice versa", extensible enums (`modelAsString`) so values can be added, "YOU SHOULD NOT add new top-level error codes to an existing API without bumping the service version". Deprecation is in-band ([§Deprecating Behavior Notification](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L820)): the `azure-deprecating` response header "notifying the caller what is being deprecated, when it will no longer function, and a URL linking to more information". Process ([ConsiderationsForServiceDesign.md](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/ConsiderationsForServiceDesign.md#L162)): "YOU SHOULD release and evaluate a minimum of 2 preview versions prior to the first GA release", "YOU SHOULD use extensible enumerations", "YOU SHOULD implement conditional requests early", "YOU SHOULD support server-side paging, even if your resource does not currently need paging".

**Dapr** versions components ([versioning.go](https://github.com/dapr/dapr/blob/3342040/pkg/components/versioning.go): `Preferred`, `Deprecated`, `Others`, `Default`; `v0` unstable, `v1` first stable; a warning on deprecated use) and decided *not* to version callbacks it could not control ([API-010](https://github.com/dapr/dapr/blob/3342040/docs/decision_records/api/API-010-appcallback-versioning.md): "Post v1.0, if required, the versioning could be implemented inside the payload but not on the API itself. A missing version in the payload could imply v1.0.").

## The rules

1. **Write the contract first:** resource schemas (JSON Schema 2020-12 or protobuf), verbs, actions, error codes, headers, events. Review it (principle 15) before implementation starts.
2. **Apply the six Kubernetes rules to every change.** Compatible: add optional field with a default that preserves behaviour; add enum value to a request enum; add a verb; add a response field; relax validation. Incompatible: remove or rename anything; add a required field; change a default; change a type or unit; tighten validation; make a field immutable; change the meaning of an error code for the same condition; add paging to an unpaged list.
3. **Coarse versions on the wire:** `/v1/` or `api-version=2026-10-30`. Never `v1.3.2`. Minor changes land in place.
4. **Maturity is explicit and gated:** `alpha` (off by default, may vanish, no SLO), `beta` (off by default, complete, upgrade path documented, two versions supported one cycle), `ga` (on, supported, in the conformance suite). Gates are removed after GA; permanent switches are configuration, not gates.
5. **Every field carries behaviour metadata:** required, optional, output-only, immutable, default. Clients and generators read it; validators enforce it.
6. **Extensible enums are strings**; clients must tolerate unknown values in responses; servers reject unknown values in requests for the requested version.
7. **Deprecation is announced in-band and dated:** a response header (`Deprecation`, `Sunset`, link) on every call to a deprecated version or field, plus the catalog, plus the release notes. Removal happens on the published date and not before.
8. **Round-trip and rollback are tested:** new version → old version → new version loses nothing; a previous server release can read what the new one stored.
9. **Conformance tests cover GA only** and run on every provider release.

## In this repository

| Mechanism | Where |
| --- | --- |
| Stable error codes as the contract (adding a code for a *new* condition is fine; changing the code for an *existing* condition is breaking) | `patterns/apierror` reserved codes, `docs/07-services/errors.md` |
| Paging from day one | `patterns/pagination` |
| Resource versions and generations as the unit of change | `patterns/resource` |
| Snapshot versions for configuration contracts | `patterns/config.Version` |
| Registry versions for extension contracts (`provider.snowflake_sql` today; `…/v2` by ID convention) | `patterns/registry` |
| Repeatability/idempotency headers as a contract clients rely on | `patterns/idempotency` |
| Testing strategy incl. golden files for wire shapes | `docs/07-services/testing.md` |

```go
// A feature gate in the Kubernetes shape: name → spec; off by default until GA; removed after.
type Maturity string
const (Alpha Maturity = "alpha"; Beta Maturity = "beta"; GA Maturity = "ga"; Deprecated Maturity = "deprecated")

type FeatureSpec struct {
    Default       bool
    LockToDefault bool      // true once GA/removed: operators cannot flip it
    Maturity      Maturity
    Since, Until  string    // release that introduced it; release that removes the gate
}
var gates = map[string]FeatureSpec{
    "HandoffMode":          {Default: false, Maturity: Alpha, Since: "2026.10"},
    "ReaderSchemaProjection": {Default: true, Maturity: Beta, Since: "2026.09"},
    "ArrowResults":         {Default: true, LockToDefault: true, Maturity: GA, Since: "2026.06", Until: "2027.01"},
}
func Enabled(name string, overrides map[string]bool) bool {
    spec := gates[name]
    if spec.LockToDefault { return spec.Default }
    if v, ok := overrides[name]; ok { return v }
    return spec.Default
}
```

## Applying it to a capability gateway

* **The manifest is the contract.** A capability manifest declares parameters (JSON Schema with `required`, `readOnly`, `default`), the result *writer schema*, modes, limits, maturity and version. The catalog controller runs the compatibility diff between generation N and N+1 (`BACKWARD` for result schemas: fields may be added, never removed or retyped; `FORWARD` for parameters: optional fields may be added) and sets `Compatible=False` with the exact violation when it fails.
* **Reader-schema projection keeps old clients whole.** A client sends the schema version it was built against (or the field list it expects); the result set is projected to that reader schema with defaults for fields that did not exist then. Columns can be added, renamed (alias retained) or re-ordered on the server without any SDK release.
* **Maturity per capability and per mode.** `handoff` starts `alpha` behind the `HandoffMode` gate for a few named tenants, becomes `beta` with the conformance tests written, and `ga` when the two preview rounds have produced no incompatible change. Agents see the maturity in discovery and prefer GA.
* **Versioning choice:** `/v1/` in the path for the gateway API (the resource model, operations, discovery); `api-version`-style dates are unnecessary because capability contracts are versioned individually in the catalog. Within a capability, the manifest `version` is the coarse version and `generation` is the fine one.
* **Deprecation is data:** a capability or field gets `deprecated: {since, sunset, replacement}` in the manifest; the gateway adds `Deprecation`/`Sunset` headers on use; the catalog lists it; removal is a controller action on the sunset date.

## Anti-patterns

* Shipping the implementation, then "documenting the API" from the code.
* `v1.4` on the wire, or a new path for every change.
* Making an optional field required "because everyone sends it".
* Removing a column from a result because the dashboard no longer needs it.
* A `beta` that is on by default for all tenants, or an `alpha` that a production client depends on.
* Deprecation announced only in a changelog.
