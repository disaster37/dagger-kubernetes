# dagger-kubernetes Helm Chart

Self-hosted, Dagger-Cloud-compatible platform: S3-backed cache with
warm-starting engine PVCs, auto-scaling engine fleets, live pipeline UI, and
drop-in CI integration.
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

> For a publicly trusted server certificate (Let's Encrypt), enable `dataCert`
> or set `dataIngress.tls.secretName` — the chart auto-switches
> `supervisor.dataplane.tls.provider` and auto-wires cert paths. See [TLS &
> certificates](#tls--certificates). The minting CA is still auto-bootstrapped.

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

Container paths and ports are fixed (control `:8080`, data `:8443`, raft
`:8081`, data dir `/var/lib/dagger-kubernetes`) — only the Service ports are
configurable (`service.control.port`, `service.data.port`).

## Required tools (chart dependencies)

| Dependency | Chart | Default | Purpose |
|---|---|---|---|
| OpenTelemetry Collector | `opentelemetry-collector` ([repo](https://open-telemetry.github.io/opentelemetry-helm-charts)) | enabled | OTLP ingest from Dagger CLI & supervisor; fans out to Tempo / Loki / VictoriaMetrics |
| MinIO | `minio` ([charts.min.io](https://charts.min.io/)) | enabled | S3-compatible object store backing the local image cache and the CLI cache; creates the `dagger-cache` and `image-cache` buckets on install |
| Grafana Tempo | `tempo` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Distributed tracing backend, stores OTLP traces |
| Grafana Loki | `loki` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Log aggregation backend, stores OTLP logs |
| VictoriaMetrics | `victoria-metrics-single` ([victoriametrics](https://victoriametrics.github.io/helm-charts/)) | enabled | PromQL-compatible metrics backend |
| Grafana | `grafana` ([grafana](https://grafana.github.io/helm-charts)) | enabled | Unified dashboards with auto-provisioned datasources |

Disable any tool (and point the supervisor elsewhere) via its own `enabled`
flag:

```yaml
opentelemetry-collector:
  enabled: false
minio:
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
      collectorUrl: "http://my-collector.telemetry.svc:4318"
      tempoUrl: "http://my-tempo.telemetry.svc:3200"
      lokiUrl: "http://my-loki.telemetry.svc:3100"
      victoriaUrl: "http://my-victoria.telemetry.svc:8428"
    cache:
      sync:
        s3Endpoint: "my-minio.cache.svc:9000"
```

In-cluster endpoints must use the `<service>.<namespace>.svc` form (never bare
service names or `.svc.<cluster-domain>` FQDNs) so a single `.svc` entry in
`NO_PROXY` covers every in-cluster component when `HTTP_PROXY` is set (see
`CONTRIBUTING.md`).

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
| **Minting CA** (`<release>-minting-ca`) | Yes | Signer of short-lived engine client certs. Ordinal 0 generates a goca CA on first boot and writes it to the Secret; other pods poll it. Set `supervisor.dataplane.tls.caCrt`/`caKey` only to bring an existing CA. |
| **Raft transport CA** (`<release>-raft-ca`) | Yes | Internal mTLS for the Raft transport. Same bootstrap pattern (see [Raft](#raft-distributed-store)). |
| **Server certificate** (control + data plane) | `embedded` (default) | Issued by the minting CA, self-signed. SANs cover the data host, cache vhost, and pod names automatically. |

### TLS providers

`supervisor.dataplane.tls.provider` selects where the server certificate comes
from. The minting CA is auto-bootstrapped for **every** provider. The Helm chart
auto-switches this value when you enable `dataCert.enabled` (→ `"cert-manager"`)
or set `dataIngress.tls.secretName` (→ `"external"`), so you rarely need to set
it manually.

- **`embedded` (default)** — the minting CA issues a self-signed server
  certificate. Zero config; no public trust, but mTLS authentication is
  unaffected.
- **`cert-manager`** — cert-manager issues a publicly trusted certificate
  (e.g. Let's Encrypt). Requires cert-manager installed + a `ClusterIssuer`.
  Enable `dataCert` (chart-rendered `Certificate`). The chart auto-wires
  `certPath`/`keyPath` to the mounted Secret
  (`/etc/dagger-kubernetes/data-tls/tls.crt` + `.key`).
- **`external`** — bring your own PEM files via
  `supervisor.dataplane.tls.certPath`/`keyPath` (paths inside the supervisor
  container), or provide `crt`/`key` values that the chart renders into the
  `<fullname>-tls` Secret and mounts at `/etc/dagger-kubernetes/tls`. When
  `dataIngress.tls.secretName` is set, the chart auto-switches to `"external"`
  and auto-wires paths to the mounted secret.

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

# 2. Install with dataCert enabled — the chart auto-switches provider to "cert-manager"
helm install dagger-kubernetes oci://ghcr.io/disaster37/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml --namespace dagger-stack \
  --set dataCert.enabled=true \
  --set dataCert.issuerName=letsencrypt-prod \
  --set dataIngress.enabled=true \
  --set dataIngress.host=data.your-domain.com
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
> VictoriaMetrics; `storageClass` for MinIO and Loki. All accept an
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
  [Raft (distributed store)](#raft-distributed-store) below. **Session leases
  are Raft-replicated** (ADR-026), and the `-control` and `-data` Services
  select the current **Raft leader** pod (label
  `dagger-kubernetes.io/raft-leader`, maintained at runtime by each pod): all
  ingress traffic — API requests and data-plane tunnels — terminates on the
  leader, the only pod that can apply Raft writes. During leader elections the
  Services briefly have no endpoints; clients reconnect automatically.
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
  `supervisor.dataplane.tls.caCrt`/`caKey` explicitly to reuse a known CA.
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
| `supervisor.dataplane.tls.provider` | Server cert source: `embedded` \| `cert-manager` \| `external`. Chart auto-switches when `dataCert.enabled` or `dataIngress.tls.secretName` is set. | `embedded` |
| `supervisor.dataplane.tls.certPath` / `.keyPath` | PEM paths for `external` provider (chart auto-wires). | `""` |
| `supervisor.dataplane.tls.clientCertTtl` | Engine client cert TTL | `2h` |
| `supervisor.dataplane.tls.caCrt` / `.caKey` | Minting CA (PEM). Empty = **auto-bootstrap** on first boot | `""` |
| `supervisor.dataplane.tls.crt` / `.key` | Server cert/key PEM (only `external` provider, rendered into `<fullname>-tls` Secret). | `""` |
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
| `minio.enabled` | `true` | MinIO S3-compatible cache storage |
| `tempo.enabled` | `true` | Grafana Tempo for traces |
| `loki.enabled` | `true` | Grafana Loki for logs |
| `victoria.enabled` | `true` | VictoriaMetrics for metrics |
| `grafana.enabled` | `true` | Grafana dashboards |

### Auto-wiring

When a tool is enabled, the supervisor configuration is automatically wired to the
dependency's in-cluster Service using Go template expressions. The mapping is:

| Config key | Template helper | Target service |
|---|---|---|
| `telemetry.collectorUrl` | `dagger-kubernetes.collectorUrl` | `<release>-opentelemetry-collector.<namespace>.svc:4318` |
| `telemetry.tempoUrl` | `dagger-kubernetes.tempoUrl` | `<release>-tempo.<namespace>.svc:3200` |
| `telemetry.lokiUrl` | `dagger-kubernetes.lokiUrl` | `<release>-loki.<namespace>.svc:3100` |
| `telemetry.victoriaUrl` | `dagger-kubernetes.victoriaUrl` | `<release>-victoria-server.<namespace>.svc:8428` |
| `server.public_url` | `dagger-kubernetes.publicUrl` | computed from ingress / service exposition |
| `server.data_hostname` | `dagger-kubernetes.dataHostname` | computed from dataIngress / service exposition |
| `cache.s3.endpoint` | `dagger-kubernetes.s3Endpoint` | `supervisor.config.cache.s3.endpoint`, else `<release>-minio.<namespace>.svc:9000` (when `minio.enabled`) |
| `cache.s3.bucket` | `dagger-kubernetes.s3Bucket` | `cache.s3.bucket`, else first `minio.buckets[].name`, else `dagger-cache` |
| `fleet.engine_registry_mirrors` | `dagger-kubernetes.engineRegistryMirrors` | user map merged with one generated image-cache mirror address per enabled upstream (when `imageCache.enabled`) |
| `fleet.engine_registry_mirrors_http` | `dagger-kubernetes.imageCacheMirrorHosts` | `<release>-<slug>-mirror.<namespace>.svc:5000` per enabled upstream |

All auto-wired endpoints use the `<service>.<namespace>.svc` form — never bare
service names or `.svc.<cluster-domain>` FQDNs — so a single `.svc` entry in
`NO_PROXY` covers every in-cluster component when `HTTP_PROXY` is set on the
supervisor (e.g. to download the Dagger CLI). See `CONTRIBUTING.md`.

### Raft (distributed store)

The supervisor persists RBAC state, trace metadata, and the cache routing tables
in a **Hashicorp Raft** replicated state machine (ADR-015/ADR-016). Raft always
runs — a single-node deployment is a one-voter cluster. The chart ships a
**StatefulSet** (with `volumeClaimTemplates` per-pod PVCs, `podManagementPolicy:
Parallel`) and a **headless Service** (`clusterIP: None`,
`publishNotReadyAddresses: true` — pods get DNS A records even before Ready,
so raft peer dialing cannot deadlock on readiness) whose DNS A records
give each pod a stable identity for peer discovery.

| Value | Default | Description |
|---|---|---|
| `supervisor.replicaCount` | `3` | Supervisor pod count = Raft voter count (derived, single source of truth). Use an odd number ≥ 3 for fault tolerance. |
| `supervisor.config.raft.tls.enabled` | `true` | mTLS for the Raft transport. |
| `supervisor.config.raft.tls.clientAuth` | `true` | Require + verify peer client certs (mTLS). |
| `supervisor.config.raft.clusterDomain` | `"cluster.local"` | Cluster DNS suffix appended to peer addresses (`<pod>.<headless>.<ns>.svc.<clusterDomain>`). Default `"cluster.local"` produces fully-qualified names that bypass CoreDNS negative-cache poisoning (short `.svc` names go through the cache plugin which can serve stale NXDOMAIN for up to 30 s during bootstrap). Set to `""` only when your cluster DNS does not serve the `cluster.local` suffix. |
| `supervisor.dns.enabled` | `true` | Bypass NodeLocal DNSCache for the Raft pods: render `dnsPolicy: None` + `dnsConfig` pointing directly at the cluster's kube-dns (CoreDNS) service, avoiding the ~30 s stale positive-A-record window after a pod is recreated. `false` restores the cluster's default DNS (NodeLocal DNSCache when installed). |
| `supervisor.dns.nameserver` | `"10.43.0.10"` | kube-dns (CoreDNS) service ClusterIP. Helm cannot query the cluster, so set this to your cluster's value (`kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'`): k3s `10.43.0.10`, kubeadm/standard `10.96.0.10`. Required (fail-closed) when `supervisor.dns.enabled` is true. |
| `supervisor.dns.ndots` | `1` | `resolv.conf` `ndots` for the supervisor pod (FQDN Raft peers). |

Everything else is **fixed or derived by the chart**: the data dir is
`/var/lib/dagger-kubernetes` (per-pod PVC), the Raft transport binds `:8081`
and advertises `<pod>.<headless>.<ns>.svc.<clusterDomain>` (stable pod FQDN —
pod IPs are deliberately NOT advertised, they change on every pod
recreation), node IDs are the
StatefulSet pod names (downward-API), peers are discovered via DNS from the
headless Service (which sets `publishNotReadyAddresses: true` so pods resolve
and can elect a leader before they are Ready), and the internal CA lives in
the `<release>-raft-ca` Secret. During startup the supervisor retries
advertise-address resolution in-process for up to 2 minutes instead of
exiting: on a fresh cluster the DNS service may not be serving yet, and an
immediate exit would put every pod into a `CrashLoopBackOff` that delays the
whole bootstrap.

**NodeLocal DNSCache bypass (default ON):** when the cluster runs the
NodeLocal DNSCache add-on, its `cache 30` can serve a stale positive A record
for up to 30 s after a pod is recreated — long enough to disturb raft
re-election. With `supervisor.dns.enabled: true` (default) the StatefulSet
renders `dnsPolicy: None` + `dnsConfig` so Raft pods query the cluster's
kube-dns (CoreDNS) service directly (`supervisor.dns.nameserver`,
`ndots: 1`). `supervisor.dns.nameserver` is **fail-closed**: rendering fails
when it is empty, with the `kubectl -n kube-system get svc kube-dns` command
and the k3s (`10.43.0.10`) / kubeadm (`10.96.0.10`) values in the message.
Set `supervisor.dns.enabled: false` to escape-hatch back to the cluster
default DNS; the complementary add-on change (reduce the `nodelocaldns`
Corefile's `cache 30` → `cache 5` for the `cluster.local` zone) then remains
the way to shorten the stale window on a cluster that keeps NodeLocal DNSCache
for other workloads. Note the bypass affects every supervisor DNS lookup:
in-cluster `<service>.<ns>.svc` short names still resolve via the search
suffix (one extra NXDOMAIN round-trip), and external names forward through
CoreDNS as usual.

**TLS auto-CA:** pod-0 generates the internal CA with `goca`, writes it to the
`<release>-raft-ca` Secret, and issues itself a leaf; pods 1..N-1 poll the
Secret (bounded by `leader_wait_timeout`) before issuing their own leaves.
Leaves are reused across restarts only while they remain valid for the
current trust setup — not within the 7-day expiry margin, not yet valid,
signed by the current CA (a re-created Secret re-issues every leaf instead of
splitting the trust chain), and covering the exact CN + DNS/IP SAN set being
advertised (a URI-form change such as `.svc` → `.svc.<clusterDomain>` re-issues
the leaf).
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

### CLI cache (S3)

The on-the-fly Dagger CLI provisioning addon (`supervisor.config.cli.enabled`)
stores verified CLI tarballs in S3 so every supervisor pod can serve them.
Configure the shared S3 client under `supervisor.config.cache.s3`:

| Value | Default | Description |
|---|---|---|
| `supervisor.config.cache.s3.bucket` | `""` | Default bucket for the CLI cache. Empty ⇒ the auto-created MinIO bucket (`dagger-cache`). |
| `supervisor.config.cache.s3.region` | `"us-east-1"` | S3 region (ignored by MinIO). |
| `supervisor.config.cache.s3.endpoint` | `""` | S3-compatible endpoint (e.g. `minio.dagger-kubernetes.svc:9000`; auto-wired to the MinIO subchart Service when `minio.enabled`). Empty = the supervisor logs a WARN and disables the CLI cache. |
| `supervisor.config.cache.s3.useSSL` | `false` | Use HTTPS for the S3 endpoint. |
| `supervisor.config.cache.s3.accessKey` / `secretKey` | `""` | Leave empty: the supervisor reads the credentials from the `engine-s3-auth` Secret via `DAGGER_KUBERNETES_CACHE_S3_ACCESS_KEY`/`..._SECRET_KEY` (empty falls back to the AWS env credential chain). |
| `supervisor.config.cli.s3Bucket` / `supervisor.config.cli.s3Prefix` | `""` / `cli-cache` | CLI-tarball bucket + key prefix. |

**Credentials:** the chart does NOT render S3 secrets into the ConfigMap.
Create the `engine-s3-auth` Secret in the release namespace (keys `accessKey`
and `secretKey`) — the image-cache mirrors read it directly for their S3
storage — and the supervisor pods read the same keys from it via optional
env injection (`DAGGER_KUBERNETES_CACHE_S3_ACCESS_KEY` /
`DAGGER_KUBERNETES_CACHE_S3_SECRET_KEY`; when the Secret is absent the
S3 client falls back to the AWS env credential chain):

```bash
kubectl -n <namespace> create secret generic engine-s3-auth \
  --from-literal=accessKey=... --from-literal=secretKey=...
```

### Local image cache (Zot mirror)

`imageCache.enabled: true` deploys one Zot OCI registry per enabled upstream (a
`Deployment` + `<name>-zot-config` ConfigMap + `Service`, backed by the shared
MinIO/S3 `image-cache` bucket by default). Zot's `extensions.sync` fetches an
image from upstream on first request (`onDemand: true`) and serves it from cache
afterwards; there is no TTL — invalidate with the admin image-cache prune. Zot's
online GC reclaims unreferenced blob bytes after `imageCache.gc.delay` on the
next `imageCache.gc.interval`. `imageCache.sync.downloadDir` (default
`/var/lib/registry/sync`, on the mounted data volume for both storage backends)
is always rendered because Zot requires it when sync is enabled with S3 storage;
`imageCache.dedupe` defaults to `false` (Zot rejects dedupe with the S3 driver
unless a remote DB is configured). The generated mirror addresses are merged
into the engine's `engine.toml` with `http = true` for the plaintext in-cluster
mirrors. See docs/README.md, "Local image cache (Zot mirror)", and
[ADR-033](../../../docs/design/ADR-033-local-image-mirror.md).

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
| `supervisor.dns.enabled` | bool | `true` | Bypass NodeLocal DNSCache for the supervisor pod: render `dnsPolicy: None` + `dnsConfig` pointing at kube-dns (CoreDNS) directly. `false` renders neither key (cluster default DNS). |
| `supervisor.dns.nameserver` | string | `"10.43.0.10"` | kube-dns (CoreDNS) service ClusterIP used by the bypass (k3s `10.43.0.10`, kubeadm `10.96.0.10`). Fail-closed: required when `supervisor.dns.enabled` is true. |
| `supervisor.dns.ndots` | int | `1` | `resolv.conf` `ndots` for the supervisor pod. |

### Supervisor configuration

| Name | Type | Default | Description |
|---|---|---|---|
| `supervisor.config.raft.tls.enabled` | bool | `true` | Enable mTLS for the Raft transport. |
| `supervisor.config.raft.tls.clientAuth` | bool | `true` | Require and verify peer client certs (mTLS). |
| `supervisor.config.raft.clusterDomain` | string | `"cluster.local"` | Cluster DNS suffix appended to peer addresses (`<pod>.<headless>.<ns>.svc.<clusterDomain>`). Default `"cluster.local"` produces fully-qualified names that bypass CoreDNS negative-cache poisoning (short `.svc` names go through the cache plugin which can serve stale NXDOMAIN for up to 30 s during bootstrap). Set to `""` only when your cluster DNS does not serve the `cluster.local` suffix. |
| `supervisor.config.telemetry.collectorUrl` | string | `""` | OTel collector URL (auto-wired to `<release>-opentelemetry-collector.<namespace>.svc:4318` when the opentelemetry-collector subchart is enabled). |
| `supervisor.config.telemetry.tempoUrl` | string | `""` | Tempo URL for trace queries (auto-wired to `<release>-tempo.<namespace>.svc:3200` when the tempo subchart is enabled). |
| `supervisor.config.telemetry.lokiUrl` | string | `""` | Loki URL for log queries (auto-wired to `<release>-loki.<namespace>.svc:3100` when the loki subchart is enabled). |
| `supervisor.config.telemetry.victoriaUrl` | string | `""` | VictoriaMetrics URL for metric queries (auto-wired to `<release>-victoria-server.<namespace>.svc:8428` when the victoria subchart is enabled). |
| `supervisor.config.cache.s3.bucket` | string | `""` | S3 bucket name (empty = auto-created MinIO bucket, `dagger-cache`). |
| `supervisor.config.cache.s3.region` | string | `"us-east-1"` | S3 region (ignored by MinIO). |
| `supervisor.config.cache.s3.endpoint` | string | `""` | S3-compatible endpoint for the shared S3 client (CLI cache). Auto-wired to `<release>-minio.<namespace>.svc:9000` when the minio subchart is enabled. |
| `supervisor.config.cache.s3.useSSL` | bool | `false` | Use HTTPS for the S3 endpoint. |
| `supervisor.config.cache.s3.accessKey` | string | `""` | S3 access key. Leave empty: the supervisor reads it from the `engine-s3-auth` Secret via `DAGGER_KUBERNETES_CACHE_S3_ACCESS_KEY`. |
| `supervisor.config.cache.s3.secretKey` | string | `""` | S3 secret key. Leave empty: the supervisor reads it from the `engine-s3-auth` Secret via `DAGGER_KUBERNETES_CACHE_S3_SECRET_KEY`. |
| `supervisor.config.history.gc.enabled` | bool | `false` | Master switch for the history auto-purge sweeper. |
| `supervisor.config.history.gc.maxAge` | string | `"720h"` | Purge traces older than this (30d default). Duration strings accept Go units plus `d` (day) and `w` (week), e.g. `7d`, `1w`. |
| `supervisor.config.history.gc.schedule` | string | `"1h"` | History sweeper ticker interval. |
| `supervisor.config.fleet.maxReplicasPerVersion` | int | `3` | Maximum engine replicas per Dagger version. |
| `supervisor.config.fleet.maxSessionsPerReplica` | int | `8` | Maximum concurrent sessions per engine replica. |
| `supervisor.config.fleet.replicaIdleTtl` | string | `"5m"` | Idle TTL before scaling down an engine replica. |
| `supervisor.config.fleet.versionRetention` | string | `"24h"` | Idle version GC: delete a version's StatefulSet + Service after this long with zero replicas and no pinned sessions (`<= 0` disables; positive values `< 1m` rejected). |
| `supervisor.config.fleet.engineImageRegistry` | string | `"registry.dagger.io/engine"` | Engine container image registry. |
| `supervisor.config.fleet.engineStorageClass` | string | `""` | StorageClass for engine PVCs (empty = cluster default). |
| `supervisor.config.fleet.engineStorageSize` | string | `"50Gi"` | PVC size for each engine. |
| `supervisor.config.fleet.enginePvcLabels` | object | `{}` | Extra labels added to engine PVCs (merged with managed labels app/version, which take precedence). Keys may contain dots (e.g. `recurring-job-group.longhorn.io/nobackup`) and are preserved verbatim. |
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
| `supervisor.config.fleet.engineDockerConfig` | string | `""` | Base64-encoded Docker config.json (auths for private registries). Stored verbatim in the `engine-image-auth` Secret `data` key `.dockerconfigjson` and base64-decoded exactly once on mount, so engine pods read raw JSON at `/etc/dagger/.dockerconfigjson`. Empty = `e30K` (`{}`). NOT an imagePullSecret. |
| `supervisor.config.fleet.engineDebug` | bool | `false` | Enable engine.toml [debug]. |
| `supervisor.config.fleet.engineLogFormat` | string | `"json"` | Engine log format (json, text; empty omits). |
| `supervisor.config.fleet.engineRegistryMirrors` | object | `{}` | Engine registry mirrors (e.g. {"docker.io": ["mirror.gcr.io"]}). Dotted keys such as `docker.io`, `ghcr.io`, `public.ecr.aws` are preserved verbatim. When `imageCache.enabled`, the generated in-cluster mirror addresses are merged into this map. |
| `supervisor.config.fleet.engineRegistryMirrorsHttp` | array | `[]` | Mirror host[:port] values dialed over plaintext HTTP; engine.toml emits `[registry."<mirror>"]` + `http = true` per entry. Auto-populated with the image-cache mirrors when `imageCache.enabled`. |
| `supervisor.config.leaseTtl` | string | `"2m"` | Engine session lease TTL. |
| `supervisor.config.version.floor` | string | `"v0.19.0"` | Minimum supported Dagger engine version. |
| `supervisor.config.version.allowlist` | array | `["0.19", "0.20", "0.21"]` | Allowed Dagger versions (major.minor prefixes; empty = admit all versions >= floor). |
| `supervisor.config.cli.enabled` | bool | `true` | Enable on-the-fly Dagger CLI provisioning addon. |
| `supervisor.config.cli.releaseListTtl` | string | `"1h"` | Upstream release-list cache TTL. |
| `supervisor.config.cli.downloadTimeout` | string | `"5m"` | Outbound upstream fetch timeout. |
| `supervisor.config.cli.upstream.releasesUrl` | string | `"https://api.github.com/repos/dagger/dagger/releases"` | Release discovery URL (mirror-able). |
| `supervisor.config.cli.upstream.downloadBase` | string | `"https://github.com/dagger/dagger/releases/download"` | Tarball + checksums.txt base URL (mirror-able). |
| `supervisor.config.cli.upstream.githubToken` | string | `""` | Optional GitHub token for the releases API (set via `DAGGER_KUBERNETES_CLI_UPSTREAM_GITHUB_TOKEN` or `supervisor.extraEnv`). |
| `supervisor.config.attribution.projectMappings` | array | `[]` | Ordered list of `{pattern, group}` mapping a project name (CI repo slug; Go regexp, case-sensitive) to a supervisor group name. First-match-wins; no match falls through to per-group `auto_assign_pattern`; empty = disabled. Target groups must already exist. |
| `supervisor.config.logLevel` | string | `"info"` | Supervisor log level. |
| `supervisor.config.logFormat` | string | `"json"` | Supervisor log format (json, text). |
| `supervisor.config.otel.otlpEndpoint` | string | `""` | Supervisor OTLP export endpoint (empty disables). |
| `supervisor.config.otel.ingestMaxBodySize` | int | `67108864` | OTLP ingest request-body cap in bytes (0 = supervisor default 64 MiB). Keep the ingress `proxyBodySize` and the collector `max_request_body_size` at least this large. |
| `supervisor.config.pipeline.metrics.enabled` | bool | `true` | Enable the trace-scoped engine resource metrics endpoint (`GET /api/v1/traces/:id/metrics`). |
| `supervisor.config.pipeline.metrics.step` | string | `"15s"` | `query_range` resolution for engine metrics (must be > 0 when enabled). |

### Local image cache (Zot mirror)

| Name | Type | Default | Description |
|---|---|---|---|
| `imageCache.enabled` | bool | `false` | Enable the local image cache (Zot mirrors). |
| `imageCache.image.repository` | string | `"ghcr.io/project-zot/zot"` | Zot image repository. |
| `imageCache.image.tag` | string | `"v2.1.21"` | Zot image tag (pinned; v2.x serves the OCI Distribution v2 API + online GC). |
| `imageCache.image.pullPolicy` | string | `"IfNotPresent"` | Image pull policy. |
| `imageCache.logLevel` | string | `"warn"` | Zot log level (`config.json` `log.level`). |
| `imageCache.storage.backend` | string | `"s3"` | Mirror storage backend: `s3` (shared MinIO) or `pvc` (per-mirror PVC). |
| `imageCache.storage.s3.bucket` | string | `"image-cache"` | S3 bucket for cached blobs (auto-created when `minio.enabled`). |
| `imageCache.storage.s3.region` | string | `"us-east-1"` | S3 region (ignored by MinIO). |
| `imageCache.storage.s3.endpoint` | string | `""` | S3 endpoint; auto-wired to `<release>-minio.<namespace>.svc:9000` when `minio.enabled`. |
| `imageCache.storage.s3.forcePathStyle` | bool | `true` | Use path-style S3 addressing (required by MinIO). |
| `imageCache.storage.s3.secure` | bool | `false` | Use HTTPS for the S3 endpoint. |
| `imageCache.storage.pvc.storageClass` | string | `""` | StorageClass for per-mirror PVCs (empty = cluster default). |
| `imageCache.storage.pvc.size` | string | `"20Gi"` | PVC size per mirror. |
| `imageCache.gc.enabled` | bool | `true` | Enable Zot online garbage collection (`storage.gc`). |
| `imageCache.gc.delay` | string | `"2h"` | Minimum age of an unreferenced blob before Zot GC reclaims it (`storage.gcDelay`). |
| `imageCache.gc.interval` | string | `"1h"` | How often Zot runs GC (`storage.gcInterval`). |
| `imageCache.gc.timeWindow` | string | `""` | Optional daily off-peak GC window `"HH:MM-HH:MM"` (`storage.gcTimeWindow`; empty = unrestricted). |
| `imageCache.dedupe` | bool | `false` | Deduplicate identical blobs across manifests (`storage.dedupe`). Off by default: Zot rejects dedupe with the S3 driver (`no remote database configured`) unless a remote cache/DB is configured, which this chart does not render. |
| `imageCache.sync.onDemand` | bool | `true` | Fetch an upstream image on first request (`extensions.sync.registry.onDemand`). |
| `imageCache.sync.preserveDigest` | bool | `true` | Keep upstream manifest digests (`extensions.sync.registry.preserveDigest`). |
| `imageCache.sync.tlsVerify` | bool | `true` | Verify upstream TLS certificates. |
| `imageCache.sync.downloadDir` | string | `"/var/lib/registry/sync"` | Local scratch directory for sync downloads (`extensions.sync.downloadDir`). Required by Zot when sync is enabled with S3 storage. Must live under the mounted data volume (`/var/lib/registry`: emptyDir for `s3`, the PVC for `pvc`). |
| `imageCache.sync.pollInterval` | string | `""` | Periodic upstream refresh interval (empty = on-demand only; do not use for Docker Hub). |
| `imageCache.sync.maxRetries` | int | `3` | Upstream fetch retry count. |
| `imageCache.sync.retryDelay` | string | `"15m"` | Delay between upstream fetch retries. |
| `imageCache.resources.requests.cpu` | string | `"100m"` | Mirror CPU request. |
| `imageCache.resources.requests.memory` | string | `"128Mi"` | Mirror memory request. |
| `imageCache.resources.limits.cpu` | string | `"500m"` | Mirror CPU limit. |
| `imageCache.resources.limits.memory` | string | `"512Mi"` | Mirror memory limit. |
| `imageCache.nodeSelector` | object | `{}` | Node selector for mirror pods. |
| `imageCache.tolerations` | array | `[]` | Tolerations for mirror pods. |
| `imageCache.securityContext` | object | `{runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532}` | Pod securityContext for mirror pods. |
| `imageCache.presets` | object | `docker.io`/`ghcr.io`/`public.ecr.aws`/`quay.io` enabled; `gcr.io`/`registry.dagger.io` disabled | Built-in upstream presets. Each entry: `{enabled, username, passwordSecretRef}`. |
| `imageCache.registries` | array | `[]` | Extra/custom upstreams (private ECR/GAR/Harbor/…): `{host, remoteUrl, username, passwordSecretRef}`. Private credentials are a documented follow-up (Zot `credentialsFile`). |

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
| `auth.jwt.refreshTtl` | string | `"168h"` | JWT refresh token TTL. Duration strings accept Go units plus `d` (day) and `w` (week), e.g. `7d`. |
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
| `auth.oauth.mappedGroupMaxRunnerSessions` | int | `0` | Default `max_runner_sessions` (0 = unlimited) for supervisor groups auto-created by `groupMappings`; applied only at creation time. |
| `auth.oauth.adminGroups` | array | `[]` | Upstream IdP group names granting the admin role on login/revalidation (exact, case-sensitive, checked pre-mapping); empty = disabled. |
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
| `supervisor.dataplane.tls.provider` | string | `"embedded"` | TLS provider for the server certificate: "embedded", "cert-manager", or "external". The chart auto-switches to "cert-manager" when `dataCert.enabled` is set, and to "external" when `dataIngress.tls.secretName` is set. |
| `supervisor.dataplane.tls.certPath` | string | `""` | Server certificate path (only "external"). cert-manager/dataIngress auto-wire the mounted data-tls secret. |
| `supervisor.dataplane.tls.keyPath` | string | `""` | Server key path (only "external"). |
| `supervisor.dataplane.tls.clientCertTtl` | string | `"2h"` | Client certificate TTL (engine session certs minted from the CA). |
| `supervisor.dataplane.tls.caCrt` | string | `""` | PEM-encoded minting CA certificate. Leave empty to AUTO-BOOTSTRAP on first boot. |
| `supervisor.dataplane.tls.caKey` | string | `""` | PEM-encoded minting CA private key. Leave empty to AUTO-BOOTSTRAP on first boot. |
| `supervisor.dataplane.tls.crt` | string | `""` | PEM-encoded server certificate (only needed for provider "external", rendered into `<fullname>-tls` Secret). |
| `supervisor.dataplane.tls.key` | string | `""` | PEM-encoded server private key (only needed for provider "external"). |

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
| `ingress.proxyBodySize` | string | `"64m"` | nginx `proxy-body-size` annotation value. Must be >= the supervisor's `otel.ingestMaxBodySize` so large OTLP log batches are not rejected with 413 at the ingress. |
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
| `minio.enabled` | bool | `true` | Install MinIO subchart (S3-compatible object store for the local image cache and the CLI cache). |
| `minio.rootUser` / `minio.rootPassword` | string | `"minioadmin"` | MinIO root credentials (dev defaults; change in production). Rendered into the `engine-s3-auth` Secret. |
| `minio.users` | array | `[]` | Extra MinIO users to create; empty (default) disables the subchart's built-in `console` user. |
| `minio.buckets` | array | `[{name: dagger-cache}, {name: image-cache}]` | Buckets created on install by the MinIO chart's post-install hook (`image-cache` backs the local image mirror). |
| `tempo.enabled` | bool | `true` | Install Grafana Tempo subchart (traces). |
| `tempo.tempo.retention` | string | `720h` | Trace retention duration. Set to match (or exceed) `supervisor.config.history.gc.maxAge`. |
| `loki.enabled` | bool | `true` | Install Grafana Loki subchart (logs). |
| `victoria.enabled` | bool | `true` | Install VictoriaMetrics subchart (metrics). |
| `victoria.server.scrape.enabled` | bool | `true` | Enable VictoriaMetrics' built-in Prometheus scraper. The subchart's default scrape config already includes the `kubernetes-nodes-cadvisor` job (kubelet `/metrics/cadvisor`), which supplies the `container_*` series the pipeline-view engine-metrics card queries; the subchart's ClusterRole grants `nodes/metrics` when scraping is enabled. Do not add a second cAdvisor job — duplicate jobs double every summed series. |
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
