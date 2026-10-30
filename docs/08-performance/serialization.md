# Serialization

**Pattern:** JSON on the control plane with streaming encoders/decoders, bounded inputs and no reflection in hot loops; `encoding/json/v2` once stable for its performance and strictness; columnar formats (Arrow IPC, Parquet) for data; protobuf for RPC.

## JSON

* **Streaming**: `json.NewDecoder(r.Body)` with `DisallowUnknownFields()` for strict inputs; `json.NewEncoder(w)` for outputs; never build the whole response in memory for lists — encode items as you go.
* **Bounded**: `http.MaxBytesReader` before decoding; reject nesting depth beyond ~32 for untrusted documents (manifests, cursors).
* **Numbers**: decimals as strings for money and yields (Arrow `decimal128` on the data plane); `json.Number` or explicit string fields in JSON; never `float64` for amounts.
* **Go 1.25 `encoding/json/v2`** (experimental under `GOEXPERIMENT=jsonv2`): faster, streaming-first, case-sensitive by default, with `jsontext` for token-level work. Plan the migration; keep v1 semantics in the public contract until then.
* **Faster libraries** (`bytedance/sonic`, `goccy/go-json`, `segmentio/encoding`) are drop-in for `Marshal/Unmarshal` and worth it only when profiling shows JSON encoding at the top; they trade build portability (assembly, cgo-free but arch-specific) for speed.

## Columnar and binary

| Need | Format |
| --- | --- |
| Query results to clients | Arrow IPC over Flight; Parquet files over HTTPS |
| RPC between services | protobuf via Connect/gRPC |
| Events | CloudEvents JSON (structured) or binary HTTP mode |
| Config and manifests | JSON validated by JSON Schema; hashed with RFC 8785 canonical JSON for versions |

Arrow IPC schemas carry field metadata (`atlas.semantic`, `atlas.unit`) so the self-describing result-set contract survives the format change; `gosnowflake`'s `arrowbatches` hands you Arrow records without a conversion step.

## Compression

Enable response compression at the edge or with middleware only for text types above ~1 KiB; never compress already-compressed data (Parquet, images) or SSE (buffering breaks streaming). HTTP/2 header compression (HPACK) is automatic; keep header sets small and stable.

## Checklist

- [ ] Decoder/encoder streaming; unknown fields rejected on inputs that are contracts.
- [ ] Money as strings/decimals.
- [ ] Bulk data never JSON past a few MB.
- [ ] Benchmarks with `-benchmem` before swapping JSON libraries.
