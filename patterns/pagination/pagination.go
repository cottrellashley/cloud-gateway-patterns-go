// Package pagination implements opaque, signed, expiring cursors.
//
// The rules come from the Azure REST guidelines (nextLink never null, absent
// on the last page; document whether pages may skip or duplicate), the
// Kubernetes list API (continue tokens expire and the client must restart
// with 410 Gone) and the Atlas spec section 9: a cursor is bound to the exact
// input it was issued for, is opaque to clients, and expires.
//
// Format: base64url(JSON{v,h,p,exp}) "." base64url(HMAC-SHA256). Signing
// prevents clients from forging positions (an unsigned offset cursor lets a
// caller read rows it cannot filter to).
package pagination

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Errors map to 400 (invalid/mismatch) and 410 (expired).
var (
	ErrCursorInvalid  = errors.New("pagination: invalid cursor")
	ErrCursorMismatch = errors.New("pagination: cursor was issued for a different request")
	ErrCursorExpired  = errors.New("pagination: cursor expired")
)

type envelope struct {
	V   int             `json:"v"`
	H   string          `json:"h"`
	P   json.RawMessage `json:"p"`
	Exp int64           `json:"exp"`
}

// Codec signs cursors with an HMAC key. Rotate keys by trying the old codec
// on decode failure for one TTL.
type Codec struct {
	key []byte
	now func() time.Time
}

// NewCodec creates a codec; key should be at least 32 random bytes.
func NewCodec(key []byte) *Codec { return &Codec{key: key, now: time.Now} }

// InputHash canonicalises the query parameters a cursor belongs to. Use a
// struct with deterministic field order (encoding/json preserves struct field
// order and sorts map keys), excluding the cursor and page size themselves.
func InputHash(input any) string {
	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Encode creates a cursor for position (any JSON-serialisable value) valid
// for ttl, bound to input.
func (c *Codec) Encode(input any, position any, ttl time.Duration) (string, error) {
	p, err := json.Marshal(position)
	if err != nil {
		return "", err
	}
	env := envelope{V: 1, H: InputHash(input), P: p, Exp: c.now().Add(ttl).Unix()}
	body, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	b := base64.RawURLEncoding.EncodeToString(body)
	return b + "." + base64.RawURLEncoding.EncodeToString(c.sign(b)), nil
}

// Decode validates the cursor against input and unmarshals the position.
func (c *Codec) Decode(cursor string, input any, position any) error {
	body, sig, ok := strings.Cut(cursor, ".")
	if !ok || len(cursor) > 4096 {
		return ErrCursorInvalid
	}
	want := c.sign(body)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrCursorInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return ErrCursorInvalid
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.V != 1 {
		return ErrCursorInvalid
	}
	if env.H != InputHash(input) {
		return ErrCursorMismatch
	}
	if c.now().Unix() >= env.Exp {
		return ErrCursorExpired
	}
	return json.Unmarshal(env.P, position)
}

func (c *Codec) sign(body string) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(body))
	return m.Sum(nil)
}

// Page is the list response shape: items plus an optional next cursor.
// NextCursor is omitted (not null) on the last page.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// ClampPageSize applies default and maximum.
func ClampPageSize(requested, def, max int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}
