// Package health serves /livez and /readyz in the Kubernetes apiserver style:
// a list of named checks, a plain "ok" body on success, a per-check report on
// failure with the reason withheld from the public body (it goes to the log),
// ?verbose for the full report, and ?exclude=name to skip a check.
//
// Rules the gateway follows (from k8s healthz.go, Caddy/Traefik health checks
// and the gRPC health service):
//   - /livez must never depend on downstreams; it answers "is this process
//     wedged". /readyz may depend on downstreams and on the lifecycle
//     readiness flag.
//   - each check runs with a timeout; a hung dependency must not hang probes.
//   - probes are cheap and unauthenticated; never leak dependency details.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lifecycle"
)

// Check is one named probe.
type Check interface {
	Name() string
	Check(ctx context.Context) error
}

type namedCheck struct {
	name string
	fn   func(ctx context.Context) error
}

func (c namedCheck) Name() string                    { return c.name }
func (c namedCheck) Check(ctx context.Context) error { return c.fn(ctx) }

// NamedCheck wraps a function.
func NamedCheck(name string, fn func(ctx context.Context) error) Check {
	return namedCheck{name: name, fn: fn}
}

// Ping always succeeds; it proves the HTTP stack answers.
var Ping Check = NamedCheck("ping", func(context.Context) error { return nil })

// ReadinessCheck fails until the lifecycle flag is set, and again once it is
// cleared during shutdown.
func ReadinessCheck(r *lifecycle.Readiness) Check {
	return NamedCheck("ready", func(context.Context) error {
		if !r.Ready() {
			return fmt.Errorf("not ready")
		}
		return nil
	})
}

// WithTimeout bounds a check.
func WithTimeout(c Check, d time.Duration) Check {
	return NamedCheck(c.Name(), func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.Check(ctx) }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", c.Name(), ctx.Err())
		}
	})
}

// Handler returns the probe handler for a set of checks. name appears in the
// body ("readyz check passed").
func Handler(name string, log *slog.Logger, checks ...Check) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		excluded := map[string]bool{}
		for _, v := range r.URL.Query()["exclude"] {
			for _, n := range strings.Split(v, ",") {
				excluded[strings.TrimSpace(n)] = true
			}
		}
		var (
			mu       sync.Mutex
			report   strings.Builder
			failed   []string
			detailed strings.Builder
			wg       sync.WaitGroup
		)
		results := make([]string, len(checks))
		errs := make([]error, len(checks))
		for i, c := range checks {
			if excluded[c.Name()] {
				results[i] = fmt.Sprintf("[+]%s excluded: ok\n", c.Name())
				continue
			}
			wg.Add(1)
			go func(i int, c Check) {
				defer wg.Done()
				errs[i] = c.Check(r.Context())
			}(i, c)
		}
		wg.Wait()
		for i, c := range checks {
			if results[i] != "" {
				report.WriteString(results[i])
				continue
			}
			if errs[i] != nil {
				mu.Lock()
				failed = append(failed, c.Name())
				fmt.Fprintf(&report, "[-]%s failed: reason withheld\n", c.Name())
				fmt.Fprintf(&detailed, "[-]%s failed: %v\n", c.Name(), errs[i])
				mu.Unlock()
			} else {
				fmt.Fprintf(&report, "[+]%s ok\n", c.Name())
			}
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if len(failed) > 0 {
			log.WarnContext(r.Context(), "health check failed", "probe", name, "failed", failed, "detail", detailed.String())
			http.Error(w, report.String()+name+" check failed", http.StatusServiceUnavailable)
			return
		}
		if _, verbose := r.URL.Query()["verbose"]; !verbose {
			_, _ = w.Write([]byte("ok"))
			return
		}
		_, _ = w.Write([]byte(report.String() + name + " check passed\n"))
	})
}
