# Dagger Cache (`dagger-kubernetes`)

A self-hosted, **Dagger-Cloud-compatible** platform that gives you remote
shared cache, auto-scaling engine fleets, a live pipeline UI, and drop-in CI
integration — without sending your builds or telemetry to a third party.

The Supervisor (`cmd/api`) provides three functions:

1. **Control Plane** (Hertz HTTPS) — `POST /v1/engines` provisions an engine
   pod for the requested Dagger version and returns a lease + certificate.
2. **Data Plane** (mTLS L4 proxy) — pins the client's TLS connection to the
   specific engine replica pod that holds its lease.
3. **OTLP Ingest** — forwards Dagger CLI telemetry to the local stack
   (Tempo / Loki / VictoriaMetrics) and powers the pipeline UI.

The Dagger CLI talks to the Supervisor exactly as it would talk to Dagger
Cloud: same `DAGGER_CLOUD_URL` / `DAGGER_CLOUD_TOKEN` env vars, same
`dagger-cloud://self` runner host, same cache-config env var.

---

## Table of contents

- [Quick start](#quick-start)
  - [Docker (local dev)](#docker-local-dev)
  - [Kubernetes (Helm)](#kubernetes-helm)
  - [Client setup](#client-setup)
- [Architecture](#architecture)
- [Configuration](#configuration)
  - [Files](#files)
  - [Environment variables](#environment-variables)
  - [Full reference](#full-reference)
- [Running the Supervisor](#running-the-supervisor)
- [Engine fleet](#engine-fleet)
- [Remote shared cache](#remote-shared-cache)
- [Authentication](#authentication)
- [TLS & client certificates](#tls--client-certificates)
- [Telemetry stack](#telemetry-stack)
- [Pipeline UI](#pipeline-ui)
- [CI integrations](#ci-integrations)
  - [GitHub Actions](#github-actions)
  - [Jenkins](#jenkins)
  - [Drone](#drone)
- [Client wrapper script](#client-wrapper-script)
- [Operations](#operations)
- [Production checklist](#production-checklist)
- [Contract drift monitoring](#contract-drift-monitoring)
- [Development](#development)

---

## Quick start

### Docker (local dev)

The fastest way to get a running stack (Supervisor + OTel Collector + Tempo +
Loki + VictoriaMetrics + Grafana + a local OCI cache registry):

```bash
cd deploy/docker
docker compose up -d --build
```

Ports exposed:

| Service        | Port | Notes                                  |
|----------------|------|----------------------------------------|
| Supervisor ctl | 8080 | control API + UI                       |
| Supervisor data| 8443 | mTLS data plane                         |
| OTel collector | 4318 | OTLP/HTTP                               |
| Tempo          | 3200 | traces API                             |
| Loki           | 3101 | logs API (host port 3101→3100)         |
| VictoriaMetrics| 8428 | metrics API                            |
| Grafana        | 3000 | anonymous login enabled                |
| Cache registry | 5000 | `registry:2`, stores BuildKit blobs    |

The compose file configures the Supervisor entirely through
`DAGGER_CACHE_*` environment variables, so no `config.app.yaml` is mounted
in dev mode.

### Kubernetes (Helm)

The recommended production deployment uses the Helm chart published to the
GitHub Container Registry (GHCR) as an OCI artifact on every release:

```bash
# 1. Generate certificates
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -days 3650 -nodes -keyout ca.key -out ca.crt \
  -subj "/CN=Dagger Minting CA"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -days 365 -nodes -keyout tls.key -out tls.crt \
  -subj "/CN=data.your-domain.com" \
  -addext "subjectAltName=DNS:data.your-domain.com"

# 2. Generate a token
TOKEN=$(openssl rand -hex 32)

# 3. Create a values override
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

# 4. Install from the GHCR OCI repository
helm install dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version 0.1.0 \
  -f my-values.yaml \
  --namespace dagger-stack --create-namespace \
  --set ca.crt="$(cat ca.crt)" \
  --set ca.key="$(cat ca.key)" \
  --set tls.crt="$(cat tls.crt)" \
  --set tls.key="$(cat tls.key)" \
  --set-string "auth.tokens[0]=$TOKEN"
```

To list available chart versions:

```bash
helm show chart oci://ghcr.io/disaster/charts/dagger-kubernetes | grep version
```

For local development or customization, you can install directly from the
chart source by cloning the repository:

```bash
git clone https://github.com/disaster/dagger-kubernetes.git
cd dagger-kubernetes
helm dependency build deploy/helm/dagger-kubernetes
helm install dagger-kubernetes deploy/helm/dagger-kubernetes -f my-values.yaml ...
```

The Helm chart deploys:
- **Supervisor** — control plane + data plane
- **OpenTelemetry Collector** — OTLP ingest, fans out to Tempo/Loki/VictoriaMetrics
- **Grafana Tempo** — distributed tracing (traces)
- **Grafana Loki** — log aggregation (logs)
- **VictoriaMetrics** — PromQL-compatible metrics
- **Grafana** — dashboards with auto-provisioned datasources
- **OCI Registry** — remote shared cache backend

Every tool is toggleable individually (`tools.<name>.enabled: false`). When
disabled, you provide your own endpoint in `supervisor.config.telemetry` and
`supervisor.config.cache`.

See [`deploy/helm/dagger-kubernetes/README.md`](../deploy/helm/dagger-kubernetes/README.md)
for full Helm documentation, production sizing, and upgrade instructions.

### Client setup

Once the Supervisor is reachable, point the Dagger CLI at it:

```bash
export DAGGER_CLOUD_URL=https://supv.example.com
export DAGGER_CLOUD_TOKEN=<token you minted>
export _EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self

# Optional: pin an engine version (recommended for cache locality).
export _EXPERIMENTAL_DAGGER_TAG=v0.21.4

# Remote shared cache ref. With ref_per_version=true the Supervisor
# tags cache refs per version (V0-21-4 here).
export _EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=cache.reg/dagger-cache:V0-21-4,mode=max"

dagger call github.com/your-org/ci@v1.0.0 build
```

Or skip the env-var juggling and use the wrapper:

```bash
./scripts/dagger-cache.sh call github.com/your-org/ci@v1.0.0 build
```

---

## Architecture

```
                ┌──────────────── DAGGER CLI ────────────────┐
                │  DAGGER_CLOUD_URL  DAGGER_CLOUD_TOKEN       │
                │  _EXPERIMENTAL_DAGGER_RUNNER_HOST=cloud://self
                │  _EXPERIMENTAL_DAGGER_CACHE_CONFIG=...      │
                └───────────────────┬───────────────────────┘
                                    │
            control API (HTTPS)     │     data plane (mTLS L4)
   POST /v1/engines ───────────────►┌────────────────────────────────────┐
   GET  /v1/leases/...               │             S U P E R V I S O R     │
   POST /v1/leases/.../renew        │  control :8080    data :8443         │
                                     │  ─ Hertz API      ─ L4 TLS proxy    │
                                     │  ─ UI (SPA)       ─ pins to pod IP   │
                                     │  ─ OTLP forward                       │
                                     └───┬─────────────┬─────────────┬─────┘
                                         │             │             │
                          mints client   │             │             │ forwards OTLP
                          cert + lease   │             │             │
                  ┌──────────────────────┘             │             └──► OTel Collector ─► Tempo/Loki/Victoria
                  │                                    │                        │
                  ▼                                    ▼                        ▼
   ┌─────────────────────────────┐      ┌────────────────────────────────┐  ┌───────────┐
   │  Engine fleet (K8s)         │      │  Cache (OCI registry / S3)      │  │  Grafana   │
   │  per-version StatefulSet    │◄─────│  registry:2 or S3 bucket        │  │ dashboards │
   │  dagger-engine-v0-21-4      │push/│  ref: cache.reg/...:V0-21-4    │  └───────────┘
   │  autoscaled 0..N            │pull │                                │
   └─────────────────────────────┘      └────────────────────────────────┘
```

**Flow for a `dagger call`:**

1. CLI sends `POST /v1/engines` to the control plane with the requested
   version (from `_EXPERIMENTAL_DAGGER_TAG`, or the CLI's default).
2. Supervisor resolves the version against `version.floor` /
   `version.allowlist`, mints a client cert (signed by the minting CA),
   creates/updates a per-version StatefulSet, and returns a lease + the
   pod's data-plane address.
3. CLI opens a TLS connection to `data_hostname` using the minted cert.
   The Supervisor's L4 proxy inspects SNI/cert, looks up the lease, and
   pipes bytes to the live engine pod.
4. CLI pushes/pulls BuildKit cache blobs from the configured registry ref
   (`_EXPERIMENTAL_DAGGER_CACHE_CONFIG`).
5. CLI emits OTLP telemetry; the Supervisor forwards it to the local
   collector, which fans out to Tempo (traces), Loki (logs) and
   VictoriaMetrics (metrics). The pipeline UI reads those backends directly.
6. Grafana provides unified dashboards over all three telemetry backends.

---

## Configuration

### Files

| File                             | Purpose                                            |
|----------------------------------|----------------------------------------------------|
| `config/config.app.yaml`         | Live config checked into the repo (example values). |
| `config/config.app.yaml.sample`  | Fully-commented reference. Copy → edit → deploy.   |

The Supervisor's `--config` flag points at the file to load (default
`config/config.app.yaml`; in the container this is typically mounted as
`/etc/dagger-kubernetes/config.app.yaml`). The `config/config.app.yaml`
shipped here is a **minimal** example: it only lists deployment-specific
values; every other option falls back to the compiled-in defaults in
`config/loader.go`.

To start from scratch:

```bash
cp config/config.app.yaml.sample config/config.app.yaml
$EDITOR config/config.app.yaml
```

### Environment variables

All keys can be overridden by environment variables using the `DAGGER_CACHE_`
prefix, with dots replaced by underscores and upper-cased. Environment
variables **take precedence** over the file. Examples:

| YAML key                                  | Environment variable                                |
|-------------------------------------------|-----------------------------------------------------|
| `server.public_url`                       | `DAGGER_CACHE_SERVER_PUBLIC_URL`                    |
| `cache.registry`                          | `DAGGER_CACHE_CACHE_REGISTRY`                       |
| `fleet.max_replicas_per_version`          | `DAGGER_CACHE_FLEET_MAX_REPLICAS_PER_VERSION`       |
| `log_level`                               | `DAGGER_CACHE_LOG_LEVEL`                            |
| `otel.otlp_endpoint`                      | `DAGGER_CACHE_OTEL_OTLP_ENDPOINT`                   |

The Docker compose stack uses only environment variables — no YAML is
mounted. Secrets (`OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`) should always
come from env/secrets, never the file.

### Full reference

See [`config/config.app.yaml.sample`](../config/config.app.yaml.sample) for every key with
inline comments. The sections below summarise the most important ones.

| Section      | Key (representative)        | Default                          | Notes                                            |
|--------------|-----------------------------|----------------------------------|--------------------------------------------------|
| `server`     | `control_addr`              | `:8080`                          | Hertz control API (TLS when cert/key configured). |
|              | `data_addr`                 | `:8443`                          | mTLS L4 data proxy.                               |
|              | `data_hostname`             | `data.supv.example.com`          | Public data-plane hostname.                       |
|              | `public_url`                | `https://supv.example.com`       | Public control/UI URL.                            |
| `auth.internal` | `enabled`                | `true`                           | Static bearer-token auth.                         |
|              | `tokens_file`               | `/etc/dagger-cache/tokens`       | One token per line.                               |
| `auth.oauth` | `enabled`                   | `false`                          | OAuth (GitHub) for UI login.                      |
|              | `provider`                  | `github`                         |                                                   |
|              | `allowed_orgs`              | —                                | Restrict login to members of these orgs.          |
| `telemetry`  | `collector_url`             | `http://otel-collector:4318`     | OTLP/HTTP.                                         |
|              | `tempo_url` / `loki_url` / `victoria_url` | `http://tempo:3200` etc. | Backend query APIs (auto-wired by Helm).          |
| `cache`      | `backend`                   | `registry`                       | `registry` (OCI) or `s3`.                         |
|              | `registry`                  | `cache.reg/dagger-cache`          | OCI repository.                                   |
|              | `s3.bucket` / `s3.region`    | —                                | Used only when `backend=s3`.                      |
|              | `ref_per_version`           | `true`                           | Tag cache refs `:V<maj>-<min>-<patch>`.           |
| `fleet`      | `namespace`                 | `dagger-cache`                   | K8s namespace for engine pods.                    |
|              | `min_replicas_per_version`  | `0`                              | Autoscaler floor per version.                     |
|              | `max_replicas_per_version`  | `3`                              | Autoscaler ceiling per version.                   |
|              | `max_sessions_per_replica`  | `8`                              | Sessions pinned per pod.                          |
|              | `replica_idle_ttl`          | `5m`                             | Idle pod TTL before scale-down.                   |
|              | `version_retention`         | `24h`                            | Time a 0-replica StatefulSet lingers.             |
| `ca`         | `minting_ca_secret`         | `supervisor-minting-ca`          | K8s Secret with the minting CA.                   |
|              | `client_cert_ttl`           | `2h`                             | TTL of minted client certs.                       |
| `tls`        | `server_cert_secret`        | `supervisor-tls`                 | K8s Secret with `tls.crt`/`tls.key`.              |
|              | `lease_ttl`                 | `2m`                             | Lease TTL; clients renew before expiry.           |
| `version`    | `floor`                     | `v0.19.0`                        | Minimum engine version.                           |
|              | `allowlist`                 | —                                | `major.minor` prefixes to admit.                  |
| `ci.github`  | `job_summary` / `check_runs`| `true` / `true`                  | CI niceties.                                       |
| `ci.jenkins` | `dynamic_stages`            | `true`                           |                                                   |
| `ci.drone`   | `config_extension`          | `true`                           |                                                   |
| `log_level`  | —                           | `info`                           | `debug`/`info`/`warn`/`error`.                    |
| `otel`       | `otlp_endpoint`             | `""`                             | If set, the Supervisor exports its own OTLP here. |

Durations are parsed by Viper (e.g. `"5m"`, `"24h"`, `"2m"`).

---

## Running the Supervisor

From source:

```bash
go build -o dagger-cache-ci ./cmd/ci    # CLI helper
go build -o supervisor ./cmd/api                # server
./supervisor --config=config/config.app.yaml
```

Or via the published Docker image from GHCR:

```bash
docker run -p 8080:8080 -p 8443:8443 \
  -v "$PWD/config/config.app.yaml:/etc/dagger-cache/config.app.yaml:ro" \
  -v "$PWD/tokens:/etc/dagger-cache/tokens:ro" \
  ghcr.io/disaster/dagger-kubernetes:latest
```

To build from source:

```bash
docker build -t dagger-cache/supervisor:latest .
docker run -p 8080:8080 -p 8443:8443 \
  -v "$PWD/config/config.app.yaml:/etc/dagger-cache/config.app.yaml:ro" \
  -v "$PWD/tokens:/etc/dagger-cache/tokens:ro" \
  dagger-cache/supervisor:latest
```

Health endpoints (control port):

- `GET /healthz` — liveness
- `GET /readyz`  — readiness

---

## Engine fleet

Engines run as a **per-version Kubernetes StatefulSet**, e.g.
`dagger-engine-v0-21-4`. The autoscaler (configured under `fleet:`) scales
each StatefulSet between `min_replicas_per_version` and
`max_replicas_per_version` based on active leases; pods with no active
sessions for `replica_idle_ttl` are scaled down, and a version that has had
zero replicas for `version_retention` is garbage-collected (StatefulSet +
PVs removed).

The fleet provider contains both a stub (in-memory, for testing) and a real
Kubernetes provider (`internal/repository/k8s_provider.go`) that manages StatefulSets,
Services, Pods, PVCs, and ConfigMaps.

---

## Remote shared cache

Self-hosted OCI registry (`registry:2`) storing BuildKit cache blobs.
Engines push/pull cache layers per solve; the client picks the cache ref
via `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`:

```
type=registry,ref=cache.reg/dagger-cache:V0-21-4,mode=max
```

With `cache.ref_per_version: true` (default), the wrapper script
automatically derives the `:V<maj>-<min>-<patch>` tag from
`_EXPERIMENTAL_DAGGER_TAG`, giving each engine version its own cache
namespace and avoiding cross-version cache poisoning.

For S3-backed cache instead of OCI:

```yaml
cache:
  backend: "s3"
  s3:
    bucket: "my-dagger-cache"
    region: "us-east-1"
```

---

## Authentication

The supervisor supports multi-user authentication with role-based access
control (RBAC). Users have roles `admin` or `user`; users belong to zero or
more **groups**; groups carry engine-session quotas and project visibility.

### Auth mechanisms

- **Username + password** → JWT (HS256, access 15m / refresh 7d, rotated on
  use). The primary path for human/UI login.
- **GitHub OAuth** (`auth.oauth.enabled: true`) → JWT. The callback is handled
  by the backend at `/api/v1/auth/oauth/github/callback`, which 302s to the
  SPA with the tokens in the URL fragment. `allowed_orgs` restricts who may
  log in (empty = allow all); `default_group` auto-joins new OAuth users to a
  group.
- **Per-user API tokens** (`dct_<32 random bytes hex>`) for CI. Each user has
  at most one token; the plaintext is shown once at creation/regeneration;
  only the SHA-256 hash is stored. Use it as `DAGGER_CLOUD_TOKEN`. This is the
  recommended path for CI.
- **Legacy flat-file tokens** (`auth.internal.tokens_file`) — DEPRECATED.
  When configured, tokens in the file still authenticate as a synthetic
  `legacy` admin identity (full access, quota bypass) for zero-breakage
  migration. Run `supervisor migrate-tokens` to import them as real users +
  API tokens, then remove the key.

### Bootstrap admin

On first boot with an empty `users` table, the supervisor creates an admin
from `auth.bootstrap_admin.username` (default `admin`). When
`auth.bootstrap_admin.password` is empty, a random 16-byte hex password is
generated and logged once at WARN (the only place a credential is ever
logged). Set the password explicitly in production.

### Groups, projects, and quota

- **Groups** carry `max_runner_sessions` (0 = unlimited),
  `agent_available` (whether engines can be provisioned for the group), and
  an optional `auto_assign_pattern` regex.
- **Projects** (CI pipelines identified by repo slug) are assigned to groups
  manually (admin UI), pre-created, or auto-matched by the first group (by id
  order) whose `auto_assign_pattern` matches the project name.
- **Quota**: a group's active sessions = active leases of all its members. A
  multi-group user's session counts against EACH of their groups. Admission to
  `POST /v1/engines` requires ≥1 group with `agent_available=true` and
  remaining capacity; admins bypass. Users with no groups get 403.

### Trace visibility

- Admins see all traces.
- Users see traces where `group_id` is one of their groups, OR `user_id` is
  themselves (owner always sees own pipelines, even when unassigned).
- Unknown/missing trace metadata → admin-only (fail-closed; non-admins get
  404, not 403, to avoid leaking existence).

### JWT secret

`auth.jwt.secret` (HS256). When empty, the supervisor auto-generates a 32-byte
random secret on first boot and persists it in the SQLite `meta` table. Set it
explicitly in secret storage (Helm Secret / env) for production before
dropping the auto-generated one.

### Configuration

See the [Full reference](#full-reference) for all `auth.*` and `database.*`
keys. Key env overrides: `DAGGER_CACHE_AUTH_JWT_SECRET`,
`DAGGER_CACHE_DATABASE_PATH`, `DAGGER_CACHE_AUTH_BOOTSTRAP_ADMIN_PASSWORD`.

### Migration (flat-file → multi-user)

Rollout is backward compatible; no big-bang.

1. **Deploy new binary.** DB auto-created at `database.path`; bootstrap admin
   created. Existing CI keeps working via the legacy fallback (runs as
   `legacy` admin identity — exactly today's full-access behavior).
2. **Import tokens.** `supervisor migrate-tokens --config config.app.yaml`
   imports each token line as user `legacy-N` (role `user`) with that exact
   token as its API token, member of an auto-created `legacy` group. Idempotent
   (skips tokens already present by hash). Reassign users/groups/projects in
   the UI afterwards.
3. **Cutover.** Remove/empty `auth.internal.tokens_file` in config. Legacy
   fallback disappears; only imported tokens, JWTs, and OAuth remain. Rotate
   the bootstrap admin password; set `auth.jwt.secret` explicitly.
4. **Configure attribution.** Admin creates real groups, assigns users, sets
   `auto_assign_pattern` per group, pre-creates projects as needed.

### Security notes

- Password endpoints (`/api/v1/auth/login`, `/api/v1/auth/password`) are
  protected by an in-memory per username+IP lockout: after 5 consecutive
  failures the key is locked for 30s, doubling per further failure up to
  15min. State resets on supervisor restart (single-node deployment).
- `auth.jwt.secret`, when set explicitly, must be at least 32 bytes (HS256,
  RFC 7518); shorter values are rejected at startup. When empty, a 32-byte
  random secret is generated and persisted in the database on first boot.
- The SQLite database file (password hashes, token hashes, JWT secret) is
  created with `0600` permissions.
- All responses carry `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: DENY`, `Content-Security-Policy: frame-ancestors 'none'`
  (clickjacking), and `Referrer-Policy: no-referrer` (keeps the SSE
  `?token=` param out of Referer headers).
- Refresh-token revocation is stateless today; password change does not
  invalidate existing JWTs until expiry (access TTL is 15m).
- Trace backfill of group metadata after project reassignment is intentional
  (set-once).
- `?token=` query-param auth (D14) is limited to the SSE `/live` endpoint
  (EventSource cannot set headers).

---

## TLS & client certificates

The data plane is mTLS-only. The Supervisor:

1. Holds a server cert in the `tls.server_cert_secret` K8s Secret
   (`tls.crt` + `tls.key`).
2. Holds a minting CA in `ca.minting_ca_secret`; it signs short-lived
   (`ca.client_cert_ttl`) client certs at lease grant.
3. Pins each minted cert's lease to a specific engine pod via the L4 proxy.

For local dev (Docker compose) mTLS is relaxed; in Kubernetes you must
provision both secrets before applying the Supervisor.

---

## Telemetry stack

The telemetry stack is integrated as Helm subchart dependencies and powers
the pipeline UI and Grafana dashboards. Each component is toggleable:

### OpenTelemetry Collector
Receives OTLP/HTTP telemetry from the Dagger CLI and the Supervisor on port
4318. Fans out traces to Tempo, logs to Loki, and metrics to VictoriaMetrics.

### Grafana Tempo
Distributed tracing backend. Stores OTLP traces. Exposes the HTTP query API
on port 3100. The Helm chart defaults to local filesystem storage with
persistent volumes.

### Grafana Loki
Log aggregation backend. Stores OTLP logs. Exposes the Loki HTTP API on port
3100. Deployed in SingleBinary mode (sufficient for up to ~20 GB/day). For
production, switch to `SimpleScalable` with S3/GCS object storage.

### VictoriaMetrics
PromQL-compatible metrics backend. Stores OTLP metrics via the Prometheus
remote write protocol. Exposes the HTTP API on port 8428. Single-server
deployment with persistent volumes.

### Grafana
Unified dashboards for Tempo (traces), Loki (logs), and VictoriaMetrics
(metrics). Datasources are auto-provisioned via a ConfigMap with label
`grafana_datasource: "1"`. Default credentials: `admin` / `admin` (change in
production). Includes trace-to-logs correlation configured out of the box.

Default URLs (auto-wired by Helm):

| Component | Config key | Default URL |
|---|---|---|
| OTel Collector | `telemetry.collector_url` | `<release>-opentelemetry-collector:4318` |
| Tempo | `telemetry.tempo_url` | `<release>-tempo:3100` |
| Loki | `telemetry.loki_url` | `<release>-loki:3100` |
| VictoriaMetrics | `telemetry.victoria_url` | `<release>-victoria-metrics-single:8428` |

To export the Supervisor's *own* OTLP (e.g. to the same collector), set
`otel.otlp_endpoint`. Leave it empty to disable.

---

## Pipeline UI

The UI is an embedded Vue 3 SPA (packaged in `ui-dist/` via `//go:embed`).
It is always served by the control plane at `/` and trace links like
`/traces/<id>`. No separate configuration is needed.

Features:
- **Pipeline list** — all CI runs with status, duration, engine version
- **Trace viewer** — waterfall view of spans, with child/parent relationships
- **Live view** — SSE-streamed trace updates during execution
- **Log viewer** — log lines correlated by trace ID, queried from Loki
- **Fleet dashboard** — active engines, replicas per version, session counts
- **Cache status** — registry health, cache hit rates

---

## CI integrations

### GitHub Actions

```yaml
- uses: ./ci-integrations/gha
  with:
    server-url: https://supv.example.com
    token: ${{ secrets.DAGGER_CLOUD_TOKEN }}
    ui-url: https://ui.supv.example.com        # optional
    version: v0.21.4                            # optional, pins engine version
    module: github.com/org/ci@v1.0.0
    args: build
```

Feature flags `ci.github.job_summary` and `ci.github.check_runs` add a
step summary with the trace link and Check Runs annotated with cache stats.

### Jenkins

Shared library at `ci-integrations/jenkins/daggerCache.groovy`:

```groovy
@Library('dagger-cache') _
daggerCache(serverUrl: 'https://supv.example.com',
            token: env.DAGGER_CLOUD_TOKEN,
            uiUrl: 'https://ui.supv.example.com',
            version: 'v0.21.4') {
  sh 'dagger call github.com/org/ci@v1.0.0 build'
}
```

`ci.jenkins.dynamic_stages: true` splits Dagger steps into Jenkins stages.

### Drone

Config extension at `ci-integrations/drone/config-extension.sh`, packaged
as the `dagger-cache/drone-config-extension` plugin:

```yaml
steps:
  - name: dagger-cache
    image: dagger-cache/drone-config-extension
    settings:
      server_url: https://supv.example.com
      token:
        from_secret: dagger_cache_token
      version: v0.21.4
```

`ci.drone.config_extension: true` enables the `.drone.yml` extension that
appends a summary step with the trace link.

---

## Client wrapper script

`scripts/dagger-cache.sh` wires up the standard env vars and prints the
pipeline-view link after the run:

```bash
export DAGGER_CACHE_SERVER=https://supv.example.com
export DAGGER_CACHE_UI=https://ui.supv.example.com
export DAGGER_CLOUD_TOKEN=<token>
export DAGGER_TAG=v0.21.4          # optional

./scripts/dagger-cache.sh call github.com/your-org/ci@v1.0.0 build
```

It derives the cache ref (`cache.reg/dagger-cache:V0-21-4`) from
`DAGGER_TAG`, sets `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`, runs `dagger "$@"`,
then greps the run log for the trace ID and prints a boxed link to
`$DAGGER_CACHE_UI/traces/<id>`. The GHA, Jenkins, and Drone integrations
all delegate to (or mirror) this script.

---

## Operations

- **Provision a token:** `openssl rand -hex 32` → append to
  `tokens_file` (or the `supervisor-tokens` Secret).
- **Admit a new Dagger version:** add its `major.minor` to
  `version.allowlist`; bump `version.floor` if needed; restart the
  Supervisor. Engines for that version are created lazily on first lease.
- **Rotate the minting CA:** create a new `supervisor-minting-ca` Secret
  and restart; existing leases keep working until they expire
  (`ca.client_cert_ttl`).
- **Tune the autoscaler:** `fleet.max_replicas_per_version` (cost ceiling),
  `fleet.max_sessions_per_replica` (per-pod density),
  `fleet.replica_idle_ttl` (scale-down aggressiveness),
  `fleet.version_retention` (how long a quiet version lingers).
- **Health:** `GET /healthz` and `GET /readyz` on the control port are
  wired to the K8s probes.
- **Backups:** the cache registry and telemetry backends use persistent
  volumes. Back up the following PVCs:
  - Registry PV (cache data)
  - Tempo PV (trace data)
  - Loki PV (log data)
  - VictoriaMetrics PV (metrics data)

---

## Production checklist

- [ ] Set `grafana.adminPassword` to a strong value
- [ ] Use `cert-manager` or external TLS provider instead of embedded CA
- [ ] Provision all required K8s Secrets before install (CA, TLS, tokens)
- [ ] Configure persistent storage for all stateful components
- [ ] Set appropriate resource requests/limits per component
- [ ] Configure ingress with TLS for the control plane
- [ ] Enable Prometheus ServiceMonitor if using Prometheus Operator
- [ ] Set `fleet.minReplicasPerVersion: 1` for warm engine pools
- [ ] Configure object storage (S3/GCS) for Loki and Tempo in production
- [ ] Use external OAuth provider (GitHub) with org restrictions for UI access
- [ ] Rotate tokens regularly

---

## Contract drift monitoring

The Supervisor mirrors the Dagger cloud contract. Monitor these files in
the Dagger source tree for breaking changes:

- `core/schema` — `EngineSpec` format returned by
  `POST /v1/engines`.
- `engine/telemetry/cloud.go` — OTLP export configuration.
- `engine/client/client.go` — cache env var handling
  (`_EXPERIMENTAL_DAGGER_CACHE_CONFIG`) and runner-host negotiation.

When any of these change shape, update
[`internal/handler`](../internal/handler) (control handlers and L4 data-plane proxy) and the
[`tests/integration/api_test.go`](../tests/integration/api_test.go) contract tests
accordingly.

---

## Development

```bash
# Build
go build ./...

# Run unit + integration tests
go test ./...

# Run the dev stack
cd deploy/docker && docker compose up -d --build

# Build the UI
cd ui && npm install && npm run build

# Lint
golangci-lint run ./...

# Run the full CI pipeline locally (Dagger CLI required — see DAGGER.md)
dagger call -m ./dagger --src . ci export --path out
```

Integration tests (`tests/integration/api_test.go`) exercise the full
provision → lease → data-plane flow against stubbed fleet/cache/CA
providers, so they run without a cluster.

## License

See [LICENSE](../LICENSE).
