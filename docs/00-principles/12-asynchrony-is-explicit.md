# 12. Asynchrony is explicit

**Principle:** anything that may take longer than a request, outlive a connection, or produce more than one result is modelled explicitly: a slow action returns an *operation resource* with an ID, terminal states, a status monitor, cancellation and retention; a stream is a typed, resumable sequence with IDs and heartbeats; a change is an event with a schema, an ID and a source, delivered through a channel the consumer can resume. Nothing blocks a request thread waiting for a warehouse, a vendor or a human.

**Why it matters at scale:** hidden asynchrony (a request that "usually takes 40 seconds") is the first thing to fail under load, behind a proxy with a 60-second timeout, or on a mobile network. Making it explicit gives clients one polling/streaming/event protocol for every slow thing, gives operators a resource to inspect and cancel, and gives the data plane bounded request durations.

## Evidence

**ARM asynchronous operations** ([Addendum.md §Asynchronous Operations](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L102)) define three flows and one vocabulary:

> PUT/PATCH: respond 201/200 with `provisioningState` non-terminal; GETs return the resource with non-terminal state until done; the field transitions to a terminal state and "should be returned on all future GETs".
> DELETE: respond 202 with a `Location` header where the operation can be monitored; the resource transitions to `Deleting`; when done the Location URL returns 200/204 and the resource is gone.
> POST action: respond 202 with `Location`; when done the Location URL returns "what would have been a successful response if the API completed".

The operation resource ([§Operation Resource format](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L205)): `{id, name, status: "RP defined | Succeeded | Failed | Canceled", startTime, endTime, percentComplete, properties (only on success), error (OData, required on failure)}`; "When reading the operation resource entity, the status code should always be 200 … If the response status code is a 4xx or 5xx value, it indicates a failure in reading the status but not in the underlying operation." Clients honour `Retry-After` ("minimum of 10 seconds and a maximum of 10 minutes"); "the resource should not be visible until the async operation completes" unless `provisioningState` is used ([§202 Accepted and Location Headers](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L171)). The RP proxy itself times out at 60 s ([common-api-details.md §Client Request Timeout](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/common-api-details.md#L59)): "If request take more than 60 seconds please consider using asynchronous request/response pattern."

**Azure REST guidelines** ([Guidelines.md §Long-Running Operations & Jobs](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L861)): 202 + `Operation-Location` (with `api-version`), status monitor with `status: NotStarted | Running | Succeeded | Failed | Canceled`, `error`, `result`, retention after completion, "YOU SHOULD allow any valid value of the api-version query parameter to be used in the GET operation on the status monitor".

**Google AIP-151** ([0151.md](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0151.md)): "Individual API methods that might take a significant amount of time to complete should return a `google.longrunning.Operation` object instead of the ultimate response message … The response must not be a streaming response … must define both response and metadata types." One operation type for every API; `GetOperation`, `ListOperations`, `CancelOperation`, `DeleteOperation`, `WaitOperation` are standard. [AIP-136](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0136.md): "The name must not include the term `Async`."

**Kubernetes** makes every slow thing a resource with a controller (principle 02): a `Job` has `status.conditions` and `active/succeeded/failed` counts; deletion is a state (`deletionTimestamp`) not a blocking call; `watch` is the resumable stream (`resourceVersion` as the cursor, `BOOKMARK` events as heartbeats, 410 `Gone` when the cursor expired, forcing a relist) — the same shape as SSE `Last-Event-ID` replay.

**Temporal** ([workflow/doc.go](https://github.com/temporalio/sdk-go/blob/18b0571/workflow/doc.go#L76)) states the discipline for durable workflows: "Execution must be deterministic … Workflow code must be deterministic and repeatable within an execution context"; side effects go through activities or `SideEffect`, which "returns the recorded result" on replay. Long-running orchestration is replayable state, not a goroutine.

**Dapr bindings and pub/sub** ([pubsub.go](https://github.com/dapr/components-contrib/blob/47da7bd/pubsub/pubsub.go#L41)): `Publish(ctx, *PublishRequest)`, `Subscribe(ctx, SubscribeRequest, handler)` with `Features()` (e.g. message TTL, bulk) advertised so the runtime knows what the broker can do; CloudEvents as the envelope (`docs/06-data-and-async/events-and-cloudevents.md`).

## The rules

1. **Classify every action at design time:** *fast* (completes within the request budget, returns 200), *slow* (returns 202 with an operation), *streaming* (returns a resumable stream), *subscription* (produces events). The manifest states the class; the gateway enforces the request deadline accordingly.
2. **One operation type for the whole platform:** `{id, kind, status: Accepted|Running|Succeeded|Failed|Canceled, createdAt, startedAt, finishedAt, percentComplete, error (envelope), result (or resultLocation), principal, idempotencyKey, expiresAt}`. No service invents its own.
3. **202 + `Operation-Location` + `Retry-After`** on start; `GET` the monitor returns 200 with the operation (never the operation's error as the HTTP status); `ETag` for cheap polling; `POST …/{id}/cancel` cooperative and idempotent; results retained for a stated period then 410 `Gone`.
4. **Results live in the data plane.** The operation holds `resultLocation` (a ticket, a Flight descriptor, a Blob SAS), not the bytes.
5. **Streams are resumable and bounded.** Every event has an ID; `Last-Event-ID` (or a cursor) resumes from a ring; expired cursors produce an explicit "resume expired" error that tells the client to restart; heartbeats keep intermediaries alive; slow consumers are disconnected, not buffered without bound.
6. **Events are typed, versioned and sourced** (CloudEvents `type`, `source`, `id`, `subject`, `time`, `dataschema`); consumers are idempotent on `id`; delivery is at-least-once through a durable channel with pull or push and dead-lettering.
7. **Workers are controllers over operation keys** (principle 02): a crashed worker's operations are picked up; a sync that cannot proceed requeues; progress is written to `status`, not kept in memory.
8. **Deadlines propagate.** The request deadline bounds synchronous work; operation deadlines bound asynchronous work; both are visible in the resource.
9. **Durable orchestration is replayable**: multi-step workflows record each step's result (Temporal-style) so a restart continues from the last recorded step without redoing side effects.

## In this repository

| Mechanism | Where |
| --- | --- |
| Operation resource, `WriteAccepted` (202 + `Operation-Location` + `Retry-After`), status monitor with weak ETag/304, cooperative cancel, idempotent start, retention sweep | `patterns/lro`, `docs/06-data-and-async/long-running-operations.md` |
| SSE with IDs, heartbeat, `Last-Event-ID` replay ring, explicit resume-expired, bounded sink | `patterns/sse`, `docs/02-server/streaming-sse-and-websockets.md` |
| Events (CloudEvents shape, pull vs push, dead letters) | `docs/06-data-and-async/events-and-cloudevents.md` |
| Durable background jobs (River shape) tied to operation records | `docs/06-data-and-async/background-jobs.md` |
| Workers as a level-triggered controller loop | `patterns/reconcile` |
| Deadlines on every request and propagated upstream | `patterns/middleware.Deadline`, `patterns/proxy` |
| Ticketed results | `docs/06-data-and-async/arrow-flight-data-plane.md` |

```go
// The same action returns 200 or 202 depending on the class declared in the manifest; the client code is one shape.
func (h *CapabilityHandler) Execute(w http.ResponseWriter, r *http.Request) {
    cap, plan, err := h.app.Plan(r.Context(), r)                        // validate, authorize, compile
    if err != nil { apierror.Write(w, err); return }
    switch cap.Spec.Class {
    case "fast":
        rs, err := h.app.Run(r.Context(), plan)                          // bounded by the request deadline
        if err != nil { apierror.Write(w, err); return }
        h.encode(w, r, rs)                                               // self-describing result set
    default: // "slow"
        op, replayed, err := h.ops.Start(r.Context(), "capabilities/execute", idemKey(r),
            func(ctx context.Context, progress lro.Progress) (json.RawMessage, error) {
                loc, err := h.app.RunToLocation(ctx, plan, progress)     // writes the result to the data plane, reports percent
                if err != nil { return nil, err }
                return json.Marshal(map[string]any{"resultLocation": loc, "schema": plan.Schema}) // pointer, never the bytes
            })
        if err != nil { apierror.Write(w, err); return }
        lro.WriteAccepted(w, op, "/v1/operations", replayed)             // 202 + Operation-Location + Retry-After
    }
}
```

## Applying it to a capability gateway

* **`execute` is slow by default** for warehouse-backed capabilities: 202 with an operation; the worker runs the query in the live session pool with the operation ID as query tag, writes Arrow/Parquet to the result location, and updates `status`. Small, indexed lookups may be declared `fast` in the manifest and are then bounded by the request deadline.
* **`handoff` is fast:** compiling the statement and minting the ticket completes within the request; the long part happens on the client's own connection. The operation record still exists (for audit and metering, filled in on ticket redemption and completion callbacks).
* **Streaming results** (`Accept: text/event-stream` or Flight `DoGet`) use IDs per batch so a dropped connection resumes from the last batch, not from the start of a 2 GB result.
* **Catalog changes are events** (`gateway.capabilities.changed`, `owned.portfolios.loaded`) on a durable channel agents and services can subscribe to; the catalog change feed is how data-plane replicas learn about new snapshots (principle 03).
* **Cancellation is real:** `POST /v1/operations/{id}/cancel` sets the intent; the worker's context is cancelled; the warehouse query is cancelled by tag; the operation ends `Canceled` with the partial-result policy stated.
* **Retention is a contract:** results kept 24 h (configurable per capability); after that the monitor returns 410 with the reason, and audit keeps the record without the bytes.

## Anti-patterns

* A request that holds a connection open for four minutes because "the query is usually fast".
* Each service with its own `{jobId, state: "DONE"}` shape and its own polling rules.
* Returning the operation's failure as HTTP 500 from the status monitor, so a poller cannot tell "the status endpoint is down" from "the job failed".
* Streams with no IDs and no heartbeat, resumed by restarting from scratch.
* Workers that keep progress in memory, so a redeploy loses every running job.
* Results streamed through the gateway into a buffered response body.
