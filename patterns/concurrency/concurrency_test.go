package concurrency

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupLimitsAndCancelsOnError(t *testing.T) {
	g, ctx := NewGroup(context.Background(), 2)
	var inflight, peak atomic.Int32
	want := errors.New("boom")
	for i := 0; i < 6; i++ {
		g.Go(func() error {
			n := inflight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer inflight.Add(-1)
			if i == 2 {
				return want
			}
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Millisecond):
			}
			return nil
		})
	}
	err := g.Wait()
	if !errors.Is(err, want) || peak.Load() > 2 {
		t.Fatalf("err=%v peak=%d", err, peak.Load())
	}
	if !errors.Is(context.Cause(ctx), want) {
		t.Fatal("context must be cancelled with the first error as cause")
	}
}

func TestGroupRecoversPanics(t *testing.T) {
	g, _ := NewGroup(context.Background(), 0)
	g.Go(func() error { panic("oops") })
	if err := g.Wait(); err == nil {
		t.Fatal("panic must become an error")
	}
}

func TestForEach(t *testing.T) {
	var sum atomic.Int64
	err := ForEach(context.Background(), []int{1, 2, 3, 4, 5}, 2, func(_ context.Context, n int) error { sum.Add(int64(n)); return nil })
	if err != nil || sum.Load() != 15 {
		t.Fatal(err, sum.Load())
	}
}

func TestSemaphore(t *testing.T) {
	s := NewSemaphore(1)
	if !s.TryAcquire() || s.TryAcquire() {
		t.Fatal("semaphore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	s.Release()
	if err := s.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGoRecovers(t *testing.T) {
	done := make(chan any, 1)
	Go(func() { panic("bg") }, func(v any, _ []byte) { done <- v })
	select {
	case v := <-done:
		if v != "bg" {
			t.Fatal(v)
		}
	case <-time.After(time.Second):
		t.Fatal("onPanic not called")
	}
}
