package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// RetryAfterError lets a callee say "retry, but not before Delay". The retry
// loop uses Delay instead of the backoff, capped by Policy.MaxDelay, exactly
// as the Azure SDK retry policy treats Retry-After.
type RetryAfterError struct {
	Delay time.Duration
	Err   error
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// Budget is gRPC's retry throttling: a token bucket shared by all calls to
// one dependency. Every failure costs one token, every success refunds
// TokenRatio; retries are permitted only while tokens exceed half of
// MaxTokens. It stops a sick dependency from being hammered by retries from
// thousands of concurrent requests, which per-call MaxAttempts cannot do.
type Budget struct {
	MaxTokens  float64
	TokenRatio float64

	mu     sync.Mutex
	tokens float64
	init   sync.Once
}

func (b *Budget) setup() {
	b.init.Do(func() {
		if b.MaxTokens <= 0 {
			b.MaxTokens = 10
		}
		if b.TokenRatio <= 0 {
			b.TokenRatio = 0.1
		}
		b.tokens = b.MaxTokens
	})
}

// Allow reports whether a retry may proceed.
func (b *Budget) Allow() bool {
	b.setup()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens > b.MaxTokens/2
}

// Failure records a failed attempt.
func (b *Budget) Failure() {
	b.setup()
	b.mu.Lock()
	b.tokens = max(0, b.tokens-1)
	b.mu.Unlock()
}

// Success records a successful attempt.
func (b *Budget) Success() {
	b.setup()
	b.mu.Lock()
	b.tokens = min(b.MaxTokens, b.tokens+b.TokenRatio)
	b.mu.Unlock()
}

// Policy configures Retry.
type Policy struct {
	// MaxAttempts includes the first call. 1 disables retries.
	MaxAttempts int
	Backoff     Backoff
	// MaxDelay caps any wait, including one requested by RetryAfterError. A
	// requested delay above the cap aborts the retry (Azure SDK behaviour).
	MaxDelay time.Duration
	// PerAttemptTimeout bounds each attempt; the parent ctx bounds the whole.
	PerAttemptTimeout time.Duration
	// Retryable decides whether an error is worth retrying. nil means every
	// error except context errors is retryable.
	Retryable func(error) bool
	// Budget, when set, is consulted before every retry.
	Budget *Budget
}

// DefaultPolicy is three attempts with gRPC backoff.
func DefaultPolicy() Policy {
	return Policy{MaxAttempts: 3, Backoff: DefaultBackoff(), MaxDelay: 30 * time.Second}
}

// Retry runs fn until it succeeds, the policy is exhausted, or ctx ends. It
// never retries after ctx is done (go-retryablehttp DefaultRetryPolicy, Azure
// SDK), and it reuses the same ctx values so idempotency keys and trace
// context propagate unchanged to every attempt.
func Retry[T any](ctx context.Context, p Policy, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		actx := ctx
		var cancel context.CancelFunc = func() {}
		if p.PerAttemptTimeout > 0 {
			actx, cancel = context.WithTimeout(ctx, p.PerAttemptTimeout)
		}
		v, err := fn(actx)
		cancel()
		if err == nil {
			if p.Budget != nil {
				p.Budget.Success()
			}
			return v, nil
		}
		lastErr = err
		if p.Budget != nil {
			p.Budget.Failure()
		}
		if ctx.Err() != nil {
			return zero, errors.Join(err, ctx.Err())
		}
		if attempt == p.MaxAttempts-1 {
			break
		}
		if p.Retryable != nil && !p.Retryable(err) {
			break
		}
		if p.Budget != nil && !p.Budget.Allow() {
			break
		}
		delay := p.Backoff.Delay(attempt)
		var ra *RetryAfterError
		if errors.As(err, &ra) {
			if p.MaxDelay > 0 && ra.Delay > p.MaxDelay {
				break
			}
			delay = ra.Delay
		}
		if p.MaxDelay > 0 && delay > p.MaxDelay {
			delay = p.MaxDelay
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return zero, errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
	return zero, lastErr
}
