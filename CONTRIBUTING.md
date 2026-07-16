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

func NewLogger(level string) *logrus.Logger {
    logger := logrus.New()
    logger.SetFormatter(&logrus.JSONFormatter{
        TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
    })
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
- Stub implementations for external dependencies (see `fleet.StubProvider`)
- Use `t.Fatalf("describe: %v", err)` for fatal assertions
- `logrus.New()` with `Discard` output for test loggers
- Place integration tests in `test/` directory; unit tests alongside source in `*_test.go` files

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

## Project structure
```
cmd/supervisor/     Main server entry point (urfave/cli)
cmd/dagger-cache-ci/ CI helper binary (urfave/cli)
internal/           Private packages (api, auth, ca, cache, config, fleet, observ, session, telemetry, version)
test/               Integration / functional tests
docs/design/        Architecture decision records
ui/                 Vue 3 SPA (Vite + TypeScript)
deploy/             Docker Compose + K8s manifests
helm/               Helm chart
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

- **`config.app.yaml.sample`** — Must reflect all config keys, their types, defaults, and a brief comment for each. Always kept in sync with `internal/config/config.go`.
- **`docs/README.md`** — User-facing documentation covering setup, configuration, operations, and CI integrations.
- **`docs/design/`** — Architecture Decision Records must be created for new architectural decisions and updated when existing decisions change.

Outdated docs are a bug. Documentation changes are part of the same PR as the code change.

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
- [ ] `config.app.yaml.sample` updated if config keys changed
- [ ] `docs/README.md` updated if user-facing behavior changed
- [ ] `docs/design/` updated if architectural decisions changed
