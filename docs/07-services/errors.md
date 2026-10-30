# Errors

**Pattern:** internal errors are wrapped with `%w` and carry context; the boundary converts them once into the single error envelope (`patterns/apierror`) with a stable code, a safe message and the right status; nothing internal leaks; clients branch on `error.code`.

## The envelope

Azure REST guidelines require `error.code` and `error.message` with optional `target`, `details[]`, `innererror`, an `x-ms-error-code` header matching the body, and documented top-level codes "since they are part of the contract". The gateway's envelope adds `retryable` and `retryAfter` so SDKs do not need per-code retry tables:

```json
{ "error": { "code": "RateLimited", "message": "rate limit \"per-user\" exceeded", "retryable": true, "retryAfter": 7,
             "hop": "gateway", "details": [ { "code": "BudgetWindow", "target": "snowflake-credits", "message": "resets 2026-10-31T00:00:00Z" } ] } }
```

Headers: `Content-Type: application/json`, `X-Error-Code: <code>`, `Retry-After` when set. Reserved codes and their statuses are in `patterns/apierror/apierror.go` and the Atlas spec section 6.5.

## Go-side rules

1. **Wrap, don't swallow**: `fmt.Errorf("orders: load %s: %w", id, err)`. Keep the chain for `errors.Is/As`.
2. **Sentinel errors for conditions callers branch on** (`ErrNotFound`, `ErrConflict`); typed errors when the caller needs data (`*apierror.Error`, `*RetryAfterError`).
3. **Map at the boundary, once.** Handlers do `apierror.Write(w, err)`; `From` turns context errors into 504/499, `*http.MaxBytesError` into 413, timeouts into 504, everything else into 500 with a generic message and the cause kept for the log.
4. **Log once.** The access log records 5xx; `Recover` logs panics with stacks; inner layers do not log and return.
5. **Never put internals in `message`**: no SQL, hostnames, stack traces, file paths or vendor error text (ardanlabs' `InternalOnlyLog` code is this rule as a type).
6. **Uber style guide, "Errors"**: error strings are not capitalised and have no trailing punctuation; handle type-assertion failures; don't panic in libraries (`style.md` sections "Errors", "Handle Type Assertion Failures", "Don't Panic").

## gRPC / Connect mapping

connect-go's `connectCodeToHTTP` is reproduced by `apierror.HTTPStatusForRPCCode`; the reverse direction (HTTP → code) for services that speak RPC: `401 → unauthenticated`, `403 → permission_denied`, `404 → not_found`, `409 → already_exists/aborted`, `429 → resource_exhausted`, `503 → unavailable`, `504 → deadline_exceeded`. Keep the gateway's `code` string as a Connect error detail so RPC clients see the same vocabulary.

## Retryability in the envelope

`retryable` is set from status by default (429, 502, 503, 504) and overridable per error. Clients retry only when it is true, honour `retryAfter`, and reuse the `Idempotency-Key` — the same rule `patterns/resilience.Retry` applies server-side.

## Provider errors

Services and providers declare their own codes (`SqlCompilationError`, `WarehouseSuspended`) with `retryable` and `httpStatus`; the gateway passes them through unchanged, appending itself to `details` as `ForwardedBy`. An undeclared code is rewritten to `ProviderContractViolation` (502) so contracts stay honest.

## Testing errors

Table tests over `apierror.From` for every sentinel; a test that greps the serialised envelope for forbidden substrings (`password`, `host=`, `/home/`, `goroutine `) on a corpus of real internal errors.
