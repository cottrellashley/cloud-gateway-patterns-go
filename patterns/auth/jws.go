package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"
)

// Header is the JOSE header subset the verifier inspects.
type Header struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ,omitempty"`
	Crit []string `json:"crit,omitempty"`
}

// compact holds the parsed parts of a compact JWS.
type compact struct {
	header       Header
	payload      []byte
	signature    []byte
	signingInput []byte
}

// ErrMalformedToken covers every structural failure.
var ErrMalformedToken = errors.New("auth: malformed token")

func parseCompact(token string) (*compact, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformedToken
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformedToken
	}
	var h Header
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, ErrMalformedToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformedToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformedToken
	}
	return &compact{
		header:       h,
		payload:      payload,
		signature:    sig,
		signingInput: []byte(parts[0] + "." + parts[1]),
	}, nil
}

type algo struct {
	hash     crypto.Hash
	newHash  func() hash.Hash
	kind     string // "rsa-pkcs1", "rsa-pss", "ecdsa"
	curveLen int    // bytes per coordinate for ECDSA
}

var algos = map[string]algo{
	"RS256": {crypto.SHA256, sha256.New, "rsa-pkcs1", 0},
	"RS384": {crypto.SHA384, sha512.New384, "rsa-pkcs1", 0},
	"RS512": {crypto.SHA512, sha512.New, "rsa-pkcs1", 0},
	"PS256": {crypto.SHA256, sha256.New, "rsa-pss", 0},
	"PS384": {crypto.SHA384, sha512.New384, "rsa-pss", 0},
	"PS512": {crypto.SHA512, sha512.New, "rsa-pss", 0},
	"ES256": {crypto.SHA256, sha256.New, "ecdsa", 32},
	"ES384": {crypto.SHA384, sha512.New384, "ecdsa", 48},
	"ES512": {crypto.SHA512, sha512.New, "ecdsa", 66},
}

// verifySignature checks the signature against key for the given alg.
func verifySignature(alg string, key crypto.PublicKey, signingInput, sig []byte) error {
	a, ok := algos[alg]
	if !ok {
		return fmt.Errorf("auth: unsupported alg %q", alg)
	}
	h := a.newHash()
	h.Write(signingInput)
	digest := h.Sum(nil)
	switch a.kind {
	case "rsa-pkcs1":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("auth: key type does not match alg")
		}
		return rsa.VerifyPKCS1v15(pub, a.hash, digest, sig)
	case "rsa-pss":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("auth: key type does not match alg")
		}
		return rsa.VerifyPSS(pub, a.hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ecdsa":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("auth: key type does not match alg")
		}
		if len(sig) != 2*a.curveLen {
			return errors.New("auth: bad ECDSA signature length")
		}
		r := new(big.Int).SetBytes(sig[:a.curveLen])
		s := new(big.Int).SetBytes(sig[a.curveLen:])
		if !ecdsa.Verify(pub, digest, r, s) {
			return errors.New("auth: invalid ECDSA signature")
		}
		return nil
	}
	return errors.New("auth: unreachable")
}
