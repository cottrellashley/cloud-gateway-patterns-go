package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// TraceParent is a W3C Trace Context traceparent header:
//
//	version-traceid-spanid-flags  e.g. 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
//
// The gateway always has a trace ID even when no tracer is configured, so
// logs, audit records and upstream calls correlate end to end.
type TraceParent struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

// ErrInvalidTraceParent is returned for malformed headers.
var ErrInvalidTraceParent = errors.New("telemetry: invalid traceparent")

// ParseTraceParent parses the header per the spec's version-00 rules.
func ParseTraceParent(s string) (TraceParent, error) {
	var tp TraceParent
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return tp, ErrInvalidTraceParent
	}
	if parts[0] == "ff" {
		return tp, ErrInvalidTraceParent
	}
	if _, err := hex.Decode(tp.TraceID[:], []byte(parts[1])); err != nil {
		return tp, ErrInvalidTraceParent
	}
	if _, err := hex.Decode(tp.SpanID[:], []byte(parts[2])); err != nil {
		return tp, ErrInvalidTraceParent
	}
	var flags [1]byte
	if _, err := hex.Decode(flags[:], []byte(parts[3])); err != nil {
		return tp, ErrInvalidTraceParent
	}
	if tp.TraceID == [16]byte{} || tp.SpanID == [8]byte{} {
		return tp, ErrInvalidTraceParent
	}
	tp.Sampled = flags[0]&1 == 1
	return tp, nil
}

// NewTraceParent starts a new trace.
func NewTraceParent(sampled bool) TraceParent {
	var tp TraceParent
	_, _ = rand.Read(tp.TraceID[:])
	_, _ = rand.Read(tp.SpanID[:])
	tp.Sampled = sampled
	return tp
}

// Child returns a traceparent with the same trace ID and a fresh span ID, for
// outbound calls.
func (tp TraceParent) Child() TraceParent {
	c := tp
	_, _ = rand.Read(c.SpanID[:])
	return c
}

// String renders the header value.
func (tp TraceParent) String() string {
	flags := "00"
	if tp.Sampled {
		flags = "01"
	}
	return "00-" + hex.EncodeToString(tp.TraceID[:]) + "-" + hex.EncodeToString(tp.SpanID[:]) + "-" + flags
}

// TraceIDString is the 32-hex trace ID for log fields.
func (tp TraceParent) TraceIDString() string { return hex.EncodeToString(tp.TraceID[:]) }

type traceKey struct{}

// TraceFrom returns the trace stored by Propagate, if any.
func TraceFrom(ctx context.Context) (TraceParent, bool) {
	tp, ok := ctx.Value(traceKey{}).(TraceParent)
	return tp, ok
}

// Propagate extracts an incoming traceparent (the front door's) or starts a
// new trace, stores it in the context and tags the context logger with
// trace_id and span_id. When trustIncoming is false (public endpoint), an
// incoming trace is still recorded as parent but a new trace is started — the
// otelhttp WithPublicEndpoint behaviour the Kubernetes apiserver uses for
// non-privileged callers.
func Propagate(trustIncoming bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tp, err := ParseTraceParent(r.Header.Get("traceparent"))
			if err != nil || !trustIncoming {
				tp = NewTraceParent(true)
			} else {
				tp = tp.Child()
			}
			ctx := context.WithValue(r.Context(), traceKey{}, tp)
			ctx = With(ctx, "trace_id", tp.TraceIDString(), "span_id", hex.EncodeToString(tp.SpanID[:]))
			w.Header().Set("X-Trace-Id", tp.TraceIDString())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Inject writes the outbound traceparent for a child call.
func Inject(ctx context.Context, h http.Header) {
	if tp, ok := TraceFrom(ctx); ok {
		h.Set("traceparent", tp.Child().String())
	}
}
