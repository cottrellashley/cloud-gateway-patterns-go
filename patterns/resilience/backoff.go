// Package resilience holds the three primitives every outbound call composes:
// backoff, retry (with a retry budget) and a circuit breaker.
//
// Sources: grpc-go backoff (base 1s, multiplier 1.6, jitter 0.2, max 120s),
// cenkalti/backoff randomisation, go-retryablehttp's retry policy and
// Retry-After handling, the Azure SDK retry policy (per-try timeout, never
// retry after ctx is done, honour Retry-After but cap it), gobreaker's
// generation-based state machine, and the gRPC retry-throttling token bucket.
package resilience

import (
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Backoff is grpc-go's exponential backoff with proportional jitter.
type Backoff struct {
	Base       time.Duration
	Max        time.Duration
	Multiplier float64
	Jitter     float64 // 0.2 means ±20%
}

// DefaultBackoff is grpc-go's DefaultConfig.
func DefaultBackoff() Backoff {
	return Backoff{Base: time.Second, Max: 120 * time.Second, Multiplier: 1.6, Jitter: 0.2}
}

// Delay returns the wait before attempt number `retries` (0 = first retry).
func (b Backoff) Delay(retries int) time.Duration {
	if retries <= 0 {
		return jitter(b.Base, b.Jitter)
	}
	d, max := float64(b.Base), float64(b.Max)
	for d < max && retries > 0 {
		d *= b.Multiplier
		retries--
	}
	if d > max {
		d = max
	}
	return jitter(time.Duration(d), b.Jitter)
}

func jitter(d time.Duration, j float64) time.Duration {
	if j <= 0 {
		return d
	}
	f := float64(d) * (1 + j*(rand.Float64()*2-1))
	if f < 0 {
		return 0
	}
	if f > math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(f)
}

// ParseRetryAfter parses a Retry-After header (seconds or HTTP-date). The
// second return is false when absent or unparsable.
func ParseRetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}
