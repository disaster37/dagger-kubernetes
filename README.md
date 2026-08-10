# dagger-cache

A self-hosted, Dagger-Cloud-compatible platform: remote shared cache,
auto-scaling engine fleets, live pipeline UI, and drop-in CI integration.

**Full documentation:** [`docs/README.md`](docs/README.md)

Quick links:

- [Quick start (Docker / Kubernetes / client)](docs/README.md#quick-start)
- [Configuration reference](docs/README.md#configuration) — copy
  [`config.app.yaml.sample`](config.app.yaml.sample) to get started
- [Architecture](docs/README.md#architecture)
- [CI integrations](docs/README.md#ci-integrations) (GitHub Actions, Jenkins, Drone)

## Layout

| Path                | Contents                                              |
|---------------------|-------------------------------------------------------|
| `cmd/supervisor`    | The Supervisor server (control + data plane + OTLP). |
| `cmd/dagger-cache-ci`, `hack/dagger-cache.sh` | CI helper + client wrapper.           |
| `internal/`         | Config, API, fleet, cache, CA, telemetry, …          |
| `dagger/`           | Local Dagger module — CI pipeline.                   |
| `hack/`             | Dev scripts (`dagger-cache.sh`, `update-helm-docs.sh`). |
| `deploy/docker`     | Local dev compose stack.                              |
| `deploy/k8s`        | Kubernetes manifests.                                 |
| `deploy/helm`       | Helm chart.                                           |
| `ci-integrations/`  | GHA action, Jenkins shared lib, Drone extension.     |
| `ui/`               | Vite SPA pipeline UI.                                 |
| `docs/README.md`    | Complete usage guide.                                 |
| `DAGGER.md`         | Dagger CI command reference.                         |
| `config.app.yaml`   | Live example config (minimal).                         |
| `config.app.yaml.sample` | Fully-commented config reference.                |

## License

See [LICENSE](LICENSE).
