# AGENTS.md — dagger-kubernetes coding conventions for AI agents

## Project identity
Module: `github.com/disaster/dagger-kubernetes`
A self-hosted, Dagger-Cloud-compatible platform: remote shared cache, auto-scaling engine fleets, live pipeline UI, and drop-in CI integration.

## Local test environment

Before deploying or updating this application on the local Kubernetes
cluster, read `AGENTS.local.md`. It is the machine-specific source of truth for:

- cluster access (kubeconfig `/home/user/.kube/home`, context `home`),
- the live release (`dagger-kubernetes-test`) and its image
  (`docker.io/disaster/dagger-kubernetes:dev`),
- the build → push → helm-upgrade → rollout workflow,
- the mandatory agent + human verification steps.

Every new feature or bug fix MUST be redeployed and validated on that cluster
(see the mandate in `AGENTS.local.md` §6).

## Required libraries (do not deviate)
| Purpose        | Import                                |
|----------------|---------------------------------------|
| CLI            | `github.com/urfave/cli/v2`            |
| Configuration  | `github.com/spf13/viper`              |
| Logging        | `github.com/sirupsen/logrus`          |
| HTTP           | `github.com/cloudwego/hertz`          |
| SSE/Streaming  | `github.com/cloudwego/hertz` (native) |

## Coding rules (mandatory)

### Strings
```go
// NEVER concatenate with +
// ALWAYS use fmt.Sprintf
name := fmt.Sprintf("engine-%s-%s", version, instanceID)
url := fmt.Sprintf("%s/api/v1/logs?trace_id=%s", base, id)
```

### Errors
```go
// Wrap with %w
return nil, fmt.Errorf("mint cert: %w", err)

// Error responses to clients
writeError(w, http.StatusNotFound, "lease not found")
```

### Logging (logrus)
```go
logger.WithFields(logrus.Fields{
    "method": "POST",
    "path":   "/v1/engines",
    "status": status,
    "duration_ms": duration,
}).Info("request completed")

logger.WithError(err).Error("failed to acquire replica")
```

### HTTP server (hertz)
```go
h := server.Default(
    server.WithHostPorts(cfg.ControlAddr),
)

h.GET("/v1/engines", handleProvisionEngine)
h.POST("/v1/traces", handleOTLPTraces)

h.GET("/healthz", handleHealthz)
h.GET("/readyz", handleReadyz)

h.Spin()
```

### SSE / streaming (hertz native)
Use Hertz's built-in `pkg/protocol/sse` for server-to-client push. No external WebSocket dependency.

```go
import "github.com/cloudwego/hertz/pkg/protocol/sse"

func handleTraceLive(ctx context.Context, c *app.RequestContext) {
    c.SetStatusCode(consts.StatusOK)
    c.Response.Header.Set("Content-Type", "text/event-stream")
    c.Response.Header.Set("Cache-Control", "no-cache")
    c.Response.Header.Set("Connection", "keep-alive")

    w := sse.NewWriter(c)
    defer w.Close()

    traceID := c.Param("traceID")
    // subscribe to hub, push events via w.WriteEvent(...)
}
```

For bidirectional communication, use `hertz-contrib/websocket` (archived) or keep `gorilla/websocket`. Prefer SSE when only server→client push is needed.

### gRPC
This project does not use gRPC. If gRPC is introduced, use `github.com/cloudwego/kitex`.

### CLI entry points (urfave/cli)
```go
func main() {
    app := &cli.App{
        Name:  "supervisor",
        Usage: "dagger-kubernetes control plane",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "config",
                Value: "config.app.yaml",
                Usage: "path to config file",
            },
        },
        Action: run,
    }
    if err := app.Run(os.Args); err != nil {
        log.Fatal(err)
    }
}
```

### Viper config
- Use `mapstructure` tags on config structs
- Every field needs `v.SetDefault()` in `config.Load()`
- Env prefix: `DAGGER_KUBERNETES_`
- Gracefully handle missing config file (skip if `ConfigFileNotFoundError`)

### Testing
- Target 100% coverage for every package
- Standard `testing` package only — no testify, no ginkgo
- Table-driven tests for parameterized cases
- Test loggers: `logrus.New()` with output set to `io.Discard`
- Stub external deps (see `repository.StubProvider` as example)
- Integration tests in `tests/integration/` directory must prove features work against real Dagger client API contract

### Project structure
```
cmd/api/main.go                 — urfave/cli entry point (control plane API server, binary name `supervisor`)
cmd/ci/main.go                  — urfave/cli entry point (CI wrapper, binary name `dagger-kubernetes-ci`)
config/loader.go                — Viper config loading + defaults (root package, returns *domain.Config)
config/config.app.yaml          — sample/default config
internal/domain/                — pure entities + interfaces (stdlib ONLY)
internal/service/               — business logic (imports domain, observ)
internal/repository/            — infrastructure implementations (imports domain + drivers)
internal/handler/               — Hertz HTTP/SSE/L4 handlers (imports service, repository, domain, observ)
internal/observ/                — logrus logger factory + Prometheus metrics (cross-cutting)
dagger/                         — local Dagger module (CI pipeline)
scripts/                        — dev scripts (dagger-kubernetes.sh, update-helm-docs.sh)
deploy/helm/                    — Helm chart
tests/integration/              — black-box integration tests
docs/design/                    — architecture decision records
```

Dependency rule (enforced by imports): `handler → service → domain ← repository`.
`domain` imports stdlib only. `observ` is a documented cross-cutting exception
(ADR-009): `service` and `handler` may inject `*observ.Metrics` / `*logrus.Logger`.

### Dependency injection pattern
```go
type Server struct {
    cfg          *ServerConfig
    logger       *logrus.Logger
    fleetManager *service.Manager
    sessions     domain.SessionStore
}

func NewServer(
    cfg *ServerConfig,
    logger *logrus.Logger,
    fleetManager *service.Manager,
    sessions domain.SessionStore,
) *Server {
    return &Server{
        cfg:          cfg,
        logger:       logger,
        fleetManager: fleetManager,
        sessions:     sessions,
    }
}
```

### Middleware pattern (net/http)
Use HTTP middleware for cross-cutting concerns (auth, logging, tracing):
```go
func withMiddleware(next http.Handler, logger *logrus.Logger) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        logger.WithFields(logrus.Fields{
            "method": r.Method,
            "path":   r.URL.Path,
        }).Info("request")

        rr := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
        next.ServeHTTP(rr, r)

        logger.WithFields(logrus.Fields{
            "method":   r.Method,
            "path":     r.URL.Path,
            "status":   rr.status,
            "duration": time.Since(start),
        }).Info("response")
    })
}
```

### Documentation maintenance
Every change that introduces, modifies, or removes a feature, a config key, or a design decision must update:
- `config/config.app.yaml.sample` — must reflect all config keys, types, defaults, and comments; always in sync with `config/loader.go`
- `docs/README.md` — user-facing docs covering setup, configuration, operations, CI integrations
- `docs/design/` — ADRs must be created for new architectural decisions and updated when existing ones change

Outdated docs are a bug. Docs changes are part of the same changeset as the code.

### File format
- Go files formatted with `gofmt` + `goimports`
- `goimports` local prefix: `github.com/disaster/dagger-kubernetes`
- YAML config files: 2-space indent
