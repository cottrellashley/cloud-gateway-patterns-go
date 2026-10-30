# Out-of-process plugins (providers as subprocesses)

**Pattern:** untrusted or independently released modules run as separate processes supervised by the host, speaking a versioned RPC protocol over a local socket, with a handshake that proves protocol compatibility, mutual TLS between host and plugin, health checks, and reattach after host upgrades. HashiCorp's `go-plugin` is the production implementation (Terraform, Vault, Nomad, Packer); MCP's stdio transport is the same idea for agent tools.

## go-plugin's handshake (`docs/internals.md` @fa60f8a)

The plugin process prints one line on stdout:

```
CORE-PROTOCOL-VERSION | APP-PROTOCOL-VERSION | NETWORK-TYPE | NETWORK-ADDR | PROTOCOL
```

where `CORE-PROTOCOL-VERSION` is go-plugin's own version (`const CoreProtocolVersion = 1`), `APP-PROTOCOL-VERSION` is yours, network is `unix` or `tcp`, and `PROTOCOL` is `netrpc` or `grpc`. The host (`client.go` `Start`):

* sets a **magic cookie** in the environment (`fmt.Sprintf("%s=%s", c.config.MagicCookieKey, c.config.MagicCookieValue)`) so a plugin binary run by hand exits with a helpful message instead of hanging;
* optionally verifies the binary's checksum before executing it (`SecureConfig.Check(cmd.Path)` — `ErrSecureConfigNoChecksum`, `ErrSecureConfigNoHash` if misconfigured);
* can negotiate **AutoMTLS** ("the client and server automatically negotiate mTLS");
* supports `VersionedPlugins` so one host speaks several app-protocol versions (`protocolVersion(opts)` picks the highest common one);
* **Reattach**: "Plugins can be 'reattached'" after a host upgrade so running plugins are not killed (mutually exclusive with `SecureConfig`).

```go
type HandshakeConfig struct {
    ProtocolVersion  uint
    MagicCookieKey   string
    MagicCookieValue string
}
```

## When to use it in a gateway

* Provider modules written by other teams or vendors (untrusted code, different release cadence).
* Modules with heavy or conflicting dependencies (Arrow, vendor SDKs) that must not share the gateway's process.
* Local tools on a developer machine (MCP stdio servers are exactly this).

Not for: hot paths needing sub-millisecond calls (use in-process interfaces) or modules you fully control and release with the gateway.

## Supervision rules

1. Start under the lifecycle group; restart with exponential backoff and a crash-loop limit; expose the plugin's health in `/readyz?verbose`.
2. Every call carries the gateway's invocation context (signed) — plugins never see end-user bearer tokens.
3. Per-plugin resource limits (cgroups on Linux; separate container on Azure) and per-call deadlines.
4. Protocol versioning from day one; `VersionedPlugins` style negotiation.
5. Capability manifests come from the plugin's `Describe` call at start, validated by the host (see the Atlas spec section 16).

## Alternatives

* **Wasm** (tetratelabs/wazero, extism/go-sdk): in-process sandbox, no fork/exec, strong isolation, but limited I/O and a different programming model.
* **Separate services** behind the proxy: the same isolation with network cost; the right answer in Azure for anything stateful or scaled independently.
