# 10. Identity and policy as platform services

**Principle:** authentication happens once, at the edge, producing a *principal* (subject, tenant, groups/roles, scopes, authentication context). Everything behind the edge receives that principal in a form it can verify (signed headers or a gateway-minted token) and never the client's credential. Authorization is a separate step that decides on *attributes* (principal, verb/action, resource group, resource name, request properties) through a chain of deciders that can say allow, deny or no opinion; the decision runs before validation so existence is not leaked; policy is data, not code; every decision is audited.

**Why it matters at scale:** when each service parses tokens and hard-codes permission checks, there are as many security models as services, keys rotate badly, and audit is impossible to assemble. Kubernetes separates `authenticator.Request` from `authorizer.Authorizer` from admission and forwards users to extension servers as headers; ARM strips the client's `Authorization`, adds `x-ms-client-principal-*` headers and names every operation for RBAC; Google's AIP-211 fixes the order (authorize first) and the error shape.

## Evidence

**Kubernetes authentication and authorization interfaces.** [`authenticator.Request`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/authentication/authenticator/interfaces.go#L34) `AuthenticateRequest(req) (*Response, bool, error)` with `Response{User user.Info, Audiences}`; a union of authenticators tries each in turn. [`authorizer.Attributes`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go#L35): `GetUser`, `GetVerb`, `GetNamespace`, `GetResource`, `GetSubresource`, `GetName`, `GetAPIGroup`, `GetAPIVersion`, `IsResourceRequest`, `GetPath`, plus field/label selectors. [`Authorizer.Authorize(ctx, a) (Decision, reason, error)`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go#L103) with [`DecisionDeny | DecisionAllow | DecisionNoOpinion`](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go#L231): deciders chain (node, RBAC, webhook) and the first non-NoOpinion wins. In the handler chain (principle 09) authentication precedes audit, impersonation, flow control and authorization.

**kube-aggregator forwards identity, never credentials** ([handler_proxy.go](https://github.com/kubernetes/kubernetes/blob/ba832c1/staging/src/k8s.io/kube-aggregator/pkg/apiserver/handler_proxy.go#L168)):

```go
proxyRoundTripper = transport.NewAuthProxyRoundTripper(user.GetName(), userUID, user.GetGroups(), user.GetExtra(), proxyRoundTripper)
```

The extension server receives `X-Remote-User`, `X-Remote-Group`, `X-Remote-Extra-*` over a client-certificate-authenticated connection and trusts them because the front proxy's CA is configured; it never sees the bearer token.

**ARM proxy header contract** ([common-api-details.md §Proxy Request Header Modifications](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/common-api-details.md#L12)):

> authorization — Always removed/changed. The authorization used by the client to the proxy will be different than the authorization used to communicate from the proxy to the resource provider.
> x-ms-client-principal-name — Always added. Set to the principal name / UPN of the client JWT making the request.
> x-ms-client-principal-id, x-ms-client-tenant-id, x-ms-client-audience, x-ms-client-issuer, x-ms-client-object-id, x-ms-client-app-id, x-ms-client-app-id-acr (how the client was authenticated), x-ms-client-authorization-source, x-ms-client-identity-provider, x-ms-client-wids, x-ms-client-authentication-methods — Always added.

The provider gets a complete, structured principal and the correlation ID; the action name in the URL (`…/{name}/{action}`) is what RBAC evaluates ([proxy-api-reference.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/proxy-api-reference.md#L15): "resource providers will be able to light-up authorization / RBAC scenarios without changing their API surface"; "All parameters for the action request should be contained in the request body – and not in the URL … will avoid impacting the authZ checks").

**Google AIP-211** ([0211.md](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0211.md)):

> Services must check authorization before validating any request, to ensure both a secure API surface and a consistent user experience. … If a request can not pass the authorization check for any reason, the service must error with `PERMISSION_DENIED`, and the corresponding error message should look like: "Permission '{p}' denied on resource '{r}' (or it might not exist)." This avoids leaking resource existence. … the service should still only check for authorization applicable to the operation being called, and should not try to "help out" by checking for related authorization.

**Admission as policy** (principle 09): Kubernetes' validating admission (webhooks, CEL `ValidatingAdmissionPolicy`) is policy-as-data evaluated after authorization on the final object; Caddy's `http.authentication.providers` namespace and Oathkeeper's authenticator → authorizer → mutator pipeline (`docs/04-identity/*`) are the same pipeline shape in gateways.

## The rules

1. **Authenticate once at the edge.** The gateway validates bearer tokens (issuer, audience, signature via JWKS, expiry, `nbf`, token age) and browser sessions; nothing behind it parses client credentials.
2. **Produce a principal as a typed record:** `subject`, `tenant`, `object id`, `app id`, `groups/roles`, `scopes`, `auth context` (how authenticated, MFA), `issuer`, `audience`, `token id`, `expiry`, `client IP`, `user agent hash`. This is the only identity object the rest of the system knows.
3. **Propagate the principal, strip the credential.** Downstream calls carry a gateway-signed principal (JWT with the gateway as issuer and the service as audience, or signed headers over mTLS/private network) plus the correlation IDs; the inbound `Authorization` header is removed.
4. **Authorize on attributes**: `(principal, action, group, collection, name, request attributes)`. Actions are the discovery names (`capabilities/execute`). Deciders chain: `deny-list → tenant isolation → RBAC/scopes → policy engine (Cedar/OPA) → entitlements (vendor licences)`; `NoOpinion` passes to the next; default deny.
5. **Authorize before validate; never leak existence.** 403 with "permission X denied on resource Y (or it might not exist)"; 404 only when the caller may list the parent.
6. **Policy is data, versioned and tested**: policy bundles are resources in the catalog with generations, dry-run evaluation, unit tests, and roll out through the same snapshot as routes.
7. **Audit every decision**: who, what action, which resource, which policy version, allow/deny, reason, correlation IDs; audit is a chain element, not a handler call.
8. **Service identity for downstream systems is separate from user identity**, and the user travels in query tags, audit records and policy inputs. When delegation (OBO) is unavailable, per-user limits and entitlements are *the platform's* policy and must be enforced before the shared credential is used.
9. **Secrets never cross the boundary**: no client tokens in logs, in tickets, in operation records, or forwarded upstream.

## In this repository

| Mechanism | Where |
| --- | --- |
| Bearer validation, JWKS cache with throttled refresh, `Principal`, scope guard, header stripping | `patterns/auth`, `docs/04-identity/bearer-tokens-and-jwks.md` |
| Browser sessions, rotation, CSRF | `patterns/session`, `docs/04-identity/sessions-and-user-state.md` |
| PEP middleware shape, Cedar/OPA as PDP, attribute model | `docs/04-identity/authorization-policy.md` |
| Managed identity for downstream, OBO constraints, user in query tags | `docs/04-identity/azure-identity.md` |
| Audit as a request record emitted by the chain | `docs/05-observability/request-telemetry-and-audit.md`, `patterns/telemetry` |
| Rule 6 of `AGENTS.md`: inbound `Authorization` never forwarded | `cmd/gateway/main.go` (`proxy` strips hop headers), `patterns/proxy` |

```go
// Attributes mirror authorizer.Attributes; deciders mirror Decision{Deny,Allow,NoOpinion}.
type Attributes struct {
    Principal  auth.Principal
    Action     string            // "capabilities/execute"
    Group      string            // "vendor.marketdata_a"
    Collection string            // "capabilities"
    Name       string            // "vendor.marketdata_a.curves"
    Request    map[string]any    // mode, parameters summary, estimated cost, rows requested
}
type Decision int
const (NoOpinion Decision = iota; Allow; Deny)
type Decider interface { Decide(ctx context.Context, a Attributes) (Decision, string, error) }

func Authorize(ctx context.Context, chain []Decider, a Attributes) (Decision, string, error) {
    for _, d := range chain {
        dec, reason, err := d.Decide(ctx, a)
        if err != nil { return Deny, "decider error", err }      // fail closed
        if dec != NoOpinion { return dec, reason, nil }
    }
    return Deny, "no decider allowed the request", nil           // default deny
}
```

## Applying it to a capability gateway

* **Edge:** Entra ID tokens (users via PKCE, agents via client credentials or user-delegated tokens) are validated by the gateway's `auth.Verifier` with audience = the gateway. The principal carries `tid`, `oid`, `appid`, roles/groups, scopes and `acr`/`amr`. Sessions exist only for the browser UI.
* **Propagation:** providers behind the gateway receive a gateway-signed JWT (`iss=gateway`, `aud=provider`, 2-minute lifetime) carrying the principal and the operation ID, over the private network. They verify the gateway's key (JWKS served by the gateway) and nothing else.
* **Authorization chain:** tenant isolation (tenant must match the catalog's tenant) → scopes (`atlas.execute`, `atlas.handoff`, `atlas.catalog.read`) → Cedar policy bundle (which groups/collections a role may act on, which modes, row/byte ceilings, time windows) → entitlements for `vendor.*` (per-user licence table, display vs derived data) → cost guard (estimated warehouse cost against the principal's budget). Each decider is named in the audit record.
* **No OBO:** the Snowflake session pool runs under the service identity. Therefore per-user limits (concurrency, bytes, cost), row-level filters derived from the principal, and vendor entitlements are enforced by the gateway's policy before a session is used; the user and operation ID are set as the query tag so warehouse-side audit still correlates.
* **Handoff tickets are policy artefacts:** a ticket embeds the principal, the exact compiled statement hash, the dataset scope, the expiry and the policy version; the ticket-redemption endpoint re-checks the decision at redemption time. A ticket cannot widen what the principal could do through `execute`.

## Anti-patterns

* Providers validating Entra tokens themselves (and each with a different clock skew and audience list).
* Forwarding the user's bearer token upstream "so the service can call Snowflake as them" without an actual OBO flow.
* Permission checks written as `if user.Role == "admin"` inside handlers.
* Validating request bodies before authorization and returning different errors for existing and non-existing resources.
* Audit written by handlers that remember to call `audit.Log` — some will not.
