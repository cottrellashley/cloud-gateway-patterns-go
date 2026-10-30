# Sessions and user state

**Pattern:** APIs are stateless and bearer-authenticated; browser UIs get a server-side session referenced by an encrypted, HttpOnly, SameSite cookie; any per-user state that must survive a request lives in a store keyed by principal, never in the process. Implemented in `patterns/session`.

## Two different problems

| Caller | Credential | Where state lives |
| --- | --- | --- |
| SDKs, CLIs, agents, services | Bearer token per request (`docs/04-identity/bearer-tokens-and-jwks.md`) | nowhere at the gateway; idempotency and LRO records are keyed by principal + key |
| Browser UI | Cookie → server-side session | session store (Cosmos DB / Redis) with TTL |

Never give a browser a long-lived bearer token in JavaScript-accessible storage; never give an API client a cookie.

## Evidence: Pomerium

Pomerium is an identity-aware proxy in Go; its session handling (`internal/sessions/cookie/cookie.go` @42e80cc) keeps the browser cookie small and opaque and everything else server-side:

```go
func (hrw *handleReaderWriter) makeCookie(value string) *http.Cookie {
    opts := hrw.getOptions()
    return &http.Cookie{
        Name: opts.Name, Value: value, Path: opts.Path, Domain: opts.Domain,
        HttpOnly: opts.HTTPOnly,
        Secure:   opts.Secure,
        Expires:  ..., SameSite: opts.SameSite,
    }
}
func (hrw *handleReaderWriter) ClearSessionHandle(w http.ResponseWriter) { c := hrw.makeCookie(""); c.MaxAge = -1; http.SetCookie(w, c) }
```

The cookie carries a *session handle* (an identifier and expiry), encoded with a `MarshalUnmarshaler` that is an AEAD cipher. `pkg/cryptutil/encrypt.go`:

```go
func Encrypt(a cipher.AEAD, data, ad []byte) []byte {
    iv := randomBytes(a.NonceSize())
    ciphertext := a.Seal(nil, iv, data, ad)
    return append(ciphertext, iv...)
}
func Decrypt(a cipher.AEAD, data, ad []byte) ([]byte, error) {
    if len(data) <= a.NonceSize() { return nil, fmt.Errorf("cryptutil: invalid input size: %d", len(data)) }
    size := len(data) - a.NonceSize()
    ciphertext := data[:size]; nonce := data[size:]
    plaintext, err := a.Open(nil, nonce, ciphertext, ad)
    if err != nil { return nil, fmt.Errorf("cryptutil: decryption failed (mismatched keys?): %w", err) }
    return plaintext, nil
}
```

Random nonce, authenticated encryption, *additional data* binding the ciphertext to its purpose. `patterns/session.Codec` is the same shape with AES-256-GCM from the standard library and the cookie name as the additional data (a value cannot be replayed under a different cookie).

## The pattern

```go
codec, _ := session.NewCodec(key32)                       // key from Key Vault; keep the previous key for Open during rotation
mgr := session.NewManager(codec, store, session.DefaultCookieOptions("gw_session")) // HttpOnly, Secure, SameSite=Lax, 8h

// login callback
s := &session.Session{Subject: claims.Subject, Data: map[string]string{"tenant": claims.TenantID}}
_ = mgr.Rotate(ctx, w, s)        // new ID at login: defeats session fixation

// every UI request
s, err := mgr.Load(r)            // ErrNoSession / ErrExpired → redirect to login

// logout
mgr.Clear(ctx, w, r)             // store delete + Max-Age=-1
```

Store contract: one document per session, keyed by the random 256-bit ID, TTL = expiry. Cosmos DB with the ID as partition key and `ttl` set, or Redis `SET ... EX`. Revocation is a delete; the cookie is then useless even though it still decrypts.

## CSRF

Go 1.25 added `http.CrossOriginProtection` (checks `Sec-Fetch-Site` and `Origin` against the host; the MCP Go SDK exposes it as `StreamableHTTPOptions.CrossOriginProtection`). Wrap every cookie-authenticated, state-changing route with it; bearer-authenticated API routes do not need it because browsers cannot attach the token cross-site.

```go
cop := http.NewCrossOriginProtection()
cop.AddTrustedOrigin("https://app.example.com")
uiMux := cop.Handler(uiRoutes)
```

## Per-user state that is not a session

* **Preferences, saved views**: a document per user in the application store, read through the service, cached with `patterns/cache.TTL` keyed by subject.
* **In-progress work**: long-running operations (`patterns/lro`) keyed by principal; agents resume by operation ID, not by session.
* **Idempotency records**: `patterns/idempotency` scoped by principal.
* **Rate-limit buckets**: `patterns/ratelimit.Keyed` by principal, evicted by TTL; never persisted.

None of these require sticky routing; every replica can serve every request, which is what lets the edge balance freely and lets drains be fast.

## Checklist

- [ ] Cookie: HttpOnly, Secure, SameSite=Lax (Strict breaks OAuth redirects), Path=/, Max-Age = session TTL, name without a `__Host-` prefix unless Domain is unset and Path is `/` (then use `__Host-`).
- [ ] Session ID 256 bits from `crypto/rand`; rotated at login and privilege change.
- [ ] Store TTL equals cookie expiry; idle timeout implemented by re-saving on activity.
- [ ] Key rotation: new codec for Seal, old codec kept for Open for one TTL.
- [ ] CSRF protection on UI routes; none needed on bearer routes.
