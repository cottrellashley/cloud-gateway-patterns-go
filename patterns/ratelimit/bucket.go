// Package ratelimit provides the two limiter shapes gateways use and the HTTP
// middleware that exposes them with standard headers.
//
//   - Bucket: a token bucket with the exact semantics of golang.org/x/time/rate
//     (advance() refills by elapsed time, reserveN() may go negative and
//     return a wait). Right for per-client smoothing with bursts.
//   - Keyed: one Bucket per key with a TTL map (Traefik's ttlmap heap), so
//     idle clients are evicted instead of leaking memory.
//   - FixedWindow: Envoy's ratelimit service algorithm — INCR a counter whose
//     key embeds the window start, with the TTL of the window — on any
//     Counter (in-memory here, Redis in production). Right for quotas shared
//     across gateway replicas.
//
// Headers follow draft-ietf-httpapi-ratelimit-headers (structured fields):
//
//	RateLimit: "<policy>";r=<remaining>;t=<seconds-until-reset>
//	RateLimit-Policy: "<policy>";q=<quota>;w=<window-seconds>
//	Retry-After: <seconds>   (on 429 only)
package ratelimit

import (
	"container/heap"
	"context"
	"math"
	"sync"
	"time"
)

// Bucket is a token bucket. rate is tokens per second; burst is the maximum
// tokens (and the largest single request). Tokens are float64 so that rates
// below one per second work.
type Bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// NewBucket creates a full bucket.
func NewBucket(rate float64, burst int) *Bucket {
	return &Bucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

func (b *Bucket) advance(now time.Time) float64 {
	last := b.last
	if now.Before(last) {
		last = now
	}
	if last.IsZero() {
		return b.tokens
	}
	elapsed := now.Sub(last).Seconds()
	tokens := b.tokens + elapsed*b.rate
	if tokens > b.burst {
		tokens = b.burst
	}
	return tokens
}

// Reserve takes n tokens at `now`, returning how long the caller must wait
// before acting. ok is false when n exceeds burst or the wait exceeds
// maxWait; in that case nothing is taken. This is x/time/rate.reserveN.
func (b *Bucket) Reserve(now time.Time, n int, maxWait time.Duration) (delay time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if float64(n) > b.burst {
		return 0, false
	}
	tokens := b.advance(now) - float64(n)
	var wait time.Duration
	if tokens < 0 {
		wait = time.Duration(math.Ceil(-tokens / b.rate * float64(time.Second)))
	}
	if wait > maxWait {
		return 0, false
	}
	b.last = now
	b.tokens = tokens
	return wait, true
}

// Allow is Reserve(now, 1, 0).
func (b *Bucket) Allow(now time.Time) bool {
	_, ok := b.Reserve(now, 1, 0)
	return ok
}

// Tokens reports the available tokens at now.
func (b *Bucket) Tokens(now time.Time) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.advance(now)
}

// Decision is the result of a limiter check, enough to render headers.
type Decision struct {
	Allowed    bool
	Limit      int           // quota in the window (burst for buckets)
	Remaining  int           // whole tokens left
	Window     time.Duration // time to refill a full burst / fixed window length
	ResetAfter time.Duration // time until Remaining == Limit (bucket) or window end
	RetryAfter time.Duration // when !Allowed: time until one request would pass
}

// Limiter is implemented by Keyed and FixedWindow.
type Limiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// Keyed keeps a Bucket per key in a bounded TTL map.
type Keyed struct {
	rate     float64
	burst    int
	capacity int
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	items map[string]*keyedItem
	heap  expiryHeap
}

type keyedItem struct {
	key    string
	bucket *Bucket
	expiry time.Time
	index  int
}

// NewKeyed creates a keyed limiter. capacity bounds the number of tracked
// keys; when full, the soonest-expiring entries are evicted first. The TTL is
// the time an idle key takes to refill fully plus one second (Traefik's rule:
// inversely proportional to the rate), so an evicted key loses nothing.
func NewKeyed(rate float64, burst, capacity int) *Keyed {
	ttl := time.Duration(float64(burst)/rate*float64(time.Second)) + time.Second
	return &Keyed{rate: rate, burst: burst, capacity: capacity, ttl: ttl, now: time.Now, items: map[string]*keyedItem{}}
}

func (k *Keyed) get(key string, now time.Time) *Bucket {
	k.mu.Lock()
	defer k.mu.Unlock()
	if it, ok := k.items[key]; ok {
		it.expiry = now.Add(k.ttl)
		heap.Fix(&k.heap, it.index)
		return it.bucket
	}
	// evict expired, then oldest if still at capacity
	for len(k.heap) > 0 && (k.heap[0].expiry.Before(now) || len(k.items) >= k.capacity) {
		old := heap.Pop(&k.heap).(*keyedItem)
		delete(k.items, old.key)
	}
	it := &keyedItem{key: key, bucket: NewBucket(k.rate, k.burst), expiry: now.Add(k.ttl)}
	k.items[key] = it
	heap.Push(&k.heap, it)
	return it.bucket
}

// Len reports tracked keys (for metrics).
func (k *Keyed) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

// Allow consumes one token for key.
func (k *Keyed) Allow(_ context.Context, key string) (Decision, error) {
	now := k.now()
	b := k.get(key, now)
	_, ok := b.Reserve(now, 1, 0)
	tokens := b.Tokens(now)
	window := time.Duration(float64(k.burst) / k.rate * float64(time.Second))
	d := Decision{
		Allowed:    ok,
		Limit:      k.burst,
		Remaining:  int(math.Floor(tokens)),
		Window:     window,
		ResetAfter: time.Duration((float64(k.burst) - tokens) / k.rate * float64(time.Second)),
	}
	if !ok {
		d.RetryAfter = time.Duration(math.Ceil((1 - tokens) / k.rate * float64(time.Second)))
	}
	return d, nil
}

type expiryHeap []*keyedItem

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expiry.Before(h[j].expiry) }
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *expiryHeap) Push(x any) {
	it := x.(*keyedItem)
	it.index = len(*h)
	*h = append(*h, it)
}
func (h *expiryHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}
