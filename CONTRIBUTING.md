# Contributing to dagger-cache

## Tooling & Dependencies

| Purpose        | Library                          |
|----------------|----------------------------------|
| CLI framework  | `github.com/urfave/cli/v2`       |
| Configuration  | `github.com/spf13/viper`         |
| Logging        | `github.com/sirupsen/logrus`     |
| HTTP server    | `github.com/cloudwego/hertz`     |

## Code Style

### String formatting
**Never concatenate strings with `+`.** Use `fmt.Sprintf` for all string composition.

```go
// WRONG
name := "engine-" + version + "-" + instanceID

// RIGHT
name := fmt.Sprintf("engine-%s-%s", version, instanceID)
```

### Error handling
Wrap errors with `fmt.Errorf` using `%w`:

```go
if err != nil {
    return nil, fmt.Errorf("generate CA key: %w", err)
}
```

Import order: stdlib, blank line, third-party (Viper / logrus / hertz), blank line, project packages. Managed by `goimports` with local prefix `github.com/disaster/dagger-kubernetes`.

### Logging (logrus)
Initialize a `*logrus.Logger` with structured fields. Pass loggers via constructor injection.

```go
import "github.com/sirupsen/logrus"

func NewLogger(level, format string) *logrus.Logger {
    logger := logrus.New()
    if strings.EqualFold(format, "text") {
        logger.SetFormatter(&logrus.TextFormatter{
            TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
            FullTimestamp:   true,
        })
    } else {
        logger.SetFormatter(&logrus.JSONFormatter{
            TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
        })
    }
    lvl, err := logrus.ParseLevel(level)
    if err != nil {
        lvl = logrus.InfoLevel
    }
    logger.SetLevel(lvl)
    return logger
}
```

Use structured fields with `WithFields` and `WithError`:

```go
logger.WithFields(logrus.Fields{
    "control_addr": cfg.Addr,
    "version":      ver,
}).Info("server starting")
```

### HTTP server (hertz)
Use `github.com/cloudwego/hertz` for the HTTP server.

```go
h := server.Default(server.WithHostPorts(cfg.Addr))
h.GET("/v1/engines", handleProvisionEngine)
h.Spin()
```

Prefer `hertz` (cloudwego) for all HTTP concerns.

### SSE / streaming
Use Hertz's native `pkg/protocol/sse` for server-to-client push (no external dep). Replaces WebSocket when communication is server→client only.

```go
import "github.com/cloudwego/hertz/pkg/protocol/sse"
```

### gRPC
Project does not use gRPC. If introduced, use `github.com/cloudwego/kitex`.

### Configuration (Viper)
Config structs use `mapstructure` tags. Every field must have a default set via `v.SetDefault()`. Env vars use `DAGGER_CACHE_` prefix.

```go
type ServerConfig struct {
    ControlAddr string `mapstructure:"control_addr"`
    DataAddr    string `mapstructure:"data_addr"`
}
```

### Dependency injection
All components receive dependencies via constructors. No global state, no `init()` for wiring.

```go
func NewManager(provider Provider, store *Store, cfg ManagerConfig, logger *logrus.Logger) *Manager
```

### HTTP responses
JSON responses via `json.NewEncoder(w).Encode(v)`. Error responses as `{"message": "..."}`.

## Testing

### Coverage target: 100%
Every package must target 100% code coverage. CI enforces this with `go test -coverprofile`. Packages below 100% require explicit justification in the PR.

### Test types required
1. **Unit tests** — Test individual functions, edge cases, error paths. Use table-driven tests.
2. **Integration / functional tests** — Tests that spin up a real server and prove the feature works end-to-end with a real Dagger client or against the Dagger Cloud API contract.

### Test conventions
- Use standard `testing` package (no testify/ginkgo)
- Stub implementations for external dependencies (see `repository.StubProvider`)
- Use `t.Fatalf("describe: %v", err)` for fatal assertions
- `logrus.New()` with `Discard` output for test loggers
- Place integration tests in `tests/integration/` directory; unit tests alongside source in `*_test.go` files

### Running tests
```bash
go test -race -coverprofile=coverage.out -covermode=atomic ./...
go tool cover -func=coverage.out | grep total
```

## Linting
`golangci-lint` v2 with the project's `.golangci.yml`. Must pass before merge.

```bash
golangci-lint run ./...
```

## Dagger

The CI pipeline is a local Dagger module in `dagger/` (module name `dagger-cache`).
It delegates lint and build to the `golang` module and helm lint to the `helm`
module from `github.com/disaster37/dagger-library-go` (pinned at `2.0.10`); test,
UI, docker, and the helm template matrix are implemented locally because the
upstream modules cannot express `-race`, the UI build, the Dockerfile smoke
test, or `helm template`. See [`DAGGER.md`](./DAGGER.md) for the full reference.

```bash
# Full CI pipeline (lint + test + ui + build + docker + helm)
dagger call -m ./dagger --src . ci export --path out

# Individual functions
dagger call -m ./dagger --src . lint
dagger call -m ./dagger --src . test export --path coverage.out
dagger call -m ./dagger --src . ui export --path ui-dist
dagger call -m ./dagger --src . build export --path .
dagger call -m ./dagger --src . docker
dagger call -m ./dagger --src . helm
```

## Project structure
```
cmd/api/             Main server entry point (urfave/cli, binary `supervisor`)
cmd/ci/              CI helper binary (urfave/cli, binary `dagger-cache-ci`)
config/              Viper config loader + config.app.yaml / sample
internal/domain/     Pure entities + interfaces (stdlib only)
internal/service/    Business logic (imports domain, observ)
internal/repository/ Infrastructure implementations (imports domain + drivers)
internal/handler/    Hertz HTTP/SSE/L4 handlers (imports service, repository, domain, observ)
internal/observ/     logrus logger factory + Prometheus metrics (cross-cutting)
dagger/              Local Dagger module — CI pipeline (delegates to dagger-library-go golang/helm)
scripts/             Dev scripts (dagger-cache.sh client wrapper, update-helm-docs.sh)
tests/integration/   Black-box integration tests
docs/design/         Architecture decision records
ui/                  Vue 3 SPA (Vite + TypeScript); embeds via internal/handler/ui-dist/
deploy/docker        Docker Compose dev stack
deploy/k8s           Kubernetes manifests
deploy/helm/         Helm chart
```

## Commit messages
Follow [Conventional Commits](https://www.conventionalcommits.org/):
```
feat(api): add engine provision endpoint
fix(fleet): handle scale-down race condition
test(session): add expiry edge case coverage
```

## Documentation maintenance

Every change that introduces, modifies, or removes a feature, a configuration key, or a design decision must update the corresponding documentation:

- **`config/config.app.yaml.sample`** — Must reflect all config keys, their types, defaults, and a brief comment for each. Always kept in sync with `config/loader.go`. If a new key is added to the config struct, add the corresponding entry with a comment in the sample file.

- **`deploy/helm/dagger-kubernetes/README.md`** — Helm chart documentation covering install, upgrade, dependencies, configuration reference, production recommendations, and storage sizing. Must be updated when:
  - A new subchart dependency is added or removed
  - A new Helm value is introduced or changed
  - Auto-wiring logic changes
  - Production sizing recommendations change

- **`deploy/helm/dagger-kubernetes/values.yaml`** — The canonical default values for the Helm chart. Every `supervisor.config.*` key must match the Viper config struct. Every subchart value exposed must have a comment. Keep comments consistent with `config/config.app.yaml.sample`.

- **`docs/README.md`** — User-facing documentation covering setup, configuration, operations, and CI integrations. Must be updated when:
  - A new feature is added or removed
  - The deployment method changes
  - New ports, endpoints, or services are introduced
  - The architecture diagram needs updating
  - CI integration instructions change

- **`docs/design/`** — Architecture Decision Records (ADRs) must be created for new architectural decisions and updated when existing decisions change. ADRs explain *why* a decision was made, not just *what* was done. Follow the existing `ADR-NNN-title.md` naming convention.

### Files that must stay in sync

These three files define the same configuration schema from different perspectives. They **must** be updated together:

| File | Perspective |
|---|---|
| `config/loader.go` | Go struct definition + defaults |
| `config/config.app.yaml.sample` | Full YAML reference with comments |
| `deploy/helm/dagger-kubernetes/values.yaml` | Helm chart default values |

When adding a config key:
1. Add the struct field + `mapstructure` tag + `v.SetDefault()` in `config/loader.go`
2. Add the key + comment in `config/config.app.yaml.sample`
3. Add the key + Helm template rendering in `deploy/helm/dagger-kubernetes/templates/configmap.yaml`
4. Add the default value + comment in `deploy/helm/dagger-kubernetes/values.yaml`

Outdated docs are a bug. Documentation changes are part of the same PR as the code change.

### Wrapper script sync
`scripts/dagger-cache.sh` and `ci-integrations/gha/dagger-cache.sh` must stay
byte-identical. The copy in `ci-integrations/gha/` exists so the GitHub Actions
composite action (`ci-integrations/gha/action.yml`) can invoke it from its own
directory. When editing one, copy it verbatim to the other and verify with
`cmp scripts/dagger-cache.sh ci-integrations/gha/dagger-cache.sh`.

## PR checklist
- [ ] Tests cover new code (target 100% coverage)
- [ ] Integration test proves feature works with real Dagger client
- [ ] `golangci-lint run ./...` passes
- [ ] No string concatenation (`+`), use `fmt.Sprintf`
- [ ] Config fields have defaults in `config.Load()`
- [ ] Errors are wrapped with `%w`
- [ ] Logging uses logrus structured fields
- [ ] HTTP uses hertz (cloudwego)
- [ ] CLI uses urfave/cli
- [ ] `config/config.app.yaml.sample` updated if config keys changed
- [ ] `docs/README.md` updated if user-facing behavior changed
- [ ] `docs/design/` updated if architectural decisions changed
