package reconcile

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTimers records delayed adds so tests fire them deterministically.
type fakeTimers struct {
	mu    sync.Mutex
	fires []fakeTimer
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped *atomic.Bool
}

func (t fakeTimer) Stop() bool { return !t.stopped.Swap(true) }

func (ft *fakeTimers) afterFunc(d time.Duration, f func()) Timer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := fakeTimer{d: d, f: f, stopped: new(atomic.Bool)}
	ft.fires = append(ft.fires, t)
	return t
}

// fireAll runs every pending timer that was not stopped.
func (ft *fakeTimers) fireAll() {
	ft.mu.Lock()
	pending := ft.fires
	ft.fires = nil
	ft.mu.Unlock()
	for _, t := range pending {
		if !t.stopped.Load() {
			t.f()
		}
	}
}

func (ft *fakeTimers) delays() []time.Duration {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []time.Duration
	for _, t := range ft.fires {
		if !t.stopped.Load() {
			out = append(out, t.d)
		}
	}
	return out
}

func TestExponential(t *testing.T) {
	e := Exponential{Base: 5 * time.Millisecond, Max: 1000 * time.Second}
	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	for i, w := range want {
		if got := e.When(i + 1); got != w {
			t.Errorf("When(%d) = %s, want %s", i+1, got, w)
		}
	}
	if e.When(100) != 1000*time.Second {
		t.Fatal("must cap at Max")
	}
	if e.When(0) != 5*time.Millisecond {
		t.Fatal("failures < 1 treated as 1")
	}
}

func TestDedupeAndReprocess(t *testing.T) {
	q := New[string](Options{})
	q.Add("a")
	q.Add("a")
	q.Add("b")
	if q.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (dedupe)", q.Len())
	}
	item, _ := q.Get()
	if item != "a" {
		t.Fatalf("Get = %q", item)
	}
	// Re-added while processing: must not be handed to another worker now...
	q.Add("a")
	if q.Len() != 1 {
		t.Fatalf("item in processing must not be queued twice, Len = %d", q.Len())
	}
	// ...but must come back after Done.
	q.Done("a")
	if q.Len() != 2 {
		t.Fatalf("item added during processing must be requeued on Done, Len = %d", q.Len())
	}
	b, _ := q.Get()
	a, _ := q.Get()
	if b != "b" || a != "a" {
		t.Fatalf("order = %q %q", b, a)
	}
	q.Done("b")
	q.Done("a")
	q.WaitIdle()
}

func TestRateLimitedRetryAndForget(t *testing.T) {
	ft := &fakeTimers{}
	q := New[string](Options{AfterFunc: ft.afterFunc, Limiter: Exponential{Base: time.Millisecond, Max: time.Second}})
	q.AddRateLimited("k")
	q.AddRateLimited("k") // second failure before the first fired: earlier deadline wins, later is ignored
	if n := q.NumRequeues("k"); n != 2 {
		t.Fatalf("NumRequeues = %d", n)
	}
	if d := ft.delays(); len(d) != 1 || d[0] != time.Millisecond {
		t.Fatalf("pending delays = %v, want [1ms]", d)
	}
	ft.fireAll()
	if q.Len() != 1 {
		t.Fatal("fired timer must enqueue the item")
	}
	q.Forget("k")
	if q.NumRequeues("k") != 0 {
		t.Fatal("Forget must reset failures")
	}
	// AddAfter with an earlier deadline replaces a later one.
	q2 := New[string](Options{AfterFunc: ft.afterFunc})
	q2.AddAfter("x", time.Hour)
	q2.AddAfter("x", time.Minute)
	if d := ft.delays(); len(d) != 1 || d[0] != time.Minute {
		t.Fatalf("earlier deadline must win, pending = %v", d)
	}
	// ShutDown cancels waiting timers and later Adds are ignored.
	q2.ShutDown()
	if len(ft.delays()) != 0 {
		t.Fatal("ShutDown must stop pending timers")
	}
	q2.Add("y")
	if _, shutdown := q2.Get(); !shutdown {
		t.Fatal("Get after ShutDown on an empty queue must report shutdown")
	}
}

func TestGetBlocksUntilAddOrShutdown(t *testing.T) {
	q := New[int](Options{})
	got := make(chan int, 1)
	go func() {
		v, _ := q.Get()
		got <- v
	}()
	time.Sleep(10 * time.Millisecond)
	q.Add(7)
	select {
	case v := <-got:
		if v != 7 {
			t.Fatalf("got %d", v)
		}
	case <-time.After(time.Second):
		t.Fatal("Get did not wake on Add")
	}
	q.Done(7)
	done := make(chan struct{})
	go func() {
		_, shutdown := q.Get()
		if shutdown {
			close(done)
		}
	}()
	q.ShutDown()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Get did not return on ShutDown")
	}
}

func TestControllerRetriesThenForgets(t *testing.T) {
	ft := &fakeTimers{}
	q := New[string](Options{AfterFunc: ft.afterFunc, Limiter: Exponential{Base: time.Millisecond, Max: time.Second}})
	var attempts atomic.Int32
	var failures atomic.Int32
	synced := make(chan struct{}, 1)
	c := &Controller[string]{
		Queue:   q,
		Workers: 2,
		OnError: func(string, error) { failures.Add(1) },
		Sync: func(ctx context.Context, key string) error {
			n := attempts.Add(1)
			if n < 3 {
				return errors.New("transient")
			}
			if n == 3 {
				return Requeue{After: time.Millisecond}
			}
			select {
			case synced <- struct{}{}:
			default:
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	q.Add("cap/portfolios")
	// Drive the fake timers until the sync succeeds.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-synced:
			goto done
		case <-deadline:
			t.Fatalf("sync never succeeded; attempts=%d", attempts.Load())
		default:
			ft.fireAll()
			time.Sleep(time.Millisecond)
		}
	}
done:
	q.WaitIdle()
	if attempts.Load() != 4 || failures.Load() != 2 {
		t.Fatalf("attempts=%d failures=%d, want 4 and 2 (Requeue is not a failure)", attempts.Load(), failures.Load())
	}
	if q.NumRequeues("cap/portfolios") != 0 {
		t.Fatal("successful sync must Forget the key")
	}
	cancel()
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
}

func TestControllerRecoversPanics(t *testing.T) {
	ft := &fakeTimers{}
	q := New[string](Options{AfterFunc: ft.afterFunc})
	var errs atomic.Int32
	c := &Controller[string]{
		Queue:   q,
		OnError: func(_ string, err error) { errs.Add(1) },
		Sync:    func(ctx context.Context, key string) error { panic("boom") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx) //nolint:errcheck
	q.Add("x")
	q.WaitIdle()
	cancel()
	if errs.Load() != 1 {
		t.Fatalf("panic must be converted to a failed sync, got %d errors", errs.Load())
	}
	if len(ft.delays()) == 0 && q.NumRequeues("x") != 1 {
		t.Fatal("panicking key must be rate-limited for retry")
	}
}

func TestResync(t *testing.T) {
	q := New[string](Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go Resync(ctx, q, 5*time.Millisecond, func(context.Context) ([]string, error) { return []string{"a", "b"}, nil }) //nolint:errcheck
	deadline := time.After(time.Second)
	for q.Len() < 2 {
		select {
		case <-deadline:
			t.Fatal("resync did not enqueue keys")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := Resync(ctx, q, 0, nil); err == nil {
		t.Fatal("non-positive period must be rejected")
	}
}
