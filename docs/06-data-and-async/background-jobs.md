# Background jobs

**Pattern:** work that must survive a process restart goes through a durable queue with transactional enqueue, bounded workers, per-kind timeouts, exponential retry with jitter, and explicit cancellation; the long-running-operation record (`patterns/lro`) is the client-facing view of a job.

## River (Postgres/SQLite) as the reference

`riverqueue/river` @e898054 shows the API a job system should have:

```go
// client.go
func (c *Client[TTx]) Insert(ctx, args JobArgs, opts *InsertOpts) (*rivertype.JobInsertResult, error)
func (c *Client[TTx]) InsertTx(ctx, tx TTx, args JobArgs, opts *InsertOpts) (*rivertype.JobInsertResult, error)  // same transaction as your write
func (c *Client[TTx]) JobCancel(ctx, jobID int64) (*rivertype.JobRow, error)
func (c *Client[TTx]) JobRetry(ctx, id int64) (*rivertype.JobRow, error)
func (c *Client[TTx]) Start(ctx) error / Stop(ctx) error / StopAndCancel(ctx) error

// worker.go
type Worker[T JobArgs] interface {
    Middleware(job *rivertype.JobRow) []rivertype.WorkerMiddleware
    NextRetry(job *Job[T]) time.Time
    Timeout(job *Job[T]) time.Duration
    Work(ctx context.Context, job *Job[T]) error
}
type JobArgs interface { Kind() string }
```

Config knobs worth copying whatever queue you use (`client.go` `Config`): `JobTimeout` (default 1 min), `MaxAttempts`, `Queues map[string]QueueConfig` (per-queue concurrency), `FetchCooldown`/`FetchPollInterval`, `RescueStuckJobsAfter`, `CompletedJobRetentionPeriod`/`CancelledJobRetentionPeriod`, and unique-job options so an enqueue is idempotent by args/period.

The transactional insert is the important idea: the job row commits with the business row, so the queue is an outbox; no "enqueue after commit" window, no lost jobs.

## Mapping to the gateway

| Concern | Where |
| --- | --- |
| Client-facing status, cancel, result | `patterns/lro` operation record |
| Durable execution, retries, concurrency | the queue (River on Postgres; on Azure without Postgres, Service Bus queues + a worker service, or Cosmos DB with a lease) |
| Idempotency | `Idempotency-Key` → unique job args; River's `UniqueOpts` |
| Cancellation | `JobCancel` + worker observes `ctx.Done()`; the LRO flips to `Canceled` when the worker returns |
| Timeouts | per-kind `Timeout`; the LRO deadline is the outer bound |
| Retry | exponential with jitter; `MaxAttempts` small (3–5) for user-facing jobs; dead-letter after |

## Workers

* One process type per worker pool; scale on queue depth (KEDA scaler on the queue), not on CPU.
* Workers are idempotent per attempt (write results keyed by job ID; upsert, don't insert).
* Progress is reported to the LRO record (`Progress(pct, msg)`), not kept in memory.
* Drain on shutdown: stop fetching, finish or checkpoint in-flight work within the grace period, then `StopAndCancel`.

## Scheduled and recurring work

Periodic jobs (sweeps, reconciliation, policy-bundle refresh) belong in the same queue with a leader-elected scheduler, not in `time.Ticker` loops spread across replicas (every replica would run them). River has periodic jobs; on Azure, a Container Apps Job on a cron or a Logic App trigger enqueues the work.

## Checklist

- [ ] Transactional enqueue (outbox) for every job created from a request.
- [ ] Per-kind timeout and max attempts; jittered backoff; dead-letter.
- [ ] Workers idempotent; progress to the LRO record.
- [ ] Cancellation cooperative and observed via ctx.
- [ ] Worker pools scaled on queue depth; one scheduler.
