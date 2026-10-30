// Package cache has the two pieces a gateway needs before reaching for a
// library: request coalescing (singleflight) so one cache miss produces one
// upstream call, and a small bounded TTL+LRU cache for things like JWKS,
// catalog documents and policy bundles.
//
// Flight is a generic adaptation of golang.org/x/sync/singleflight.Group.Do.
// For a general-purpose high-throughput cache use maypok86/otter or
// dgraph-io/ristretto; this cache is deliberately simple and lock-based.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

type call[V any] struct {
	wg   sync.WaitGroup
	val  V
	err  error
	dups int
}

// Flight deduplicates concurrent calls with the same key.
type Flight[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]*call[V]
}

// Do runs fn once per key at a time; concurrent callers with the same key
// wait for and share the first result. shared reports whether the result was
// shared with another caller.
func (f *Flight[K, V]) Do(key K, fn func() (V, error)) (v V, err error, shared bool) {
	f.mu.Lock()
	if f.m == nil {
		f.m = make(map[K]*call[V])
	}
	if c, ok := f.m[key]; ok {
		c.dups++
		f.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call[V])
	c.wg.Add(1)
	f.m[key] = c
	f.mu.Unlock()

	func() {
		defer func() {
			f.mu.Lock()
			delete(f.m, key)
			f.mu.Unlock()
			c.wg.Done()
		}()
		c.val, c.err = fn()
	}()
	return c.val, c.err, c.dups > 0
}

// TTL is a bounded LRU cache with per-entry expiry.
type TTL[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[K]*list.Element
	order    *list.List // front = most recently used
	flight   Flight[K, V]
	now      func() time.Time
}

type entry[K comparable, V any] struct {
	key    K
	val    V
	expiry time.Time
}

// NewTTL creates a cache holding at most capacity entries for ttl each.
func NewTTL[K comparable, V any](capacity int, ttl time.Duration) *TTL[K, V] {
	return &TTL[K, V]{capacity: capacity, ttl: ttl, items: map[K]*list.Element{}, order: list.New(), now: time.Now}
}

// Get returns a live entry.
func (c *TTL[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.now().After(e.expiry) {
		c.order.Remove(el)
		delete(c.items, key)
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

// Set stores a value with the default TTL.
func (c *TTL[K, V]) Set(key K, val V) { c.SetTTL(key, val, c.ttl) }

// SetTTL stores a value with an explicit TTL.
func (c *TTL[K, V]) SetTTL(key K, val V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		e.val, e.expiry = val, c.now().Add(ttl)
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&entry[K, V]{key: key, val: val, expiry: c.now().Add(ttl)})
	c.items[key] = el
	for c.order.Len() > c.capacity {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry[K, V]).key)
	}
}

// Delete removes a key.
func (c *TTL[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

// Len returns the number of entries (including not-yet-evicted expired ones).
func (c *TTL[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// GetOrLoad returns the cached value or loads it once, coalescing concurrent
// misses for the same key (the cache-stampede protection pattern).
func (c *TTL[K, V]) GetOrLoad(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (V, error) {
	if v, ok := c.Get(key); ok {
		return v, nil
	}
	v, err, _ := c.flight.Do(key, func() (V, error) {
		if v, ok := c.Get(key); ok {
			return v, nil
		}
		v, err := load(ctx)
		if err != nil {
			return v, err
		}
		c.Set(key, v)
		return v, nil
	})
	return v, err
}
