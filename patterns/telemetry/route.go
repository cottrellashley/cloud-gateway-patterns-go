package telemetry

import (
	"context"
	"net/http"
	"time"
)

// contextI is context.Context (alias kept local to avoid an import cycle in
// generated docs); contextT is a minimal context carrying the route.
type contextI = context.Context

type contextT struct {
	context.Context
	route string
}

func (c contextT) Value(key any) any {
	if _, ok := key.(routeKey); ok {
		return c.route
	}
	return c.Context.Value(key)
}

// Recorder receives request metrics. Implement it with Prometheus
// (promhttp-style histogram with method, route, status labels) or OTel
// metrics; the gateway only depends on this interface.
type Recorder interface {
	ObserveRequest(method, route string, status int, duration time.Duration, reqBytes, respBytes int64)
	InFlight(delta int)
}

// NoopRecorder discards metrics.
type NoopRecorder struct{}

func (NoopRecorder) ObserveRequest(string, string, int, time.Duration, int64, int64) {}
func (NoopRecorder) InFlight(int)                                                    {}

// RequestMetrics records RED metrics per request. The route label comes from
// RouteFrom (set by the router) or r.Pattern, never from the raw path, which
// keeps label cardinality bounded.
func RequestMetrics(rec Recorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec.InFlight(1)
			defer rec.InFlight(-1)
			start := time.Now()
			rw := wrapWriter(w)
			next.ServeHTTP(rw, r)
			route := RouteFrom(r.Context())
			if route == "" {
				route = r.Pattern
			}
			if route == "" {
				route = "unmatched"
			}
			status := rw.Status()
			if status == 0 {
				status = http.StatusOK
			}
			rec.ObserveRequest(r.Method, route, status, time.Since(start), r.ContentLength, rw.BytesWritten())
		})
	}
}
