// Package auth validates bearer tokens the way go-oidc, jwx and Oathkeeper do,
// with the standard library only:
//
//   - KeySet fetches a JWKS document, caches keys by kid, honours
//     Cache-Control max-age, refreshes when an unknown kid appears (the
//     rotation strategy OpenID Connect Core recommends and go-oidc
//     implements) and collapses concurrent refreshes into one request.
//   - Verifier checks the compact JWS: allowed algorithms only (never "none"),
//     kid required, signature, then iss, aud, exp, nbf with a 5 minute leeway
//     for nbf (go-oidc's choice, matching Microsoft's token validation).
//   - Middleware extracts the bearer token, verifies it and stores a Principal
//     in the context; failures are 401 with WWW-Authenticate.
//
// Supported algorithms: RS256, RS384, RS512, PS256, PS384, PS512, ES256,
// ES384, ES512. Entra ID issues RS256.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JWK is the subset of RFC 7517 the verifier needs.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// PublicKey converts the JWK into a crypto.PublicKey.
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64BigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("jwk %s: n: %w", k.Kid, err)
		}
		e, err := b64BigInt(k.E)
		if err != nil {
			return nil, fmt.Errorf("jwk %s: e: %w", k.Kid, err)
		}
		if n.BitLen() < 2048 {
			return nil, fmt.Errorf("jwk %s: RSA key too small (%d bits)", k.Kid, n.BitLen())
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("jwk %s: unsupported curve %q", k.Kid, k.Crv)
		}
		x, err := b64BigInt(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64BigInt(k.Y)
		if err != nil {
			return nil, err
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: x, Y: y}
		if !curve.IsOnCurve(x, y) {
			return nil, fmt.Errorf("jwk %s: point not on curve", k.Kid)
		}
		return pub, nil
	}
	return nil, fmt.Errorf("jwk %s: unsupported kty %q", k.Kid, k.Kty)
}

func b64BigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// KeyProvider resolves a kid to a public key.
type KeyProvider interface {
	Key(ctx context.Context, kid string) (crypto.PublicKey, error)
}

// ErrUnknownKey is returned when no key matches the kid even after a refresh.
var ErrUnknownKey = errors.New("auth: unknown signing key")

// KeySet is a caching JWKS client.
type KeySet struct {
	URL    string
	Client *http.Client
	// MinRefreshInterval throttles refreshes triggered by unknown kids, so a
	// flood of tokens with bogus kids cannot turn into a flood of JWKS
	// fetches (jwx and Microsoft both recommend a floor of this kind).
	MinRefreshInterval time.Duration
	// MaxAge bounds how long a document is trusted when the server sends no
	// Cache-Control. Default 24h.
	MaxAge time.Duration
	now    func() time.Time

	mu          sync.Mutex
	keys        map[string]crypto.PublicKey
	fetchedAt   time.Time
	expiresAt   time.Time
	inflight    chan struct{} // closed when the current refresh completes
	inflightErr error
}

// NewKeySet creates a KeySet with production defaults.
func NewKeySet(url string) *KeySet {
	return &KeySet{
		URL:                url,
		Client:             &http.Client{Timeout: 5 * time.Second},
		MinRefreshInterval: time.Minute,
		MaxAge:             24 * time.Hour,
		now:                time.Now,
		keys:               map[string]crypto.PublicKey{},
	}
}

// Key returns the key for kid, refreshing the set when the kid is unknown or
// the cached document has expired.
func (s *KeySet) Key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	s.mu.Lock()
	now := s.now()
	if k, ok := s.keys[kid]; ok && now.Before(s.expiresAt) {
		s.mu.Unlock()
		return k, nil
	}
	// Refresh needed. Throttle unknown-kid refreshes unless the cache is empty or stale.
	if len(s.keys) > 0 && now.Before(s.expiresAt) && now.Sub(s.fetchedAt) < s.MinRefreshInterval {
		s.mu.Unlock()
		return nil, ErrUnknownKey
	}
	if err := s.refreshLocked(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	k, ok := s.keys[kid]
	s.mu.Unlock()
	if !ok {
		return nil, ErrUnknownKey
	}
	return k, nil
}

// Refresh fetches the document unconditionally (call at startup).
func (s *KeySet) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx)
}

// refreshLocked performs or joins a single in-flight fetch. It is called with
// s.mu held and releases it while the HTTP request runs.
func (s *KeySet) refreshLocked(ctx context.Context) error {
	if s.inflight != nil {
		ch := s.inflight
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			s.mu.Lock()
			return ctx.Err()
		}
		s.mu.Lock()
		return s.inflightErr
	}
	ch := make(chan struct{})
	s.inflight = ch
	s.mu.Unlock()

	keys, maxAge, err := s.fetch(ctx)

	s.mu.Lock()
	if err == nil {
		s.keys = keys
		s.fetchedAt = s.now()
		if maxAge <= 0 {
			maxAge = s.MaxAge
		}
		s.expiresAt = s.fetchedAt.Add(maxAge)
	}
	s.inflightErr = err
	s.inflight = nil
	close(ch)
	return err
}

func (s *KeySet) fetch(ctx context.Context) (map[string]crypto.PublicKey, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("auth: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("auth: fetch jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	var doc struct {
		Keys []JWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, 0, fmt.Errorf("auth: parse jwks: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kid == "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		pub, err := k.PublicKey()
		if err != nil {
			continue // skip unusable keys, keep the rest
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, 0, errors.New("auth: jwks contains no usable signing keys")
	}
	return keys, parseMaxAge(resp.Header.Get("Cache-Control")), nil
}

func parseMaxAge(cc string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return 0
}

// StaticKeys is a KeyProvider for tests and for pinned keys.
type StaticKeys map[string]crypto.PublicKey

// Key implements KeyProvider.
func (s StaticKeys) Key(_ context.Context, kid string) (crypto.PublicKey, error) {
	k, ok := s[kid]
	if !ok {
		return nil, ErrUnknownKey
	}
	return k, nil
}
