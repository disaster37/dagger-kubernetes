# dagger-kubernetes Helm Chart

Self-hosted, Dagger-Cloud-compatible platform: remote shared cache, auto-scaling
engine fleets, live pipeline UI, and drop-in CI integration.
<!-- version-marker -->
[^1]: Latest released version: `0.1.0`


The chart deploys the Supervisor control plane and all required infrastructure
as Helm subchart dependencies, each toggleable independently.
<!-- version-marker: Latest released version `0.1.0` -->

## Install from the GHCR OCI repository

Published images and the chart are pushed to GHCR on release tags.
This is the recommended way to install for production. The minting CA and the
Raft transport CA are **auto-bootstrapped** on first boot — no manual
certificate generation is required (see
[TLS & certificates](#tls--certificates)).

```bash
# Create a values override for your environment: the UI and data-plane URLs
# are computed automatically from the exposition (see "Exposition & URLs").
cat > my-values.yaml <<'EOF'
ingress:
  hosts:
    - supv.example.com
  tls:
    - hosts: [supv.example.com]
      secretName: my-tls-secret

dataIngress:
  enabled: true
  host: data.supv.example.com
EOF

# Install directly from GHCR (no local clone needed). The embedded TLS provider
# auto-issues the minting CA + a self-signed server cert on first boot.
helm install dagger-kubernetes oci://ghcr.io/disaster37/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml \
  --namespace dagger-stack --create-namespace

# Optionally pre-seed credentials (or create API tokens from the UI):
helm upgrade dagger-kubernetes oci://ghcr.io/disaster37/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml --namespace dagger-stack \
  --set auth.bootstrapAdmin.password="change-me"
```

List available versions:

```bash
helm show chart oci://ghcr.io/disaster37/charts/dagger-kubernetes | grep version
```

> For a publicly trusted server certificate (Let's Encrypt), set
> `tls.provider: cert-manager` and enable `dataCert` /
> `dataIngress.tls.secretName` — see [TLS & certificates](#tls--certificates).
> The minting CA is still auto-bootstrapped.

## Exposition & URLs

The chart never asks for URLs — `server.public_url` (UI + API) and
`server.data_hostname` (engine data plane) are computed from the exposition:

| Exposition | `public_url` | `data_hostname` |
|---|---|---|
| `ingress.enabled` | `https://<host>` with `ingress.tls`, `http://<host>` without | — |
| `dataIngress.enabled` | — | `<dataIngress.host>` (TLS passthrough, port 443) |
| `service.*.type: LoadBalancer` | `https://<service.control.host>[:<port>]` | `<service.data.host>[:<port>]` |
| `service.*.type: NodePort` | `https://<service.control.host>[:<port>]` | `<service.data.host>:<service.data.nodePort>` |
| `ClusterIP` only | internal `https://<release>-control.<ns>.svc:<port>` | internal `<release>-data.<ns>.svc:<port>` |

`service.control.host` / `service.data.host` are required when the respective
Service is exposed via LoadBalancer/NodePort without an ingress (the chart
cannot know the LB hostname or the auto-assigned nodePort).

The **cache vhost** (`server cache public_host`, the host engines push/pull
through) is derived the same way: `cache.<control-plane host>` by default, or
any explicit name via `supervisor.config.cache.publicHost`. When
`ingress.enabled`, the chart automatically adds the cache host as a second
Ingress host rule (routing to the `-control` Service) and appends it to
`ingress.tls[].hosts` — you never list it in `ingress.hosts` yourself. Its DNS
must resolve to the same ingress IP.

> **Wildcard certificates:** the derived name `cache.<host>` is a
> second-level subdomain (`cache.dagger.company.com`), which a single-level
> wildcard certificate (`*.company.com`) does **not** cover. In that case set
> `supervisor.config.cache.publicHost` to a one-level name, e.g.
> `dagger-cache.company.com` — the override is free-form.

Container paths and ports are fixed (control `:8080`, data `:8443`, raft
`:8081`, data dir `/var/lib/dagger-kubernetes`) — only the Service ports are
configurable (`service.control.port`, `service.data.port`).

## Required tools (chart dependencies)

| Dependency | Chart | Default | Purpose |
|---|---|---|---|
| OpenTelemetry Collector | `opentelemetry-collector` ([repo](https://open-telemetry.github.io/opentelemetry-helm-charts)) | enabled | OTLP ingest from Dagger CLI & supervisor; fans out to Tempo / Loki / VictoriaMetrics |
| OCI Registry | `docker-registry` ([stable](https://charts.helm.sh/stable), aliased `registry`) | enabled | Backs the remote shared cache (BuildKit cache blobs) |
| Grafana Tempo | `tempo` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Distributed tracing backend, stores OTLP traces |
| Grafana Loki | `loki` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Log aggregation backend, stores OTLP logs |
| VictoriaMetrics | `victoria-metrics-single` ([victoriametrics](https://victoriametrics.github.io/helm-charts/)) | enabled | PromQL-compatible metrics backend |
| Grafana | `grafana` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Unified dashboards with auto-provisioned datasources |

Disable any tool (and point the supervisor elsewhere) via its own `enabled`
flag:

```yaml
opentelemetry-collector:
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
      registries:
        - id: my-registry
          internalAddr: "my-registry:5000"
```

## Install from source (local development)

Use this approach when customizing the chart or developing locally:

```bash
# 0. Clone the repository
git clone https://github.com/disaster37/dagger-kubernetes.git
cd dagger-kubernetes

# 1. Fetch dependencies (downloads subcharts to charts/)
helm dependency build deploy/helm/dagger-kubernetes

# 2. Copy and edit the values file
cp deploy/helm/dagger-kubernetes/values.yaml my-values.yaml
#   ... set ingress.hosts to your domain (URLs are computed automatically) ...
#   ... (grafana.adminPassword is auto-generated by default; set it only with a rotation workflow) ...

# 3. Install. No certificate generation needed: the minting CA + Raft CA are
#    auto-bootstrapped and the embedded provider issues the server cert.
helm install dagger-kubernetes deploy/helm/dagger-kubernetes \
  -f my-values.yaml \
  --namespace dagger-stack --create-namespace \
  --set auth.bootstrapAdmin.password="change-me"
```

## TLS & certificates

There are three certificate authorities/keypairs in play, and **none of them
require manual generation** in a standard Helm install:

| Certificate | Auto-bootstrapped | Notes |
|---|---|---|
| **Minting CA** (`<release>-minting-ca`) | Yes | Signer of short-lived engine client certs. Ordinal 0 generates a goca CA on first boot and writes it to the Secret; other pods poll it. Set `tls.caCrt`/`tls.caKey` only to bring an existing CA. |
| **Raft transport CA** (`<release>-raft-ca`) | Yes | Internal mTLS for the Raft transport. Same bootstrap pattern (see [Raft](#raft-distributed-store)). |
| **Server certificate** (control + data plane) | `embedded` (default) | Issued by the minting CA, self-signed. SANs cover the data host, cache vhost, and pod names automatically. |

### TLS providers

`tls.provider` selects where the server certificate comes from. The minting
CA is auto-bootstrapped for **every** provider.

- **`embedded` (default)** — the minting CA issues a self-signed server
  certificate. Zero config; no public trust, but mTLS authentication is
  unaffected.
- **`cert-manager`** — cert-manager issues a publicly trusted certificate
  (e.g. Let's Encrypt). Requires cert-manager installed + a `ClusterIssuer`.
  Enable `dataCert` (chart-rendered `Certificate`) or
  `dataIngress.tls.secretName` (Let's Encrypt via the Ingress annotation). The
  chart auto-wires `certPath`/`keyPath` to the mounted Secret
  (`/etc/dagger-kubernetes/data-tls/tls.crt` + `.key`).
- **`external`** — bring your own PEM files via `tls.certPath`/`keyPath`
  (paths inside the supervisor container).

### cert-manager example

```bash
# 1. Create a ClusterIssuer once (cluster-wide)
kubectl apply -f - <<'EOF'
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: you@example.com
    privateKeySecretRef:
      name: letsencrypt-prod-key
    solvers:
      - http01:
          ingress:
            class: nginx
EOF

# 2. Install with the cert-manager provider (chart-rendered Certificate)
helm install dagger-kubernetes oci://ghcr.io/disaster37/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml --namespace dagger-stack \
  --set tls.provider=cert-manager \
  --set dataCert.enabled=true \
  --set dataCert.issuerName=letsencrypt-prod \
  --set dataIngress.enabled=true \
  --set dataIngress.host=data.your-domain.com
```

> **Cache vhost SAN:** the cache proxy shares the control-plane TLS listener,
> so the server certificate must include the cache vhost (derived
> `cache.<control-plane host>` or the `supervisor.config.cache.publicHost`
> override) as a SAN. The `embedded` provider adds it automatically; the
> chart's `dataCert` template only adds `dataIngress.host`, so when using
> cert-manager add the cache host to the `Certificate` `dnsNames` (via a
> custom `Certificate`) or use a wildcard certificate.

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
      storageClass: "fast-ssd"

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
    storageClass: "fast-ssd"

supervisor:
  persistence:
    enabled: true
    storageClass: "fast-ssd"
    size: 10Gi
```

> **Note**: Supervisor persistence uses `storageClass` (rendered as Kubernetes
> `storageClassName`) under `supervisor.persistence`. Subchart persistence keys
> match each subchart's native API: `storageClassName` for Tempo and
> VictoriaMetrics; `storageClass` for docker-registry and Loki. All accept an
> empty string for the cluster default.

### Resource sizing

Minimum recommended resources for a production cluster handling ~50 CI pipelines/hour:

```yaml
supervisor:
  config:
    fleet:
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

- **Supervisor**: the RBAC/trace/cache-routing state lives in a Raft cluster
  (`supervisor.config.raft.*`). The chart ships a **StatefulSet** (default
  `replicaCount: 3`) with a **headless Service** for stable pod DNS; peers are
  discovered by DNS arithmetic and the Raft transport is **mTLS** (internal
  goca CA shared via the `<release>-raft-ca` Secret). See
  [Raft (distributed store)](#raft-distributed-store) below. Sessions are
  in-memory and shift on pod restart; clients reconnect automatically.
  There is **no HPA**: the supervisor is a quorum-based Raft store, so the
  voter count must follow `supervisor.replicaCount` exactly and cannot track
  an autoscaler.
- **Loki**: use `deploymentMode: SimpleScalable` with S3/GCS object storage for
  multi-replica setups. SingleBinary is sufficient for up to ~20 GB/day.
- **Tempo**: use object storage (S3/GCS) for persistence beyond pod lifetime.
  Local filesystem is acceptable for dev/staging.
- **VictoriaMetrics**: single replica is sufficient for most workloads. Add
  `vmalert` and `vmagent` for HA and alerting.

### Security

- `grafana.adminPassword` defaults to `""` — the Grafana subchart auto-generates
  a random 40-char password stored in the `<release>-grafana` Secret. Retrieve it
  with `kubectl get secret <release>-grafana -o jsonpath="{.data.admin-password}" -n <ns> | base64 -d`. Set an explicit value only with a rotation workflow.
- The supervisor ServiceAccount uses a **namespaced** Role/RoleBinding by default
  (`supervisor.serviceAccount.clusterScope: false`), limited to the release
  namespace. Set `clusterScope: true` only if engine fleets span multiple
  namespaces — this grants cluster-wide access and is security-sensitive.
- The `<release>-raft-ca` and `<release>-minting-ca` Secrets contain the
  internal raft CA **private key** and the engine-client minting CA **private
  key** respectively (any pod may issue certificates from them). The chart's
  RBAC already restricts `secrets` verbs to the supervisor ServiceAccount
  within the release namespace — do **not** broaden it or share these Secrets
  with other workloads.
- Restrict network policies so only the collector can reach Tempo/Loki/Victoria.
- Use Kubernetes Secrets for all credentials (tokens, TLS keys).
- Enable `supervisor.podSecurityContext` and `supervisor.securityContext` (enabled by default).
- The minting CA and Raft CA are auto-bootstrapped on first boot (see
  [TLS & certificates](#tls--certificates)); back up the resulting
  `<release>-minting-ca` and `<release>-raft-ca` Secrets, or set
  `tls.caCrt`/`tls.caKey` explicitly to reuse a known CA.
- Configure `ingress.tls` for the control-plane Ingress; the default ships
  with no TLS (`tls: []`) to avoid binding a placeholder secret.

### Pipeline history retention

- Tempo spans are **not** deleted by the supervisor's history purge — set
  `tempo.tempo.retention` to match (or exceed) `supervisor.config.history.gc.maxAge`
  so spans age out alongside the purge.
- Loki log deletion is enabled by default: the chart runs the Loki compactor
  with `limits_config.deletion_mode: filter-and-delete`,
  `compactor.retention_enabled: true`, and a `delete_request_store`
  (`filesystem` by default). For object-storage deployments, set
  `loki.loki.compactor.delete_request_store` to the S3/GCS bucket used for
  delete requests.
- VictoriaMetrics `delete_series` is admin-only — ensure no `-deleteAuthKey`
  is set on the VictoriaMetrics server (or provide the matching key to the
  supervisor) so series deletion is not rejected.

## Configuration reference

### Top-level values

| Key | Description | Default |
|---|---|---|
| `supervisor.enabled` | Enable the supervisor (control plane + data plane) and its resources | `true` |
| `supervisor.image.repository` | Supervisor image | `ghcr.io/disaster37/dagger-kubernetes` |
| `supervisor.image.tag` | Image tag (defaults to `Chart.appVersion`) | `""` |
| `supervisor.replicaCount` | Supervisor pods = Raft voters (single source of truth) | `3` |
| `supervisor.persistence.enabled` | Enable per-pod PVC for the Raft data directory (StatefulSet volumeClaimTemplate) | `true` |
| `supervisor.resources` | Supervisor container resources | see `values.yaml` |
| `supervisor.serviceAccount.annotations` | ServiceAccount annotations | `{}` |
| `supervisor.serviceAccount.clusterScope` | Use ClusterRole instead of namespaced Role | `false` |
| `supervisor.podSecurityContext` | Pod-level security context | see `values.yaml` |
| `supervisor.securityContext` | Container-level security context | see `values.yaml` |
| `namespace` | Target namespace (empty = release namespace) | `""` |
| `supervisor.config.*` | Supervisor runtime config (see `values.yaml`; URLs are computed, see [Exposition & URLs](#exposition--urls)) | see `values.yaml` |
| `auth.bootstrapAdmin.username` / `.password` | Bootstrap admin credentials (password empty = random, logged once) | `admin` / `""` |
| `auth.bootstrapAdmin.secretRef.name` / `.key` | Reference an existing Secret instead of plaintext `password` (takes precedence; key empty = `password`) | `""` / `"password"` |
| `auth.jwt.secret` | JWT signing secret (empty = auto-generated, persisted in DB) | `""` |
| `auth.jwt.secretRef.name` / `.key` | Reference an existing Secret instead of plaintext `secret` (takes precedence; key empty = `secret`) | `""` / `"secret"` |
| `auth.jwt.*` | JWT access/refresh TTLs | `15m` / `168h` |
| `auth.oauth.*` | OAuth2 provider config (github/oidc) | see `values.yaml` |
| `auth.oauth.clientSecretRef.name` / `.key` | Reference an existing Secret instead of plaintext `clientSecret` (takes precedence; key empty = `client_secret`) | `""` / `"client_secret"` |
| `auth.cookie.*` | Session cookie names + Secure flag | see `values.yaml` |
| `auth.cors.allowedOrigins` | Cross-origin Origin allowlist | `[]` |
| `tls.provider` | Server cert source: `embedded` \| `cert-manager` \| `external` | `embedded` |
| `tls.clientCertTtl` | Engine client cert TTL | `2h` |
| `tls.caCrt` / `tls.caKey` | Minting CA (PEM). Empty = **auto-bootstrap** on first boot | `""` |
| `tls.crt` / `tls.key` | Server cert/key PEM (only `external` provider). cert-manager auto-wires this | `""` |
| `dataCert.enabled` | Render a cert-manager `Certificate` for the data plane | `false` |
| `dataIngress.tls.secretName` | cert-manager (Let's Encrypt) secret for the data plane | `""` |
| `ingress.enabled` | Enable control-plane Ingress | `true` |
| `service.control.host` / `service.data.host` | Routable hostname when exposed via LoadBalancer/NodePort (no ingress) | `""` |
| `serviceMonitor.enabled` | Enable Prometheus ServiceMonitor | `false` |

### Tool toggles

Each dependency is toggled with its own `enabled` flag:

| Key | Default | Description |
|---|---|---|
| `opentelemetry-collector.enabled` | `true` | OTel Collector for OTLP ingest |
| `registry.enabled` | `true` | OCI registry for cache storage |
| `tempo.enabled` | `true` | Grafana Tempo for traces |
| `loki.enabled` | `true` | Grafana Loki for logs |
| `victoria.enabled` | `true` | VictoriaMetrics for metrics |
| `grafana.enabled` | `true` | Grafana dashboards |

### Auto-wiring

When a tool is enabled, the supervisor configuration is automatically wired to the
dependency's in-cluster Service using Go template expressions. The mapping is:

| Config key | Template helper | Target service |
|---|---|---|
| `telemetry.collectorUrl` | `dagger-kubernetes.collectorUrl` | `<release>-opentelemetry-collector:4318` |
| `telemetry.tempoUrl` | `dagger-kubernetes.tempoUrl` | `<release>-tempo:3100` |
| `telemetry.lokiUrl` | `dagger-kubernetes.lokiUrl` | `<release>-loki:3100` |
| `telemetry.victoriaUrl` | `dagger-kubernetes.victoriaUrl` | `<release>-victoria-metrics-single:8428` |
| `server.public_url` | `dagger-kubernetes.publicUrl` | computed from ingress / service exposition |
| `server.data_hostname` | `dagger-kubernetes.dataHostname` | computed from dataIngress / service exposition |
| `cache.public_host` | `dagger-kubernetes.cachePublicHost` | `supervisor.config.cache.publicHost`, else `cache.<control-plane host>` |
| `cache.internal_addr` | `dagger-kubernetes.cacheInternalAddr` | `<release>-registry:5000` (when `registry.enabled`) |
| `cache.registry` | `dagger-kubernetes.cacheRegistry` | `<cachePublicHost>/dagger-cache` (public ref emitted to clients) |

### Raft (distributed store)

The supervisor persists RBAC state, trace metadata, and the cache routing tables
in a **Hashicorp Raft** replicated state machine (ADR-015/ADR-016). Raft always
runs — a single-node deployment is a one-voter cluster. The chart ships a
**StatefulSet** (with `volumeClaimTemplates` per-pod PVCs, `podManagementPolicy:
Parallel`) and a **headless Service** (`clusterIP: None`) whose DNS A records
give each pod a stable identity for peer discovery.

| Value | Default | Description |
|---|---|---|
| `supervisor.replicaCount` | `3` | Supervisor pod count = Raft voter count (derived, single source of truth). Use an odd number ≥ 3 for fault tolerance. |
| `supervisor.config.raft.tls.enabled` | `true` | mTLS for the Raft transport. |
| `supervisor.config.raft.tls.clientAuth` | `true` | Require + verify peer client certs (mTLS). |

Everything else is **fixed or derived by the chart**: the data dir is
`/var/lib/dagger-kubernetes` (per-pod PVC), the Raft transport binds `:8081`
and advertises `<pod>.<headless>.<ns>.svc.cluster.local`, node IDs are the
StatefulSet pod names (downward-API), peers are discovered via DNS from the
headless Service, and the internal CA lives in the `<release>-raft-ca` Secret.

**TLS auto-CA:** pod-0 generates the internal CA with `goca`, writes it to the
`<release>-raft-ca` Secret, and issues itself a leaf; pods 1..N-1 poll the
Secret (bounded by `leader_wait_timeout`) before issuing their own leaves.
Leaves are reused across restarts and re-issued within a 7-day expiry margin.
The engine-client **minting CA** is likewise auto-bootstrapped and shared
across pods via the `<release>-minting-ca` Secret (ordinal 0 generates it, the
rest poll) so engine mTLS client certs minted by any pod are trusted by every
pod's data-plane listener. Both Secrets hold CA **private keys** — keep them
RBAC-restricted to the supervisor ServiceAccount (see [Security](#security)).

**Follower reads:** every pod waits until *a* leader exists, then serves stale
local reads; writes on a follower return `ErrNotLeader` (503) and clients retry.

**Scale-up / scale-down:** the leader reconciles membership (`raft.AddVoter` /
`raft.RemoveServer`) automatically. To scale up, bump `supervisor.replicaCount`
(the voter count follows it) and rolling-restart. To scale down, shrink it,
rolling-restart, then delete the removed pod.

> **Fresh start:** the Raft store starts empty on first boot. There is **no
> migration** from any prior SQLite-backed release — existing SQLite data is
> intentionally not carried over. The bootstrap-admin flow provisions a fresh
> admin when the user count is 0.

### Cache proxy (engine → Supervisor → registry)

The Supervisor acts as an OCI Distribution v2 reverse proxy in front of the
cache registry(ies). Configure it under `supervisor.config.cache`:

| Value | Default | Description |
|---|---|---|
| `supervisor.config.cache.backend` | `registry` | `registry` (OCI) or `s3`. |
| `supervisor.config.cache.publicHost` | `""` | Dedicated cache vhost engines push/pull through. Empty ⇒ `cache.<control-plane host>`. Must differ from the control-plane host. The emitted public ref is always `<publicHost>/dagger-cache`, tagged per engine version (`:V<maj>-<min>-<patch>`). |
| `supervisor.config.cache.registries` | `[]` | Multi-backend list of `{id, internalAddr, username, password, passwordSecret}`. The proxy load-balances least-charged first (registry cache size). Empty ⇒ single-backend mode (the bundled registry at `<release>-registry:5000`). |

When `ingress.enabled`, the chart adds a second Ingress host rule for the cache
vhost (routing to the `-control` Service) and appends it to `ingress.tls[].hosts`
— no need to list the cache host in `ingress.hosts`. Override the vhost with
`supervisor.config.cache.publicHost` (empty = derived `cache.<control-plane
host>`; any format works, e.g. `dagger-cache.company.com`). The control-plane
TLS certificate must include the cache vhost as a SAN.

Grafana datasources (Tempo, Loki, VictoriaMetrics) are auto-provisioned via a
ConfigMap with label `grafana_datasource: "1"`, picked up by the `k8s-sidecar`.

### Pipeline history retention

The supervisor auto-purges pipeline history (trace metadata + Loki logs +
VictoriaMetrics series) for traces whose last update is older than `maxAge`.
Configure it under `supervisor.config.history`:

| Value | Default | Description |
|---|---|---|
| `supervisor.config.history.gc.enabled` | `false` | Master switch for history auto-purge. |
| `supervisor.config.history.gc.maxAge` | `720h` | Purge traces older than this (30d). |
| `supervisor.config.history.gc.schedule` | `1h` | Sweeper ticker interval. |

## Parameters

### Chart metadata

| Name | Type | Default | Description |
|---|---|---|---|
| `nameOverride` | string | `""` | Override the chart name used in resource labels. |
| `fullnameOverride` | string | `""` | Override the full name of the release. |
| `namespace` | string | `""` | Namespace for the supervisor and subchart dependencies. Defaults to the release namespace when empty. |

### Supervisor

| Name | Type | Default | Description |
|---|---|---|---|
| `supervisor.enabled` | bool | `true` | Enable the supervisor (control plane + data plane) and its resources. |
| `supervisor.image.repository` | string | `ghcr.io/disaster37/dagger-kubernetes` | Supervisor container image repository. |
| `supervisor.image.tag` | string | `""` | Image tag (empty defaults to Chart.appVersion). |
| `supervisor.image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `supervisor.image.pullSecrets` | array | `[]` | Image pull secrets for private registries. |
| `supervisor.replicaCount` | int | `3` | Number of supervisor pods. Single source of truth for the Raft cluster size: each supervisor pod is one Raft voter, so the voter count is derived from this value (never asked separately). Use an odd number >= 3 for quorum fault tolerance (a 2-node cluster has no failure tolerance). |
| `supervisor.resources.requests.cpu` | string | `250m` | Supervisor CPU request. |
| `supervisor.resources.requests.memory` | string | `256Mi` | Supervisor memory request. |
| `supervisor.resources.limits.cpu` | string | `1000m` | Supervisor CPU limit. |
| `supervisor.resources.limits.memory` | string | `1Gi` | Supervisor memory limit. |
| `supervisor.persistence.enabled` | bool | `true` | Enable a per-pod PVC for the Raft data directory (StatefulSet volumeClaimTemplate). disabled = emptyDir (dev only). |
| `supervisor.persistence.storageClass` | string | `""` | StorageClass for the supervisor PVC (empty = cluster default). |
| `supervisor.persistence.size` | string | `2Gi` | PVC size for each supervisor pod's Raft data directory. |
| `supervisor.podAnnotations` | object | `{}` | Annotations added to the supervisor pod. |
| `supervisor.podSecurityContext.runAsNonRoot` | bool | `true` | Run supervisor container as non-root. |
| `supervisor.podSecurityContext.runAsUser` | int | `10001` | User ID for the supervisor container. |
| `supervisor.podSecurityContext.runAsGroup` | int | `10001` | Group ID for the supervisor container. |
| `supervisor.podSecurityContext.fsGroup` | int | `10001` | Filesystem group for mounted volumes. |
| `supervisor.securityContext.allowPrivilegeEscalation` | bool | `false` | Allow privilege escalation. |
| `supervisor.securityContext.readOnlyRootFilesystem` | bool | `true` | Mount root filesystem as read-only. |
| `supervisor.securityContext.capabilities.drop` | array | `["ALL"]` | Linux capabilities to drop. |
| `supervisor.nodeSelector` | object | `{}` | Node selector for supervisor pod scheduling. |
| `supervisor.tolerations` | array | `[]` | Tolerations for supervisor pod scheduling. |
| `supervisor.affinity` | object | `{}` | Affinity rules for supervisor pod scheduling. |
| `supervisor.serviceAccount.annotations` | object | `{}` | Annotations for the supervisor ServiceAccount. |
| `supervisor.serviceAccount.clusterScope` | bool | `false` | Use ClusterRole instead of namespaced Role/RoleBinding (cluster-wide access - security-sensitive). |
| `supervisor.extraEnv` | array | `[]` | Extra environment variables for the supervisor container. |

### Supervisor configuration

| Name | Type | Default | Description |
|---|---|---|---|
| `supervisor.config.raft.tls.enabled` | bool | `true` | Enable mTLS for the Raft transport. |
| `supervisor.config.raft.tls.clientAuth` | bool | `true` | Require and verify peer client certs (mTLS). |
| `supervisor.config.telemetry.collectorUrl` | string | `""` | OTel collector URL (auto-wired when the opentelemetry-collector subchart is enabled). |
| `supervisor.config.telemetry.tempoUrl` | string | `""` | Tempo URL for trace queries (auto-wired when the tempo subchart is enabled). |
| `supervisor.config.telemetry.lokiUrl` | string | `""` | Loki URL for log queries (auto-wired when the loki subchart is enabled). |
| `supervisor.config.telemetry.victoriaUrl` | string | `""` | VictoriaMetrics URL for metric queries (auto-wired when the victoria subchart is enabled). |
| `supervisor.config.cache.backend` | string | `"registry"` | Cache backend type: registry (OCI) or s3. |
| `supervisor.config.cache.publicHost` | string | `""` | Dedicated cache vhost engines push/pull through (empty = derived `cache.<control-plane host>`). Must differ from the control-plane host. Also drives the extra ingress host rule + TLS SAN entry when ingress is enabled. |
| `supervisor.config.cache.authToken` | string | `""` | Engine→proxy bearer for the cache. Rendered into the engine-registry-auth Secret (key `token`); the supervisor reads it from there. Empty = "placeholder". |
| `supervisor.config.cache.registries` | array | `[]` | Multi-backend list of {id, internalAddr, username, password, passwordSecret}. Empty = single-backend mode (the bundled registry). |
| `supervisor.config.cache.registries[].passwordSecret.name` | string | — | K8s Secret name holding the backend password (rendered as a reference, never the secret itself). |
| `supervisor.config.cache.registries[].passwordSecret.key` | string | `"password"` | Key inside the Secret holding the password (empty = "password"). |
| `supervisor.config.cache.s3.bucket` | string | `""` | S3 bucket name (when backend=s3). |
| `supervisor.config.cache.s3.region` | string | `""` | S3 region (when backend=s3). |
| `supervisor.config.history.gc.enabled` | bool | `false` | Master switch for the history auto-purge sweeper. |
| `supervisor.config.history.gc.maxAge` | string | `"720h"` | Purge traces older than this (30d default). |
| `supervisor.config.history.gc.schedule` | string | `"1h"` | History sweeper ticker interval. |
| `supervisor.config.fleet.maxReplicasPerVersion` | int | `3` | Maximum engine replicas per Dagger version. |
| `supervisor.config.fleet.maxSessionsPerReplica` | int | `8` | Maximum concurrent sessions per engine replica. |
| `supervisor.config.fleet.replicaIdleTtl` | string | `"5m"` | Idle TTL before scaling down an engine replica. |
| `supervisor.config.fleet.versionRetention` | string | `"24h"` | Idle version GC: delete a version's StatefulSet + Service after this long with zero replicas and no pinned sessions (`<= 0` disables; positive values `< 1m` rejected). |
| `supervisor.config.fleet.engineImageRegistry` | string | `"registry.dagger.io/engine"` | Engine container image registry. |
| `supervisor.config.fleet.engineStorageClass` | string | `""` | StorageClass for engine PVCs (empty = cluster default). |
| `supervisor.config.fleet.engineStorageSize` | string | `"50Gi"` | PVC size for each engine. |
| `supervisor.config.fleet.enginePvcLabels` | object | `{}` | Extra labels added to engine PVCs (merged with managed labels app/version, which take precedence). |
| `supervisor.config.fleet.engineCPURequest` | string | `"500m"` | Engine CPU request. |
| `supervisor.config.fleet.engineCPULimit` | string | `"2000m"` | Engine CPU limit. |
| `supervisor.config.fleet.engineMemoryRequest` | string | `"1Gi"` | Engine memory request. |
| `supervisor.config.fleet.engineMemoryLimit` | string | `"8Gi"` | Engine memory limit. |
| `supervisor.config.fleet.engineTerminationGraceSeconds` | int | `120` | Engine pod termination grace period. |
| `supervisor.config.fleet.enginePullPolicy` | string | `"IfNotPresent"` | Engine image pull policy. |
| `supervisor.config.fleet.enginePrivileged` | bool | `true` | Run engine container in privileged mode (required by BuildKit/Dagger engine). |
| `supervisor.config.fleet.engineNodeSelector` | object | `{}` | Node selector for engine pods. |
| `supervisor.config.fleet.engineTolerations` | array | `[]` | Tolerations for engine pods. |
| `supervisor.config.fleet.engineExtraArgs` | array | `[]` | Additional CLI args passed to the engine. |
| `supervisor.config.fleet.engineExtraEnv` | object | `{}` | Extra env vars on engine pods. |
| `supervisor.config.fleet.engineExtraEnvFrom` | object | `{}` | Extra env vars sourced from Secret keys on engine pods: map of env name -> {secretName, key}. |
| `supervisor.config.fleet.engineCaSecret` | string | `""` | Secret with custom CA PEM bundle for engines (empty = disabled). |
| `supervisor.config.fleet.engineCaSecretKey` | string | `"ca.crt"` | Key inside engineCaSecret containing the CA cert. |
| `supervisor.config.fleet.engineDockerConfig` | string | `""` | Base64-encoded Docker config.json (auths for private registries). Stored verbatim in the engine-registry-auth Secret `data` key `.dockerconfigjson` and base64-decoded exactly once on mount, so engine pods read raw JSON at `/etc/dagger/.dockerconfigjson`. Empty = `e30K` (`{}`). NOT an imagePullSecret. |
| `supervisor.config.fleet.engineDebug` | bool | `false` | Enable engine.toml [debug]. |
| `supervisor.config.fleet.engineLogFormat` | string | `"json"` | Engine log format (json, text; empty omits). |
| `supervisor.config.fleet.engineRegistryMirrors` | object | `{}` | Engine registry mirrors (e.g. {"docker.io": ["mirror.gcr.io"]}). |
| `supervisor.config.leaseTtl` | string | `"2m"` | Engine session lease TTL. |
| `supervisor.config.version.floor` | string | `"v0.19.0"` | Minimum supported Dagger engine version. |
| `supervisor.config.version.allowlist` | array | `["0.19", "0.20", "0.21"]` | Allowed Dagger versions (major.minor prefixes; empty = admit all versions >= floor). |
| `supervisor.config.logLevel` | string | `"info"` | Supervisor log level. |
| `supervisor.config.logFormat` | string | `"json"` | Supervisor log format (json, text). |
| `supervisor.config.otel.otlpEndpoint` | string | `""` | Supervisor OTLP export endpoint (empty disables). |

### Authentication

| Name | Type | Default | Description |
|---|---|---|---|
| `auth.bootstrapAdmin.username` | string | `"admin"` | Bootstrap admin username. |
| `auth.bootstrapAdmin.password` | string | `""` | Bootstrap admin password. When empty, a random password is generated and logged once at first boot. |
| `auth.bootstrapAdmin.secretRef.name` | string | `""` | K8s Secret name holding the bootstrap admin password (takes precedence over `auth.bootstrapAdmin.password`; the chart-managed `<release>-admin-password` Secret is not rendered). |
| `auth.bootstrapAdmin.secretRef.key` | string | `"password"` | Key inside the Secret holding the password. |
| `auth.jwt.secret` | string | `""` | JWT signing secret (HS256). Empty = auto-generated and persisted in DB. |
| `auth.jwt.secretRef.name` | string | `""` | K8s Secret name holding the JWT secret (takes precedence over `auth.jwt.secret`; the chart-managed `<release>-jwt` Secret is not rendered). |
| `auth.jwt.secretRef.key` | string | `"secret"` | Key inside the Secret holding the JWT secret. |
| `auth.jwt.accessTtl` | string | `"15m"` | JWT access token TTL. |
| `auth.jwt.refreshTtl` | string | `"168h"` | JWT refresh token TTL. |
| `auth.oauth.enabled` | bool | `false` | Enable OAuth2 authentication. |
| `auth.oauth.provider` | string | `"github"` | OAuth2 provider: "github" or "oidc". |
| `auth.oauth.clientId` | string | `""` | OAuth2 client ID. |
| `auth.oauth.clientSecret` | string | `""` | OAuth2 client secret (rendered into the `<release>-oauth` Secret). |
| `auth.oauth.clientSecretRef.name` | string | `""` | K8s Secret name holding the OAuth2 client secret (takes precedence over `auth.oauth.clientSecret`; the chart-managed `<release>-oauth` Secret is not rendered). |
| `auth.oauth.clientSecretRef.key` | string | `"client_secret"` | Key inside the Secret holding the client secret. |
| `auth.oauth.redirectUrl` | string | `""` | OAuth2 redirect URL (empty = computed). |
| `auth.oauth.allowedOrgs` | array | `[]` | Allowed OAuth organizations (github: org membership; oidc: deprecated alias for allowedGroups). |
| `auth.oauth.allowedTeams` | array | `[]` | (github) Allowed "org/team" slugs; when set with allowedOrgs, both must be satisfied. |
| `auth.oauth.allowedGroups` | array | `[]` | (oidc) Allowed provider group names (groups-claim allowlist; union with allowedOrgs). |
| `auth.oauth.groupMappings` | array | `[]` | Regex group mapping: list of {pattern, replacement} mapping provider groups to supervisor group names (first-match-wins; no match drops the group). |
| `auth.oauth.defaultGroup` | string | `""` | Default group for OAuth users. |
| `auth.oauth.cookieSecure` | bool | `false` | Force the Secure flag on the oauth_state cookie (set true when TLS terminates in front of the supervisor). |
| `auth.oauth.issuerUrl` | string | `""` | (oidc) OIDC issuer URL (e.g. https://dex.example.com). |
| `auth.oauth.scopes` | array | `["openid", "profile", "email"]` | (oidc) OIDC scopes; "openid" is always appended. |
| `auth.oauth.usernameClaim` | string | `"preferred_username"` | (oidc) OIDC username claim (fallback: email). |
| `auth.oauth.groupsClaim` | string | `"groups"` | (oidc) OIDC groups claim (array or single string). |
| `auth.cookie.accessName` | string | `"dagger_kubernetes_access"` | Access-JWT session cookie name (httpOnly). |
| `auth.cookie.refreshName` | string | `"dagger_kubernetes_refresh"` | Refresh-JWT session cookie name (httpOnly). |
| `auth.cookie.secure` | bool | `false` | Force the Secure flag on session cookies. |
| `auth.cors.allowedOrigins` | array | `[]` | Exact-match Origin allowlist for cross-origin API access (empty = same-origin only). |

### TLS and certificates

| Name | Type | Default | Description |
|---|---|---|---|
| `tls.provider` | string | `"embedded"` | TLS provider for the server certificate: "embedded", "cert-manager", or "external". |
| `tls.certPath` | string | `""` | Server certificate path (only "external"). cert-manager auto-wires the mounted data-tls secret. |
| `tls.keyPath` | string | `""` | Server key path (only "external"). |
| `tls.clientCertTtl` | string | `"2h"` | Client certificate TTL (engine session certs minted from the CA). |
| `tls.caCrt` | string | `""` | PEM-encoded minting CA certificate. Leave empty to AUTO-BOOTSTRAP on first boot. |
| `tls.caKey` | string | `""` | PEM-encoded minting CA private key. Leave empty to AUTO-BOOTSTRAP on first boot. |
| `tls.crt` | string | `""` | PEM-encoded server certificate (only needed for provider "external"). |
| `tls.key` | string | `""` | PEM-encoded server private key (only needed for provider "external"). |

### Services

| Name | Type | Default | Description |
|---|---|---|---|
| `service.control.type` | string | `ClusterIP` | Control-plane service type. |
| `service.control.port` | int | `80` | Control-plane service port (maps to the fixed supervisor :8080). |
| `service.control.nodePort` | string | `""` | Control-plane service node port (when type=NodePort). |
| `service.control.host` | string | `""` | Routable hostname/IP for the control plane when exposed via LoadBalancer/NodePort. |
| `service.data.type` | string | `ClusterIP` | Data-plane service type. ClusterIP when using ingress. |
| `service.data.port` | int | `443` | Data-plane service port (maps to the fixed supervisor :8443). |
| `service.data.nodePort` | string | `""` | Data-plane node port (when type=NodePort and no ingress). |
| `service.data.host` | string | `""` | Routable hostname/IP for the data plane when exposed via LoadBalancer/NodePort. |

### Ingress

| Name | Type | Default | Description |
|---|---|---|---|
| `ingress.enabled` | bool | `true` | Enable control-plane Ingress (web UI + API). |
| `ingress.className` | string | `""` | Ingress class name (empty = default ingress controller). |
| `ingress.annotations` | object | `{}` | Ingress annotations. |
| `ingress.hosts` | array | see `values.yaml` | Ingress host rules. |
| `ingress.hosts[].host` | string | `supv.example.com` | Hostname for the ingress rule. |
| `ingress.hosts[].paths[].path` | string | `/` | URL path for the ingress rule. |
| `ingress.hosts[].paths[].pathType` | string | `Prefix` | Path matching type (Prefix, Exact, ImplementationSpecific). |
| `ingress.tls` | array | `[]` | Ingress TLS configuration. |

### Data-plane Ingress (mTLS passthrough)

| Name | Type | Default | Description |
|---|---|---|---|
| `dataIngress.enabled` | bool | `false` | Expose data plane via TLS passthrough on a dedicated host. |
| `dataIngress.host` | string | `"data.supv.example.com"` | Hostname for the data plane. |
| `dataIngress.className` | string | `""` | Ingress class name. |
| `dataIngress.annotations` | object | `{}` | Additional ingress annotations. |
| `dataIngress.tls.secretName` | string | `""` | Secret name for the data-plane TLS cert. |

### Data-plane certificate (cert-manager)

| Name | Type | Default | Description |
|---|---|---|---|
| `dataCert.enabled` | bool | `false` | Provision a certificate for the data plane via cert-manager. |
| `dataCert.issuerName` | string | `"letsencrypt-prod"` | cert-manager ClusterIssuer name. |
| `dataCert.issuerKind` | string | `"ClusterIssuer"` | cert-manager issuer kind: ClusterIssuer or Issuer. |
| `dataCert.secretName` | string | `""` | Secret name where cert-manager stores the certificate. |

### Prometheus ServiceMonitor

| Name | Type | Default | Description |
|---|---|---|---|
| `serviceMonitor.enabled` | bool | `false` | Enable Prometheus ServiceMonitor. |
| `serviceMonitor.labels` | object | `{}` | Labels applied to the ServiceMonitor. |
| `serviceMonitor.interval` | string | `30s` | Scrape interval. |
| `serviceMonitor.scrapeTimeout` | string | `10s` | Scrape timeout. |

### Subchart overrides

| Name | Type | Default | Description |
|---|---|---|---|
| `opentelemetry-collector.enabled` | bool | `true` | Install OpenTelemetry Collector subchart. |
| `registry.enabled` | bool | `true` | Install Docker Registry subchart (cache backend). |
| `tempo.enabled` | bool | `true` | Install Grafana Tempo subchart (traces). |
| `tempo.tempo.retention` | string | `720h` | Trace retention duration. Set to match (or exceed) `supervisor.config.history.gc.maxAge`. |
| `loki.enabled` | bool | `true` | Install Grafana Loki subchart (logs). |
| `victoria.enabled` | bool | `true` | Install VictoriaMetrics subchart (metrics). |
| `grafana.enabled` | bool | `true` | Install Grafana subchart (dashboards with auto-provisioned datasources). |

## Upgrading

### Breaking: HPA removed, raft voter count derived from `supervisor.replicaCount`

- `supervisor.autoscaling` (HPA) has been **removed**. The supervisor is a
  quorum-based Raft store: its voter count must equal the pod count exactly
  and cannot track an HPA (scaling down disturbs quorum, scaling up would
  spawn independent single-voter clusters). The chart fails-closed and
  refuses to render if your values still carry `supervisor.autoscaling.enabled:
  true` — delete the `supervisor.autoscaling` block.
- `supervisor.config.raft.replicas` has been **removed**. The Raft voter
  count is now derived from `supervisor.replicaCount` (each supervisor pod is
  one voter) and injected as `DAGGER_KUBERNETES_RAFT_REPLICAS`. The chart
  fails-closed if your values still set `supervisor.config.raft.replicas` —
  delete the key; to change the cluster size, change
  `supervisor.replicaCount` (odd number ≥ 3 for fault tolerance) and
  rolling-restart.

### Breaking: supervisor pod values moved under `supervisor:` (v0.2.0)

The following top-level keys have been moved under the `supervisor:` block:
`image`, `replicaCount`, `resources`, `persistence`, `podAnnotations`,
`podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`,
`affinity`, `serviceAccount` (`autoscaling` was moved too but has since been
removed entirely — see above).

**You must prefix these keys with `supervisor:` in your values override files.**
Helm will **silently ignore** old top-level keys, causing the supervisor to fall
back to chart defaults. Example migration:

```yaml
# BEFORE (old — will be silently ignored)
image:
  tag: "0.2.0"
replicaCount: 3
persistence:
  enabled: true
  size: 5Gi

# AFTER (new)
supervisor:
  image:
    tag: "0.2.0"
  replicaCount: 3
  persistence:
    enabled: true
    size: 5Gi
```

Loki now enables PVC persistence by default (`loki.singleBinary.persistence.enabled: true`,
`size: 20Gi`). To keep the old ephemeral (emptyDir) behavior, set
`loki.singleBinary.persistence.enabled: false`.

From the OCI repository (recommended):

```bash
helm upgrade dagger-kubernetes oci://ghcr.io/disaster37/charts/dagger-kubernetes \
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
