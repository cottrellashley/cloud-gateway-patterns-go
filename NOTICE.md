# NOTICE

This repository's own code and prose are MIT licensed (see `LICENSE`).

The `docs/` quote short excerpts from the following open-source projects for the purpose of explanation and attribution. Excerpts remain under their original licences; the commit read for each is in `docs/SOURCES.md`.

| Project | Licence |
| --- | --- |
| Go standard library (golang/go) | BSD-3-Clause |
| golang.org/x/time, golang.org/x/sync | BSD-3-Clause |
| kubernetes/kubernetes (apiserver, kube-aggregator) | Apache-2.0 |
| traefik/traefik | MIT |
| caddyserver/caddy | Apache-2.0 |
| zalando/skipper | Apache-2.0 |
| vulcand/oxy | Apache-2.0 |
| go-chi/chi | MIT |
| grpc/grpc-go | Apache-2.0 |
| connectrpc/connect-go | Apache-2.0 |
| grpc-ecosystem/go-grpc-middleware | Apache-2.0 |
| pomerium/pomerium | Apache-2.0 |
| ory/oathkeeper | Apache-2.0 |
| envoyproxy/ratelimit | Apache-2.0 |
| envoyproxy/go-control-plane | Apache-2.0 |
| coreos/go-oidc | Apache-2.0 |
| lestrrat-go/jwx | MIT |
| cedar-policy/cedar-go | Apache-2.0 |
| sony/gobreaker | MIT |
| failsafe-go/failsafe-go | MIT |
| hashicorp/go-retryablehttp | MPL-2.0 |
| hashicorp/go-plugin | MPL-2.0 |
| cenkalti/backoff | MIT |
| oklog/run | Apache-2.0 |
| tmaxmax/go-sse | MIT |
| coder/websocket | ISC |
| open-telemetry/opentelemetry-go-contrib | Apache-2.0 |
| prometheus/client_golang | Apache-2.0 |
| maypok86/otter | Apache-2.0 |
| uber-go/automaxprocs | MIT |
| KimMachineGun/automemlimit | MIT |
| ardanlabs/service | Apache-2.0 |
| go-kratos/kratos | MIT |
| cloudevents/sdk-go | Apache-2.0 |
| apache/arrow-go | Apache-2.0 |
| snowflakedb/gosnowflake | Apache-2.0 |
| Azure/azure-sdk-for-go | MIT |
| riverqueue/river | MPL-2.0 |
| modelcontextprotocol/go-sdk | MIT |
| uber-go/guide | Apache-2.0 |
| dgryski/go-perfbook | CC-BY-SA-4.0 |
| kubernetes/community (contributor docs) | Apache-2.0 / CC-BY-4.0 |
| kubernetes/sample-controller | Apache-2.0 |
| caddyserver/website (docs) | Apache-2.0 |
| envoyproxy/envoy (docs) | Apache-2.0 |
| dapr/components-contrib | Apache-2.0 |
| dapr/dapr (runtime, decision records) | Apache-2.0 |
| hashicorp/terraform-plugin-go | MPL-2.0 |
| microsoft/api-guidelines | CC-BY-4.0 |
| aip-dev/google.aip.dev | CC-BY-4.0 (text), Apache-2.0 (code) |
| enihcam/azure-resource-manager-rpc (mirror of Microsoft's ARM RPC contract) | MIT |
| ThreeDotsLabs/wild-workouts-go-ddd-example | MIT |
| go-kratos/kratos-layout | MIT |
| temporalio/sdk-go | MIT |

`patterns/` contains original implementations written for this repository. Where an implementation follows an upstream algorithm closely (the EDF weighted round robin, the P2C balancer, the token bucket, the circuit breaker state machine, the run group, singleflight, the workqueue dirty/processing semantics, the Caddy-style module registry lifecycle, the Kubernetes object metadata and condition semantics), the package documentation names the source.
