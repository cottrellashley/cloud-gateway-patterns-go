# Configuration

**Pattern:** static configuration (addresses, timeouts, identities, feature flags) comes from environment variables with typed defaults and a printable, secret-masked dump at startup; dynamic configuration (routes, policies, limits) is hot-reloaded (`docs/03-traffic/dynamic-configuration.md`); secrets come from Key Vault references, never from files in the image.

## Shape

ardanlabs/service's `conf` approach (struct tags with defaults, `mask`, env prefix) is the right ergonomics:

```go
cfg := struct {
    conf.Version
    Web struct {
        ReadTimeout     time.Duration `conf:"default:5s"`
        WriteTimeout    time.Duration `conf:"default:10s"`
        IdleTimeout     time.Duration `conf:"default:120s"`
        ShutdownTimeout time.Duration `conf:"default:20s"`
        APIHost         string        `conf:"default:0.0.0.0:6000"`
        DebugHost       string        `conf:"default:0.0.0.0:6010"`
    }
    DB struct {
        Password string `conf:"default:postgres,mask"`
        ...
    }
}{ Build: tag, Desc: "Auth" }
help, err := conf.Parse("AUTH", &cfg)   // env AUTH_WEB_READ_TIMEOUT etc.
```

With the standard library alone: a struct, `flag` for a handful of operator overrides, `os.LookupEnv` with parsing helpers, and a `String()` that redacts fields tagged secret. `knadh/koanf` or `spf13/viper` when you need file + env + remote merging; keep the resulting struct immutable after startup.

## Rules

1. **Defaults are production-safe**; local development overrides, not the reverse.
2. **Validate at startup and fail fast** with every error listed, not the first one.
3. **Log the effective configuration once** at startup with secrets masked (the `mask` tag) and the config version; it is the first thing read in an incident.
4. **No reads of `os.Getenv` outside the config package.** Everything flows from the struct.
5. **Environment per deployment, image per build**: the same container image runs in dev, staging and prod with different environment; never bake environment-specific files.
6. **Feature flags** are configuration with a kill switch semantics: default off, read through the hot-config snapshot so they can be flipped without a restart, and logged when they change.

## Azure specifics

* App Service / Container Apps "application settings" become environment variables; Key Vault references (`@Microsoft.KeyVault(SecretUri=…)`) resolve at start with the app's managed identity.
* Azure App Configuration (with Key Vault references) is the managed store for dynamic settings; poll it from `patterns/config.Watch` or use its push refresh.
* Config revision = Container Apps revision; a bad config rolls back by routing traffic to the previous revision.

## Versioning the config

Hash the effective static config (`patterns/config.Version`) and expose it in `/readyz?verbose` and the startup log so that "which config is this replica running" is answerable without SSH.
