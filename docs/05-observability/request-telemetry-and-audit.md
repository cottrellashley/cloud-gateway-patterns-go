# Request telemetry and audit

**Pattern:** one access-log line per request with a fixed field set, written after the handler returns; a separate append-only audit stream for security decisions with the inputs needed to replay them; request IDs reused from the edge; nothing sensitive in either. Implemented in `patterns/telemetry.AccessLog` and the audit shape below.

## Access log fields

Kubernetes `httplog` (`server/httplog/httplog.go` @ba832c1, `respLogger.Log`) records:

```go
keysAndValues := []interface{}{
    "verb", verb, "URI", rl.req.RequestURI, "latency", latency,
    "userAgent", rl.userAgent, "audit-ID", auditID, "srcIP", rl.req.RemoteAddr,
}
... "resp", rl.status  ... "statusStack" (on error) ... "addedInfo"
```

chi's `DefaultLogFormatter` prints `[reqID] "METHOD scheme://host/path proto" from clientIP` then status, bytes and elapsed; Traefik's access log adds `ServiceName`, `ServiceURL`, `RetryAttempts`, `Overhead` (gateway time minus upstream time) and `OriginDuration`.

The gateway's line (JSON, from `patterns/telemetry.AccessLog`):

| Field | Source |
| --- | --- |
| `ts`, `level`, `msg="request"` | logger |
| `request_id` | `middleware.RequestID` (edge's ID reused when well-formed) |
| `trace_id`, `span_id` | `telemetry.Propagate` |
| `http.method`, `http.route`, `url.path`, `protocol` | request / `RouteFrom` |
| `http.status`, `http.bytes`, `duration_ms` | wrapped writer |
| `client.ip` | `middleware.RealIP` |
| `user_agent` | request |
| `principal`, `tenant` | `auth.PrincipalFrom` |
| `upstream` | `proxy.UpstreamFrom` |
| `url.query` | only with `LogQuery: true` |

Level: `info` below 500, `error` at 500+. Health probes are skipped (`Skip`). A `499` is logged like any status so cancelled clients are visible.

What is deliberately absent: headers, bodies, tokens, cookies, query strings by default. Add `upstream_ms` and `retry_attempts` when the proxy exposes them (Traefik logs both).

## Request IDs

`middleware.RequestID` keeps an incoming `X-Request-Id` when it is 1–128 characters of `[A-Za-z0-9._:-]`, else mints 128 random bits. Azure Front Door sets `X-Azure-Ref`; Application Gateway `X-Request-Id`? — configure the header name to the edge's and echo it in every response and error envelope so support tickets carry one handle through edge, gateway and services. chi's `RequestID` uses a hostname+counter prefix; random is simpler and unguessable.

## Audit

Audit is not logging. It is an append-only record of who did what, with the decision inputs, retained longer and read by different people. The Kubernetes apiserver runs it as its own backend started before serving ("Start the audit backend before any request comes in") and stopped only after in-flight requests drain; `WithAuditInit` is the outermost filter so every request has an audit ID; failed authentication is audited too (`WithFailedAuthenticationAudit`).

Record shape (JSON, one per decision point):

```json
{
  "schema": "gateway.audit/1",
  "id": "au_01J9…",
  "prevHash": "sha256:…",
  "at": "2026-10-30T10:00:00Z",
  "gateway": "gw-weu-3",
  "event": "invoke.decision",
  "request_id": "fd-…", "trace_id": "4bf9…",
  "principal": { "sub": "ashley@contoso.com", "tenant": "t1", "tokenHash": "9f3c1a2b", "mfa": true },
  "action": "invoke", "resource": "atlas.snowflake/query",
  "decision": "allow", "policies": ["analysts-snowflake"], "policyBundle": "pb_2026-10-29-2",
  "limits": { "rate": "ok", "budget": "reserved:0.5" },
  "input": { "mode": "hash", "sha256": "…" }
}
```

Events: `catalog.read`, `invoke.decision`, `invoke.completed`, `auth.failed`, `ticket.issued`, `ticket.used`, `config.applied`, `policy.bundle.loaded`. `prevHash` chains records per gateway so tampering is detectable; the stream goes to Event Hubs and then to immutable Blob storage (WORM policy), never to the same Log Analytics table as application logs.

## Correlating the three

`request_id` and `trace_id` appear in the access log, the application log and the audit record; the trace ID also appears in upstream query tags. Kubernetes puts `audit-id` on the trace span for the same reason (`filters/traces.go`).

## Checklist

- [ ] Access log after the handler; probes skipped; no PII; 499 visible.
- [ ] Request ID reused from the edge, validated, echoed.
- [ ] Audit backend starts before serving, stops after drain; failed auth audited.
- [ ] Audit records carry decision inputs and versions (policy bundle, config version).
- [ ] Separate sinks with separate retention and access control.
