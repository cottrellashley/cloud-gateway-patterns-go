// Package lb implements upstream selection the way Traefik and Caddy do it:
// an Upstream carries health, in-flight and failure counters; a Balancer picks
// among the available ones.
//
// Two algorithms are provided because they cover almost every gateway need:
//
//   - WeightedRoundRobin: Traefik's wrr.Balancer — an earliest-deadline-first
//     heap where each pick advances the chosen server's deadline by 1/weight.
//     Smooth, deterministic, honours weights exactly, O(log n) per pick.
//   - PowerOfTwoChoices: Traefik's p2c.Balancer — pick two random healthy
//     servers, send to the one with fewer in-flight requests. Near-optimal
//     load distribution with no shared state beyond a counter per server.
//
// Caddy's Upstream.Available() == Healthy() && !Full() is copied verbatim in
// spirit: a server is skipped when it is unhealthy or at MaxRequests.
package lb

import (
	"container/heap"
	"errors"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoAvailableUpstream is returned when every upstream is unhealthy or full.
var ErrNoAvailableUpstream = errors.New("lb: no available upstream")

// Upstream is one backend address with its live state.
type Upstream struct {
	Name   string
	URL    *url.URL
	Weight float64 // > 0; 1 if unset
	// MaxRequests caps concurrent in-flight requests; 0 means unlimited.
	MaxRequests int64

	inflight atomic.Int64
	fails    atomic.Int64
	healthy  atomic.Bool
}

// NewUpstream creates a healthy upstream.
func NewUpstream(name string, u *url.URL, weight float64) *Upstream {
	if weight <= 0 {
		weight = 1
	}
	up := &Upstream{Name: name, URL: u, Weight: weight}
	up.healthy.Store(true)
	return up
}

// SetHealthy is called by active health checks.
func (u *Upstream) SetHealthy(ok bool) { u.healthy.Store(ok) }

// Healthy is true when active checks pass and passive failures are below the
// threshold the balancer was given (see MaxFails on the balancers).
func (u *Upstream) Healthy() bool { return u.healthy.Load() }

// Full reports whether MaxRequests is reached.
func (u *Upstream) Full() bool { return u.MaxRequests > 0 && u.inflight.Load() >= u.MaxRequests }

// Available is Caddy's rule: healthy and not full.
func (u *Upstream) Available(maxFails int64) bool {
	if !u.Healthy() || u.Full() {
		return false
	}
	return maxFails <= 0 || u.fails.Load() < maxFails
}

// Acquire marks a request in flight. Call Release when it completes.
func (u *Upstream) Acquire() { u.inflight.Add(1) }

// Release ends an in-flight request.
func (u *Upstream) Release() { u.inflight.Add(-1) }

// InFlight returns the current in-flight count.
func (u *Upstream) InFlight() int64 { return u.inflight.Load() }

// Fails returns the current passive failure count.
func (u *Upstream) Fails() int64 { return u.fails.Load() }

// RecordFailure implements Caddy's passive health check: count the failure now
// and forget it after failDuration. With MaxFails on the balancer this takes a
// flapping backend out of rotation for failDuration without any active probe.
func (u *Upstream) RecordFailure(failDuration time.Duration) {
	u.fails.Add(1)
	if failDuration <= 0 {
		return
	}
	time.AfterFunc(failDuration, func() { u.fails.Add(-1) })
}

// Balancer selects an upstream for a request.
type Balancer interface {
	Pick() (*Upstream, error)
}

// WeightedRoundRobin is Traefik's EDF balancer.
type WeightedRoundRobin struct {
	MaxFails int64

	mu          sync.Mutex
	handlers    []*wrrEntry
	curDeadline float64
}

type wrrEntry struct {
	up       *Upstream
	deadline float64
}

// heap.Interface over handlers.
func (b *WeightedRoundRobin) Len() int { return len(b.handlers) }
func (b *WeightedRoundRobin) Less(i, j int) bool {
	return b.handlers[i].deadline < b.handlers[j].deadline
}
func (b *WeightedRoundRobin) Swap(i, j int) {
	b.handlers[i], b.handlers[j] = b.handlers[j], b.handlers[i]
}
func (b *WeightedRoundRobin) Push(x any) { b.handlers = append(b.handlers, x.(*wrrEntry)) }
func (b *WeightedRoundRobin) Pop() any {
	h := b.handlers[len(b.handlers)-1]
	b.handlers = b.handlers[:len(b.handlers)-1]
	return h
}

// Add registers an upstream. A newly added server starts at the current
// deadline so it competes fairly with existing ones (Traefik's comment:
// "curDeadline should be handler's deadline so that new added entry would
// have a fair competition environment with the old ones").
func (b *WeightedRoundRobin) Add(up *Upstream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := &wrrEntry{up: up, deadline: b.curDeadline + 1/up.Weight}
	heap.Push(b, e)
}

// Pick returns the available upstream with the earliest deadline.
func (b *WeightedRoundRobin) Pick() (*Upstream, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.handlers) == 0 {
		return nil, ErrNoAvailableUpstream
	}
	// Bound the loop: each iteration pops and pushes one entry, so after
	// len(handlers) iterations without finding an available one, give up.
	for i := 0; i < len(b.handlers); i++ {
		e := heap.Pop(b).(*wrrEntry)
		b.curDeadline = e.deadline
		e.deadline += 1 / e.up.Weight
		heap.Push(b, e)
		if e.up.Available(b.MaxFails) {
			return e.up, nil
		}
	}
	return nil, ErrNoAvailableUpstream
}

// PowerOfTwoChoices is Traefik's p2c balancer.
type PowerOfTwoChoices struct {
	MaxFails int64

	mu       sync.RWMutex
	upstream []*Upstream
}

// Add registers an upstream.
func (b *PowerOfTwoChoices) Add(up *Upstream) {
	b.mu.Lock()
	b.upstream = append(b.upstream, up)
	b.mu.Unlock()
}

// Pick chooses two distinct available upstreams at random and returns the one
// with fewer requests in flight.
func (b *PowerOfTwoChoices) Pick() (*Upstream, error) {
	b.mu.RLock()
	healthy := make([]*Upstream, 0, len(b.upstream))
	for _, u := range b.upstream {
		if u.Available(b.MaxFails) {
			healthy = append(healthy, u)
		}
	}
	b.mu.RUnlock()
	switch len(healthy) {
	case 0:
		return nil, ErrNoAvailableUpstream
	case 1:
		return healthy[0], nil
	}
	n1 := rand.IntN(len(healthy))
	n2 := rand.IntN(len(healthy) - 1)
	if n2 >= n1 {
		n2++
	}
	h1, h2 := healthy[n1], healthy[n2]
	if h2.InFlight() < h1.InFlight() {
		return h2, nil
	}
	return h1, nil
}
