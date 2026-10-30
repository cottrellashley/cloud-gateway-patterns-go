# Azure identity: credentials, managed identity, on-behalf-of

**Pattern:** the gateway and services authenticate to Azure with a managed identity through `azidentity`, never with secrets in configuration; local development uses the developer's CLI login through the same credential type; downstream user-delegated calls use On-Behalf-Of only when an admin can register the apps, otherwise the service identity plus audit.

## `DefaultAzureCredential` is a chain you can read

`sdk/azidentity/default_azure_credential.go` @e948fd2 builds the chain in this order and lets `AZURE_TOKEN_CREDENTIALS` narrow it:

```go
selected = env | workloadIdentity | managedIdentity | az | azd | azurePowerShell
if atc, ok := os.LookupEnv(azureTokenCredentials); ok {
    switch {
    case atc == "dev":  selected = az | azd | azurePowerShell
    case atc == "prod": selected = env | workloadIdentity | managedIdentity
    ...
```

Set `AZURE_TOKEN_CREDENTIALS=prod` in deployed environments: the chain then never tries CLI credentials, which both speeds up startup and removes a surprising fallback. Managed identity probing (IMDS) is enabled "only when the chain contains another credential" (`dac: selected^managedIdentity != 0`).

`ChainedTokenCredential.GetToken` remembers the first credential that worked and, by default, never iterates again (`c.successfulCredential`), serialising concurrent first calls with a `sync.Cond`; it only moves to the next source on `credentialUnavailable`, not on `AuthenticationFailedError`. So a misconfigured first credential fails fast rather than silently falling through — the behaviour you want in production.

```go
cred, err := azidentity.NewDefaultAzureCredential(nil)         // prod: managed identity; dev: az login
client, err := azcosmos.NewClient(endpoint, cred, nil)
```

## Pipeline, retries, logging

Every Azure SDK client runs requests through `azcore`'s policy pipeline. Its retry policy (`sdk/azcore/runtime/policy_retry.go`) is the one to copy for your own clients (see `docs/03-traffic/retries-backoff-circuit-breakers.md`): exponential with jitter, `TryTimeout` per attempt, `Retry-After` honoured and capped by `MaxRetryDelay`, non-retriable errors (`errorinfo.NonRetriable`) and the parent context respected, body rewound per try, response drained before retrying. `policy.RetryOptions{MaxRetries, TryTimeout, RetryDelay, MaxRetryDelay, StatusCodes, ShouldRetry}` are the knobs; `runtime.WithRetryOptions(ctx, opts)` overrides per call.

## On-Behalf-Of

`azidentity.NewOnBehalfOfCredentialWithSecret / WithCertificate / WithClientAssertions(tenantID, clientID, userAssertion, ...)` implements the OBO exchange: the middle tier (your gateway) presents the user's token as the assertion and gets a token for the downstream API. Constraints from the identity platform docs: user principals only (no app-only tokens), delegated scopes only, the assertion's audience must be the middle tier, and conditional-access claims challenges must be surfaced to the client as a `401` with `WWW-Authenticate`.

If you cannot register the middle-tier application (no admin consent, no Snowflake External OAuth integration), OBO is unavailable; the gateway then calls downstream with its own managed identity or a service credential and carries the end user in audit, query tags and policy — see the Atlas report sections 5 and 8.

## Local development

`az login` and the same `DefaultAzureCredential` give developers real tokens with their own identity; never ship client secrets in `.env` files. For Snowflake, key-pair JWT auth for the service identity is a one-hour JWT signed with a private key kept in Key Vault; `gosnowflake` builds it for you.

## Secrets

* Key Vault references in App Service / Container Apps settings resolve secrets into environment variables at start; rotate by versioning the secret and restarting a revision.
* The gateway reads secrets once at startup into memory; nothing writes them to disk or logs. `conf:"mask"` in ardanlabs/service's config struct (`Password string \`conf:"default:postgres,mask"\``) is the right idea: mark secrets so the config dump redacts them.

## Checklist

- [ ] `AZURE_TOKEN_CREDENTIALS=prod` in deployed environments; user-assigned managed identity per workload.
- [ ] One `TokenCredential` per process, shared by every SDK client (token caching is inside it).
- [ ] Retry options tuned per dependency; never stack SDK retries on top of your own.
- [ ] Secrets via Key Vault references; masked in config dumps.
