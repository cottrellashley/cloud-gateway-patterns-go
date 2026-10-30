// Package session is server-side user state with an encrypted cookie handle,
// the model Pomerium uses: the browser holds a small AEAD-sealed handle
// (session ID + expiry) and everything else lives in a store the gateway
// controls. Revocation is a store delete; the cookie cannot be forged because
// it is authenticated encryption keyed by the gateway.
//
// Cryptography is Pomerium's cryptutil.Encrypt/Decrypt shape (random nonce,
// AEAD seal with additional data, nonce carried alongside) using AES-256-GCM
// from the standard library. The additional data binds the ciphertext to the
// cookie name, so a value cannot be replayed under another cookie.
//
// CSRF: Go 1.25 ships http.CrossOriginProtection (Sec-Fetch-Site / Origin
// checks); use it on every state-changing route that is cookie-authenticated.
// API routes authenticated by bearer tokens do not need CSRF protection.
package session

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Codec seals and opens small blobs.
type Codec struct{ aead cipher.AEAD }

// NewCodec takes a 32-byte key (AES-256). Rotate by keeping the previous
// codec for Open only.
func NewCodec(key []byte) (*Codec, error) {
	if len(key) != 32 {
		return nil, errors.New("session: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead}, nil
}

// Seal encrypts plaintext bound to ad and returns base64url(nonce || ciphertext).
func (c *Codec) Seal(plaintext, ad []byte) string {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	out := c.aead.Seal(nonce, nonce, plaintext, ad)
	return base64.RawURLEncoding.EncodeToString(out)
}

// Open reverses Seal; any tampering fails authentication.
func (c *Codec) Open(s string, ad []byte) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("session: decode: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) <= ns {
		return nil, errors.New("session: ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], ad)
	if err != nil {
		return nil, errors.New("session: authentication failed")
	}
	return pt, nil
}

// Session is the server-side record.
type Session struct {
	ID        string            `json:"id"`
	Subject   string            `json:"sub"`
	IssuedAt  time.Time         `json:"iat"`
	ExpiresAt time.Time         `json:"exp"`
	Data      map[string]string `json:"data,omitempty"`
}

// Store persists sessions. Production: Cosmos DB with the session ID as the
// partition key and TTL set to the expiry; Redis with EXPIRE; both are
// one-document-per-session with no cross-session queries.
type Store interface {
	Get(ctx context.Context, id string) (*Session, error)
	Put(ctx context.Context, s *Session) error
	Delete(ctx context.Context, id string) error
}

// Errors.
var (
	ErrNoSession = errors.New("session: no session")
	ErrExpired   = errors.New("session: expired")
)

// MemoryStore is for tests and single-replica use.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]*Session
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]*Session{}} }

func (s *MemoryStore) Get(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[id]
	if !ok {
		return nil, ErrNoSession
	}
	cp := *v
	return &cp, nil
}

func (s *MemoryStore) Put(_ context.Context, v *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *v
	s.m[v.ID] = &cp
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

// CookieOptions configure the handle cookie. Defaults are the secure ones.
type CookieOptions struct {
	Name     string
	Domain   string
	Path     string
	Secure   bool
	HTTPOnly bool
	SameSite http.SameSite
	// TTL is the session lifetime; the cookie Max-Age matches it.
	TTL time.Duration
}

// DefaultCookieOptions: HttpOnly, Secure, SameSite=Lax, 8 hours.
func DefaultCookieOptions(name string) CookieOptions {
	return CookieOptions{Name: name, Path: "/", Secure: true, HTTPOnly: true, SameSite: http.SameSiteLaxMode, TTL: 8 * time.Hour}
}

// Manager ties codec, store and cookie together.
type Manager struct {
	codec *Codec
	store Store
	opts  CookieOptions
	now   func() time.Time
}

// NewManager builds a Manager.
func NewManager(codec *Codec, store Store, opts CookieOptions) *Manager {
	return &Manager{codec: codec, store: store, opts: opts, now: time.Now}
}

type handle struct {
	ID  string `json:"id"`
	Exp int64  `json:"exp"`
}

// NewID returns a 256-bit random session ID.
func NewID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Load reads and validates the cookie, then loads the session from the store.
func (m *Manager) Load(r *http.Request) (*Session, error) {
	c, err := r.Cookie(m.opts.Name)
	if err != nil {
		return nil, ErrNoSession
	}
	pt, err := m.codec.Open(c.Value, []byte(m.opts.Name))
	if err != nil {
		return nil, ErrNoSession
	}
	var h handle
	if err := json.Unmarshal(pt, &h); err != nil || h.ID == "" {
		return nil, ErrNoSession
	}
	now := m.now()
	if now.Unix() >= h.Exp {
		return nil, ErrExpired
	}
	s, err := m.store.Get(r.Context(), h.ID)
	if err != nil {
		return nil, err
	}
	if !s.ExpiresAt.After(now) {
		_ = m.store.Delete(r.Context(), h.ID)
		return nil, ErrExpired
	}
	return s, nil
}

// Save persists the session and writes the cookie. Call after login and
// after any change to Data.
func (m *Manager) Save(ctx context.Context, w http.ResponseWriter, s *Session) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	now := m.now()
	if s.IssuedAt.IsZero() {
		s.IssuedAt = now
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = now.Add(m.opts.TTL)
	}
	if err := m.store.Put(ctx, s); err != nil {
		return err
	}
	hb, _ := json.Marshal(handle{ID: s.ID, Exp: s.ExpiresAt.Unix()})
	http.SetCookie(w, m.cookie(m.codec.Seal(hb, []byte(m.opts.Name)), int(time.Until(s.ExpiresAt).Seconds())))
	return nil
}

// Rotate issues a new ID for an existing session (do this at login to defeat
// session fixation) and deletes the old record.
func (m *Manager) Rotate(ctx context.Context, w http.ResponseWriter, s *Session) error {
	old := s.ID
	s.ID = NewID()
	if err := m.Save(ctx, w, s); err != nil {
		return err
	}
	if old != "" {
		_ = m.store.Delete(ctx, old)
	}
	return nil
}

// Clear deletes the session and expires the cookie.
func (m *Manager) Clear(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if s, err := m.Load(r); err == nil {
		_ = m.store.Delete(ctx, s.ID)
	}
	http.SetCookie(w, m.cookie("", -1))
}

func (m *Manager) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     m.opts.Name,
		Value:    value,
		Domain:   m.opts.Domain,
		Path:     m.opts.Path,
		MaxAge:   maxAge,
		Secure:   m.opts.Secure,
		HttpOnly: m.opts.HTTPOnly,
		SameSite: m.opts.SameSite,
	}
}
