# Sources read

Every repository below was cloned and read at the listed commit on 2026-10-30 to write `docs/` and `patterns/`. Links in the docs point at these commits so excerpts stay stable.

| Repository | Commit | Paths read | Licence |
| --- | --- | --- | --- |
| https://github.com/golang/go | go1.25.14 (toolchain source) | net/http/server.go, net/http/httputil/reverseproxy.go, net/http/pattern.go, log/slog | BSD-3 |
| https://github.com/golang/time | fb013b3 | rate/rate.go | BSD-3 |
| https://github.com/golang/sync | 1f920df | singleflight, errgroup | BSD-3 |
| https://github.com/kubernetes/kubernetes | ba832c1 | staging/src/k8s.io/apiserver/pkg/{server,endpoints/filters,endpoints/discovery,util/flowcontrol}, kube-aggregator/pkg/apiserver | Apache-2.0 |
| https://github.com/traefik/traefik | 38d53dd | pkg/server, pkg/middlewares, pkg/healthcheck, pkg/safe | MIT |
| https://github.com/caddyserver/caddy | eb04ed5 | caddy.go, modules/caddyhttp (app.go, server.go, reverseproxy/*) | Apache-2.0 |
| https://github.com/zalando/skipper | 2237f1e | proxy, loadbalancer | Apache-2.0 |
| https://github.com/vulcand/oxy | 56ba15b | buffer, cbreaker | Apache-2.0 |
| https://github.com/go-chi/chi | 9f8adb1 | mux.go, chain.go, middleware/* | MIT |
| https://github.com/grpc/grpc-go | bc90736 | server.go, keepalive, health, backoff, examples/features/retry, balancer/roundrobin | Apache-2.0 |
| https://github.com/connectrpc/connect-go | 5791e34 | connecthttp/handler.go, connecthttp/protocol_connect.go | Apache-2.0 |
| https://github.com/grpc-ecosystem/go-grpc-middleware | e4b8359 | interceptors/{recovery,ratelimit,timeout,retry} | Apache-2.0 |
| https://github.com/pomerium/pomerium | 42e80cc | internal/sessions, pkg/cryptutil | Apache-2.0 |
| https://github.com/ory/oathkeeper | ab46001 | proxy/request_handler.go, pipeline/authn | Apache-2.0 |
| https://github.com/envoyproxy/ratelimit | 11a831b | src/limiter, src/redis, src/utils | Apache-2.0 |
| https://github.com/envoyproxy/go-control-plane | 370bc89 | pkg/cache/v3 | Apache-2.0 |
| https://github.com/coreos/go-oidc | c914bd3 | oidc/verify.go, oidc/jwks.go | Apache-2.0 |
| https://github.com/lestrrat-go/jwx | 5acb476 | jwk, jwt | MIT |
| https://github.com/cedar-policy/cedar-go | 34be9aa | README, package layout | Apache-2.0 |
| https://github.com/sony/gobreaker | fed8e9e | v2/gobreaker.go | MIT |
| https://github.com/failsafe-go/failsafe-go | 2942d1a | README, package layout | MIT |
| https://github.com/hashicorp/go-retryablehttp | fd00458 | client.go | MPL-2.0 |
| https://github.com/hashicorp/go-plugin | fa60f8a | client.go, server.go, docs/internals.md | MPL-2.0 |
| https://github.com/cenkalti/backoff | 6f228bf | exponential.go | MIT |
| https://github.com/oklog/run | c769e58 | group.go, actors.go | Apache-2.0 |
| https://github.com/tmaxmax/go-sse | fb578c3 | server.go, session.go, replay.go | MIT |
| https://github.com/coder/websocket | bd31a8f | accept.go, read.go, conn.go | ISC |
| https://github.com/open-telemetry/opentelemetry-go-contrib | 71cf443 | instrumentation/net/http/otelhttp, instrumentation/google.golang.org/grpc/otelgrpc | Apache-2.0 |
| https://github.com/prometheus/client_golang | 315e020 | prometheus/promhttp | Apache-2.0 |
| https://github.com/maypok86/otter | 5d99494 | README | Apache-2.0 |
| https://github.com/uber-go/automaxprocs | 1ea14c3 | maxprocs/maxprocs.go | MIT |
| https://github.com/KimMachineGun/automemlimit | 713a988 | memlimit/memlimit.go | MIT |
| https://github.com/ardanlabs/service | 9ea1316 | api/services/auth/main.go, foundation/web, app/sdk/mid, app/sdk/errs | Apache-2.0 |
| https://github.com/go-kratos/kratos | dd0fb7d | app.go, transport/http/server.go, middleware | MIT |
| https://github.com/cloudevents/sdk-go | 05f0f68 | v2/client, v2/protocol/http | Apache-2.0 |
| https://github.com/apache/arrow-go | 56815b7 | arrow/flight | Apache-2.0 |
| https://github.com/snowflakedb/gosnowflake | ba85f95 | arrowbatches | Apache-2.0 |
| https://github.com/Azure/azure-sdk-for-go | e948fd2 | sdk/azcore/runtime, sdk/azcore/policy, sdk/azidentity, sdk/data/azcosmos | MIT |
| https://github.com/riverqueue/river | e898054 | client.go, worker.go, retry_policy.go | MPL-2.0 |
| https://github.com/modelcontextprotocol/go-sdk | eafb671 | mcp/streamable.go, mcp/server.go | MIT |
| https://github.com/uber-go/guide | 1d60a91 | style.md | Apache-2.0 |
| https://github.com/dgryski/go-perfbook | 648834a | performance.md | CC-BY-SA-4.0 |

Specifications referenced: Azure REST API Guidelines (microsoft/api-guidelines, vNext), ARM async operations and throttling docs, W3C Trace Context, RFC 6750, RFC 7515/7517/7518, RFC 8252, RFC 8785, RFC 9110, IETF draft-ietf-httpapi-ratelimit-headers, IETF draft-ietf-httpapi-idempotency-key-header, CloudEvents 1.0, MCP 2026-07-28 and the Tasks extension.
