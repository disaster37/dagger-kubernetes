# dagger-cache

A self-hosted, Dagger-Cloud-compatible platform: remote shared cache,
auto-scaling engine fleets, live pipeline UI, and drop-in CI integration.

**Full documentation:** [`docs/README.md`](docs/README.md)

Quick links:

- [Quick start (Docker / Kubernetes / client)](docs/README.md#quick-start)
- [Configuration reference](docs/README.md#configuration) — copy
  [`config/config.app.yaml.sample`](config/config.app.yaml.sample) to get started
- [Architecture](docs/README.md#architecture)
- [CI integrations](docs/README.md#ci-integrations) (GitHub Actions, Jenkins, Drone)

## Layout

| Path                | Contents                                              |
|---------------------|-------------------------------------------------------|
| `cmd/api`           | The Supervisor server (control + data plane + OTLP). |
| `cmd/ci`, `scripts/dagger-cache.sh` | CI helper + client wrapper.           |
| `internal/`         | Layered clean architecture: `domain`, `service`, `repository`, `handler`, `observ`. |
| `config/`           | Viper config loader + `config.app.yaml` / sample.    |
| `dagger/`           | Local Dagger module — CI pipeline.                   |
| `scripts/`          | Dev scripts (`dagger-cache.sh`, `update-helm-docs.sh`). |
| `deploy/docker`     | Local dev compose stack.                              |
| `deploy/k8s`        | Kubernetes manifests.                                 |
| `deploy/helm`       | Helm chart.                                           |
| `ci-integrations/`  | GHA action, Jenkins shared lib, Drone extension.     |
| `ui/`               | Vite SPA pipeline UI.                                 |
| `tests/integration` | Black-box integration tests.                          |
| `docs/README.md`    | Complete usage guide.                                 |
| `DAGGER.md`         | Dagger CI command reference.                         |
| `config/config.app.yaml`   | Live example config (minimal).                         |
| `config/config.app.yaml.sample` | Fully-commented config reference.                |

## License

See [LICENSE](LICENSE).
