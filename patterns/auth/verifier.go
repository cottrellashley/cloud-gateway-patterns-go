package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Audience accepts both the string and the array form of "aud".
type Audience []string

// UnmarshalJSON implements json.Unmarshaler.
func (a *Audience) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = Audience{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	*a = arr
	return nil
}

// Claims are the registered claims plus the ones Entra ID and most issuers
// use for authorization. Raw holds everything for custom claims.
type Claims struct {
	Issuer    string   `json:"iss"`
	Subject   string   `json:"sub"`
	Audience  Audience `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
	ID        string   `json:"jti"`
	Scope     string   `json:"scp"`   // Entra delegated scopes, space separated
	Scope2    string   `json:"scope"` // RFC 8693 / many issuers
	Roles     []string `json:"roles"` // Entra app roles
	TenantID  string   `json:"tid"`
	ObjectID  string   `json:"oid"`
	Email     string   `json:"preferred_username"`

	Raw map[string]json.RawMessage `json:"-"`
}

// Scopes merges scp and scope into one list.
func (c *Claims) Scopes() []string {
	s := c.Scope
	if s == "" {
		s = c.Scope2
	}
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// Verification errors. They wrap a stable sentinel so callers can map them to
// WWW-Authenticate error codes without string matching.
var (
	ErrInvalidToken = errors.New("auth: invalid token")
	ErrExpired      = fmt.Errorf("%w: expired", ErrInvalidToken)
	ErrNotYetValid  = fmt.Errorf("%w: not yet valid", ErrInvalidToken)
	ErrIssuer       = fmt.Errorf("%w: issuer mismatch", ErrInvalidToken)
	ErrAudience     = fmt.Errorf("%w: audience mismatch", ErrInvalidToken)
	ErrAlgorithm    = fmt.Errorf("%w: algorithm not allowed", ErrInvalidToken)
	ErrSignature    = fmt.Errorf("%w: signature verification failed", ErrInvalidToken)
)

// Verifier validates compact JWS access tokens.
type Verifier struct {
	// Issuer must match "iss" exactly (no normalisation; Entra v2 issuers are
	// https://login.microsoftonline.com/{tenant}/v2.0).
	Issuer string
	// Audiences: the token's aud must contain at least one.
	Audiences []string
	// Keys resolves kids.
	Keys KeyProvider
	// Algorithms allowed; default RS256 only, like go-oidc and Oathkeeper.
	Algorithms []string
	// Leeway for nbf/exp clock skew; default 5 minutes for nbf, 0 for exp.
	Leeway time.Duration
	// MaxTokenAge rejects tokens whose iat is older than this (0 = off).
	MaxTokenAge time.Duration
	Now         func() time.Time
}

// Verify parses, checks the signature and validates the claims.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error) {
	c, err := parseCompact(token)
	if err != nil {
		return nil, err
	}
	algs := v.Algorithms
	if len(algs) == 0 {
		algs = []string{"RS256"}
	}
	if c.header.Alg == "" || strings.EqualFold(c.header.Alg, "none") || !slices.Contains(algs, c.header.Alg) {
		return nil, ErrAlgorithm
	}
	if len(c.header.Crit) > 0 {
		return nil, fmt.Errorf("%w: unsupported crit header", ErrInvalidToken)
	}
	if c.header.Kid == "" {
		return nil, fmt.Errorf("%w: missing kid", ErrInvalidToken)
	}
	key, err := v.Keys.Key(ctx, c.header.Kid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	if err := verifySignature(c.header.Alg, key, c.signingInput, c.signature); err != nil {
		return nil, ErrSignature
	}
	var claims Claims
	if err := json.Unmarshal(c.payload, &claims); err != nil {
		return nil, ErrMalformedToken
	}
	if err := json.Unmarshal(c.payload, &claims.Raw); err != nil {
		return nil, ErrMalformedToken
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	leeway := v.Leeway
	if leeway == 0 {
		leeway = 5 * time.Minute
	}
	if v.Issuer != "" && claims.Issuer != v.Issuer {
		return nil, ErrIssuer
	}
	if len(v.Audiences) > 0 {
		ok := false
		for _, want := range v.Audiences {
			if slices.Contains(claims.Audience, want) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, ErrAudience
		}
	}
	if claims.ExpiresAt == 0 || now.After(time.Unix(claims.ExpiresAt, 0)) {
		return nil, ErrExpired
	}
	if claims.NotBefore != 0 && now.Add(leeway).Before(time.Unix(claims.NotBefore, 0)) {
		return nil, ErrNotYetValid
	}
	if v.MaxTokenAge > 0 && claims.IssuedAt != 0 && now.Sub(time.Unix(claims.IssuedAt, 0)) > v.MaxTokenAge {
		return nil, fmt.Errorf("%w: token too old", ErrInvalidToken)
	}
	return &claims, nil
}
