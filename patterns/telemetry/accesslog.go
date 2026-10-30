package telemetry

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/auth"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/middleware"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/proxy"
)

// AccessLogOptions tune the access log.
type AccessLogOptions struct {
	// Skip returns true for requests not to log (health probes).
	Skip func(*http.Request) bool
	// LogQuery includes the raw query string; off by default because query
	// strings often carry identifiers and filters that are PII.
	LogQuery bool
}

// AccessLog writes one structured line per request when it completes. Fields
// (names chosen to match OpenTelemetry semantic conventions where they exist):
//
//	http.method, http.route (if set by the router via RouteFrom), url.path,
//	http.status, http.bytes, duration_ms, request_id, trace_id (from the
//	context logger), client.ip, user_agent, principal, upstream, protocol
//
// Level is info for < 500, error for >= 500 so dashboards can alert on the
// log stream alone. Never log Authorization, cookies or bodies.
func AccessLog(log *slog.Logger, opts AccessLogOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.Skip != nil && opts.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rw := middleware.Wrap(w)
			ctx := r.Context()
			if rid := middleware.RequestIDFrom(ctx); rid != "" {
				ctx = With(ctx, "request_id", rid)
			}
			r = r.WithContext(ctx)
			next.ServeHTTP(rw, r)

			status := rw.Status()
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []any{
				"http.method", r.Method,
				"url.path", r.URL.Path,
				"http.status", status,
				"http.bytes", rw.BytesWritten(),
				"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
				"protocol", r.Proto,
				"user_agent", r.UserAgent(),
			}
			if opts.LogQuery && r.URL.RawQuery != "" {
				attrs = append(attrs, "url.query", r.URL.RawQuery)
			}
			if ip := middleware.ClientIPFrom(ctx); ip != "" {
				attrs = append(attrs, "client.ip", ip)
			}
			if p := auth.PrincipalFrom(ctx); p != nil {
				attrs = append(attrs, "principal", p.Subject, "tenant", p.TenantID)
			}
			if up := proxy.UpstreamFrom(ctx); up != nil {
				attrs = append(attrs, "upstream", up.Name)
			}
			if route := RouteFrom(ctx); route != "" {
				attrs = append(attrs, "http.route", route)
			}
			l := FromContext(ctx)
			if log != nil && l == slog.Default() {
				l = log
			}
			if status >= 500 {
				l.ErrorContext(ctx, "request", attrs...)
			} else {
				l.InfoContext(ctx, "request", attrs...)
			}
		})
	}
}

// Route labelling: routers set the matched pattern so metrics and logs use
// "/users/{id}" instead of "/users/42" (bounded cardinality).

type routeKey struct{}

// WithRoute stores the matched route pattern.
func WithRoute(r *http.Request, pattern string) *http.Request {
	return r.WithContext(contextWithRoute(r, pattern))
}

func contextWithRoute(r *http.Request, pattern string) contextT {
	return contextT{r.Context(), pattern}
}

// RouteFrom returns the pattern set by WithRoute, or r.Pattern when using the
// Go 1.22+ ServeMux (which records it on the request).
func RouteFrom(ctx contextI) string {
	if v, ok := ctx.Value(routeKey{}).(string); ok {
		return v
	}
	return ""
}
