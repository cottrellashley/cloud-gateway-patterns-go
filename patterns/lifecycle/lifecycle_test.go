package lifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupInterruptsEveryoneWhenOneReturns(t *testing.T) {
	var g Group
	var interrupted atomic.Int32
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		g.Add(func() error { <-stop; return nil }, func(error) { interrupted.Add(1) })
	}
	want := errors.New("first")
	g.Add(func() error { return want }, func(error) { interrupted.Add(1) })
	// closing stop from an interrupt simulates actors honouring interrupt
	g.actors[0].interrupt = func(error) { interrupted.Add(1); close(stop) }

	err := g.Run()
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if interrupted.Load() != 4 {
		t.Fatalf("interrupted %d actors, want 4", interrupted.Load())
	}
}

func TestContextActorCancels(t *testing.T) {
	execute, interrupt := ContextActor(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- execute() }()
	interrupt(nil)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("actor did not stop")
	}
}

func TestReadiness(t *testing.T) {
	var r Readiness
	if r.Ready() {
		t.Fatal("zero value must be not ready")
	}
	r.Set(true)
	if !r.Ready() {
		t.Fatal("expected ready")
	}
}
