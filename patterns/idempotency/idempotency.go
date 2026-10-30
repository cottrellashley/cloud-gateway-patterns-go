// Package idempotency implements the Idempotency-Key pattern (IETF
// draft-ietf-httpapi-idempotency-key-header; the Stripe design; the Azure
// REST guidelines' Repeatability-Request-ID) for unsafe methods:
//
//   - same key + same request fingerprint → the stored response is replayed
//     with Idempotent-Replayed: true;
//   - same key + different fingerprint → 422 IdempotencyKeyReused;
//   - same key while the first request is still running → 409 Conflict
//     (the client should retry after a moment, not run it twice);
//   - keys expire after Retention (24h is the common choice).
//
// The fingerprint is a SHA-256 of method, path and body, so a retry of the
// exact same request is honoured and a different request reusing a key is
// refused. Responses are captured only up to MaxResponseBytes; larger ones
// are not cached and a replay re-executes (document this per route).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// Record is what the store keeps per key.
type Record struct {
	Fingerprint string
	InProgress  bool
	Status      int
	Header      http.Header
	Body        []byte
	ExpiresAt   time.Time
}

// Store persists records. Production: Cosmos DB with the key as id and TTL,
// or Redis SET NX + EXPIRE for Begin and SET for Complete.
type Store interface {
	// Begin atomically creates an in-progress record. It returns the existing
	// record and false when the key already exists.
	Begin(ctx context.Context, key string, rec Record) (existing Record, created bool, err error)
	Complete(ctx context.Context, key string, rec Record) error
	Delete(ctx context.Context, key string) error
}

// MemoryStore is for tests and single replicas.
type MemoryStore struct {
	mu  sync.Mutex
	m   map[string]Record
	now func() time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]Record{}, now: time.Now} }

func (s *MemoryStore) Begin(_ context.Context, key string, rec Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ex, ok := s.m[key]; ok && ex.ExpiresAt.After(s.now()) {
		return ex, false, nil
	}
	s.m[key] = rec
	return rec, true, nil
}

func (s *MemoryStore) Complete(_ context.Context, key string, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = rec
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

// Options configure the middleware.
type Options struct {
	Store Store
	// Header name; default Idempotency-Key.
	Header string
	// Retention of records; default 24h.
	Retention time.Duration
	// MaxResponseBytes caps captured responses; default 1 MiB.
	MaxResponseBytes int64
	// MaxBodyBytes caps the request body read for fingerprinting; default 4 MiB.
	MaxBodyBytes int64
	// Required rejects unsafe requests without a key when true.
	Required bool
	// Scope returns a per-principal prefix so keys never collide across
	// tenants or users; nil uses the raw key.
	Scope func(*http.Request) string
}

// Middleware applies idempotency to POST, PATCH and DELETE (PUT is idempotent
// by definition and GET/HEAD/OPTIONS are safe).
func Middleware(o Options) func(http.Handler) http.Handler {
	if o.Header == "" {
		o.Header = "Idempotency-Key"
	}
	if o.Retention <= 0 {
		o.Retention = 24 * time.Hour
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = 1 << 20
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 4 << 20
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodDelete:
			default:
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(o.Header)
			if key == "" {
				if o.Required {
					apierror.Write(w, apierror.New(http.StatusBadRequest, apierror.CodeInvalidInput, o.Header+" header is required").WithDetail("MissingHeader", o.Header, ""))
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if len(key) > 255 {
				apierror.Write(w, apierror.New(http.StatusBadRequest, apierror.CodeInvalidInput, o.Header+" is too long"))
				return
			}
			if o.Scope != nil {
				key = o.Scope(r) + ":" + key
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, o.MaxBodyBytes+1))
			if err != nil {
				apierror.Write(w, apierror.Wrap(err, http.StatusBadRequest, apierror.CodeMalformedRequest, "could not read request body"))
				return
			}
			if int64(len(body)) > o.MaxBodyBytes {
				apierror.Write(w, apierror.New(http.StatusRequestEntityTooLarge, apierror.CodePayloadTooLarge, "request body too large for idempotent processing"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			fp := fingerprint(r.Method, r.URL.Path, body)

			existing, created, err := o.Store.Begin(r.Context(), key, Record{Fingerprint: fp, InProgress: true, ExpiresAt: time.Now().Add(o.Retention)})
			if err != nil {
				apierror.Write(w, apierror.Wrap(err, http.StatusServiceUnavailable, apierror.CodeOverloaded, "idempotency store unavailable").WithRetryAfter(1))
				return
			}
			if !created {
				if existing.Fingerprint != fp {
					apierror.Write(w, apierror.New(http.StatusUnprocessableEntity, apierror.CodeIdempotencyReused,
						"this "+o.Header+" was already used with a different request"))
					return
				}
				if existing.InProgress {
					apierror.Write(w, apierror.New(http.StatusConflict, apierror.CodeConflict,
						"a request with this "+o.Header+" is still in progress").WithRetryAfter(1))
					return
				}
				for k, v := range existing.Header {
					w.Header()[k] = v
				}
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(existing.Status)
				_, _ = w.Write(existing.Body)
				return
			}

			cw := &captureWriter{ResponseWriter: w, limit: o.MaxResponseBytes}
			next.ServeHTTP(cw, r)
			if cw.status == 0 {
				cw.status = http.StatusOK
			}
			if cw.overflow || cw.status >= 500 {
				// Do not memoise server errors or oversized responses: let the client retry for real.
				_ = o.Store.Delete(r.Context(), key)
				return
			}
			_ = o.Store.Complete(context.WithoutCancel(r.Context()), key, Record{
				Fingerprint: fp, Status: cw.status, Header: cw.cacheableHeaders(), Body: cw.buf.Bytes(),
				ExpiresAt: time.Now().Add(o.Retention),
			})
		})
	}
}

func fingerprint(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

type captureWriter struct {
	http.ResponseWriter
	status   int
	buf      bytes.Buffer
	limit    int64
	overflow bool
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if !c.overflow {
		if int64(c.buf.Len()+len(b)) > c.limit {
			c.overflow = true
			c.buf.Reset()
		} else {
			c.buf.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

var replayable = []string{"Content-Type", "Location", "Operation-Location", "ETag", "X-Error-Code"}

func (c *captureWriter) cacheableHeaders() http.Header {
	h := http.Header{}
	for _, k := range replayable {
		if v := c.Header().Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	return h
}

// ErrInProgress is exported for custom stores that need it.
var ErrInProgress = errors.New("idempotency: request in progress")
