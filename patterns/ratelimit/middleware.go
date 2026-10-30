package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// KeyFunc derives the limiter key for a request: principal, client IP, API
// key, or a combination. Return "" to skip limiting for the request.
type KeyFunc func(*http.Request) string

// Middleware enforces l per key and renders RateLimit headers. policy names
// the limit in the headers ("per-user", "per-ip"). failOpen decides what
// happens when the Limiter errors (Redis down): true lets the request through
// and should be paired with an alert; false returns 503.
func Middleware(l Limiter, key KeyFunc, policy string, failOpen bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k == "" {
				next.ServeHTTP(w, r)
				return
			}
			d, err := l.Allow(r.Context(), policy+":"+k)
			if err != nil {
				if failOpen {
					next.ServeHTTP(w, r)
					return
				}
				apierror.Write(w, apierror.Wrap(err, http.StatusServiceUnavailable, apierror.CodeOverloaded, "rate limiter unavailable").WithRetryAfter(1))
				return
			}
			SetHeaders(w.Header(), policy, d)
			if !d.Allowed {
				secs := int(math.Ceil(d.RetryAfter.Seconds()))
				if secs < 1 {
					secs = 1
				}
				apierror.Write(w, apierror.New(http.StatusTooManyRequests, apierror.CodeRateLimited,
					fmt.Sprintf("rate limit %q exceeded", policy)).WithRetryAfter(secs))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SetHeaders writes the IETF structured-field headers for a decision.
func SetHeaders(h http.Header, policy string, d Decision) {
	h.Set("RateLimit", fmt.Sprintf("%q;r=%d;t=%d", policy, d.Remaining, int(math.Ceil(d.ResetAfter.Seconds()))))
	h.Set("RateLimit-Policy", fmt.Sprintf("%q;q=%d;w=%d", policy, d.Limit, int(math.Ceil(d.Window.Seconds()))))
}

// wait is exposed for tests and for callers that prefer delaying over
// rejecting (Traefik's maxDelay behaviour): if the delay is short, sleep.
func wait(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.Context().Done():
		return false
	case <-t.C:
		return true
	}
}
