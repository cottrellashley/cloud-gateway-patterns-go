package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Requeue is returned by a Sync function to ask for the key to be processed
// again after a delay without counting as a failure (for example, waiting
// for an upstream to finish provisioning).
type Requeue struct{ After time.Duration }

func (r Requeue) Error() string { return fmt.Sprintf("requeue after %s", r.After) }

// Controller runs Workers goroutines that take keys from Queue and call
// Sync. Sync must be level-triggered: read the current desired state and
// the current actual state, and converge, without assuming it saw every
// intermediate change. Returning nil forgets the key's failures; returning
// Requeue re-adds it after the delay; any other error re-adds it with
// exponential backoff.
type Controller[T comparable] struct {
	Queue   *Queue[T]
	Sync    func(ctx context.Context, key T) error
	Workers int
	// Log receives sync failures; nil disables logging.
	Log *slog.Logger
	// OnError, if set, is called for every failed sync (metrics, alerts).
	OnError func(key T, err error)
}

// Run starts the workers and blocks until ctx is done, then shuts the queue
// down and waits for in-flight syncs to finish. It returns ctx.Err().
func (c *Controller[T]) Run(ctx context.Context) error {
	if c.Queue == nil || c.Sync == nil {
		return errors.New("reconcile: Controller needs Queue and Sync")
	}
	workers := c.Workers
	if workers <= 0 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c.processNext(ctx) {
			}
		}()
	}
	<-ctx.Done()
	c.Queue.ShutDown()
	wg.Wait()
	return ctx.Err()
}

// processNext handles one key. It returns false when the queue is shut down.
func (c *Controller[T]) processNext(ctx context.Context) bool {
	key, shutdown := c.Queue.Get()
	if shutdown {
		return false
	}
	defer c.Queue.Done(key)

	err := c.safeSync(ctx, key)
	switch {
	case err == nil:
		c.Queue.Forget(key)
	case errors.As(err, new(Requeue)):
		var rq Requeue
		errors.As(err, &rq)
		c.Queue.AddAfter(key, rq.After)
	case ctx.Err() != nil:
		// Shutting down: do not count the interruption as a failure; the
		// key is re-added so a restart picks it up (level-triggered).
		c.Queue.Add(key)
	default:
		if c.Log != nil {
			c.Log.Error("sync failed; requeuing", "key", fmt.Sprint(key), "err", err, "requeues", c.Queue.NumRequeues(key))
		}
		if c.OnError != nil {
			c.OnError(key, err)
		}
		c.Queue.AddRateLimited(key)
	}
	return true
}

func (c *Controller[T]) safeSync(ctx context.Context, key T) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("reconcile: panic in sync for %v: %v\n%s", key, r, debug.Stack())
		}
	}()
	return c.Sync(ctx, key)
}

// Resync re-adds every key returned by list on a fixed period until ctx is
// done. It is the safety net that makes the loop level-triggered even if an
// event was missed: eventually every object is looked at again.
func Resync[T comparable](ctx context.Context, q *Queue[T], every time.Duration, list func(context.Context) ([]T, error)) error {
	if every <= 0 {
		return errors.New("reconcile: Resync period must be positive")
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			keys, err := list(ctx)
			if err != nil {
				continue
			}
			for _, k := range keys {
				q.Add(k)
			}
		}
	}
}
