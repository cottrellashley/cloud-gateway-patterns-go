// Package reconcile is a level-triggered controller loop: a work queue with
// Kubernetes workqueue semantics (de-duplication, no two workers on the same
// key, re-queue after processing if the key was added meanwhile, per-key
// exponential retry) and a Controller that drives a Sync function from it.
//
// It implements docs/00-principles/02-declarative-intent-and-reconciliation.md.
// The queue follows k8s.io/client-go/util/workqueue (queue.go: dirty and
// processing sets; rate_limiting_queue.go: AddRateLimited / Forget;
// default_rate_limiters.go: ItemExponentialFailureRateLimiter). The
// controller follows kubernetes/sample-controller processNextWorkItem and
// the guidelines in kubernetes/community controllers.md.
package reconcile

import (
	"math"
	"sync"
	"time"
)

// RateLimiter decides how long to wait before retrying a key that has
// failed n times (n >= 1).
type RateLimiter interface {
	When(failures int) time.Duration
}

// Exponential is base * 2^(failures-1), capped at Max. The Kubernetes
// controller default is 5ms base and 1000s max.
type Exponential struct {
	Base time.Duration
	Max  time.Duration
}

// When implements RateLimiter.
func (e Exponential) When(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	base, maxDelay := e.Base, e.Max
	if base <= 0 {
		base = 5 * time.Millisecond
	}
	if maxDelay <= 0 {
		maxDelay = 1000 * time.Second
	}
	d := float64(base) * math.Pow(2, float64(failures-1))
	if d > float64(maxDelay) || d > math.MaxInt64 {
		return maxDelay
	}
	return time.Duration(d)
}

// DefaultRateLimiter mirrors workqueue.DefaultControllerRateLimiter's
// per-item part: 5ms doubling to 1000s.
func DefaultRateLimiter() RateLimiter {
	return Exponential{Base: 5 * time.Millisecond, Max: 1000 * time.Second}
}

// Timer abstracts time.AfterFunc so tests can control delays.
type Timer interface{ Stop() bool }

// Options configure a Queue.
type Options struct {
	// Limiter decides retry delays for AddRateLimited. Default: DefaultRateLimiter.
	Limiter RateLimiter
	// AfterFunc schedules a delayed Add. Default: time.AfterFunc.
	AfterFunc func(d time.Duration, f func()) Timer
	// Now is the clock used for delayed-add coalescing. Default: time.Now.
	Now func() time.Time
}

type stdTimer struct{ *time.Timer }

func defaultAfterFunc(d time.Duration, f func()) Timer { return stdTimer{time.AfterFunc(d, f)} }

// Queue is a de-duplicating work queue. Keys are usually object names.
type Queue[T comparable] struct {
	mu   sync.Mutex
	cond *sync.Cond

	queue      []T
	dirty      map[T]struct{} // needs processing
	processing map[T]struct{} // being processed right now
	failures   map[T]int
	waiting    map[T]waiter
	shutDown   bool

	limiter   RateLimiter
	afterFunc func(time.Duration, func()) Timer
	now       func() time.Time
}

type waiter struct {
	readyAt time.Time
	timer   Timer
}

// New returns an empty queue.
func New[T comparable](opts Options) *Queue[T] {
	if opts.Limiter == nil {
		opts.Limiter = DefaultRateLimiter()
	}
	if opts.AfterFunc == nil {
		opts.AfterFunc = defaultAfterFunc
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	q := &Queue[T]{
		dirty:      map[T]struct{}{},
		processing: map[T]struct{}{},
		failures:   map[T]int{},
		waiting:    map[T]waiter{},
		limiter:    opts.Limiter,
		afterFunc:  opts.AfterFunc,
		now:        opts.Now,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add marks item dirty and queues it unless it is already queued. If it is
// being processed, it is queued again once Done is called, so a change that
// arrives mid-sync is never lost. Adds after ShutDown are ignored.
func (q *Queue[T]) Add(item T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.add(item)
}

func (q *Queue[T]) add(item T) {
	if q.shutDown {
		return
	}
	if _, isDirty := q.dirty[item]; isDirty {
		return
	}
	q.dirty[item] = struct{}{}
	if _, inProgress := q.processing[item]; inProgress {
		return
	}
	q.queue = append(q.queue, item)
	q.cond.Signal()
}

// AddAfter adds item after d. If the item is already waiting, the earlier
// of the two deadlines wins.
func (q *Queue[T]) AddAfter(item T, d time.Duration) {
	if d <= 0 {
		q.Add(item)
		return
	}
	q.mu.Lock()
	if q.shutDown {
		q.mu.Unlock()
		return
	}
	readyAt := q.now().Add(d)
	if w, ok := q.waiting[item]; ok {
		if !readyAt.Before(w.readyAt) {
			q.mu.Unlock()
			return
		}
		w.timer.Stop()
	}
	q.mu.Unlock()

	timer := q.afterFunc(d, func() {
		q.mu.Lock()
		delete(q.waiting, item)
		q.add(item)
		q.mu.Unlock()
	})

	q.mu.Lock()
	q.waiting[item] = waiter{readyAt: readyAt, timer: timer}
	q.mu.Unlock()
}

// AddRateLimited records a failure for item and re-adds it after the
// limiter's delay for that failure count.
func (q *Queue[T]) AddRateLimited(item T) {
	q.mu.Lock()
	q.failures[item]++
	n := q.failures[item]
	q.mu.Unlock()
	q.AddAfter(item, q.limiter.When(n))
}

// Forget clears the failure count for item. Call it after a successful sync.
func (q *Queue[T]) Forget(item T) {
	q.mu.Lock()
	delete(q.failures, item)
	q.mu.Unlock()
}

// NumRequeues returns how many times item has failed since it was last
// forgotten.
func (q *Queue[T]) NumRequeues(item T) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failures[item]
}

// Get blocks until an item is available or the queue is shut down and
// drained. The caller must call Done(item) when finished.
func (q *Queue[T]) Get() (item T, shutdown bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.shutDown {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		var zero T
		return zero, true
	}
	item = q.queue[0]
	q.queue[0] = *new(T)
	q.queue = q.queue[1:]
	q.processing[item] = struct{}{}
	delete(q.dirty, item)
	return item, false
}

// Done marks item as no longer being processed. If it was re-added while
// processing it goes back onto the queue.
func (q *Queue[T]) Done(item T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, item)
	if _, isDirty := q.dirty[item]; isDirty {
		q.queue = append(q.queue, item)
		q.cond.Signal()
	} else if len(q.processing) == 0 {
		q.cond.Broadcast()
	}
}

// Len returns the number of queued (not in-progress) items.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// ShutDown stops accepting items and cancels pending delayed adds. Workers
// drain what is queued, then Get returns shutdown = true.
func (q *Queue[T]) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutDown = true
	for item, w := range q.waiting {
		w.timer.Stop()
		delete(q.waiting, item)
	}
	q.cond.Broadcast()
}

// ShuttingDown reports whether ShutDown has been called.
func (q *Queue[T]) ShuttingDown() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.shutDown
}

// WaitIdle blocks until nothing is queued or in progress (or the queue has
// shut down). Useful in tests and for graceful shutdown.
func (q *Queue[T]) WaitIdle() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for (len(q.queue) > 0 || len(q.processing) > 0) && !q.shutDown {
		q.cond.Wait()
	}
}
