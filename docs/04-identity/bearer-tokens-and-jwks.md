# Bearer tokens and JWKS

**Pattern:** validate bearer JWTs at the gateway with a cached JWKS, a strict algorithm allowlist, exact issuer and audience checks, and clock-skew leeway; put a `Principal` in the context; strip the inbound `Authorization` header before anything is proxied. Implemented in `patterns/auth`.

## What the mature verifiers do

**go-oidc** (`oidc/verify.go` @c914bd3) — the checks, in order: signature via the key set, issuer (exact string compare; the only tolerated deviation is Google's historical `accounts.google.com`), audience contains the client ID, expiry, and `nbf` with a **5 minute leeway** citing Microsoft's token validation defaults:

```go
// Check issuer.
if !v.config.SkipIssuerCheck && t.Issuer != v.issuer { ... "oidc: id token issued by a different provider" }
// If a client ID has been provided, make sure it's part of the audience.
if !slices.Contains(t.Audience, v.config.ClientID) { ... "oidc: expected audience" }
...
if t.Expiry.Before(nowTime) { return nil, &TokenExpiredError{Expiry: t.Expiry} }
if token.NotBefore != nil {
    // Set to 5 minutes since this is what other OpenID Connect providers do to deal with clock skew.
    leeway := 5 * time.Minute
    if nowTime.Add(leeway).Before(nbfTime) { ... }
}
```

and its `Config` documents the defaults: `SupportedSigningAlgs` "defaults to RS256"; `InsecureSkipSignatureCheck` "MUST NOT be used when receiving an ID Token from sources other than the token endpoint".

**go-oidc's RemoteKeySet** (`oidc/jwks.go`) is the JWKS cache to copy: verify against cached keys first; if the `kid` is unknown, fetch — "the strategy recommended by the spec" — and collapse concurrent fetches into one in-flight request:

```go
keys := r.keysFromCache()
for _, key := range keys {
    if keyID == "" || key.KeyID == keyID {
        if payload, err := jws.Verify(&key); err == nil { return payload, nil }
    }
}
// If the kid doesn't match, check for new keys from the remote. This is the
// strategy recommended by the spec.
// https://openid.net/specs/openid-connect-core-1_0.html#RotateSigKeys
keys, err := r.keysFromRemote(ctx)
```

`keysFromRemote` holds a single `inflight` request: "This goroutine has exclusive ownership over the current inflight request".

**jwx** (`jwk` @5acb476) wraps the same idea in a generic auto-refreshing fetcher (`jwk/fetcher.go`) with `jwt.WithAcceptableSkew`, `WithIssuer`, `WithAudience`, `WithRequiredClaim` validation options.

**Oathkeeper** (`pipeline/authn/authenticator_jwt.go` @ab46001) shows the gateway-side policy: declare "not responsible" when the bearer is not a JWT so other authenticators can try, default `AllowedAlgorithms` to `["RS256"]`, and validate scope/issuer/audience as configuration, not code.

**Kubernetes** (`endpoints/filters/authentication.go` @ba832c1): on success, `req.Header.Del("Authorization")` and clear any front-proxy identity headers — the authenticated identity lives in the context, never in forwarded headers the upstream could confuse with its own.

## The pattern

```go
ks := auth.NewKeySet("https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys") // MinRefreshInterval 1m, Cache-Control max-age honoured
_ = ks.Refresh(ctx)                                                                     // warm at startup
v := &auth.Verifier{
    Issuer:     "https://login.microsoftonline.com/<tenant>/v2.0",
    Audiences:  []string{"api://atlas-gateway"},
    Keys:       ks,
    Algorithms: []string{"RS256"},        // Entra issues RS256; never accept "none"
}
chain := middleware.Chain(h, auth.Middleware(v, "atlas"), auth.RequireScope("atlas.read"))
```

`Verifier.Verify` rejects, in order: malformed compact JWS; `alg` not in the allowlist or `none`; any `crit` header; missing `kid`; unknown key (after at most one throttled refresh); bad signature; issuer; audience; `exp`; `nbf` (5 min leeway); optional max token age. Claims parsed include Entra's `scp`, `roles`, `tid`, `oid`.

Failure responses are RFC 6750: `401` with `WWW-Authenticate: Bearer realm="atlas"` when no token, `error="invalid_token"` when invalid, and `403` with `error="insufficient_scope", scope="..."` from `RequireScope` — the same challenge the MCP authorization spec requires, so agent clients can step up scopes.

## JWKS cache rules

1. Fetch at startup; fail readiness if the first fetch fails and the cache is empty.
2. Honour `Cache-Control: max-age`; default 24 h when absent.
3. Refresh on unknown `kid`, but at most once per `MinRefreshInterval` (1 minute) — a flood of forged `kid`s must not become a flood of fetches.
4. One in-flight fetch at a time; concurrent verifications wait for it.
5. Keep serving stale keys if a refresh fails; only expiry of the whole cache plus an unknown kid is a hard failure.
6. Bound the document (1 MiB) and the HTTP client timeout (5 s).

## Identity forwarded to services

Services must not receive the client's bearer token (Kubernetes deletes it; the MCP spec says servers "MUST NOT accept or transit any other tokens"). Forward identity as either:

* a **gateway-signed context token** (short-lived JWS with `sub`, `tenant`, scopes, request ID; services verify with the gateway's JWKS), or
* **mTLS + headers** (`X-Remote-User`, `X-Remote-Group`, the Kubernetes front-proxy model) where the service trusts only the gateway's client certificate.

`Principal.TokenHash` (first 8 bytes of SHA-256 of the token) is what goes into audit so sessions can be correlated without storing tokens.

## Tests

`patterns/auth/auth_test.go` generates RSA and EC keys, serves a JWKS from `httptest`, and asserts: RS256 and ES256 verify; expired, wrong audience, wrong issuer, `alg: none`, missing `kid`, tampered signature and garbage all fail with the right sentinel; `nbf` within leeway passes; an unknown `kid` right after a fetch does **not** refetch; key rotation causes exactly one fetch; the middleware strips `Authorization` and `RequireScope` returns 403 with `PolicyDenied`.
