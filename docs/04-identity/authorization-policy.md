# Authorization and policy enforcement

**Pattern:** authentication produces a principal; a single policy enforcement point (PEP) asks a policy decision point (PDP) "may principal P do action A on resource R in context C"; the decision is default-deny, explicit deny beats allow, and every decision is audited with its inputs. The PDP is a library (Cedar via `cedar-go`, or OPA/Rego) evaluating a signed policy bundle; the PEP is middleware with the shape of Envoy's `ext_authz` and Oathkeeper's pipeline.

## Shapes worth copying

**Oathkeeper's request pipeline** (`proxy/request_handler.go` @ab46001, `HandleRequest`): for the matched rule, run authenticators in order until one is responsible (`ErrAuthenticatorNotResponsible` lets the next one try), then the authorizer, then mutators that reshape the request for the upstream (e.g. mint a header or a JWT). The three stages are configuration (`rule.Authenticators`, `rule.Authorizer`, `rule.Mutators`), not code — adding a route needs no gateway release.

**Kubernetes** (`endpoints/filters/authorization.go` @ba832c1): `WithAuthorization(handler, authorizer, serializer)` runs after authentication and the in-flight limit; on deny it writes a `403` with the authorizer's reason, and records a metric. The authorizer is an interface (`authorizer.Authorize(ctx, attributes) (Decision, reason, error)`) implemented by RBAC, webhook, node, ABAC — the gateway's PDP interface should be equally small.

**Envoy `ext_authz`**: the proxy calls an external service per request with the request attributes (and optionally a bounded body prefix, `max_request_bytes`), **fails closed by default** (`failure_mode_allow: false`), with a 200 ms default timeout. Whether your PDP is in-process or remote, keep those three properties: bounded input, short timeout, fail closed.

## The PEP middleware

```go
type Decision struct { Allow bool; Policies []string; Reason string }
type PDP interface { Authorize(ctx context.Context, principal *auth.Principal, action, resource string, ctxAttrs map[string]any) (Decision, error) }

func Authorize(pdp PDP, action func(*http.Request) (string, string)) middleware.Middleware {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            p := auth.PrincipalFrom(r.Context())
            if p == nil { apierror.Write(w, apierror.New(401, apierror.CodeUnauthenticated, "a bearer token is required")); return }
            act, res := action(r)
            ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)   // ext_authz default
            defer cancel()
            d, err := pdp.Authorize(ctx, p, act, res, map[string]any{"ip": middleware.ClientIPFrom(r.Context()), "mfa": p.Claims.Raw["acr"] != nil})
            if err != nil || !d.Allow {                                              // fail closed
                audit(r.Context(), p, act, res, d, err)
                apierror.Write(w, apierror.New(403, apierror.CodePolicyDenied, "not permitted").WithDetail("Policy", strings.Join(d.Policies, ","), d.Reason))
                return
            }
            audit(r.Context(), p, act, res, d, nil)
            next.ServeHTTP(w, r)
        })
    }
}
```

Rules encoded above: principal required; action and resource derived from the route (never from the body alone); bounded time; deny on error; audit both outcomes.

## Cedar with `cedar-go`

`cedar-policy/cedar-go` @34be9aa (Apache-2.0) implements the Cedar authorizer, JSON entities and schema parsing in Go ("supports the core authorizer, JSON handling, and schema parsing but not yet the CLI, formatter, or policy templates"). Shape of use:

```go
ps, _ := cedar.NewPolicySetFromBytes("bundle.cedar", bundle)        // signed bundle, reloaded via patterns/config
entities := cedar.EntityMap{...}                                      // principal, groups, resource attributes
ok, diag := ps.IsAuthorized(entities, cedar.Request{
    Principal: cedar.NewEntityUID("User", cedar.String(p.Subject)),
    Action:    cedar.NewEntityUID("Action", "invoke"),
    Resource:  cedar.NewEntityUID("Capability", "atlas.snowflake/query"),
    Context:   cedar.NewRecord(cedar.RecordMap{"database": cedar.String(db)}),
})
```

Cedar semantics that matter: a matching `forbid` always overrides `permit`; no matching `permit` means deny; the schema validates policies at authoring time, not at evaluation time. That maps onto Azure RBAC's evaluation (deny assignments first, then additive allows, then conditions).

Example policies (from the Atlas report):

```cedar
permit(principal in Group::"analysts", action == Action::"invoke", resource == Capability::"atlas.snowflake/query")
when { context.database == "ANALYTICS" };
forbid(principal, action == Action::"delete", resource in Namespace::"atlas.github")
unless { context.mfa == true };
```

## OPA/Rego

Use OPA when decisions are data-shaped (compute an allowed row filter, a quota envelope) rather than yes/no; embed it as a Go library (`rego.New(...).PrepareForEval`) and distribute policy as bundles (`bundle` API with signing, `decision_logs` for audit). OPA ships no control plane; `patterns/config.Watch` polling a bundle URL is sufficient.

## Policy distribution

Policies are configuration: signed bundle → `patterns/config.Hot[PolicySet]` with validation (compile the bundle before swap) → `OnChange` swaps the PDP → `/readyz?verbose` shows the bundle version. Fail closed when the bundle is older than a grace period.

## Audit

Every decision writes one record: principal, action, resource, decision, policy IDs, bundle version, request ID, trace ID. Denials are recorded with the same fields. See `docs/05-observability/request-telemetry-and-audit.md`.

## Checklist

- [ ] One PEP middleware, after authn and rate limiting, before the handler.
- [ ] Action/resource from the route pattern plus validated input, never from untrusted headers.
- [ ] 200 ms timeout, fail closed, decisions audited.
- [ ] Policy bundle hot-reloaded with validation and version; grace period then fail closed.
- [ ] Simple scope checks (`auth.RequireScope`) are fine for coarse gates; the PDP decides anything resource-specific.
