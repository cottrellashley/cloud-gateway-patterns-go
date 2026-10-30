# Reverse proxy

**Pattern:** `net/http/httputil.ReverseProxy` configured with a production transport, a trusted-edge `X-Forwarded-*` policy, per-request upstream selection, passive health, a safe retry rule and the gateway's error envelope. Implemented in `patterns/proxy`.

## Why the standard library proxy

`httputil.ReverseProxy` already implements the parts that are easy to get wrong:

* **Hop-by-hop headers** are stripped on both legs (`reverseproxy.go` go1.25.14):
  ```go
  var hopHeaders = []string{
      "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
      "Te", "Trailer", "Transfer-Encoding", "Upgrade",
  }
  ```
  Caddy (`reverseproxy.go` lines ~1012-1021) and Skipper (`proxy.go` `hopHeaders` map, `cloneHeaderExcluding`) maintain the same list.
* **Upgrade** (WebSocket) requests are detected and tunnelled.
* **Streaming**: `flushInterval` returns `-1` ("flush immediately") for `text/event-stream` and for responses with unknown length:
  ```go
  func (p *ReverseProxy) flushInterval(res *http.Response) time.Duration {
      resCT := res.Header.Get("Content-Type")
      if baseCT, _, _ := mime.ParseMediaType(resCT); baseCT == "text/event-stream" { return -1 }
      if res.ContentLength == -1 { return -1 }
      return p.FlushInterval
  }
  ```
  Caddy's `streaming.go` has the identical rule.
* **Rewrite vs Director**: use `Rewrite`; it clears incoming `X-Forwarded-*` first so the proxy decides what to trust ("The Rewrite func may use SetXForwarded to set new values"), and the request body is set to `nil` when empty "for http.Transport retries" (issue 16036).

## Transport

Defaults from the two most deployed Go proxies, in `patterns/proxy.DefaultTransport`:

| Field | Caddy `httptransport.go` @eb04ed5 | Traefik `service/transport.go` @38d53dd | Chosen |
| --- | --- | --- | --- |
| Dial timeout | `3 * time.Second` | configurable | 3s |
| KeepAlive probe | `30 * time.Second` | `30 * time.Second` | 30s |
| MaxIdleConnsPerHost | `32 // seems about optimal, see #2805` | configurable | 32 |
| IdleConnTimeout | `2 * time.Minute` | `90 * time.Second` | 90s |
| TLSHandshakeTimeout | configurable | `10 * time.Second` | 10s |
| Read/WriteBufferSize | configurable | `64 * 1024` | 64 KiB |
| ForceAttemptHTTP2 | yes | yes | yes |
| ResponseHeaderTimeout | configurable | configurable | 0; the request deadline bounds it |

`Proxy: nil` — never let `HTTP_PROXY` environment variables redirect upstream calls.

## X-Forwarded-* policy

Caddy only honours incoming forwarding headers when the client is in `trusted_proxies`, and otherwise deletes them before setting its own (`reverseproxy.go` ~1086-1169). `patterns/proxy.Options.TrustForwarded` is the same switch; combine it with `middleware.RealIP(trustedCIDRs, "X-Azure-ClientIP")` so the access log and the forwarded chain agree.

Keep the original `Host`: `pr.Out.Host = pr.In.Host` (services behind virtual hosting need it; Caddy's `reverse_proxy` keeps it unless `header_up Host` says otherwise).

## Upstream selection, in-flight accounting, passive health

Caddy's rule for a usable backend (`hosts.go` @eb04ed5):

```go
func (u *Upstream) Available() bool { return u.Healthy() && !u.Full() }
func (u *Upstream) Healthy() bool {
    healthy := u.healthy()
    if healthy && u.healthCheckPolicy != nil { healthy = u.Host.Fails() < u.healthCheckPolicy.MaxFails }
    if healthy && u.cb != nil { healthy = u.cb.OK() }
    return healthy
}
func (u *Upstream) Full() bool { return u.MaxRequests > 0 && u.Host.NumRequests() >= u.MaxRequests }
```

and its passive check counts a failure immediately and forgets it after `fail_duration` in a goroutine with a timer. `patterns/lb.Upstream.RecordFailure` does the same with `time.AfterFunc`. The proxy records a failure for every transport error except client cancellation.

## Retry rule

Both Traefik and Skipper refuse to retry in the cases that corrupt state:

* Traefik `retry.go` @38d53dd: `isIdempotent := req.Method != http.MethodPost && req.Method != http.MethodPatch && req.Method != "LOCK"`; the body is read into a reusable request up to `maxRequestBodyBytes` (`mirror.NewReusableRequest`) and if it is too large "disable the HTTP retry"; attempts end "as soon as the client received something or when the request is hijacked" (`retryResponseWriter.written || hijacked`).
* Skipper `proxy.go` @2237f1e:
  ```go
  func retryable(ctx *context, perr *proxyError) bool {
      req := ctx.Request()
      return perr.code != 499 && perr.DialError() &&
          ctx.route.BackendType == eskip.LBBackend &&
          req != nil && (req.Body == nil || req.Body == http.NoBody)
  }
  ```
  — dial errors only, no body, and never when the client cancelled (499).

`patterns/proxy` encodes the union: idempotent method, body either absent or buffered under `MaxRetryBodyBytes`, no bytes written to the client, not cancelled, not past the deadline, and the dead upstream is taken out of rotation for `FailDuration` between attempts.

## Errors

The default `httputil` error handler writes a bare `502`. The gateway's `ErrorHandler` records the attempt error, and `ServeHTTP` writes the envelope once the retry decision is made: `UpstreamUnavailable` (503, `Retry-After: 1`) when no upstream is available, `DeadlineExceeded` (504) on context deadline, `UpstreamError` (502) otherwise. `apierror.StatusClientClosedRequest` (499) is recorded for cancelled clients so logs distinguish them from failures (nginx/Traefik convention).

## What the Kubernetes aggregator adds

The aggregator proxy is a specialised `ReverseProxy` that **adds identity headers for the upstream** (`X-Remote-User`, `X-Remote-Group`, `X-Remote-Extra-*` via `transport.NewAuthProxyRoundTripper`) and the extension server trusts them only over mTLS with a known client CA. If your services need the principal, do the same: a signed context (JWT minted by the gateway, `patterns/auth` style) or mTLS + headers — never the client's bearer token, which the authentication filter deletes (`req.Header.Del("Authorization")`).

## Checklist

- [ ] `Rewrite`, not `Director`; `SetURL` + `SetXForwarded` after deciding trust.
- [ ] Transport values above; `Proxy: nil`.
- [ ] Balancer with `MaxFails` and `FailDuration` set together.
- [ ] Retries only for idempotent methods with replayable bodies and nothing written.
- [ ] Envelope on every proxy error; upstream name in the access log via `proxy.UpstreamFrom`.
- [ ] Inbound `Authorization` never forwarded; services get a gateway-signed context.
