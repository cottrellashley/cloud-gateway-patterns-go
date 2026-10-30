// Package lifecycle holds the process-level run loop: a group of actors that
// start together and stop together, a signal actor, and a readiness flag that
// health endpoints read.
//
// The Group is a direct adaptation of oklog/run (Apache-2.0): every actor has an
// execute function and an interrupt function; when the first actor returns,
// every other actor is interrupted and Run waits for all of them. Kratos and
// Traefik implement the same idea with errgroup / safe.Pool. The property that
// matters is: there is exactly one place where "the process is stopping" is
// decided, and every long-running goroutine learns about it.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

type actor struct {
	execute   func() error
	interrupt func(error)
}

// Group collects actors. The zero value is ready to use.
type Group struct {
	actors []actor
}

// Add registers an actor. execute must block until the actor is done or
// interrupt is called; interrupt must be safe to call more than once and must
// make execute return promptly.
func (g *Group) Add(execute func() error, interrupt func(error)) {
	g.actors = append(g.actors, actor{execute, interrupt})
}

// Run starts every actor, waits for the first one to return, interrupts the
// rest, waits for all of them, and returns the first error.
func (g *Group) Run() error {
	if len(g.actors) == 0 {
		return nil
	}
	errs := make(chan error, len(g.actors))
	for _, a := range g.actors {
		go func(a actor) { errs <- a.execute() }(a)
	}
	err := <-errs
	for _, a := range g.actors {
		a.interrupt(err)
	}
	for i := 1; i < cap(errs); i++ {
		<-errs
	}
	return err
}

// SignalError is returned by the signal actor so callers can log which signal
// stopped the process and exit 0 rather than treating it as a failure.
type SignalError struct{ Signal os.Signal }

func (e SignalError) Error() string { return fmt.Sprintf("received signal %s", e.Signal) }

// IsSignal reports whether err came from the signal actor.
func IsSignal(err error) bool {
	var se SignalError
	return errors.As(err, &se)
}

// SignalActor returns an actor that completes on SIGINT or SIGTERM (or any
// signals given) or when ctx is done. Register it first in the group.
func SignalActor(ctx context.Context, signals ...os.Signal) (execute func() error, interrupt func(error)) {
	if len(signals) == 0 {
		signals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	}
	ctx, cancel := context.WithCancel(ctx)
	return func() error {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, signals...)
			defer signal.Stop(ch)
			select {
			case sig := <-ch:
				return SignalError{Signal: sig}
			case <-ctx.Done():
				return ctx.Err()
			}
		}, func(error) {
			cancel()
		}
}

// ContextActor turns a function that takes a context into an actor whose
// interrupt cancels that context. Use it for background loops: config
// watchers, health checkers, queue consumers.
func ContextActor(parent context.Context, run func(ctx context.Context) error) (execute func() error, interrupt func(error)) {
	ctx, cancel := context.WithCancel(parent)
	return func() error { return run(ctx) }, func(error) { cancel() }
}

// Readiness is the single flag that /readyz reports. Flip it to false before
// draining so the load balancer stops sending traffic (Kubernetes
// ShutdownDelayDuration, Caddy shutdown_delay). Startup code sets it true only
// after dependencies are verified.
type Readiness struct{ ready atomic.Bool }

// Set marks the process ready or not ready.
func (r *Readiness) Set(ready bool) { r.ready.Store(ready) }

// Ready reports the current state.
func (r *Readiness) Ready() bool { return r.ready.Load() }
