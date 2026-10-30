// Package config is hot-swappable configuration: the running config is an
// immutable value behind an atomic pointer; a new version is validated, then
// swapped in one step, and listeners are notified. Readers never lock.
//
// This is how Traefik (ConfigurationWatcher: dedupe with DeepEqual, then call
// every listener), Caddy (changeConfig: If-Match on a config hash, load the
// new config, roll back to the old one on failure) and Envoy's go-control-plane
// (SnapshotCache.SetSnapshot: versioned immutable snapshots, watchers
// notified) all do it. Kubernetes informers are the same idea with a watch.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type snapshot[T any] struct {
	value   T
	version string
}

// Hot holds the live value of T.
type Hot[T any] struct {
	p         atomic.Pointer[snapshot[T]]
	validate  func(T) error
	mu        sync.Mutex
	listeners []func(T)
}

// New creates a Hot with an initial value, which must pass validate.
func New[T any](initial T, validate func(T) error) (*Hot[T], error) {
	h := &Hot[T]{validate: validate}
	if validate != nil {
		if err := validate(initial); err != nil {
			return nil, err
		}
	}
	h.p.Store(&snapshot[T]{value: initial, version: Version(initial)})
	return h, nil
}

// Load returns the current value. It is safe for concurrent use and must be
// called once per request (hold the returned value for the request's
// lifetime so a mid-request swap cannot mix two configs).
func (h *Hot[T]) Load() T { return h.p.Load().value }

// CurrentVersion returns the hash of the current value (an ETag).
func (h *Hot[T]) CurrentVersion() string { return h.p.Load().version }

// Swap validates next and installs it. It returns false when next is equal
// to the current value (no listeners run), and an error when validation
// fails (the old value stays live).
func (h *Hot[T]) Swap(next T) (changed bool, err error) {
	if h.validate != nil {
		if err := h.validate(next); err != nil {
			return false, fmt.Errorf("config: rejected: %w", err)
		}
	}
	cur := h.p.Load()
	if reflect.DeepEqual(cur.value, next) {
		return false, nil
	}
	h.p.Store(&snapshot[T]{value: next, version: Version(next)})
	h.mu.Lock()
	ls := append([]func(T){}, h.listeners...)
	h.mu.Unlock()
	for _, l := range ls {
		l(next)
	}
	return true, nil
}

// OnChange registers a listener called after every successful swap. Keep
// listeners fast and non-blocking; they run on the swapper's goroutine.
func (h *Hot[T]) OnChange(fn func(T)) {
	h.mu.Lock()
	h.listeners = append(h.listeners, fn)
	h.mu.Unlock()
}

// Version hashes the JSON form of v. Two values with the same JSON share a
// version, which is what an ETag needs.
func Version(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// ErrPreconditionFailed is returned by CheckIfMatch on a version mismatch.
var ErrPreconditionFailed = errors.New("config: If-Match does not match current version")

// CheckIfMatch implements Caddy's admin API rule: an update must carry the
// version it was based on, so two operators cannot clobber each other.
// ifMatch is the raw header value (quoted ETag); empty means unconditional.
func CheckIfMatch(ifMatch, current string) error {
	if ifMatch == "" {
		return nil
	}
	v := strings.Trim(strings.TrimPrefix(ifMatch, "W/"), `"`)
	if v != current {
		return ErrPreconditionFailed
	}
	return nil
}

// Watch polls load on the interval and swaps new values in. A load error
// keeps the last good config live (Traefik keeps serving the last
// configuration when a provider fails). It returns when ctx ends.
func Watch[T any](ctx context.Context, h *Hot[T], every time.Duration, load func(ctx context.Context) (T, error), log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			next, err := load(ctx)
			if err != nil {
				log.WarnContext(ctx, "config: load failed, keeping previous", "err", err)
				continue
			}
			changed, err := h.Swap(next)
			if err != nil {
				log.ErrorContext(ctx, "config: new version rejected, keeping previous", "err", err)
				continue
			}
			if changed {
				log.InfoContext(ctx, "config: applied", "version", h.CurrentVersion())
			}
		}
	}
}
