package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type signer struct {
	kid string
	alg string
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
	jwk JWK
}

func newRSASigner(t *testing.T, kid string) *signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{kid: kid, alg: "RS256", rsa: k, jwk: JWK{Kty: "RSA", Kid: kid, Use: "sig",
		N: base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes())}}
}

func newECSigner(t *testing.T, kid string) *signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(b *big.Int) string {
		out := make([]byte, 32)
		b.FillBytes(out)
		return base64.RawURLEncoding.EncodeToString(out)
	}
	return &signer{kid: kid, alg: "ES256", ec: k, jwk: JWK{Kty: "EC", Kid: kid, Crv: "P-256", X: pad(k.X), Y: pad(k.Y)}}
}

func (s *signer) sign(t *testing.T, claims map[string]any, header map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{"alg": s.alg, "kid": s.kid, "typ": "JWT"}
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	if s.rsa != nil {
		sig, err = rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, digest[:])
	} else {
		r, sv, e := ecdsa.Sign(rand.Reader, s.ec, digest[:])
		err = e
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		sv.FillBytes(sig[32:])
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func jwksServer(t *testing.T, keys *atomic.Pointer[[]JWK], hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		json.NewEncoder(w).Encode(map[string]any{"keys": *keys.Load()})
	}))
}

func baseClaims(now time.Time) map[string]any {
	return map[string]any{"iss": "https://issuer.example/v2.0", "sub": "u1", "aud": "api://atlas",
		"exp": now.Add(time.Hour).Unix(), "nbf": now.Unix(), "iat": now.Unix(), "scp": "atlas.read atlas.write", "tid": "t1"}
}

func TestVerifyHappyPathsAndRejections(t *testing.T) {
	now := time.Now()
	rs := newRSASigner(t, "k1")
	es := newECSigner(t, "k2")
	var keys atomic.Pointer[[]JWK]
	keys.Store(&[]JWK{rs.jwk, es.jwk})
	var hits atomic.Int32
	srv := jwksServer(t, &keys, &hits)
	defer srv.Close()

	ks := NewKeySet(srv.URL)
	v := &Verifier{Issuer: "https://issuer.example/v2.0", Audiences: []string{"api://atlas"}, Keys: ks, Algorithms: []string{"RS256", "ES256"}}
	ctx := context.Background()

	c, err := v.Verify(ctx, rs.sign(t, baseClaims(now), nil))
	if err != nil || c.Subject != "u1" || len(c.Scopes()) != 2 || c.TenantID != "t1" {
		t.Fatalf("rsa verify: %v %+v", err, c)
	}
	if _, err := v.Verify(ctx, es.sign(t, baseClaims(now), nil)); err != nil {
		t.Fatalf("ecdsa verify: %v", err)
	}

	cases := map[string]struct {
		tok  string
		want error
	}{
		"expired":   {rs.sign(t, func() map[string]any { c := baseClaims(now); c["exp"] = now.Add(-time.Minute).Unix(); return c }(), nil), ErrExpired},
		"wrong aud": {rs.sign(t, func() map[string]any { c := baseClaims(now); c["aud"] = []string{"other"}; return c }(), nil), ErrAudience},
		"wrong iss": {rs.sign(t, func() map[string]any { c := baseClaims(now); c["iss"] = "https://evil"; return c }(), nil), ErrIssuer},
		"alg none":  {rs.sign(t, baseClaims(now), map[string]any{"alg": "none", "kid": "k1"}), ErrAlgorithm},
		"no kid":    {rs.sign(t, baseClaims(now), map[string]any{"alg": "RS256"}), ErrInvalidToken},
		"tampered":  {rs.sign(t, baseClaims(now), nil)[:len(rs.sign(t, baseClaims(now), nil))-4] + "AAAA", ErrSignature},
		"garbage":   {"not.a.jwt", ErrMalformedToken},
	}
	for name, c := range cases {
		_, err := v.Verify(ctx, c.tok)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
	// nbf within leeway is accepted
	tok := rs.sign(t, func() map[string]any { c := baseClaims(now); c["nbf"] = now.Add(2 * time.Minute).Unix(); return c }(), nil)
	if _, err := v.Verify(ctx, tok); err != nil {
		t.Fatalf("nbf leeway: %v", err)
	}
}

func TestKeyRotationRefreshesOnceAndIsThrottled(t *testing.T) {
	now := time.Now()
	old := newRSASigner(t, "old")
	var keys atomic.Pointer[[]JWK]
	keys.Store(&[]JWK{old.jwk})
	var hits atomic.Int32
	srv := jwksServer(t, &keys, &hits)
	defer srv.Close()
	ks := NewKeySet(srv.URL)
	ks.MinRefreshInterval = time.Hour
	v := &Verifier{Keys: ks}
	ctx := context.Background()
	if _, err := v.Verify(ctx, old.sign(t, baseClaims(now), nil)); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
	// An unknown kid right after a fetch must NOT trigger another fetch (throttle).
	bogus := newRSASigner(t, "bogus")
	if _, err := v.Verify(ctx, bogus.sign(t, baseClaims(now), nil)); !errors.Is(err, ErrSignature) {
		t.Fatalf("bogus kid: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("throttle failed: hits %d", hits.Load())
	}
	// Rotate: publish new key, move the clock past the throttle, verify fetches once.
	fresh := newRSASigner(t, "fresh")
	keys.Store(&[]JWK{old.jwk, fresh.jwk})
	ks.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	v.Now = ks.now
	if _, err := v.Verify(ctx, fresh.sign(t, baseClaims(ks.now()), nil)); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected exactly one refresh, hits %d", hits.Load())
	}
}

func TestMiddleware(t *testing.T) {
	now := time.Now()
	rs := newRSASigner(t, "k1")
	pub, _ := rs.jwk.PublicKey()
	v := &Verifier{Audiences: []string{"api://atlas"}, Keys: StaticKeys{"k1": pub}}
	var seen *Principal
	var fwdAuth string
	h := Middleware(v, "atlas")(RequireScope("atlas.write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = PrincipalFrom(r.Context())
		fwdAuth = r.Header.Get("Authorization")
		w.WriteHeader(204)
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="atlas"` {
		t.Fatalf("no token: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+rs.sign(t, baseClaims(now), nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 || seen == nil || seen.Subject != "u1" || fwdAuth != "" {
		t.Fatalf("valid: %d principal=%+v auth=%q", rec.Code, seen, fwdAuth)
	}

	req.Header.Set("Authorization", "Bearer "+rs.sign(t, func() map[string]any { c := baseClaims(now); c["scp"] = "atlas.read"; return c }(), nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 || rec.Header().Get("X-Error-Code") != "PolicyDenied" {
		t.Fatalf("scope: %d %s", rec.Code, rec.Header().Get("X-Error-Code"))
	}
}
