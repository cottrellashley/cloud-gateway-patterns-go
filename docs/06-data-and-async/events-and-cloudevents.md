# Events: CloudEvents, delivery, outbox

**Pattern:** every event is a CloudEvents 1.0 envelope; producers write events transactionally with their state change (outbox) and a relay publishes them; consumers are idempotent on `id`; delivery is at-least-once with explicit acknowledgement and a resume token; Azure Event Grid carries them natively.

## Envelope

CloudEvents 1.0 (CNCF, graduated; Azure Event Grid supports the JSON format and HTTP binding) requires `specversion`, `id`, `source`, `type`; optional `subject`, `time`, `datacontenttype`, `dataschema`, `data`. The Go SDK (`cloudevents/sdk-go` @05f0f68) gives a `Client` with `Send`, `Request`, `StartReceiver` over an HTTP `Protocol` that implements `http.Handler` (`protocol.go: func (p *Protocol) ServeHTTP(rw, req)`) with an optional `WithRateLimiter` and request timeout, and `WithPollGoroutines(n)` to bound consumer concurrency.

```go
c, _ := cloudevents.NewClientHTTP()
e := cloudevents.NewEvent()
e.SetID(uuid)                       // the de-duplication key
e.SetSource("atlas://gw/pipeline")  // URI-reference
e.SetType("atlas.pipeline.run.changed")
e.SetSubject("sales_daily/run_2026-10-30")
e.SetTime(time.Now())
_ = e.SetData(cloudevents.ApplicationJSON, payload)
result := c.Send(ctx, e)            // protocol.Result; check cloudevents.IsACK(result)
```

Binary mode (attributes as `ce-*` headers, body is `data`) is the efficient HTTP binding; structured mode (whole envelope as JSON body) is what queues store. Extension attributes (`atlasseq`, `atlasresume`, `atlasattempt`) are allowed by the spec and are how a gateway adds ordering and resume metadata without touching `data`.

## Producing: the outbox

The only correct way to publish an event about a state change is to write both in one transaction and publish from the committed row:

```
BEGIN;
  UPDATE orders SET status='paid' WHERE id=$1;
  INSERT INTO outbox(id, type, subject, payload, created_at) VALUES (...);
COMMIT;
-- relay: SELECT ... FROM outbox WHERE published_at IS NULL ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED; publish; mark published
```

River (`docs/06-data-and-async/background-jobs.md`) exists for exactly this shape: `Client.InsertTx(ctx, tx, args, opts)` inserts a job in the same transaction as the business write, so "publish event" becomes a job that cannot be lost. With Cosmos DB, the change feed plays the outbox role: write the document, and a change-feed processor publishes.

## Consuming: at-least-once, idempotent, acknowledged

* Consumers de-duplicate on `id` (keep a short-lived set of seen IDs in the store keyed by subscription).
* Acknowledge after the side effect is durable; unacknowledged events are redelivered with a growing attempt counter and go to a dead-letter store after `maxAttempts` (Event Grid: `maxDeliveryCount`, dead-letter destination).
* Ordering is per `subject` only; design consumers to tolerate reordering across subjects.
* Pull delivery (Event Grid namespaces: receive → lock token → acknowledge/release/reject) lets a consumer control concurrency and visibility timeout; push (webhook) must verify the signature (`Standard Webhooks`: `webhook-id`, `webhook-timestamp`, `webhook-signature`) and respond `2xx` only after durable handling.

## Serving events to clients

Through the gateway, subscriptions are durable records (`subscriptions/{id}`) with a resume token; delivery is pull (`:pull` with `waitMs` long-poll and `:ack`) or SSE (`patterns/sse.Serve` with the ring buffer, `Last-Event-ID` = `atlasseq`). Offline for longer than retention → `410 ResumeExpired` with the earliest available token; the client decides whether to replay or skip.

## Checklist

- [ ] CloudEvents envelope; `id` unique forever per source; `type` in reverse-DNS form.
- [ ] Outbox or change feed for producers; never "publish after commit" from application code.
- [ ] Consumers idempotent on `id`; ack after durability; dead-letter after N attempts.
- [ ] Webhooks signed and verified; HTTPS only; challenge on registration.
- [ ] Resume tokens persisted by clients and by the gateway for proxied subscriptions.
