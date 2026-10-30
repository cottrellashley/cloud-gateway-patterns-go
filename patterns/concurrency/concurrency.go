// Package concurrency collects the goroutine patterns a server needs and the
// rules that keep them safe (Uber Go style guide: "Don't fire-and-forget
// goroutines", "Channel size is one or none"; x/sync errgroup and semaphore;
// Traefik's safe.Go panic recovery).
//
//   - Group: errgroup with a concurrency limit and context cancellation on
//     the first error (SetLimit + WithContext).
//   - ForEach: bounded parallel map over a slice.
//   - Semaphore: counting semaphore with context-aware Acquire.
//   - Go: a goroutine that recovers panics into a callback instead of
//     crashing the process (for background work; request handlers already
//     have middleware.Recover).
package concurrency

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
)

// Group runs tasks with a limit; the first error cancels the group's context.
type Group struct {
	cancel  context.CancelCauseFunc
	wg      sync.WaitGroup
	sem     chan struct{}
	errOnce sync.Once
	err     error
}

// NewGroup returns a group and a context cancelled on the first error or
// when Wait returns. limit <= 0 means unlimited.
func NewGroup(ctx context.Context, limit int) (*Group, context.Context) {
	ctx, cancel := context.WithCancelCause(ctx)
	g := &Group{cancel: cancel}
	if limit > 0 {
		g.sem = make(chan struct{}, limit)
	}
	return g, ctx
}

// Go starts fn, blocking while the limit is reached. Panics in fn are
// converted to errors so a task cannot take the process down silently.
func (g *Group) Go(fn func() error) {
	if g.sem != nil {
		g.sem <- struct{}{}
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if g.sem != nil {
			defer func() { <-g.sem }()
		}
		defer func() {
			if r := recover(); r != nil {
				g.setErr(fmt.Errorf("concurrency: task panicked: %v\n%s", r, debug.Stack()))
			}
		}()
		if err := fn(); err != nil {
			g.setErr(err)
		}
	}()
}

func (g *Group) setErr(err error) {
	g.errOnce.Do(func() {
		g.err = err
		g.cancel(err)
	})
}

// Wait blocks until all tasks finish and returns the first error.
func (g *Group) Wait() error {
	g.wg.Wait()
	g.cancel(nil)
	return g.err
}

// ForEach applies fn to every item with at most limit in flight; it stops
// scheduling new items after the first error and returns it.
func ForEach[T any](ctx context.Context, items []T, limit int, fn func(ctx context.Context, item T) error) error {
	g, ctx := NewGroup(ctx, limit)
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fn(ctx, it)
		})
	}
	return g.Wait()
}

// Semaphore bounds concurrent access to a resource (connection pool slots,
// expensive computations).
type Semaphore struct{ ch chan struct{} }

// NewSemaphore allows n concurrent holders.
func NewSemaphore(n int) *Semaphore { return &Semaphore{ch: make(chan struct{}, n)} }

// Acquire blocks until a slot is free or ctx ends.
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire takes a slot without waiting.
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot.
func (s *Semaphore) Release() { <-s.ch }

// Go runs fn in a goroutine with panic recovery; onPanic receives the value
// and stack. Use for background work that must not crash the process.
func Go(fn func(), onPanic func(v any, stack []byte)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if onPanic != nil {
					onPanic(r, debug.Stack())
				}
			}
		}()
		fn()
	}()
}
