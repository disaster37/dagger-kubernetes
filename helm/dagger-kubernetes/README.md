# dagger-kubernetes Helm Chart

Self-hosted, Dagger-Cloud-compatible platform: remote shared cache, auto-scaling
engine fleets, live pipeline UI, and drop-in CI integration.

## Required tools (chart dependencies)

The supervisor depends on two supporting services, integrated as Helm chart
dependencies and toggleable independently. When enabled, the supervisor
configuration is wired automatically to the dependency's in-cluster Service.

| Dependency | Chart | Default | Purpose |
|------------|-------|---------|---------|
| OpenTelemetry Collector | `opentelemetry-collector` ([repo](https://open-telemetry.github.io/opentelemetry-helm-charts)) | enabled | OTLP ingest from Dagger CLI & supervisor; fans out to Tempo / Loki / Prometheus |
| OCI registry | `docker-registry` ([stable](https://charts.helm.sh/stable), aliased `registry`) | enabled | Backs the remote shared cache (BuildKit cache blobs) |

Disable either (and point the supervisor elsewhere) via:

```yaml
tools:
  otelCollector:
    enabled: false
  registry:
    enabled: false

supervisor:
  config:
    telemetry:
      collectorUrl: "http://my-collector:4318"
    cache:
      registry: "my-registry:5000/dagger-cache"
```

## Install

```bash
# 1. Fetch dependencies (creates charts/ from Chart.lock)
helm dependency build helm/dagger-kubernetes

# 2. Configure secrets (minting CA, TLS, bearer tokens)
cp helm/dagger-kubernetes/values.yaml my-values.yaml
#   ... edit ca.crt/ca.key, tls.crt/tls.key, auth.tokens ...

# 3. Install
helm install dagger-kubernetes helm/dagger-kubernetes -f my-values.yaml -n dagger-kubernetes --create-namespace
```

## Configuration

| Key | Description | Default |
|-----|-------------|---------|
| `image.repository` | Supervisor image | `ghcr.io/disaster/dagger-kubernetes` |
| `image.tag` | Image tag (defaults to `Chart.appVersion`) | `""` |
| `replicaCount` | Supervisor replicas | `2` |
| `namespace` | Target namespace | `dagger-kubernetes` |
| `supervisor.config.*` | Supervisor runtime config (server, cache, fleet, version…) | see `values.yaml` |
| `auth.tokens` | Static bearer tokens | `[]` |
| `ca.crt` / `ca.key` | Minting CA (PEM) | `""` |
| `tls.crt` / `tls.key` | Data-plane TLS (PEM) | `""` |
| `ingress.enabled` | Enable control-plane Ingress | `true` |
| `autoscaling.enabled` | Enable HPA | `false` |
| `serviceMonitor.enabled` | Enable Prometheus ServiceMonitor | `false` |
| `tools.otelCollector.enabled` | Deploy OTel Collector | `true` |
| `tools.registry.enabled` | Deploy OCI registry | `true` |

### Using the published OCI chart

Published images and the chart are pushed to GHCR on release tags:

```bash
helm install dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version <version> -f my-values.yaml
```
