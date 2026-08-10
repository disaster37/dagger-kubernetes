# Plan: Helm `values.yaml` Restructure — Scope Supervisor Pod Values + Subchart Persistence

## Overview

The chart at `deploy/helm/dagger-kubernetes/` scatters supervisor-pod-specific values
(`image`, `replicaCount`, `resources`, `persistence`, `podAnnotations`,
`podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity`,
`autoscaling`, `serviceAccount`) at the top level of `values.yaml`, while
`supervisor.extraEnv` and `supervisor.config.*` are already nested. This plan moves
all supervisor-pod values under `supervisor:`, adds a `storageClassName` key to every
tool subchart persistence block that lacks one, and adds a PVC persistence option for
Loki's `SingleBinary` mode.

This is a **BREAKING CHANGE** for existing values overrides: every top-level key listed
above moves under `supervisor:`. Users must prefix existing overrides with
`supervisor:`. There is no in-cluster state impact (the rendered manifests are
identical when values are migrated); only the values file schema changes.

---

## ⚠️ Deviations from the original request (corrected against actual subchart contracts)

The original request contained two specifications that conflict with the actual
subchart APIs and the chart's own `README.md`. They are corrected in this plan:

### Deviation 1 — Subchart persistence key name: `storageClass` → `storageClassName`

The request asked to add `persistence.storageClass: ""` to `registry`, `tempo`, and
`victoria`. **This is wrong.** Helm forwards every key under a dependency's alias
verbatim to the subchart. The subcharts do not recognize `storageClass`; they
recognize `storageClassName`:

| Subchart (alias)              | Chart version | Recognized PVC key                    |
|-------------------------------|---------------|---------------------------------------|
| `registry` (docker-registry)  | 1.9.6         | `persistence.storageClassName`        |
| `tempo` (grafana/tempo)        | 1.24.4        | `persistence.storageClassName`        |
| `victoria` (victoria-metrics-single) | 0.44.0 | `server.persistentVolume.storageClassName` |

Evidence: the chart's own `README.md` (lines 135–160) documents
`storageClassName: "fast-ssd"` for all four subcharts. Adding `storageClass` would be
silently ignored — users setting `registry.persistence.storageClass: "fast-ssd"`
would get the cluster default StorageClass with no warning. **This plan uses
`storageClassName` for all subchart blocks.**

Note: the supervisor's OWN PVC (`templates/pvc.yaml`) uses the values key
`persistence.storageClass` and the template maps it to the Kubernetes
`storageClassName` field. That is the chart's internal convention for its own PVC and
is unchanged here. Only the **subchart** values use the subchart-native
`storageClassName` key.

### Deviation 2 — Loki persistence path: `loki.persistence` → `loki.singleBinary.persistence`

The request asked to add a top-level `loki.persistence` block mapping to "chunks
and/or index storage when using the `SingleBinary` deployment mode." **This is
wrong.** The Grafana Loki subchart (v7.2.0) has no top-level `persistence` key. In
`SingleBinary` mode the pod's PVC (which holds chunks + index + compactor on the
filesystem) is configured at `loki.singleBinary.persistence`. A top-level
`loki.persistence` would be forwarded to the subchart and silently ignored.

Evidence: the chart's own `README.md` (lines 142–146) documents the correct path:
```yaml
loki:
  singleBinary:
    persistence:
      enabled: true
      size: 100Gi
      storageClassName: "fast-ssd"
```
The existing `values.yaml` already sets `loki.singleBinary.replicas` and
`loki.singleBinary.resources`, so adding `loki.singleBinary.persistence` is consistent
with the existing structure. **This plan adds `loki.singleBinary.persistence`
(with `enabled`, `size`, `storageClassName`), not a top-level `loki.persistence`.**

Object storage for Loki (S3/GCS) remains configured via the existing
`loki.loki.storage.bucketNames` and is out of scope for this change.

---

## Files in scope

| File | Change |
|------|--------|
| `deploy/helm/dagger-kubernetes/values.yaml` | Full restructure (see below) |
| `deploy/helm/dagger-kubernetes/templates/deployment.yaml` | 13 path updates |
| `deploy/helm/dagger-kubernetes/templates/pvc.yaml` | 3 path updates |
| `deploy/helm/dagger-kubernetes/templates/hpa.yaml` | 7 path updates |
| `deploy/helm/dagger-kubernetes/templates/rbac.yaml` | 1 path update |
| `deploy/helm/dagger-kubernetes/README.md` | Docs sync (config table, HA, Security, Storage) |

## Files explicitly NOT changed (verified)

- `templates/_helpers.tpl` — uses only `nameOverride`/`fullnameOverride`/`namespace`
  (top-level, unchanged) and `supervisor.config.*` / `tools.*` (unchanged).
- `templates/service.yaml` — uses `.Values.service.*` (top-level, unchanged).
- `templates/ingress.yaml` — uses `.Values.ingress.*` (top-level, unchanged).
- `templates/servicemonitor.yaml` — uses `.Values.serviceMonitor.*` (top-level, unchanged).
- `templates/grafana-datasources.yaml` — uses `.Values.tools.grafana.enabled` (unchanged).
- `templates/configmap.yaml` — uses `.Values.supervisor.config.*` (unchanged).
- `templates/secret.yaml` — uses `.Values.auth.*`, `.Values.ca.*`, `.Values.tls.*`
  (top-level chart-level, unchanged).
- `Chart.yaml`, `Chart.lock`, `charts/*.tgz` — no dependency changes.
- `dagger/main.go` — the `helmTemplateMatrix` only `--set`s `tools.*` keys; unaffected.
- `.github/workflows/release.yml` — runs `helm lint`; passes once values/templates are consistent.
- `config/config.app.yaml.sample`, `config/loader.go` — Go app config, not Helm; unaffected.
- `tests/integration/*` — Go API tests; do not render the chart; unaffected.

---

## 1. Complete new `values.yaml`

Replace the entire contents of `deploy/helm/dagger-kubernetes/values.yaml` with the
following. Line numbers are illustrative; the file is a full rewrite.

```yaml
# dagger-kubernetes Helm chart values
# ---------------------------------------------------------------------------
# Copy to your own values file and override as needed, e.g.:
#   helm install dagger-kubernetes ./deploy/helm/dagger-kubernetes -f my-values.yaml

# --- Naming / namespace (chart-level) -------------------------------------
nameOverride: ""
fullnameOverride: ""
# Namespace for the supervisor and its subchart dependencies.
# Defaults to the release namespace when empty.
namespace: ""

# --- Supervisor (control-plane pod) ---------------------------------------
# All supervisor-pod settings are scoped under `supervisor:`. The supervisor
# config block (`supervisor.config.*`) is rendered into the configmap and is
# unchanged from prior versions; only the pod-level keys moved here from the
# top level.
supervisor:
  image:
    repository: ghcr.io/disaster/dagger-kubernetes
    tag: ""                # defaults to Chart.appVersion when empty
    pullPolicy: IfNotPresent
    pullSecrets: []        # e.g. [{ name: ghcr-pull-secret }]
  replicaCount: 2
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 512Mi
  # SQLite database PVC. With enabled=false an emptyDir is used and all
  # users/tokens are LOST on pod restart (dev only). storageClass="" uses the
  # cluster default StorageClass.
  persistence:
    enabled: false
    storageClass: ""
    size: 1Gi
  podAnnotations: {}
  podSecurityContext:
    runAsNonRoot: true
    runAsUser: 10001
    runAsGroup: 10001
    fsGroup: 10001
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: ["ALL"]
  nodeSelector: {}
  tolerations: []
  affinity: {}
  autoscaling:
    enabled: false
    minReplicas: 2
    maxReplicas: 6
    targetCPUUtilizationPercentage: 80
    targetMemoryUtilizationPercentage: ""
  serviceAccount:
    annotations: {}
  extraEnv: []
  config:
    server:
      dataHostname: "data.supv.example.com"
      publicUrl: "https://supv.example.com"
    database:
      path: "/var/lib/dagger-cache/dagger-cache.db"
    auth:
      internal:
        enabled: true
        tokensFile: "/etc/dagger-cache/tokens"
      bootstrapAdmin:
        username: "admin"
        password: ""    # empty = random password logged once at first boot
      jwt:
        secret: ""      # empty = auto-generated and persisted in the DB
        accessTtl: "15m"
        refreshTtl: "168h"
      oauth:
        enabled: false
        provider: "github"
        clientId: "${OAUTH_CLIENT_ID}"
        clientSecret: "${OAUTH_CLIENT_SECRET}"
        redirectUrl: "https://supv.example.com/api/v1/auth/oauth/github/callback"
        allowedOrgs: ["acme"]
        defaultGroup: ""
    telemetry:
      # collectorUrl is derived from the otel-collector dependency when enabled.
      collectorUrl: ""
      tempoUrl: "http://tempo:3200"
      lokiUrl: "http://loki:3100"
      victoriaUrl: "http://victoria:8428"
    cache:
      backend: "registry"            # "registry" (OCI) or "s3"
      # registry is derived from the docker-registry dependency when enabled.
      registry: ""
      refPerVersion: true
      s3:
        bucket: ""
        region: ""
    fleet:
      minReplicasPerVersion: 0
      maxReplicasPerVersion: 3
      maxSessionsPerReplica: 8
      replicaIdleTtl: "5m"
      versionRetention: "24h"
      engineImageRegistry: "registry.dagger.io/engine"
      engineStorageClass: ""
      engineStorageSize: "50Gi"
      engineCPURequest: "500m"
      engineCPULimit: "2000m"
      engineMemoryRequest: "1Gi"
      engineMemoryLimit: "8Gi"
      engineTerminationGraceSeconds: 120
      enginePullPolicy: "IfNotPresent"
      enginePrivileged: true
      engineNodeSelector: {}
      engineTolerations: []
      engineExtraArgs: []
      engineExtraEnv: {}            # extra env vars on engine pods (proxy vars etc.)
      engineExtraEnvFrom: {}        # env vars from Secret keys: {ENV_NAME: {secret_name, key}} (proxy credentials)
      engineCaSecret: ""            # Secret with custom CA PEM bundle; empty = disabled
      engineCaSecretKey: "ca.crt"   # key inside engineCaSecret
      engineDebug: false            # engine.toml debug = true
      engineLogFormat: "json"       # engine.toml [log] format; "" omits
      engineRegistryMirrors: {}     # e.g. {"docker.io": ["mirror.gcr.io"]}
    ca:
      clientCertTtl: "2h"
    leaseTtl: "2m"
    version:
      floor: "v0.19.0"
      allowlist: ["0.19", "0.20", "0.21"]
    logLevel: "info"
    logFormat: "json"               # supervisor log format: json | text
    otel:
      otlpEndpoint: ""               # empty disables the supervisor's own OTLP export

# --- Authentication secrets (chart-level) ----------------------------------
auth:
  # Static bearer tokens, one per line (newline-separated at runtime).
  # DEPRECATED: legacy fallback only. Migrate with `supervisor migrate-tokens`
  # then remove. See ADR-010.
  tokens: []
  # - "super-secret-token-1"
  # Optional JWT signing secret (HS256). When empty, the supervisor
  # auto-generates one on first boot and persists it in the SQLite DB.
  jwtSecret: ""

# --- Minting CA & TLS (chart-level, provide your own) ----------------------
ca:
  crt: ""    # PEM-encoded CA certificate
  key: ""    # PEM-encoded CA private key
tls:
  crt: ""    # PEM-encoded server certificate
  key: ""    # PEM-encoded server private key

# --- Services (chart-level) -----------------------------------------------
service:
  control:
    type: ClusterIP
    port: 80
    nodePort: ""
  data:
    type: LoadBalancer
    port: 443

# --- Ingress (chart-level) -------------------------------------------------
ingress:
  enabled: true
  className: ""
  annotations: {}
  hosts:
    - host: supv.example.com
      paths:
        - path: /
          pathType: Prefix
  tls: []
  #  - secretName: supv-tls
  #    hosts:
  #      - supv.example.com

# --- Prometheus ServiceMonitor (chart-level) -------------------------------
serviceMonitor:
  enabled: false
  labels: {}
  interval: 30s
  scrapeTimeout: 10s

# ===========================================================================
# Required tools, integrated as Helm chart dependencies.
# Each can be toggled independently; when enabled, the supervisor config is
# wired to point at the dependency's in-cluster Service automatically.
# ===========================================================================
tools:
  # OpenTelemetry Collector — receives OTLP from Dagger CLI & supervisor,
  # fans out to Tempo / Loki / Prometheus backends.
  otelCollector:
    enabled: true
  # Docker registry (OCI) — backs the remote shared cache (BuildKit blobs).
  registry:
    enabled: true
  # Grafana Tempo — distributed tracing backend, stores OTLP traces.
  tempo:
    enabled: true
  # Grafana Loki — log aggregation backend, stores OTLP logs.
  loki:
    enabled: true
  # VictoriaMetrics — PromQL-compatible metrics backend, stores OTLP metrics.
  victoria:
    enabled: true
  # Grafana — unified dashboards for Tempo, Loki, and VictoriaMetrics data.
  grafana:
    enabled: true

# ---------------------------------------------------------------------------
# Subchart values. Helm forwards these to each dependency. Keys are top-level:
# `opentelemetry-collector` (chart name) and `registry` (alias of
# docker-registry). Override freely; defaults shown below.
# All PVC-bearing subcharts expose `storageClassName` (empty = cluster default).
# ---------------------------------------------------------------------------
opentelemetry-collector:
  mode: deployment
  replicaCount: 1
  image:
    repository: otel/opentelemetry-collector-contrib
    tag: "0.108.0"
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
  service:
    type: ClusterIP
  # The collector subchart uses tpl() on config, so Go template expressions
  # like {{ .Release.Name }} are resolved correctly at install time.
  config:
    receivers:
      otlp:
        protocols:
          http:
            endpoint: 0.0.0.0:4318
    processors:
      batch: {}
    exporters:
      otlphttp/tempo:
        endpoint: http://{{ .Release.Name }}-tempo:4318
        tls:
          insecure: true
      loki:
        endpoint: http://{{ .Release.Name }}-loki:3100/loki/api/v1/push
        tls:
          insecure: true
      prometheusremotewrite:
        endpoint: http://{{ .Release.Name }}-victoria-metrics-single:8428/prometheus/api/v1/write
        tls:
          insecure: true
    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [batch]
          exporters: [otlphttp/tempo]
        logs:
          receivers: [otlp]
          processors: [batch]
          exporters: [loki]
        metrics:
          receivers: [otlp]
          processors: [batch]
          exporters: [prometheusremotewrite]

# docker-registry subchart (stable/docker-registry, aliased as `registry`).
registry:
  replicaCount: 1
  persistence:
    enabled: true
    size: 50Gi
    storageClassName: ""    # empty = cluster default StorageClass
  storage: filesystem
  resources:
    requests:
      cpu: 100m
      memory: 128Mi

# Tempo — distributed tracing backend. Receives OTLP traces from the collector.
tempo:
  persistence:
    enabled: true
    size: 20Gi
    storageClassName: ""    # empty = cluster default StorageClass
  tempo:
    retention: 48h
    storage:
      trace:
        backend: local
        local:
          path: /var/tempo/traces
    resources:
      requests:
        cpu: 100m
        memory: 256Mi

# Loki — log aggregation backend. Receives OTLP logs from the collector.
# SingleBinary mode stores chunks + index + compactor on the pod PVC
# (filesystem storage). For S3/GCS object storage see `loki.loki.storage`.
loki:
  deploymentMode: SingleBinary
  singleBinary:
    replicas: 1
    persistence:
      enabled: true
      size: 20Gi
      storageClassName: ""    # empty = cluster default StorageClass
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
  backend:
    replicas: 0
  read:
    replicas: 0
  write:
    replicas: 0
  loki:
    useTestSchema: true
    storage:
      bucketNames:
        chunks: chunks
        ruler: ruler

# VictoriaMetrics single-server — metrics backend. Receives OTLP metrics from the collector.
victoria:
  server:
    persistentVolume:
      enabled: true
      size: 20Gi
      storageClassName: ""    # empty = cluster default StorageClass
    resources:
      requests:
        cpu: 100m
        memory: 256Mi

# Grafana — dashboards for Tempo (traces), Loki (logs), and VictoriaMetrics (metrics).
# Connect to the UI at the Grafana Service URL (default credentials: admin / admin).
# Datasources are auto-provisioned via a ConfigMap template (see grafana-datasources.yaml).
grafana:
  adminPassword: "admin"
  sidecar:
    datasources:
      enabled: true
      label: grafana_datasource
```

### What changed vs. the old `values.yaml` (summary diff)

- Removed top-level: `image`, `replicaCount`, `persistence`, `podAnnotations`,
  `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`,
  `affinity`, `autoscaling`, `resources`, `serviceAccount`.
- Added under `supervisor:`: `image`, `replicaCount`, `resources`, `persistence`,
  `podAnnotations`, `podSecurityContext`, `securityContext`, `nodeSelector`,
  `tolerations`, `affinity`, `autoscaling`, `serviceAccount` (in that order,
  before the existing `extraEnv` and `config`).
- `supervisor.extraEnv` and `supervisor.config.*` — unchanged content, now
  preceded by the moved pod keys.
- `registry.persistence`: added `storageClassName: ""`.
- `tempo.persistence`: added `storageClassName: ""`.
- `victoria.server.persistentVolume`: added `storageClassName: ""`.
- `loki.singleBinary`: added `persistence: { enabled: true, size: 20Gi, storageClassName: "" }`.
- All other top-level keys (`nameOverride`, `fullnameOverride`, `namespace`,
  `auth`, `ca`, `tls`, `service`, `ingress`, `serviceMonitor`, `tools`,
  `opentelemetry-collector`, `grafana`) — unchanged.

---

## 2. Template changes (exact, line-level)

All line numbers refer to the CURRENT files. Only the listed lines change; every
other line is byte-for-byte identical.

### 2.1 `templates/deployment.yaml`

| Line | Current | New |
|------|---------|-----|
| 9  | `  replicas: {{ .Values.replicaCount }}` | `  replicas: {{ .Values.supervisor.replicaCount }}` |
| 19 | `        {{- with .Values.podAnnotations }}` | `        {{- with .Values.supervisor.podAnnotations }}` |
| 24 | `      {{- with .Values.image.pullSecrets }}` | `      {{- with .Values.supervisor.image.pullSecrets }}` |
| 29 | `        {{- toYaml .Values.podSecurityContext | nindent 8 }}` | `        {{- toYaml .Values.supervisor.podSecurityContext | nindent 8 }}` |
| 32 | `          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"` | `          image: "{{ .Values.supervisor.image.repository }}:{{ .Values.supervisor.image.tag | default .Chart.AppVersion }}"` |
| 33 | `          imagePullPolicy: {{ .Values.image.pullPolicy }}` | `          imagePullPolicy: {{ .Values.supervisor.image.pullPolicy }}` |
| 77 | `            {{- toYaml .Values.resources | nindent 12 }}` | `            {{- toYaml .Values.supervisor.resources | nindent 12 }}` |
| 79 | `            {{- toYaml .Values.securityContext | nindent 12 }}` | `            {{- toYaml .Values.supervisor.securityContext | nindent 12 }}` |
| 94 | `          {{- if .Values.persistence.enabled }}` | `          {{- if .Values.supervisor.persistence.enabled }}` |
| 100 | `      {{- with .Values.nodeSelector }}` | `      {{- with .Values.supervisor.nodeSelector }}` |
| 104 | `      {{- with .Values.affinity }}` | `      {{- with .Values.supervisor.affinity }}` |
| 108 | `      {{- with .Values.tolerations }}` | `      {{- with .Values.supervisor.tolerations }}` |

Lines 37–43 (`DAGGER_CACHE_LOG_LEVEL`, `DAGGER_CACHE_DATABASE_PATH`,
`.Values.supervisor.extraEnv`) and lines 44–93, 101–103, 105–107, 109–111 are
unchanged (they already use `supervisor.*` or are structural).

### 2.2 `templates/pvc.yaml`

| Line | Current | New |
|------|---------|-----|
| 1  | `{{- if .Values.persistence.enabled }}` | `{{- if .Values.supervisor.persistence.enabled }}` |
| 14 | `      storage: {{ .Values.persistence.size | quote }}` | `      storage: {{ .Values.supervisor.persistence.size | quote }}` |
| 15 | `  {{- with .Values.persistence.storageClass }}` | `  {{- with .Values.supervisor.persistence.storageClass }}` |

Lines 16–17 (`storageClassName: {{ . | quote }}` and `{{- end }}`) are unchanged —
the values KEY is `storageClass` (chart's own PVC convention) and the rendered
Kubernetes field remains `storageClassName`. This is intentional and consistent
with the supervisor's own PVC; only the values path moves.

### 2.3 `templates/hpa.yaml`

| Line | Current | New |
|------|---------|-----|
| 1  | `{{- if .Values.autoscaling.enabled }}` | `{{- if .Values.supervisor.autoscaling.enabled }}` |
| 14 | `  minReplicas: {{ .Values.autoscaling.minReplicas }}` | `  minReplicas: {{ .Values.supervisor.autoscaling.minReplicas }}` |
| 15 | `  maxReplicas: {{ .Values.autoscaling.maxReplicas }}` | `  maxReplicas: {{ .Values.supervisor.autoscaling.maxReplicas }}` |
| 17 | `    {{- if .Values.autoscaling.targetCPUUtilizationPercentage }}` | `    {{- if .Values.supervisor.autoscaling.targetCPUUtilizationPercentage }}` |
| 23 | `          averageUtilization: {{ .Values.autoscaling.targetCPUUtilizationPercentage }}` | `          averageUtilization: {{ .Values.supervisor.autoscaling.targetCPUUtilizationPercentage }}` |
| 25 | `    {{- if .Values.autoscaling.targetMemoryUtilizationPercentage }}` | `    {{- if .Values.supervisor.autoscaling.targetMemoryUtilizationPercentage }}` |
| 31 | `          averageUtilization: {{ .Values.autoscaling.targetMemoryUtilizationPercentage }}` | `          averageUtilization: {{ .Values.supervisor.autoscaling.targetMemoryUtilizationPercentage }}` |

### 2.4 `templates/rbac.yaml`

| Line | Current | New |
|------|---------|-----|
| 8  | `{{- with .Values.serviceAccount.annotations }}` | `{{- with .Values.supervisor.serviceAccount.annotations }}` |

All other lines (ClusterRole, ClusterRoleBinding) unchanged.

### 2.5 `templates/_helpers.tpl`

**No changes.** Verified: lines 1–78 reference only `.Values.nameOverride`,
`.Values.fullnameOverride`, `.Values.namespace`, `.Values.tools.*`, and
`.Values.supervisor.config.*` — all unchanged by this plan.

---

## 3. `README.md` documentation sync (mandatory per AGENTS.md)

AGENTS.md requires docs to stay in sync with values. The chart README references
several top-level keys that are moving. Update these sections:

### 3.1 "Configuration reference > Top-level values" table (lines 231–243)

Update the rows that reference moved keys:

| Old row | New row |
|---------|---------|
| `\| \`image.repository\` \| Supervisor image \| \`ghcr.io/disaster/dagger-kubernetes\` \|` | `\| \`supervisor.image.repository\` \| Supervisor image \| \`ghcr.io/disaster/dagger-kubernetes\` \|` |
| `\| \`image.tag\` \| Image tag (defaults to \`Chart.appVersion\`) \| \`""\` \|` | `\| \`supervisor.image.tag\` \| Image tag (defaults to \`Chart.appVersion\`) \| \`""\` \|` |
| `\| \`replicaCount\` \| Supervisor replicas \| \`2\` \|` | `\| \`supervisor.replicaCount\` \| Supervisor replicas \| \`2\` \|` |
| `\| \`autoscaling.enabled\` \| Enable HPA for supervisor \| \`false\` \|` | `\| \`supervisor.autoscaling.enabled\` \| Enable HPA for supervisor \| \`false\` \|` |

Add new rows documenting the moved keys (optional but recommended for completeness):
`supervisor.persistence.enabled`, `supervisor.resources`, `supervisor.serviceAccount.annotations`,
`supervisor.podSecurityContext`, `supervisor.securityContext`.

### 3.2 "Production recommendations > High availability" (line 210)

Change:
`- **Supervisor**: run at least 2 replicas (\`replicaCount: 2\`).`
to:
`- **Supervisor**: run at least 2 replicas (\`supervisor.replicaCount: 2\`).`

### 3.3 "Production recommendations > Security" (line 225)

Change:
`- Enable \`podSecurityContext\` and \`securityContext\` (enabled by default).`
to:
`- Enable \`supervisor.podSecurityContext\` and \`supervisor.securityContext\` (enabled by default).`

### 3.4 "Production recommendations > Storage" (lines 130–160)

This block is already correct for the subcharts (uses `storageClassName` and
`loki.singleBinary.persistence`). No content change required, but add a one-line
note that the supervisor's own SQLite PVC is configured at
`supervisor.persistence` (with `storageClass` key, mapped to `storageClassName`
in the rendered PVC). Example to append after the existing code block:

```yaml
supervisor:
  persistence:
    enabled: true
    storageClass: "fast-ssd"
    size: 10Gi
```

### 3.5 "Install from source" step 4 comment (line 112)

The comment `#   ... edit supervisor.config.server.{dataHostname,publicUrl} ...`
remains valid. No change needed, but optionally add a note that
`supervisor.image.tag` / `supervisor.replicaCount` are now the override paths
for image and replica count.

---

## 4. Edge cases & backward compatibility

### 4.1 Breaking change — values override migration

Any user with an existing `my-values.yaml` that sets top-level `image:`,
`replicaCount:`, `resources:`, `persistence:`, `podAnnotations:`,
`podSecurityContext:`, `securityContext:`, `nodeSelector:`, `tolerations:`,
`affinity:`, `autoscaling:`, or `serviceAccount:` must move those keys under
`supervisor:`. Example migration:

```yaml
# BEFORE (old)
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

There is no automatic migration. `helm upgrade` with old-style overrides will
silently ignore the top-level keys (Helm does not error on unknown top-level
keys), causing the supervisor to fall back to chart defaults — e.g. `replicaCount: 2`
instead of the user's `3`. **Document this prominently in the README "Upgrading"
section** (add a subsection "Breaking: supervisor pod values moved under `supervisor:`").

### 4.2 Empty / omitted values behavior (unchanged)

All template guards use `{{- with ... }}` or `{{- if ... }}`, which are falsy for
empty/nil/zero values. Behavior is identical before and after the move:

| Value | Default | Template guard | Rendered when default |
|-------|---------|----------------|----------------------|
| `supervisor.image.tag` | `""` | `default .Chart.AppVersion` | falls back to `Chart.appVersion` |
| `supervisor.image.pullSecrets` | `[]` | `{{- with ... }}` | block omitted |
| `supervisor.podAnnotations` | `{}` | `{{- with ... }}` | block omitted |
| `supervisor.podSecurityContext` | (map) | `toYaml` (no guard) | always rendered |
| `supervisor.securityContext` | (map) | `toYaml` (no guard) | always rendered |
| `supervisor.resources` | (map) | `toYaml` (no guard) | always rendered |
| `supervisor.persistence.enabled` | `false` | `{{- if ... }}` in pvc.yaml + deployment `db` volume | PVC template skipped; `db` volume = emptyDir |
| `supervisor.persistence.storageClass` | `""` | `{{- with ... }}` | `storageClassName` line omitted → cluster default |
| `supervisor.nodeSelector` | `{}` | `{{- with ... }}` | block omitted |
| `supervisor.tolerations` | `[]` | `{{- with ... }}` | block omitted |
| `supervisor.affinity` | `{}` | `{{- with ... }}` | block omitted |
| `supervisor.autoscaling.enabled` | `false` | `{{- if ... }}` | HPA template skipped |
| `supervisor.autoscaling.targetMemoryUtilizationPercentage` | `""` | `{{- if ... }}` | memory metric omitted |
| `supervisor.serviceAccount.annotations` | `{}` | `{{- with ... }}` | annotations block omitted |

### 4.3 Subchart `storageClassName: ""` behavior

For `registry`, `tempo`, `victoria`, and `loki.singleBinary`, setting
`storageClassName: ""` (the new default) means "use the cluster default
StorageClass" — the standard Kubernetes convention. Setting it to a concrete
class name (e.g. `"fast-ssd"`) selects that class. This matches the existing
README documentation.

### 4.4 Loki persistence default change

Previously Loki had no PVC persistence values in `values.yaml`, relying on the
subchart's own default (which in grafana/loki 7.2.0 `SingleBinary` mode defaults
`persistence.enabled` to `false` — emptyDir, logs lost on restart). This plan
sets `loki.singleBinary.persistence.enabled: true` with `size: 20Gi`. **This
changes default behavior**: a fresh install will now provision a 20 Gi PVC for
Loki. Operators who want the old emptyDir behavior must set
`loki.singleBinary.persistence.enabled: false`. This is an intentional
improvement (persistence by default for a stateful backend) and is called out
here as a default change, not just a schema move.

### 4.5 `supervisor.config.fleet.engineStorageClass` is unrelated

Note: `supervisor.config.fleet.engineStorageClass` (line ~71 in the config
block) controls the StorageClass for **engine pods** spawned by the supervisor
fleet manager — it is NOT a subchart value and is NOT affected by this change.
Do not confuse it with the subchart `storageClassName` keys.

---

## 5. Validation checklist

Execute in order. All must pass.

1. **YAML lint**: `python3 -c "import yaml,sys; yaml.safe_load(open('deploy/helm/dagger-kubernetes/values.yaml'))"` (or `yamllint`) — confirms valid YAML, 2-space indent.
2. **helm dependency update**: `helm dependency build deploy/helm/dagger-kubernetes` — pulls subcharts (already vendored in `charts/`).
3. **helm lint**: `helm lint deploy/helm/dagger-kubernetes` — must pass with no errors.
4. **helm template default**: `helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug` — must render. Inspect:
   - Deployment `replicas:` equals `2`.
   - Deployment `image:` equals `ghcr.io/disaster/dagger-kubernetes:<appVersion>`.
   - Deployment `securityContext` (pod-level) has `runAsNonRoot: true` etc.
   - PVC template is **absent** (because `supervisor.persistence.enabled=false`).
   - HPA template is **absent** (because `supervisor.autoscaling.enabled=false`).
   - ServiceAccount has no `annotations:` block.
5. **helm template with persistence + autoscaling on**:
   ```
   helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug \
     --set supervisor.persistence.enabled=true \
     --set supervisor.persistence.size=5Gi \
     --set supervisor.persistence.storageClass=fast-ssd \
     --set supervisor.autoscaling.enabled=true \
     --set supervisor.autoscaling.maxReplicas=10 \
     --set supervisor.replicaCount=3
   ```
   - PVC `spec.resources.requests.storage` equals `"5Gi"`.
   - PVC `spec.storageClassName` equals `"fast-ssd"`.
   - HPA `maxReplicas` equals `10`, `minReplicas` equals `2`.
   - Deployment `replicas:` equals `3`.
6. **helm template with image override**:
   ```
   helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug \
     --set supervisor.image.tag=0.9.9 \
     --set supervisor.image.pullSecrets[0].name=ghcr-pull
   ```
   - Container `image:` equals `ghcr.io/disaster/dagger-kubernetes:0.9.9`.
   - `imagePullSecrets:` block present with `- name: ghcr-pull`.
7. **helm template with serviceAccount annotations**:
   ```
   helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug \
     --set supervisor.serviceAccount.annotations.iam\.gke\.io/gcp-service-account=svc@proj.iam
   ```
   - ServiceAccount `metadata.annotations` contains the key.
8. **helm template matrix (CI parity)** — run the three variants from `dagger/main.go` `helmTemplateMatrix`:
   - default (no sets)
   - `--set tools.otelCollector.enabled=false --set tools.registry.enabled=false`
   - all tools disabled
   All three must render without error (proves the move didn't break the existing CI matrix).
9. **Subchart storageClassName forwarding** — verify the subcharts receive the key:
   ```
   helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug \
     --set registry.persistence.storageClassName=fast-ssd \
     --set tempo.persistence.storageClassName=fast-ssd \
     --set victoria.server.persistentVolume.storageClassName=fast-ssd \
     --set loki.singleBinary.persistence.storageClassName=fast-ssd \
     --set loki.singleBinary.persistence.size=50Gi
   ```
   - The rendered docker-registry StatefulSet/Deployment PVC claims `storageClassName: fast-ssd`.
   - The rendered tempo PVC claims `storageClassName: fast-ssd`.
   - The rendered victoria PVC claims `storageClassName: fast-ssd`.
   - The rendered loki StatefulSet PVC claims `storageClassName: fast-ssd` and `50Gi`.
10. **Negative test — old top-level keys are ignored**: confirm the breaking-change behavior is silent (no error):
    ```
    helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug \
      --set replicaCount=5
    ```
    Deployment `replicas:` should be `2` (the new default), NOT `5` — proving the
    old top-level key is no longer read. (This documents the migration hazard.)
11. **README grep**: `grep -nE '\.Values\.(image|replicaCount|resources|persistence|podAnnotations|podSecurityContext|securityContext|nodeSelector|tolerations|affinity|autoscaling|serviceAccount)\b' deploy/helm/dagger-kubernetes/templates/` returns **no matches** after the template edits.
12. **Docs sync check**: `grep -n 'replicaCount: 2' deploy/helm/dagger-kubernetes/README.md` should only match `supervisor.replicaCount: 2`. `grep -nE '^\| .image.repository.' deploy/helm/dagger-kubernetes/README.md` should show `supervisor.image.repository`.

---

## 6. Dependencies & order of changes

Execute strictly in this order. Each step depends on the previous.

1. **`values.yaml`** — apply the full rewrite from section 1. This is the schema
   source of truth; templates reference it.
2. **`templates/deployment.yaml`** — apply the 12 path updates from section 2.1.
3. **`templates/pvc.yaml`** — apply the 3 path updates from section 2.2.
4. **`templates/hpa.yaml`** — apply the 7 path updates from section 2.3.
5. **`templates/rbac.yaml`** — apply the 1 path update from section 2.4.
6. **`README.md`** — apply the documentation sync from section 3.
7. **Validate** — run the checklist in section 5.

Steps 2–5 are independent of each other (different files) but all depend on
step 1. They may be done in any order after step 1. Step 6 depends on steps 1–5
being final (so doc paths match the actual template paths). Step 7 is the gate.

---

## 7. Risks

- **Silent override breakage** (mitigated by README "Upgrading" note): users on
  old top-level keys get chart defaults with no error. The negative test
  (checklist item 10) documents this; consider adding a `NOTES.txt` to print a
  warning if top-level `image`/`replicaCount`/etc. are present (out of scope for
  this plan — flagged as a possible follow-up).
- **Loki default now provisions a PVC** (section 4.4): clusters without a default
  StorageClass and with `storageClassName: ""` will fail to schedule Loki until a
  class is set. This is the standard Kubernetes behavior for all the other
  stateful subcharts (registry, tempo, victoria already default
  `persistence.enabled: true`), so Loki now matches them. Acceptable.
- **No `NOTES.txt` upgrade guidance**: the chart has no `templates/NOTES.txt`.
  Adding one with the migration mapping would help operators. **Out of scope**
  unless requested.

---

## 8. Out of scope

- Adding `templates/NOTES.txt` for upgrade guidance.
- Migrating `auth`, `ca`, `tls`, `service`, `ingress`, `serviceMonitor` under
  `supervisor:` (they are chart-level, not supervisor-pod-specific — correctly
  left at top level).
- Changing the supervisor's own PVC values key from `storageClass` to
  `storageClassName` (would be a second breaking change with no benefit; the
  template already maps it correctly).
- Object-storage configuration for Loki (`loki.loki.storage.*`) — unchanged.
- Any change to `supervisor.config.*` structure — unchanged.
- Any change to subchart versions or `Chart.yaml` dependencies.
