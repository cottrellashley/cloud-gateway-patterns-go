package ratelimit

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Counter is the storage a FixedWindow needs: an atomic increment with a TTL.
// Redis INCR + EXPIRE (pipelined) is the production implementation; this is
// exactly what envoyproxy/ratelimit does per cache key.
type Counter interface {
	Increment(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error)
}

// MemoryCounter is a Counter for a single replica and for tests.
type MemoryCounter struct {
	mu   sync.Mutex
	vals map[string]memVal
	now  func() time.Time
}

type memVal struct {
	n      int64
	expiry time.Time
}

// NewMemoryCounter returns an empty counter.
func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{vals: map[string]memVal{}, now: time.Now}
}

// Increment adds n and returns the new value, resetting expired keys.
func (m *MemoryCounter) Increment(_ context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	v, ok := m.vals[key]
	if !ok || v.expiry.Before(now) {
		v = memVal{expiry: now.Add(ttl)}
	}
	v.n += n
	m.vals[key] = v
	// opportunistic sweep to bound memory
	if len(m.vals) > 10000 {
		for k, x := range m.vals {
			if x.expiry.Before(now) {
				delete(m.vals, k)
			}
		}
	}
	return v.n, nil
}

// FixedWindow allows Limit hits per Unit per key. The counter key embeds the
// window start (floor(now/Unit)), so all replicas sharing the Counter agree on
// the window without coordination (Envoy ratelimit's cache key design).
type FixedWindow struct {
	Counter Counter
	Limit   int64
	Unit    time.Duration
	Policy  string
	now     func() time.Time
}

// Allow records one hit and reports the decision.
func (f *FixedWindow) Allow(ctx context.Context, key string) (Decision, error) {
	now := f.now
	if now == nil {
		now = time.Now
	}
	t := now()
	windowStart := t.Truncate(f.Unit)
	reset := windowStart.Add(f.Unit).Sub(t)
	k := key + ":" + strconv.FormatInt(windowStart.Unix(), 10)
	// TTL slightly longer than the window so a late reader still sees it.
	v, err := f.Counter.Increment(ctx, k, 1, f.Unit+time.Second)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Allowed: v <= f.Limit, Limit: int(f.Limit), Window: f.Unit, ResetAfter: reset}
	if rem := f.Limit - v; rem > 0 {
		d.Remaining = int(rem)
	}
	if !d.Allowed {
		d.RetryAfter = reset
	}
	return d, nil
}
