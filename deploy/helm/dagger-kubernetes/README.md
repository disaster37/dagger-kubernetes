# dagger-kubernetes Helm Chart

Self-hosted, Dagger-Cloud-compatible platform: remote shared cache, auto-scaling
engine fleets, live pipeline UI, and drop-in CI integration.

The chart deploys the Supervisor control plane and all required infrastructure
as Helm subchart dependencies, each toggleable independently.
<!-- version-marker -->
   [^1]: Latest released version: `0.1.0`

## Install from the GHCR OCI repository

Published images and the chart are pushed to GHCR on release tags.
This is the recommended way to install for production:

```bash
# Create a values override for your environment
cat > my-values.yaml <<'EOF'
ingress:
  hosts:
    - supv.example.com
supervisor:
  config:
    server:
      dataHostname: data.your-domain.com
      publicUrl: https://supv.example.com
EOF

# Install directly from GHCR (no local clone needed)
helm install dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml \
  --namespace dagger-stack --create-namespace \
  --set ca.crt="$(cat ca.crt)" \
  --set ca.key="$(cat ca.key)" \
  --set tls.crt="$(cat tls.crt)" \
  --set tls.key="$(cat tls.key)" \
  --set-string "auth.tokens[0]=$TOKEN"
```

List available versions:

```bash
helm show chart oci://ghcr.io/disaster/charts/dagger-kubernetes | grep version
```

## Required tools (chart dependencies)

| Dependency | Chart | Default | Purpose |
|---|---|---|---|
| OpenTelemetry Collector | `opentelemetry-collector` ([repo](https://open-telemetry.github.io/opentelemetry-helm-charts)) | enabled | OTLP ingest from Dagger CLI & supervisor; fans out to Tempo / Loki / VictoriaMetrics |
| OCI Registry | `docker-registry` ([stable](https://charts.helm.sh/stable), aliased `registry`) | enabled | Backs the remote shared cache (BuildKit cache blobs) |
| Grafana Tempo | `tempo` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Distributed tracing backend, stores OTLP traces |
| Grafana Loki | `loki` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Log aggregation backend, stores OTLP logs |
| VictoriaMetrics | `victoria-metrics-single` ([victoriametrics](https://victoriametrics.github.io/helm-charts/)) | enabled | PromQL-compatible metrics backend |
| Grafana | `grafana` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Unified dashboards with auto-provisioned datasources |

Disable any tool (and point the supervisor elsewhere) via:

```yaml
tools:
  otelCollector:
    enabled: false
  registry:
    enabled: false
  tempo:
    enabled: false
  loki:
    enabled: false
  victoria:
    enabled: false
  grafana:
    enabled: false

supervisor:
  config:
    telemetry:
      collectorUrl: "http://my-collector:4318"
      tempoUrl: "http://my-tempo:3200"
      lokiUrl: "http://my-loki:3100"
      victoriaUrl: "http://my-victoria:8428"
    cache:
      registry: "my-registry:5000/dagger-cache"
```

## Install from source (local development)

Use this approach when customizing the chart or developing locally:

```bash
# 0. Clone the repository
git clone https://github.com/disaster/dagger-kubernetes.git
cd dagger-kubernetes

# 1. Fetch dependencies (downloads subcharts to charts/)
helm dependency build deploy/helm/dagger-kubernetes

# 2. Generate TLS certificates and minting CA
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -days 3650 -nodes -keyout ca.key -out ca.crt \
  -subj "/CN=Dagger Minting CA"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -days 365 -nodes -keyout tls.key -out tls.crt \
  -subj "/CN=data.your-domain.com" \
  -addext "subjectAltName=DNS:data.your-domain.com"

# 3. Generate an API token
TOKEN=$(openssl rand -hex 32)

# 4. Copy and edit the values file
cp deploy/helm/dagger-kubernetes/values.yaml my-values.yaml
#   ... edit supervisor.config.server.{dataHostname,publicUrl} ...
#   ... set ingress.hosts to your domain ...
#   ... change grafana.adminPassword from 'admin' ...

# 5. Install
helm install dagger-kubernetes deploy/helm/dagger-kubernetes \
  -f my-values.yaml \
  --namespace dagger-stack --create-namespace \
  --set ca.crt="$(cat ca.crt)" \
  --set ca.key="$(cat ca.key)" \
  --set tls.crt="$(cat tls.crt)" \
  --set tls.key="$(cat tls.key)" \
  --set-string "auth.tokens[0]=$TOKEN"
```

## Production recommendations

### Storage

Every stateful component uses PVCs. Ensure your cluster has a default StorageClass
or override `storageClassName` per subchart:

```yaml
tempo:
  persistence:
    enabled: true
    size: 100Gi
    storageClassName: "fast-ssd"

loki:
  singleBinary:
    persistence:
      enabled: true
      size: 100Gi
      storageClassName: "fast-ssd"

victoria:
  server:
    persistentVolume:
      enabled: true
      size: 100Gi
      storageClassName: "fast-ssd"

registry:
  persistence:
    enabled: true
    size: 200Gi
    storageClassName: "fast-ssd"
```

### Resource sizing

Minimum recommended resources for a production cluster handling ~50 CI pipelines/hour:

```yaml
supervisor:
  config:
    fleet:
      minReplicasPerVersion: 1    # keep one warm engine per version
      maxReplicasPerVersion: 5
      maxSessionsPerReplica: 8
      engineStorageSize: "100Gi"
      engineCPULimit: "4000m"
      engineMemoryLimit: "16Gi"

opentelemetry-collector:
  resources:
    requests: { cpu: 200m, memory: 256Mi }
    limits: { cpu: 1000m, memory: 512Mi }

tempo:
  tempo:
    retention: 720h   # 30 days
    resources:
      requests: { cpu: 500m, memory: 1Gi }
      limits: { cpu: 2000m, memory: 2Gi }

loki:
  singleBinary:
    replicas: 1
    resources:
      requests: { cpu: 500m, memory: 1Gi }
      limits: { cpu: 2000m, memory: 2Gi }

victoria:
  server:
    resources:
      requests: { cpu: 500m, memory: 512Mi }
      limits: { cpu: 2000m, memory: 2Gi }

grafana:
  resources:
    requests: { cpu: 100m, memory: 128Mi }
    limits: { cpu: 200m, memory: 256Mi }
```

### High availability

- **Supervisor**: run at least 2 replicas (`replicaCount: 2`). All state is in the
  session store (in-memory), so sessions shift on pod restart. Clients will
  reconnect automatically.
- **Loki**: use `deploymentMode: SimpleScalable` with S3/GCS object storage for
  multi-replica setups. SingleBinary is sufficient for up to ~20 GB/day.
- **Tempo**: use object storage (S3/GCS) for persistence beyond pod lifetime.
  Local filesystem is acceptable for dev/staging.
- **VictoriaMetrics**: single replica is sufficient for most workloads. Add
  `vmalert` and `vmagent` for HA and alerting.

### Security

- Change `grafana.adminPassword` from `admin`.
- Restrict network policies so only the collector can reach Tempo/Loki/Victoria.
- Use Kubernetes Secrets for all credentials (tokens, TLS keys).
- Enable `podSecurityContext` and `securityContext` (enabled by default).

## Configuration reference

### Top-level values

| Key | Description | Default |
|---|---|---|
| `image.repository` | Supervisor image | `ghcr.io/disaster/dagger-kubernetes` |
| `image.tag` | Image tag (defaults to `Chart.appVersion`) | `""` |
| `replicaCount` | Supervisor replicas | `2` |
| `namespace` | Target namespace (empty = release namespace) | `""` |
| `supervisor.config.*` | Supervisor runtime config | see `values.yaml` |
| `auth.tokens` | Static bearer tokens | `[]` |
| `ca.crt` / `ca.key` | Minting CA (PEM) | `""` |
| `tls.crt` / `tls.key` | Data-plane TLS (PEM) | `""` |
| `ingress.enabled` | Enable control-plane Ingress | `true` |
| `autoscaling.enabled` | Enable HPA for supervisor | `false` |
| `serviceMonitor.enabled` | Enable Prometheus ServiceMonitor | `false` |

### Tool toggles

| Key | Default | Description |
|---|---|---|
| `tools.otelCollector.enabled` | `true` | OTel Collector for OTLP ingest |
| `tools.registry.enabled` | `true` | OCI registry for cache storage |
| `tools.tempo.enabled` | `true` | Grafana Tempo for traces |
| `tools.loki.enabled` | `true` | Grafana Loki for logs |
| `tools.victoria.enabled` | `true` | VictoriaMetrics for metrics |
| `tools.grafana.enabled` | `true` | Grafana dashboards |

### Auto-wiring

When a tool is enabled, the supervisor configuration is automatically wired to the
dependency's in-cluster Service using Go template expressions. The mapping is:

| Config key | Template helper | Target service |
|---|---|---|
| `telemetry.collectorUrl` | `dagger-kubernetes.collectorUrl` | `<release>-opentelemetry-collector:4318` |
| `telemetry.tempoUrl` | `dagger-kubernetes.tempoUrl` | `<release>-tempo:3100` |
| `telemetry.lokiUrl` | `dagger-kubernetes.lokiUrl` | `<release>-loki:3100` |
| `telemetry.victoriaUrl` | `dagger-kubernetes.victoriaUrl` | `<release>-victoria-metrics-single:8428` |
| `cache.registry` | `dagger-kubernetes.cacheRegistry` | `<release>-docker-registry:5000/dagger-cache` |

Grafana datasources (Tempo, Loki, VictoriaMetrics) are auto-provisioned via a
ConfigMap with label `grafana_datasource: "1"`, picked up by the `k8s-sidecar`.

## Upgrading

From the OCI repository (recommended):

```bash
helm upgrade dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml --namespace dagger-stack
```

From source:

```bash
helm dependency update deploy/helm/dagger-kubernetes
helm upgrade dagger-kubernetes deploy/helm/dagger-kubernetes \
  -f my-values.yaml --namespace dagger-stack
```

## Uninstalling

```bash
helm uninstall dagger-kubernetes -n dagger-stack

# PVCs and PVs are NOT deleted by default. Clean up manually if needed:
kubectl delete pvc -n dagger-stack -l app.kubernetes.io/instance=dagger-kubernetes
```
