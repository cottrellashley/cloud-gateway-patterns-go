# Long-running operations

**Pattern:** anything that may outlive an HTTP request returns `202 Accepted` with an `Operation-Location`; the operation is a resource with a fixed state machine, polled with `Retry-After` and ETags, cancellable cooperatively, deduplicated by `Idempotency-Key`, and retained for a documented period. Implemented in `patterns/lro`.

## The contract (Azure REST guidelines + ARM + MCP Tasks)

The Azure REST API guidelines require: `202-Accepted` with an absolute `operation-location`, a status monitor with `id, kind, status, error, result`, statuses `NotStarted | Running | Succeeded | Failed | Canceled`, `retry-after` while running, retention ≥ 24 h, and `409` when an `Operation-Id` is reused except for identical retries. ARM's older convention is `Azure-AsyncOperation` or `Location` plus `provisioningState`. The MCP Tasks extension converged on the same shape (`taskId`, `working | input_required | completed | failed | cancelled`, `ttlMs`, `pollIntervalMs`, cooperative `tasks/cancel`). `patterns/lro` uses `Accepted | Running | Succeeded | Failed | Canceled` and the `Operation-Location` header.

```
POST /v1/queries                      → 202, Operation-Location: /v1/operations/op_…, Retry-After: 1
GET  /v1/operations/op_…              → 200 {"operation":{status:"Running",percentComplete:42}} ETag: W/"op_…:7" Retry-After: 1
GET  /v1/operations/op_…  (If-None-Match) → 304 while unchanged
GET  /v1/operations/op_…              → 200 {"operation":{status:"Succeeded",result:{…}}}   (no Retry-After)
POST /v1/operations/op_…/cancel       → 202 {"operation":{status:"Running"}} then Canceled once the worker stops
```

## Implementation notes from the package

* **Durable before 202.** `Manager.Start` creates the record in the store *before* spawning the worker (MCP: "The task must be durably created before sending the response").
* **Detached but inherited context.** The worker runs under `context.WithoutCancel(requestCtx)` so the HTTP request returning does not cancel it, while trace and principal values are inherited.
* **Cooperative cancel.** `Cancel` cancels the worker's context with a cause; the state becomes `Canceled` only when the worker returns and the cause is the client's cancel — a worker that finishes anyway ends `Succeeded`/`Failed`, as gobreaker-style generation checks and MCP ("cancellation is cooperative") both accept.
* **Idempotent start.** Same key → same operation, `Idempotent-Replayed: true` on the 202.
* **Failures are envelopes.** A worker error becomes `apierror.From(err)`; internal messages are hidden; a panic becomes `Failed` instead of crashing the process.
* **ETag/304.** `GetHandler` uses a weak ETag on the record version so pollers can long-poll cheaply.
* **Retention.** `SweepLoop` deletes terminal operations older than `Retention` (24 h default).

## Mapping real backends

| Backend | Start | Poll | Cancel |
| --- | --- | --- | --- |
| Snowflake SQL API | `POST /api/v2/statements?async=true&requestId=<key>` → `statementHandle` | `GET /api/v2/statements/{handle}` | `POST /api/v2/statements/{handle}/cancel` |
| Airflow REST API | `POST /api/v2/dags/{id}/dagRuns` with `dag_run_id = atlas-<key>` (409 on duplicate → map to existing) | `GET .../dagRuns/{id}` state | `PATCH` state to `failed` |
| ARM | `PUT/POST` → `Azure-AsyncOperation` | `GET` that URL honouring `Retry-After` | not generally supported |

Store the backend handle in the operation (`providerRef` in the Atlas spec) for operators; clients never see it.

## Durable alternative

When operations must survive a gateway restart, back the `Store` with Cosmos DB/Postgres and run workers through a job queue (`docs/06-data-and-async/background-jobs.md`): the HTTP layer stays identical, only `Manager.Start` changes from "spawn a goroutine" to "insert a job".

## Checklist

- [ ] 202 + `Operation-Location` + `Retry-After`; never block an HTTP request for more than the edge timeout.
- [ ] Record created before responding; worker context detached with `WithoutCancel`.
- [ ] Idempotency-Key honoured on start.
- [ ] `GET` has ETag/304 and `Retry-After` while non-terminal; `404` after retention.
- [ ] Cancel cooperative; deadlines cancel the worker and stop downstream billing.
- [ ] Worker errors wrapped into the envelope; panics contained.
