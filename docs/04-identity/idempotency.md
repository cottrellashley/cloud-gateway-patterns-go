# Idempotency keys

**Pattern:** every unsafe request (POST/PATCH/DELETE) that a client might retry carries `Idempotency-Key`; the gateway remembers the key, the request fingerprint and the response for a retention window, replays the response on an exact retry, refuses reuse with a different request, and serialises concurrent duplicates. Implemented in `patterns/idempotency`.

## Why at the gateway

Retries happen in clients, SDKs, agents and the proxy. Without a key, a retried `POST /orders` after a timeout creates two orders. The Azure REST guidelines require `Repeatability-Request-ID`/`Repeatability-First-Sent` support for actions that need idempotent retries (OASIS Repeatable Requests, tracked for at least 5 minutes); Stripe popularised `Idempotency-Key` with 24 h retention; the IETF `draft-ietf-httpapi-idempotency-key-header` standardises the header. The gateway is the one place that sees every write.

## Semantics (what the tests assert)

| Condition | Response |
| --- | --- |
| Safe method (GET/HEAD/OPTIONS) or PUT | not applicable, passes through |
| No key, `Required: false` | passes through |
| No key, `Required: true` | `400 InvalidInput` naming the header |
| First time | execute; capture status + replayable headers + body up to `MaxResponseBytes`; store with fingerprint |
| Same key, same fingerprint, stored | replay stored response with `Idempotent-Replayed: true`; handler not run |
| Same key, different fingerprint | `422 IdempotencyKeyReused` |
| Same key, first request still running | `409 Conflict` + `Retry-After: 1` |
| Handler returned 5xx or response too large | record deleted; a retry executes for real |
| Key scoped per principal | same key from two users are independent (`Options.Scope`) |

The fingerprint is `sha256(method \0 path \0 body)`; it detects accidental reuse (a client generating one key per session). Bodies are bounded (`MaxBodyBytes`, 4 MiB default) because the body must be read to fingerprint it.

## Store contract

```go
type Store interface {
    Begin(ctx, key string, rec Record) (existing Record, created bool, err error)  // atomic create-if-absent
    Complete(ctx, key string, rec Record) error
    Delete(ctx, key string) error
}
```

`Begin` must be atomic across replicas: Redis `SET key value NX EX ttl`, Cosmos DB `CreateItem` (409 on conflict → read existing), Postgres `INSERT ... ON CONFLICT DO NOTHING RETURNING`. Retention 24 h via TTL. Records are small (status, a few headers, a bounded body).

## Downstream interplay

* The proxy forwards `Idempotency-Key` unchanged so services can be idempotent too (Snowflake SQL API `requestId` + `retry=true`; Airflow `dag_run_id` derived from the key; GitHub issue creation with a hidden marker). Gateway memoisation is the first line; service-side idempotency is the backstop.
* `patterns/lro.Manager.Start` takes the key directly so a replayed start returns the same operation.
* Never memoise `5xx`: the client must be able to retry a real failure.

## Checklist

- [ ] Middleware after authn (scoped keys), after `MaxBytes`.
- [ ] `Required: true` on routes that create resources or spend money.
- [ ] Store `Begin` atomic; TTL 24 h; records bounded.
- [ ] Clients told: one UUIDv4 per logical operation, reuse it on retry.
- [ ] Headers replayed: `Content-Type`, `Location`, `Operation-Location`, `ETag`, `X-Error-Code` (the `replayable` list in the package).
