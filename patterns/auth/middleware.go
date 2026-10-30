package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// Principal is what the rest of the gateway sees. It never carries the raw
// token; TokenHash lets audit correlate requests made with the same token
// without storing it.
type Principal struct {
	Subject   string
	Issuer    string
	TenantID  string
	Scopes    []string
	Roles     []string
	Claims    *Claims
	TokenHash string
}

// HasScope reports whether the principal holds the scope.
func (p *Principal) HasScope(s string) bool {
	for _, x := range p.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// WithPrincipal stores a principal in the context (for tests and for trusted
// internal callers).
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated principal, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// BearerToken extracts the token from Authorization: Bearer. Tokens are never
// accepted from the query string (OAuth 2.1 and the MCP spec forbid it).
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Middleware verifies the bearer token with v and stores the Principal.
// realm appears in WWW-Authenticate. Requests without a token get 401 with a
// bare challenge; invalid tokens get error="invalid_token" (RFC 6750 §3).
//
// Mirroring the Kubernetes authentication filter, the Authorization header is
// removed from the request after successful verification so that downstream
// proxies never forward the gateway's inbound credential.
func Middleware(v *Verifier, realm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := BearerToken(r)
			if tok == "" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q`, realm))
				apierror.Write(w, apierror.New(http.StatusUnauthorized, apierror.CodeUnauthenticated, "a bearer token is required"))
				return
			}
			claims, err := v.Verify(r.Context(), tok)
			if err != nil {
				desc := "the token is invalid"
				switch {
				case errors.Is(err, ErrExpired):
					desc = "the token has expired"
				case errors.Is(err, ErrAudience):
					desc = "the token was not issued for this resource"
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q, error="invalid_token", error_description=%q`, realm, desc))
				apierror.Write(w, apierror.Wrap(err, http.StatusUnauthorized, apierror.CodeUnauthenticated, desc))
				return
			}
			sum := sha256.Sum256([]byte(tok))
			p := &Principal{
				Subject:   claims.Subject,
				Issuer:    claims.Issuer,
				TenantID:  claims.TenantID,
				Scopes:    claims.Scopes(),
				Roles:     claims.Roles,
				Claims:    claims,
				TokenHash: hex.EncodeToString(sum[:8]),
			}
			r.Header.Del("Authorization")
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// RequireScope is an authorization guard for simple cases; policy engines
// (Cedar, OPA) belong behind the same shape of middleware.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil {
				apierror.Write(w, apierror.New(http.StatusUnauthorized, apierror.CodeUnauthenticated, "a bearer token is required"))
				return
			}
			if !p.HasScope(scope) {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q`, scope))
				apierror.Write(w, apierror.New(http.StatusForbidden, apierror.CodePolicyDenied, "insufficient scope").WithDetail("MissingScope", scope, ""))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
