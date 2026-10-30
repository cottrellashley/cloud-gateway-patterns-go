package resilience

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestBackoffGrowsAndCaps(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Multiplier: 2, Jitter: 0}
	if b.Delay(0) != 100*time.Millisecond || b.Delay(1) != 200*time.Millisecond || b.Delay(2) != 400*time.Millisecond {
		t.Fatalf("delays %v %v %v", b.Delay(0), b.Delay(1), b.Delay(2))
	}
	if b.Delay(10) != time.Second {
		t.Fatalf("cap %v", b.Delay(10))
	}
	j := Backoff{Base: time.Second, Max: time.Second, Multiplier: 1, Jitter: 0.2}
	for i := 0; i < 100; i++ {
		d := j.Delay(0)
		if d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("jitter out of range: %v", d)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	if d, ok := ParseRetryAfter(h, time.Now()); !ok || d != 7*time.Second {
		t.Fatalf("seconds: %v %v", d, ok)
	}
	now := time.Now()
	h.Set("Retry-After", now.Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d, ok := ParseRetryAfter(h, now); !ok || d < 29*time.Second || d > 31*time.Second {
		t.Fatalf("date: %v %v", d, ok)
	}
}

func TestRetryStopsOnNonRetryableAndContext(t *testing.T) {
	calls := 0
	p := Policy{MaxAttempts: 5, Backoff: Backoff{Base: time.Millisecond, Max: time.Millisecond, Multiplier: 1},
		Retryable: func(err error) bool { return !errors.Is(err, errFatal) }}
	_, err := Retry(context.Background(), p, func(context.Context) (int, error) { calls++; return 0, errFatal })
	if calls != 1 || !errors.Is(err, errFatal) {
		t.Fatalf("calls %d err %v", calls, err)
	}

	calls = 0
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Retry(ctx, p, func(context.Context) (int, error) { calls++; cancel(); return 0, errors.New("transient") })
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls %d err %v", calls, err)
	}
}

var errFatal = errors.New("fatal")

func TestRetrySucceedsEventuallyAndHonoursRetryAfter(t *testing.T) {
	calls := 0
	p := Policy{MaxAttempts: 4, Backoff: Backoff{Base: time.Hour, Max: time.Hour, Multiplier: 1}, MaxDelay: 50 * time.Millisecond}
	start := time.Now()
	v, err := Retry(context.Background(), p, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", &RetryAfterError{Delay: 5 * time.Millisecond, Err: errors.New("429")}
		}
		return "ok", nil
	})
	if err != nil || v != "ok" || calls != 3 {
		t.Fatalf("v=%q err=%v calls=%d", v, err, calls)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("RetryAfter not honoured (backoff of 1h would have been used)")
	}
}

func TestBudgetStopsRetryStorm(t *testing.T) {
	b := &Budget{MaxTokens: 4, TokenRatio: 0.1}
	p := Policy{MaxAttempts: 10, Backoff: Backoff{Base: time.Microsecond, Max: time.Microsecond, Multiplier: 1}, Budget: b}
	calls := 0
	_, _ = Retry(context.Background(), p, func(context.Context) (int, error) { calls++; return 0, errors.New("down") })
	// tokens: 4 -> after 2 failures = 2, not > 2, so the third attempt is refused.
	if calls != 2 {
		t.Fatalf("budget did not stop retries: %d calls", calls)
	}
}

func TestBreakerStateMachine(t *testing.T) {
	now := time.Unix(0, 0)
	var transitions []string
	b := NewBreaker(BreakerSettings{Name: "db", MaxRequests: 2, Timeout: 10 * time.Second,
		ReadyToTrip:   func(c Counts) bool { return c.ConsecutiveFailures >= 3 },
		OnStateChange: func(n string, from, to State) { transitions = append(transitions, from.String()+"->"+to.String()) }})
	b.now = func() time.Time { return now }
	fail := func() error { return errors.New("x") }
	ok := func() error { return nil }

	for i := 0; i < 3; i++ {
		_ = b.Execute(fail)
	}
	if b.State() != Open {
		t.Fatalf("state %v", b.State())
	}
	if err := b.Execute(ok); !errors.Is(err, ErrOpen) {
		t.Fatalf("open breaker must reject: %v", err)
	}
	now = now.Add(11 * time.Second)
	if b.State() != HalfOpen {
		t.Fatalf("state %v", b.State())
	}
	_ = b.Execute(ok)
	_ = b.Execute(ok)
	if b.State() != Closed {
		t.Fatalf("state %v after trial successes", b.State())
	}
	want := "closed->open,open->half-open,half-open->closed"
	got := ""
	for i, s := range transitions {
		if i > 0 {
			got += ","
		}
		got += s
	}
	if got != want {
		t.Fatalf("transitions %s", got)
	}
}

func TestDoGeneric(t *testing.T) {
	b := NewBreaker(BreakerSettings{})
	v, err := Do(b, func() (int, error) { return 42, nil })
	if err != nil || v != 42 {
		t.Fatal(v, err)
	}
}
