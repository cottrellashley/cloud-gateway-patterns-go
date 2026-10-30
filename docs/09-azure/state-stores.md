# State stores

**Pattern:** each kind of gateway state has one store chosen for its access pattern; every record has an owner, a partition key, a TTL and a version for optimistic concurrency; the gateway never keeps authoritative state in process memory.

## What state the gateway has

| State | Pattern | Store | Key / TTL |
| --- | --- | --- | --- |
| Operation records (LRO) | point read/update by ID, sweep by age | Cosmos DB (or Postgres) | `id`; TTL = retention after terminal |
| Idempotency records | atomic create-if-absent, point read | Cosmos DB / Redis | `principal:key`; TTL 24 h |
| Sessions (UI) | point read/write by ID, delete | Cosmos DB / Redis | `id`; TTL = expiry |
| Rate-limit counters (cross-replica) | atomic increment with TTL | Redis (Azure Managed Redis) | `policy:key:window`; TTL = window |
| Subscriptions and resume tokens | point read/update, event append | Cosmos DB + Event Hubs/Service Bus | `id` |
| Config, routes, policy bundles | versioned document, watch/poll | Git + App Configuration, or Cosmos DB | `name`; no TTL; `version` |
| Catalog / manifests | versioned documents with ETags | Cosmos DB | `name@version` |
| Audit | append-only | Event Hubs → immutable Blob | time-ordered |
| Jobs | queue with transactional enqueue | Postgres (River) or Service Bus | job ID |

## Cosmos DB with `azcosmos`

`sdk/data/azcosmos` @e948fd2: `ContainerClient.CreateItem / ReadItem / ReplaceItem / UpsertItem / DeleteItem / PatchItem / NewQueryItemsPager` all take a `PartitionKey`; `ItemOptions` carries `IfMatchEtag` for optimistic concurrency; `NewTransactionalBatch(partitionKey)` with `CreateItem/ReplaceItem/UpsertItem/DeleteItem/ReadItem/PatchItem` executes atomically within one partition (`ExecuteTransactionalBatch`).

Rules that follow:

1. **Partition key = the ID you read by** (operation ID, session ID, idempotency key). Cross-partition queries are for admin tooling, not request paths.
2. **ETag on every update**: read, modify, `ReplaceItem` with `IfMatchEtag`; on `412` re-read and retry (`patterns/lro.Store.Update` is this loop behind an interface).
3. **TTL per container** (`DefaultTimeToLive`) and per item (`ttl` field) for operations, idempotency records and sessions; no sweeper needed.
4. **Transactional batch** for multi-document changes within one partition (operation record + its idempotency pointer share the principal partition if you want both atomic).
5. **Session consistency** is the default and enough for "read your own write" through the gateway; use the session token when a follow-up request may land on another replica.
6. **429 (request-rate-too-large)** is retried by the SDK's retry policy with `Retry-After`; budget RU/s per container and alert on throttling.

## Redis (Azure Managed Redis / Cache for Redis)

For counters and short-lived keys: `INCR` + `EXPIRE` pipelined (the Envoy ratelimit shape), `SET NX EX` for idempotency `Begin`, `SET EX` for sessions. Use a client with pipelining and connection pooling; never run blocking commands from request paths; one connection pool per process sized to the limiter concurrency.

## Postgres (Flexible Server)

When you need transactions across tables or River: `pgx` pool sized to (cores × 2–4) per replica; statement timeouts; `SKIP LOCKED` queues; `ON CONFLICT` upserts for idempotency.

## Snowflake as a data source, not a state store

Query results are planned, materialised and ticketed (`docs/06-data-and-async/arrow-flight-data-plane.md`); the gateway stores the statement handle in the operation record and nothing else.

## Checklist

- [ ] Every record type has: partition key, TTL, ETag/version, owner principal.
- [ ] No request-path cross-partition queries.
- [ ] Conflicts handled with re-read + retry, bounded.
- [ ] Store clients created once at startup with managed identity; connectivity verified before readiness.
- [ ] Private endpoints; RU/s and throttling alerts.
