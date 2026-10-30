# Pagination

**Pattern:** opaque, signed, expiring cursors bound to the request they were issued for; `items` arrays with `nextCursor` omitted on the last page; declared consistency; page sizes clamped; `410` when a cursor expires. Implemented in `patterns/pagination`.

## Rules from the mature APIs

* **Azure REST guidelines**: list responses are objects with a top-level array (`value`), `nextLink` is an absolute URL present only when more results exist and never `null`, and services must document whether items may be skipped or duplicated across pages unless a snapshot prevents it.
* **Kubernetes list API**: `continue` tokens are opaque and expire; an expired token returns `410 Gone` and the client must relist. Chunked lists (`limit=500`) plus `resourceVersion` give consistency; `resourceVersion=0` serves from cache.
* **Cost Management / ARM**: `nextLink` carries every query parameter including `api-version`, so a client follows it blindly.
* **MCP `tools/list`**: `nextCursor`, opaque, deterministic ordering so clients can cache.

## Why signed cursors

An unsigned offset cursor (`?offset=100`) lets a client jump anywhere, including past filters it could not express, and leaks ordinal information. Signing (HMAC-SHA256) makes the cursor unforgeable; binding it to a hash of the query input (`filter`, `sort`, excluding the cursor and page size) makes "same cursor, different filter" a `400`; an expiry makes abandoned cursors harmless and lets a snapshot be released.

```go
codec := pagination.NewCodec(key)
cur, _ := codec.Encode(query, position{AfterID: last.ID}, 10*time.Minute)   // issue
var pos position
switch err := codec.Decode(r.URL.Query().Get("cursor"), query, &pos); {
case errors.Is(err, pagination.ErrCursorExpired):  // 410 ResumeExpired → client restarts
case errors.Is(err, pagination.ErrCursorMismatch): // 400 CursorMismatch
case err != nil:                                   // 400 invalid
}
```

Cursor wire format: `base64url({"v":1,"h":"<16 hex of sha256(input)>","p":<position>,"exp":<unix>}).base64url(hmac)`. Clients treat it as a string.

## Position encoding

Keyset pagination (`WHERE (sort_key, id) > ($1, $2) ORDER BY sort_key, id LIMIT n+1`) is stable under inserts and O(1) per page; offset pagination skips or duplicates under writes and costs O(offset). Encode the keyset tuple as the position. For Snowflake result sets, the position is the statement handle plus partition index; for Cosmos DB, the continuation token the SDK returns (opaque, already signed by the service) wrapped in ours.

## Consistency declaration

Say which one each list provides and echo it per page:

* `snapshot` — every page reflects one point in time (materialised result, `resourceVersion`); cursors may be long-lived.
* `eventual` — items may move between pages; clients de-duplicate by `id`.

## Response shape

```json
{ "items": [ … ], "nextCursor": "eyJ2…", "truncated": false }
```

`nextCursor` absent on the last page (never `null`, never `""`); `truncated: true` when a governance cap (max rows/bytes) ended the sequence — a success with partial data, with `OutputLimitExceeded` in `warnings`, not an error.

## Checklist

- [ ] Keyset positions; cursors signed, input-bound, expiring (10 min default; longer for snapshots).
- [ ] `pagination.ClampPageSize(requested, default, max)`; max ≤ a few thousand JSON rows.
- [ ] `nextCursor` omitted on the last page; `410` on expiry; `400` on mismatch.
- [ ] Deterministic ordering (sort key + id).
- [ ] Bulk results are not pages: hand off to the data plane past a few MB.
