# 11. Idempotency and optimistic concurrency

**Principle:** every operation in the platform is safe to repeat and every write is conditional. Creates are idempotent by name (or by a client request ID); updates carry the version they were based on and fail with 409 if it moved; actions carry a request ID the server remembers for a window; reads can be conditional (`If-None-Match`); retry policy (what, when, how long) is defined by the server and advertised in responses, not invented by clients.

**Why it matters at scale:** clients lose responses. Retrying a non-idempotent operation duplicates trades, double-provisions, or double-bills; refusing to retry turns every network blip into a human incident. Kubernetes makes name idempotency and `resourceVersion` universal so hundreds of controllers can retry blindly; ARM makes PUT a full overwrite; Azure's guidance is that "for cloud services it is important to make *all* operations idempotent so that clients can use retry in failure scenarios without risk of unintended consequences".

## Evidence

**Kubernetes name idempotency** ([api-conventions.md §Idempotency](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L804)): "All compatible Kubernetes APIs MUST support name idempotency and respond with an HTTP status code 409 when a request is made to POST an object that has the same name as an existing object in the system." `generateName` lets the server mint a unique suffix and tells the client to retry on 409 with `Retry-After`.

**Kubernetes optimistic concurrency** ([§Concurrency Control and Consistency](https://github.com/kubernetes/community/blob/309fa9a/contributors/devel/sig-architecture/api-conventions.md#L1146)): "When a record is about to be updated, its version is checked against a pre-saved value, and if it doesn't match, the update fails with a StatusConflict (HTTP status code 409). … The only way for a client to know the expected value of resourceVersion is to have received it from the server in response to a prior operation … This value MUST be treated as opaque by clients and passed unmodified back to the server." Controllers follow the read-modify-write-retry pattern; a conflict means re-read and reconcile again (principle 02), never force.

**ARM PUT is a full overwrite** ([resource-api-reference.md](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/resource-api-reference.md#L26)): "ARM does not distinguish between creation and update … a PUT should always be allowed to overwrite an existing resource." ETags are supported for resources ([Addendum.md §ETags](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L34)). Retry policy is the server's ([§Retrying REST Calls](https://github.com/enihcam/azure-resource-manager-rpc/blob/3a19a6d/v1.0/Addendum.md#L249)): retry 408, 5xx and socket errors; "The retry interval, strategy (e.g. exponential/linear) and max timeout should be defined by the *server* and not the client. That allows various server workloads to advertise different retry policies."

**Google AIPs:** [AIP-155](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0155.md) `request_id`: "Providing a request ID must guarantee idempotency. If a duplicate request is detected, the server should return the response for the previously successful request"; UUID4, server-side window. [AIP-154](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0154.md) `etag`: "must be provided by the server on output … If a user sends back an etag which matches the current etag value, the service must permit the request"; mismatch is `ABORTED`. [AIP-134](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0134.md): updates are `PATCH` with an `update_mask` so partial updates do not clobber unrelated fields. [AIP-231](https://github.com/aip-dev/google.aip.dev/blob/2ccc61a/aip/general/0231.md): batch operations "must be atomic".

**Azure REST guidelines** ([Guidelines.md §Repeatability of requests](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L851)): "all HTTP operations are naturally idempotent except for POST used to create a resource and POST when used to invoke an action … YOU SHOULD support repeatable requests as defined in OASIS Repeatable Requests Version 1.0 for POST operations … The tracked time window … MUST be at least 5 minutes"; `Repeatability-Request-ID`, `Repeatability-First-Sent`, `Repeatability-Result`. Conditional requests ([§Conditional Requests](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/Guidelines.md#L1191)) with `ETag`/`If-Match`/`If-None-Match` for concurrency and caching; "YOU SHOULD implement conditional requests early. This allows you to support concurrency, which tends to be a concern later on" ([ConsiderationsForServiceDesign.md](https://github.com/microsoft/api-guidelines/blob/1c58ca7/azure/ConsiderationsForServiceDesign.md#L83)).

**Dapr** states the concurrency model per building block ([API-005](https://github.com/dapr/dapr/blob/3342040/docs/decision_records/api/API-005-state-store-behavior.md)): "two flavors of optimistic concurrency: first-write wins and last-write wins. First-write wins is implemented through ETag … We'll choose last-write wins as the default", and stores advertise `ETAG` in `Features()` so the runtime can refuse first-write-wins on a store that cannot do it.

**gRPC retry throttling** (`docs/03-traffic/retries-backoff-circuit-breakers.md`): retries only for idempotent methods on listed codes, with a budget (`maxTokens`, `tokenRatio`) so retries cannot amplify an outage.

## The rules

1. **Create is idempotent by name.** `PUT /{collection}/{name}` creates or replaces; `POST /{collection}` with a client-chosen `name` returns 409 `AlreadyExists` on a clash; server-generated names use a client `requestId`/`Idempotency-Key` so a retried POST returns the first result.
2. **Every write carries the version it was based on.** `If-Match: "<resourceVersion>"` (or `metadata.resourceVersion` in the body); mismatch is 409 `Conflict` with the current version in the error details; the client re-reads and re-applies its intent. Empty version = unconditional, allowed only for last-write-wins collections and stated in the contract.
3. **Actions carry a request ID.** `POST …:execute` with `Idempotency-Key` (or `Repeatability-Request-ID` + `Repeatability-First-Sent`); the server stores the fingerprint and the result for a retention window (≥ 5 minutes, typically 24 h); same key + same fingerprint → replay with `Idempotent-Replayed: true`; same key + different body → 422; in progress → 409.
4. **Reads are conditional.** Every single-object `GET` sets `ETag`; `If-None-Match` yields 304; discovery and schema documents are cacheable with `Cache-Control`.
5. **Partial updates use a mask or merge patch**, never "send the whole object you think you have" without a version.
6. **Retry policy is the server's.** Responses say whether an error is retryable (`retryable` in the envelope) and when (`Retry-After`); clients implement exponential backoff with jitter and a budget; servers never rely on clients being well-behaved.
7. **Batch is atomic or explicitly partial**, documented per operation; never silently partial.
8. **Downstream side effects are idempotent too.** A provider that creates something in a vendor system or a warehouse uses the operation ID as the downstream idempotency key, so a retried step is a no-op.
9. **Conflicts are normal, not errors to alert on.** Controllers and clients treat 409 as "re-read and try again", with backoff.

## In this repository

| Mechanism | Where |
| --- | --- |
| Name idempotency (`ErrAlreadyExists`), `ResourceVersion` conflict (`ErrConflict`), unconditional vs conditional writes, status subresource, UID mismatch on recreate | `patterns/resource.MemoryStore` |
| `ETag` / `If-Match` / `If-None-Match` evaluation | `patterns/resource.CheckPreconditions`, `SetETag`; `patterns/lro.GetHandler` (304) |
| `Idempotency-Key` replay / fingerprint mismatch / in-progress semantics, retention, scope by principal | `patterns/idempotency`, `docs/04-identity/idempotency.md` |
| Idempotent operation start (same key → same operation) | `patterns/lro.Manager.Start(ctx, kind, idemKey, worker)` |
| Server-defined retry: `Retry-After` on 429/503/202, `retryable` in the envelope, client budget | `patterns/apierror`, `patterns/resilience` (`Budget`, `ParseRetryAfter`, `RetryAfterError`) |
| Safe proxy retries (idempotent methods, replayable body, nothing written) | `patterns/proxy` |
| Snapshot writes with `If-Match` | `patterns/config.CheckIfMatch` |

```go
// Read-modify-write with conflict retry: the universal controller/client loop.
func updateWithRetry[S, T any](ctx context.Context, st resource.Store[S, T], name string, mutate func(*resource.Object[S, T]) error) error {
    b := resilience.Backoff{Base: 20 * time.Millisecond, Max: 2 * time.Second, Multiplier: 2, Jitter: 0.2}
    for attempt := 0; ; attempt++ {
        cur, err := st.Get(ctx, name)
        if err != nil { return err }
        if err := mutate(cur); err != nil { return err }
        if _, err = st.Update(ctx, cur); !errors.Is(err, resource.ErrConflict) { return err } // success or non-conflict error
        if attempt == 5 { return err }
        if err := sleepCtx(ctx, b.Delay(attempt)); err != nil { return err }
    }
}
```

## Applying it to a capability gateway

* **Manifest registration** is `PUT /v1/capabilities/{name}` with `If-Match` on re-registration; a provider CI pipeline that retries after a timeout cannot create a duplicate or clobber a newer generation.
* **`execute` and `handoff` are POST actions with `Idempotency-Key`.** An agent that times out and retries gets the same `Operation` back (same operation ID, same result location), not a second warehouse query. Keys are scoped to the principal; the fingerprint covers capability, mode, parameters and reader schema.
* **Operations are conditional resources:** `GET /v1/operations/{id}` returns `ETag`; pollers send `If-None-Match` and get 304 while nothing changed (cheap polling at scale); `POST …/{id}/cancel` is idempotent (cancelling a terminal operation is a no-op with the terminal state in the body).
* **Tickets are single-intent:** a handoff ticket is bound to one compiled statement hash and one principal; redeeming it twice returns the same descriptor (idempotent) until expiry; it cannot be re-pointed.
* **Downstream idempotency:** the operation ID is the warehouse query tag and the vendor request ID, so a retried worker step that already ran is detected and skipped.
* **Policy says which collections are last-write-wins** (labels, annotations on operational objects) and which require `If-Match` (manifests, policies, schema versions). The store refuses unconditional writes to the latter.

## Anti-patterns

* `POST /execute` with no key: a retried call runs the query twice and bills twice.
* `PUT` that reads the whole object, modifies, and writes without a version — the lost-update problem in production.
* Clients that retry 400s, or never retry 503s, because the server did not say.
* Status monitors that return 200 with identical bodies to thousands of pollers per second because they have no `ETag`.
* "We'll add idempotency keys later": the retention store, the fingerprint rules and the headers are part of the v1 contract (principle 05).
