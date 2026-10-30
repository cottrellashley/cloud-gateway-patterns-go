// Package middleware is the handler-chain toolkit: composition, panic
// recovery, request IDs, trusted client IP, body limits, deadlines and a
// response writer wrapper that records status and bytes.
//
// Ordering (outermost first) that the gateway uses, derived from the
// Kubernetes DefaultBuildHandlerChain and Traefik/Caddy chains:
//
//	Recover → RequestID → RealIP → AccessLog → Tracing → MaxBytes → Deadline →
//	Authn → Admission/InFlight → RateLimit → Authz → handler
//
// See docs/01-architecture/request-lifecycle-and-middleware-order.md.
package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first one listed is the outermost,
// exactly like chi's chain(): Chain(h, a, b, c) == a(b(c(h))).
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ResponseWriter records status and bytes while forwarding Flush, Hijack and
// Unwrap so that http.ResponseController, SSE and WebSockets keep working
// through the wrapper (the same contract as chi's WrapResponseWriter).
type ResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

// Wrap returns a recording writer. If w is already wrapped it is returned as is.
func Wrap(w http.ResponseWriter) *ResponseWriter {
	if rw, ok := w.(*ResponseWriter); ok {
		return rw
	}
	return &ResponseWriter{ResponseWriter: w}
}

func (w *ResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *ResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Status returns the status written, or 0 if none yet.
func (w *ResponseWriter) Status() int { return w.status }

// BytesWritten returns the body bytes written so far.
func (w *ResponseWriter) BytesWritten() int64 { return w.bytes }

// Written reports whether the header has been sent.
func (w *ResponseWriter) Written() bool { return w.wroteHeader }

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *ResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush supports streaming responses through the wrapper.
func (w *ResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack supports WebSocket upgrades through the wrapper.
func (w *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("middleware: underlying ResponseWriter does not support hijacking")
}

// Recover converts panics into 500 responses and logs the stack. It re-panics
// http.ErrAbortHandler, which net/http uses to abort a response silently (chi
// and Traefik do the same). If the header was already written, nothing more
// is sent; the connection is left to net/http.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := Wrap(w)
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
						panic(rec)
					}
					log.ErrorContext(r.Context(), "panic recovered",
						"panic", rec, "method", r.Method, "path", r.URL.Path,
						"stack", string(debug.Stack()))
					if !rw.Written() {
						apierror.Write(rw, apierror.New(http.StatusInternalServerError, apierror.CodeInternal, "an internal error occurred"))
					}
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyClientIP
)

// RequestID reuses a well-formed incoming ID (so that the front door's ID is
// kept end to end) or mints a 128-bit random one, stores it in the context and
// echoes it in the response header.
func RequestID(header string) Middleware {
	if header == "" {
		header = "X-Request-Id"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(header)
			if !validRequestID(id) {
				id = newID()
			}
			w.Header().Set(header, id)
			ctx := context.WithValue(r.Context(), keyRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func validRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RequestIDFrom returns the request ID stored by RequestID.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// RealIP determines the client IP safely. chi's RealIP trusts the leftmost
// X-Forwarded-For entry, which any client can spoof. This version only trusts
// forwarding headers when the TCP peer is one of the trusted proxy prefixes
// (Caddy's trusted_proxies), and then walks X-Forwarded-For from the right,
// skipping trusted proxies, which is the only spoof-resistant reading.
//
// Behind Azure Front Door, trust the Front Door backend IP ranges and prefer
// the X-Azure-ClientIP header it sets; pass it as preferredHeader.
func RealIP(trusted []netip.Prefix, preferredHeader string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r, trusted, preferredHeader)
			ctx := context.WithValue(r.Context(), keyClientIP, ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func clientIP(r *http.Request, trusted []netip.Prefix, preferred string) string {
	peer := remoteIP(r.RemoteAddr)
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	if preferred != "" {
		if v := strings.TrimSpace(r.Header.Get(preferred)); v != "" {
			if a, err := netip.ParseAddr(stripPort(v)); err == nil {
				return a.String()
			}
		}
	}
	// Walk X-Forwarded-For right to left; the first untrusted hop is the client.
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, p := range strings.Split(v, ",") {
			hops = append(hops, strings.TrimSpace(p))
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(stripPort(hops[i]))
		if err != nil {
			break
		}
		if !isTrusted(a, trusted) {
			return a.String()
		}
	}
	return peer.String()
}

func remoteIP(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func stripPort(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return strings.Trim(s, "[]")
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIPFrom returns the IP stored by RealIP, or "" if RealIP did not run.
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(keyClientIP).(string)
	return ip
}

// MaxBytes caps request bodies; reading past the limit fails the request with
// *http.MaxBytesError, which apierror maps to 413. http.MaxBytesReader also
// closes the connection after the error so the client cannot keep streaming.
func MaxBytes(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Deadline gives every request a context deadline. The effective deadline is
// the earliest of: the client's header (RFC 3339, if allowed), the
// per-route default, and max. Handlers and upstream calls inherit it through
// r.Context(), which is how cancellation propagates to Snowflake, GitHub and
// gRPC calls without any extra plumbing.
//
// If the handler returns after the deadline without writing, a 504 envelope
// is sent. This middleware never writes concurrently with the handler, which
// is the race the Kubernetes timeout filter and http.TimeoutHandler have to
// guard against with a goroutine and a buffered writer; the price is that a
// handler which ignores ctx can still run long. Pair it with per-call ctx use.
func Deadline(header string, def, max time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d := def
			if header != "" {
				if v := r.Header.Get(header); v != "" {
					if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
						if until := time.Until(t); until < d || d == 0 {
							d = until
						}
					}
				}
			}
			if max > 0 && (d <= 0 || d > max) {
				d = max
			}
			if d <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeoutCause(r.Context(), d, errRequestDeadline)
			defer cancel()
			w.Header().Set("X-Request-Deadline", time.Now().Add(d).UTC().Format(time.RFC3339Nano))
			rw := Wrap(w)
			next.ServeHTTP(rw, r.WithContext(ctx))
			if !rw.Written() && errors.Is(context.Cause(ctx), errRequestDeadline) {
				apierror.Write(rw, apierror.New(http.StatusGatewayTimeout, apierror.CodeDeadlineExceeded,
					"the request did not complete before its deadline"))
			}
		})
	}
}

var errRequestDeadline = errors.New("request deadline exceeded")
