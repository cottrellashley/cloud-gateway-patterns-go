// Package proxy builds the reverse proxy a gateway uses to forward to
// services. It is a thin configuration of net/http/httputil.ReverseProxy,
// which already does the hard parts correctly (hop-by-hop header removal,
// Upgrade handling, trailers, flushing SSE immediately). What this package
// adds is what every production proxy adds on top:
//
//   - a Transport with explicit dial, TLS, idle and buffer settings
//     (values from Caddy httptransport.go and Traefik service/transport.go),
//   - X-Forwarded-* handling that only trusts the front door,
//   - per-request upstream selection with in-flight accounting and passive
//     failure recording (lb package),
//   - an error handler that writes the gateway's error envelope and records
//     the upstream for the access log,
//   - a retry rule that is safe: idempotent method, body not yet consumed or
//     buffered under a limit, no bytes written to the client yet
//     (Traefik retry.go, Skipper proxy.go retryable()).
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/apierror"
	"github.com/cottrellashley/cloud-gateway-patterns-go/patterns/lb"
)

// DefaultTransport returns a production transport. The numbers are the ones
// Caddy and Traefik ship with:
//
//	DialTimeout            3s   (Caddy)       KeepAlive probes  30s (both)
//	MaxIdleConnsPerHost    32   (Caddy, #2805) IdleConnTimeout  90s (Traefik)
//	TLSHandshakeTimeout    10s  (Traefik)     Read/WriteBuffer  64 KiB (Traefik)
//	ExpectContinueTimeout  1s   (stdlib default transport)
//	ResponseHeaderTimeout  0    (set per route via the request deadline)
func DefaultTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 nil, // never use environment proxies for upstream calls
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ReadBufferSize:        64 << 10,
		WriteBufferSize:       64 << 10,
	}
}

// Options configures the proxy handler.
type Options struct {
	// Balancer picks the upstream per request. Required.
	Balancer lb.Balancer
	// Transport defaults to DefaultTransport().
	Transport http.RoundTripper
	// TrustForwarded controls whether incoming X-Forwarded-* headers are kept
	// (true only when the TCP peer is a trusted front door; combine with
	// middleware.RealIP). When false they are replaced with this hop's view.
	TrustForwarded bool
	// FlushInterval for non-SSE streaming bodies. httputil flushes SSE and
	// unknown-length bodies immediately regardless (negative interval).
	FlushInterval time.Duration
	// FailDuration is how long a passive failure keeps an upstream out of
	// rotation. 0 disables passive health.
	FailDuration time.Duration
	// MaxRetryBodyBytes bounds the body buffered to allow a retry; 0 disables
	// retries for requests with a body.
	MaxRetryBodyBytes int64
	// Attempts is the maximum number of upstream attempts (1 = no retry).
	Attempts int
	// Hop names this gateway in error envelopes and access logs.
	Hop string
	Log *slog.Logger
}

type ctxKey int

const keyUpstream ctxKey = iota

// UpstreamFrom returns the upstream used for the request, for access logs.
func UpstreamFrom(ctx context.Context) *lb.Upstream {
	u, _ := ctx.Value(keyUpstream).(*lb.Upstream)
	return u
}

// New returns the proxy handler.
func New(o Options) http.Handler {
	if o.Transport == nil {
		o.Transport = DefaultTransport()
	}
	if o.Attempts < 1 {
		o.Attempts = 1
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	p := &handler{o: o}
	p.rp = &httputil.ReverseProxy{
		Transport:     o.Transport,
		FlushInterval: o.FlushInterval,
		Rewrite:       p.rewrite,
		ErrorHandler:  p.errorHandler,
		ErrorLog:      slog.NewLogLogger(o.Log.Handler(), slog.LevelWarn),
	}
	return p
}

type handler struct {
	o  Options
	rp *httputil.ReverseProxy
}

type attemptKey struct{}

type attempt struct {
	upstream *lb.Upstream
	err      error
}

func (h *handler) rewrite(pr *httputil.ProxyRequest) {
	at := pr.In.Context().Value(attemptKey{}).(*attempt)
	pr.SetURL(at.upstream.URL)
	pr.Out.Host = pr.In.Host // keep the original Host for virtual-hosted services
	if !h.o.TrustForwarded {
		// Rewrite cleared the incoming X-Forwarded-* already; set ours.
		pr.SetXForwarded()
	} else {
		// Keep the front door's values and append this hop.
		pr.Out.Header["X-Forwarded-For"] = pr.In.Header["X-Forwarded-For"]
		pr.Out.Header["X-Forwarded-Host"] = pr.In.Header["X-Forwarded-Host"]
		pr.Out.Header["X-Forwarded-Proto"] = pr.In.Header["X-Forwarded-Proto"]
		pr.SetXForwarded()
	}
}

func (h *handler) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	at := r.Context().Value(attemptKey{}).(*attempt)
	at.err = err
	// Only the final attempt writes; retries are decided in ServeHTTP.
}

// ServeHTTP picks an upstream, forwards, and retries when safe.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body []byte
	retryable := isIdempotent(r.Method)
	if retryable && r.Body != nil && r.Body != http.NoBody && h.o.Attempts > 1 {
		if h.o.MaxRetryBodyBytes <= 0 {
			retryable = false
		} else {
			b, err := io.ReadAll(io.LimitReader(r.Body, h.o.MaxRetryBodyBytes+1))
			if err != nil {
				apierror.Write(w, apierror.Wrap(err, http.StatusBadRequest, apierror.CodeMalformedRequest, "could not read request body"))
				return
			}
			if int64(len(b)) > h.o.MaxRetryBodyBytes {
				// Too large to buffer: forward the prefix + the rest once, no retry.
				retryable = false
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), r.Body))
			} else {
				body = b
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
	}

	rec := &recorder{ResponseWriter: w}
	var lastErr error
	attempts := h.o.Attempts
	if !retryable {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		up, err := h.o.Balancer.Pick()
		if err != nil {
			apierror.Write(w, apierror.Wrap(err, http.StatusServiceUnavailable, apierror.CodeUpstreamUnavailable, "no healthy upstream").WithRetryAfter(1))
			return
		}
		if body != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		at := &attempt{upstream: up}
		ctx := context.WithValue(r.Context(), attemptKey{}, at)
		ctx = context.WithValue(ctx, keyUpstream, up)
		req := r.WithContext(ctx)

		up.Acquire()
		h.rp.ServeHTTP(rec, req)
		up.Release()

		if at.err == nil {
			return
		}
		lastErr = at.err
		if h.o.FailDuration > 0 && !errors.Is(at.err, context.Canceled) {
			up.RecordFailure(h.o.FailDuration)
		}
		// Never retry once bytes reached the client, on client cancel, or
		// on deadline: the client's budget is spent.
		if rec.wrote || errors.Is(at.err, context.Canceled) || errors.Is(at.err, context.DeadlineExceeded) || r.Context().Err() != nil {
			break
		}
		h.o.Log.WarnContext(r.Context(), "upstream attempt failed", "upstream", up.Name, "attempt", i+1, "err", at.err)
	}
	if rec.wrote {
		return
	}
	e := apierror.From(lastErr)
	if e.Code == apierror.CodeInternal {
		e = apierror.Wrap(lastErr, http.StatusBadGateway, apierror.CodeUpstreamError, "upstream request failed")
	}
	e.Hop = h.o.Hop
	apierror.Write(rec, e)
}

func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
		return true
	}
	return false
}

// recorder notes whether any bytes/headers reached the client so that a retry
// is never attempted after a partial response (Traefik's responseWriter.written).
type recorder struct {
	http.ResponseWriter
	wrote bool
}

func (r *recorder) WriteHeader(code int) {
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	r.wrote = true
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ParseURL is a helper for config loading.
func ParseURL(s string) (*url.URL, error) { return url.Parse(s) }
