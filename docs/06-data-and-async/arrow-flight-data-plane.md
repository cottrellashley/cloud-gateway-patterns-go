# Arrow Flight data plane

**Pattern:** the gateway plans and authorises; bytes go from the data endpoint to the client. A plan is an Arrow schema plus endpoints plus short-lived tickets (Flight's `FlightInfo` shape); the data endpoint verifies the ticket and streams record batches; Parquet over HTTPS (Blob SAS) is the fallback for clients without Flight.

## Flight in Go

`arrow-go/arrow/flight/server.go` @56815b7 builds a gRPC server with Flight-specific middleware:

```go
type ServerMiddleware struct {
    Stream grpc.StreamServerInterceptor
    Unary  grpc.UnaryServerInterceptor
}
type CustomServerMiddleware interface {
    // StartCall will be called with the current context of the call, grpc.SetHeader can be used to add outgoing headers
    StartCall(ctx context.Context) context.Context
    // CallCompleted is a callback which is called with the return from the handler
    CallCompleted(ctx context.Context, err error)
}
func NewServerWithMiddleware(middleware []ServerMiddleware, opts ...grpc.ServerOption) Server
```

A ticket-verifying `CustomServerMiddleware` reads `authorization` from the incoming metadata in `StartCall`, verifies the JWS (`patterns/auth` verification code works unchanged for ES256 tickets), and puts the claims in the context; `CallCompleted` writes the `ticket.used` audit record with bytes served. `server_auth.go` shows the built-in equivalents (`createServerBearerTokenUnaryInterceptor(validator)` / stream variant) that call a `BasicAuthValidator`.

Serving data: implement `DoGet(ticket, stream)` with `flight.NewRecordWriter(stream, ipc.WithSchema(schema))` and write each `arrow.Record`; `GetFlightInfo` returns `FlightInfo{Schema, Endpoint: []*FlightEndpoint{{Ticket, Location}}, TotalRecords, TotalBytes, Ordered}`. Flight SQL (`arrow/flight/flightsql`) layers SQL commands on the same methods; ADBC's Go drivers (`github.com/apache/arrow-adbc/go/adbc/driver/flightsql`, `.../driver/snowflake`) are the clients.

## Snowflake as the source

`gosnowflake` returns Arrow batches directly (`arrowbatches` package @ba85f95): `ctx = arrowbatches.WithArrowBatches(ctx)`, run the query, then `arrowbatches.GetArrowBatches(rows)` yields `[]*ArrowBatch` each with `Fetch() (*[]arrow.Record, error)`, `GetRowCount()`, `GetRowTypes()`. Each batch maps to one Flight endpoint: the plan has as many endpoints as Snowflake result chunks, clients fetch them in parallel, and the gateway never holds the data.

## Plan and ticket

```json
{ "plan": { "id": "pl_…", "schema": {"format":"application/vnd.apache.arrow.schema","base64":"…"},
  "ordered": true, "totalRows": 48213090, "expiresAt": "…",
  "endpoints": [
    {"transport":"arrow-flight","locations":["grpc+tls://flight.gw.example:443"],"ticket":"<JWS>","rows":12053272},
    {"transport":"https+parquet","locations":["https://acct.blob.core.windows.net/results/op_…/part-0000.parquet?sv=…"],"ticket":null}
  ] } }
```

Ticket claims: `iss`, `aud` (the data endpoint), `sub` (principal), `exp` (minutes), `jti`, `op`, `plan`, `ep` (endpoint index), `max {bytes, rows}`, `trace`, `once`. The endpoint enforces `max` by closing the stream; `once` tickets are recorded in a replay cache until `exp`.

## Parquet fallback

Materialise to Parquet (`arrow-go/parquet` writer, same schema) in Blob storage; mint a read-only SAS URL with the plan's expiry (and an IP restriction when the client's egress address is known). The SAS issuance is logged as a ticket. Any HTTP client can download; `Range` requests let readers fetch footers first.

## Why not proxy the bytes

A gateway that proxies 2 GB results per request becomes the bottleneck and the memory risk (experiment 7 in the Atlas report measures exactly this). Tailscale's principle applies: the control plane carries keys and policy, "virtually no traffic". ARM keeps data-plane endpoints separate for the same reason.

## Checklist

- [ ] Plan returns schema before data; endpoints carry tickets; Flight and Parquet both offered.
- [ ] Tickets: short expiry, audience = data endpoint, bound to principal and operation, size caps enforced at the endpoint.
- [ ] Data endpoint audits `ticket.used` with bytes.
- [ ] JSON page fallback with a row cap for thin clients (`docs/06-data-and-async/pagination.md`).
- [ ] Never buffer whole results in the gateway process.
