# Dagger Kubernetes

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
- [Pipeline history retention](#pipeline-history-retention)
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
GitHub Container Registry (GHCR) as an OCI artifact on every release. The
minting CA and the Raft transport CA are **auto-bootstrapped** on first boot —
you no longer need to generate certificates by hand (see
[TLS & client certificates](#tls--client-certificates)).

```bash
# 1. Create a values override
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

# 2. Install from the GHCR OCI repository. No certificate generation needed:
#    the minting CA + data-plane server cert are auto-generated on first boot.
helm install dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version 0.1.0 \
  -f my-values.yaml \
  --namespace dagger-stack --create-namespace

# 3. Create an API token from the UI (Settings page) or import a legacy token:
TOKEN=$(openssl rand -hex 32)
helm upgrade dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml --namespace dagger-stack \
  --set-string "auth.tokens[0]=$TOKEN"
```

> **Public certificates via cert-manager (optional):** the default `embedded`
> TLS provider issues a self-signed server certificate from the auto-generated
> minting CA — sufficient for mTLS but not publicly trusted. To serve a
> Let's Encrypt certificate instead, install
> [cert-manager](https://cert-manager.io) and enable
> `dataCert` (see [TLS & client certificates](#tls--client-certificates)).
> The minting CA is still auto-bootstrapped either way.

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

The chart also wires the **cache vhost**: when `ingress.enabled`, the Ingress
adds a host rule for `supervisor.config.cache.publicHost` (or the derived
`cache.<server.publicUrl host>`, overridable via `ingress.cacheHost`) routing
to the `-control` Service, and appends it to `ingress.tls[].hosts`. Set
`supervisor.config.cache.authToken` (or leave it empty to read the
`engine-registry-auth` Secret key `token`); the same token is injected into
engine pods as `DAGGER_CACHE_TOKEN`. The control-plane TLS certificate must
include the cache vhost as a SAN — the `embedded` provider adds it
automatically; when using cert-manager, include `cache.<host>` in the
certificate's `dnsNames`. For multiple backend registries, set
`supervisor.config.cache.registries` (list of
`{id, internalAddr, username, password}`).

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

# Remote shared cache ref. The Supervisor rewrites the ref to its dedicated
# cache vhost (cache.public_host), so the CLI/engine never talks to the raw
# registry. With ref_per_version=true the tag is per version (V0-21-4 here).
export _EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=cache.supv.example.com/dagger-cache:V0-21-4,mode=max"

dagger call github.com/your-org/ci@v1.0.0 build
```

Or skip the env-var juggling and use the wrapper:

```bash
./scripts/dagger-cache.sh call github.com/your-org/ci@v1.0.0 build
```

### Connect your environment (UI)

Instead of hand-assembling the variables above, use the **Connect** page in
the web UI (log in, then click **Connect** in the nav):

The Connect page **always** includes the remote shared cache ("MagicCache")
env var `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`, targeting the client's effective
engine version (the latest allowed release, or the version floor when no
release list is available). `_EXPERIMENTAL_DAGGER_TAG` is only added when you
explicitly pin a version.

1. Pick an engine version from the dropdown (optional — leave "No pin" to use
   the CLI default).
2. Check **"Show token plaintext"** to include your API token in the snippets.
3. Click the copy button for your target:
   - **Bash/zsh exports** or **.bashrc snippet** for interactive shells
     (these include the token plaintext directly).
   - **GitHub Actions env** or **GitLab CI variables** for CI (these use a
     secret reference by default; enable "Include plaintext token" only if you
     accept the risk, and paste the token into your CI secret store once).
4. Source the environment and run Dagger:

```bash
# 1. On the Connect page, check "Show token plaintext", click "Copy .bashrc snippet", paste into a shell.
# 2. Reload your shell (or: source ~/.dagger-cache.env).
dagger call github.com/your-org/ci@v1.0.0 build
```

Tokens created before this feature cannot be revealed by the Connect page —
regenerate them on the **Settings** page to enable full-snippet copy.

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
                                     │  ─ cache proxy (Host=cache vhost)    │
                                     └───┬─────────────┬─────────────┬─────┘
                                         │             │             │
                          mints client   │             │             │ forwards OTLP
                          cert + lease   │             │             │
                  ┌──────────────────────┘             │             └──► OTel Collector ─► Tempo/Loki/Victoria
                  │                                    │                        │
                  ▼                                    ▼                        ▼
   ┌─────────────────────────────┐      ┌──────────────────────────────┐  ┌───────────┐
   │  Engine fleet (K8s)         │      │  Cache proxy                  │  │  Grafana   │
   │  per-version StatefulSet    │      │  holds creds, routes across   │  │ dashboards │
   │  dagger-engine-v0-21-4      │      │  N registries (least-charged) │  └───────────┘
   │  autoscaled 0..N            │      └──────┬──────────┬────────────┘
   └─────────────────────────────┘             │          │
                                   push/pull   ▼          ▼
                                         ┌──────────┐ ┌──────────┐
                                         │ registry │ │ registry │  (or S3)
                                         │  reg-1   │ │  reg-2   │
                                         └──────────┘ └──────────┘
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
4. Engines push/pull BuildKit cache blobs through the Supervisor's cache
   proxy (Host = `cache.public_host`); the Supervisor validates the engine
   token, injects backend credentials, and routes to the right registry.
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
| `server.pipeline_url`                     | `DAGGER_CACHE_SERVER_PIPELINE_URL`                  |
| `cache.registry`                          | `DAGGER_CACHE_CACHE_REGISTRY`                       |
| `fleet.max_replicas_per_version`          | `DAGGER_CACHE_FLEET_MAX_REPLICAS_PER_VERSION`       |
| `log_level`                               | `DAGGER_CACHE_LOG_LEVEL`                            |
| `otel.otlp_endpoint`                      | `DAGGER_CACHE_OTEL_OTLP_ENDPOINT`                   |

The Docker compose stack uses only environment variables — no YAML is
mounted. Secrets (`OAUTH_CLIENT_ID`, `OAUTH_CLIENT_SECRET`) should always
come from env/secrets, never the file.

> **Note:** map-valued config keys (`fleet.engine_extra_env`,
> `fleet.engine_extra_env_from`, `fleet.engine_registry_mirrors`,
> `fleet.engine_node_selector`) cannot be overridden via `DAGGER_CACHE_`
> environment variables — Viper does not bind env vars to map types. Set
> these in the YAML config file or Helm values instead.

### Full reference

See [`config/config.app.yaml.sample`](../config/config.app.yaml.sample) for every key with
inline comments. The sections below summarise the most important ones.

| Section      | Key (representative)        | Default                          | Notes                                            |
|--------------|-----------------------------|----------------------------------|--------------------------------------------------|
| `server`     | `control_addr`              | `:8080`                          | Hertz control API (TLS when cert/key configured). |
|              | `data_addr`                 | `:8443`                          | mTLS L4 data proxy.                               |
|              | `data_hostname`             | `data.supv.example.com`          | Public data-plane hostname.                       |
|              | `public_url`                | `https://supv.example.com`       | Public control/UI URL.                            |
|              | `pipeline_url`              | `""` (falls back to `public_url`) | Base URL for pipeline-view links (absolute http(s)); only scheme + host are used. |
| `auth.internal` | `enabled`                | `true`                           | Username/password + legacy-token auth. `false` = OAuth-only (requires `auth.oauth` fully configured); auth is always enforced. |
|              | `tokens_file`               | `/etc/dagger-cache/tokens`       | One token per line.                               |
| `auth.oauth` | `enabled`                   | `false`                          | OAuth for UI login. Single active provider.       |
|              | `provider`                  | `github`                         | `"github"` \| `"oidc"` (generic OIDC).           |
|              | `allowed_orgs`              | —                                | Restrict login to members of these orgs (github) / groups-claim intersection (oidc). |
|              | `issuer_url`                | `""`                             | OIDC issuer; required for `provider: oidc`.        |
|              | `scopes`                    | `["openid","profile","email"]`   | OIDC scopes; `openid` always included.            |
|              | `username_claim`            | `preferred_username`             | OIDC username claim; fallback `email`.            |
|              | `groups_claim`              | `groups`                         | OIDC groups claim (array or single string).       |
|              | `cookie_secure`             | `false`                          | Set `true` when TLS terminates in front of the supervisor so the `oauth_state` cookie is marked `Secure`. |
| `auth.token` | `encryption_key`            | `""` (auto-generated)            | AES-256-GCM key (≥32 bytes) for token plaintext recovery (Connect page). |
| `database`   | `dir`                       | `/var/lib/dagger-cache`          | Raft data dir: `raft.db`, `snapshots/`, `node-id`. Fresh-start store (no migration). |
| `raft`       | `node_id`                   | `""` (auto-generated)            | Stable Raft node ID (persisted at `<dir>/node-id`). |
|              | `bind_addr`                 | `:8081`                          | Dedicated Raft transport port. |
|              | `advertise_addr`            | `""` (derived)                   | Routable `host:port`; empty = derived from `<hostname>.<headless_service>.<namespace>.svc.<cluster_domain>`. |
|              | `peers`                     | `[]` (single-node)               | Explicit voter list `[{id, address}]`; empty = DNS discovery. |
|              | `replicas`                  | `1`                              | Voter count for DNS peer discovery. |
|              | `statefulset_name`          | `""`                             | StatefulSet name for DNS discovery. |
|              | `headless_service`          | `""`                             | Headless Service name for stable pod DNS. |
|              | `namespace`                 | `""` (fleet ns)                  | K8s namespace for pod DNS. |
|              | `cluster_domain`            | `cluster.local`                  | K8s cluster DNS suffix. |
|              | `apply_timeout`             | `5s`                             | `raft.Apply` enqueue timeout. |
|              | `leader_wait_timeout`       | `30s`                            | Startup wait for leadership. |
|              | `snapshot_threshold`        | `1000`                           | Raft log snapshot threshold. |
|              | `snapshot_interval`         | `10m`                            | Raft snapshot interval. |
|              | `trailing_logs`             | `256`                            | Raft trailing logs after snapshot. |
|              | `tls.enabled`               | `false` (chart: `true`)          | mTLS for the Raft transport (recommended for multi-node). |
|              | `tls.dir`                   | `<database.dir>/tls`             | Internal raft CA + per-pod leaf cert directory. |
|              | `tls.validity`              | `8760h`                          | Leaf cert TTL. |
|              | `tls.organization`          | `dagger-cache-raft`              | CA/leaf subject organization. |
|              | `tls.ca_cert` / `tls.cert` / `tls.key` | `""`                   | Manual mode: pre-provisioned CA + leaf PEM paths (all three together). |
|              | `tls.ca_secret`             | `""`                             | Auto/K8s mode: Secret name for sharing the internal CA. |
|              | `tls.ca_bootstrap`          | `false`                          | Auto/K8s mode: force this node to generate + write the CA (auto-detects ordinal 0). |
|              | `tls.client_auth`           | `true`                           | mTLS: require + verify peer client certs. |
| `telemetry`  | `collector_url`             | `http://otel-collector:4318`     | OTLP/HTTP.                                         |
|              | `tempo_url` / `loki_url` / `victoria_url` | `http://tempo:3200` etc. | Backend query APIs (auto-wired by Helm).          |
| `cache`      | `backend`                   | `registry`                       | `registry` (OCI) or `s3`.                         |
|              | `registry`                  | `cache.reg/dagger-cache`          | OCI repository (legacy single-backend mode).      |
|              | `internal_addr`             | `""`                             | Legacy single backend address (used when `registries` empty). |
|              | `public_host`               | `cache.<public_url host>`         | Dedicated cache vhost (Supervisor proxy).         |
|              | `auth_token`                | `""`                             | Engine→proxy bearer; empty reads `engine-registry-auth` secret. |
|              | `registries`                | `[]`                             | Multi-backend list for load balancing.            |
|              | `s3.bucket` / `s3.region`    | —                                | Used only when `backend=s3`.                      |
|              | `ref_per_version`           | `true`                           | Tag cache refs `:V<maj>-<min>-<patch>`.           |
|              | `gc.enabled`                | `false`                          | Master switch for the cache auto-clean sweeper.   |
|              | `gc.max_age`                | `168h`                           | Purge tags older than this (7d).                  |
|              | `gc.schedule`               | `1h`                             | Sweeper ticker interval.                          |
|              | `gc.min_refs_to_keep`       | `3`                              | Keep at least this many most-recent tags per minor version. |
|              | `gc.protect_active_versions`| `true`                           | Never purge tags for versions with active replicas. |
| `history`    | `gc.enabled`                | `false`                          | Master switch for the history auto-purge sweeper. |
|              | `gc.max_age`                | `720h`                           | Purge traces whose last update is older than this (30d). |
|              | `gc.schedule`               | `1h`                             | History sweeper ticker interval.                  |
| `pipeline`   | `disconnect_grace`          | `0s`                             | Linger window before a closed tunnel fails the trace; `0s` = immediate. |
|              | `stale_sweep.enabled`       | `true`                           | Master switch for the pipeline stale-trace sweeper. |
|              | `stale_sweep.schedule`      | `1m`                             | Stale sweeper ticker interval.                    |
|              | `stale_sweep.stale_after`   | `5m`                             | Mark running traces with no active lease failed once older than this. |
| `fleet`      | `namespace`                 | `dagger-cache`                   | K8s namespace for engine pods.                    |
|              | `min_replicas_per_version`  | `0`                              | Autoscaler floor per version.                     |
|              | `max_replicas_per_version`  | `3`                              | Autoscaler ceiling per version.                   |
|              | `max_sessions_per_replica`  | `8`                              | Sessions pinned per pod.                          |
|              | `replica_idle_ttl`          | `5m`                             | Idle pod TTL before scale-down.                   |
|              | `version_retention`         | `24h`                            | Time a 0-replica StatefulSet lingers.             |
|              | `engine_extra_env`          | `{}`                             | Extra env vars on engine pods (proxy vars etc.).  |
|              | `engine_extra_env_from`     | `{}`                             | Env vars from Secret keys (proxy credentials).    |
|              | `engine_ca_secret`          | `""`                             | Secret with custom CA PEM bundle; empty = off.    |
|              | `engine_ca_secret_key`      | `ca.crt`                         | Key inside `engine_ca_secret`.                    |
|              | `engine_debug`              | `false`                          | `engine.toml: debug = true`.                      |
|              | `engine_log_format`         | `json`                           | `engine.toml: [log] format`; `""` omits.          |
|              | `engine_registry_mirrors`   | `{}`                             | `engine.toml` registry mirrors.                   |
| `ca`         | `minting_ca_secret`         | `supervisor-minting-ca`          | K8s Secret for the minting CA (holds the CA private key). **Auto-bootstrapped** on first boot; set `ca.crt`/`ca.key` to bring an existing CA. |
|              | `client_cert_ttl`           | `2h`                             | TTL of minted client certs.                       |
| `tls`        | `provider`                  | `embedded`                       | Server cert source: `embedded` (auto, self-signed) \| `cert-manager` \| `external`. Minting CA is auto-bootstrapped for all. |
|              | `cert_path` / `key_path`    | see loader                      | PEM paths for the `cert-manager`/`external` providers (chart auto-wires cert-manager). |
|              | `server_cert_secret`        | `supervisor-tls`                 | K8s Secret with `tls.crt`/`tls.key`.              |
|              | `lease_ttl`                 | `2m`                             | Lease TTL; clients renew before expiry.           |
| `version`    | `floor`                     | `v0.19.0`                        | Minimum engine version.                           |
|              | `allowlist`                 | —                                | `major.minor` prefixes to admit.                  |
| `ci.github`  | `job_summary` / `check_runs`| `true` / `true`                  | CI niceties.                                       |
| `ci.jenkins` | `dynamic_stages`            | `true`                           |                                                   |
| `ci.drone`   | `config_extension`          | `true`                           |                                                   |
| `log_level`  | —                           | `info`                           | `debug`/`info`/`warn`/`error`.                    |
| `log_format` | —                           | `json`                           | Supervisor log format: `json` / `text`.           |
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

### Enterprise engine environment

The supervisor can inject proxy settings, a custom CA bundle, and a generated
Dagger `engine.toml` into every engine pod, driven by `fleet.*` config:

- **Proxy env vars** (`fleet.engine_extra_env`, `map[string]string`) —
  injected as literal env vars on the engine container, sorted by name for
  deterministic pod specs. Covers `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` and
  their lowercase variants, plus any future env (e.g.
  `KUBERNETES_SERVICE_HOST`).
- **Secret-sourced env vars** (`fleet.engine_extra_env_from`,
  `map[string]{secret_name, key}`) — each entry is injected as an env var
  sourced from a Kubernetes Secret key (`SecretKeyRef`, not `Optional`).
  Use this for credentials that must never appear in plaintext config or
  Helm values, e.g. an authenticated proxy whose `HTTP_PROXY` value is
  `http://user:pass@proxy.corp.example:3128`. If the referenced Secret or
  key is missing in the cluster, the pod stays in `CreateContainerConfig`
  error until the operator fixes the Secret — the engine cannot reach the
  network without the proxy credentials anyway, so silent fallback is
  undesirable. The supervisor validates at startup that no env name is
  duplicated across `engine_extra_env` and `engine_extra_env_from`, and
  that none collides with the supervisor-injected `DAGGER_CACHE_TOKEN` (or
  `SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS` when CA injection is enabled).
- **Custom CA bundle** (`fleet.engine_ca_secret` + `engine_ca_secret_key`,
  default `ca.crt`) — references an existing K8s Secret holding a PEM CA
  bundle. The bundle is mounted read-only at
  `/etc/ssl/certs/custom-ca.pem` (the Secret key is normalized to file
  `ca.crt` via a volume `Items` projection, so any key name works), and
  `SSL_CERT_FILE` + `NODE_EXTRA_CA_CERTS` are pointed at it. The volume is
  NOT `Optional`: a missing Secret/key fails the pod loudly rather than
  running with a dangling `SSL_CERT_FILE`. Empty `engine_ca_secret`
  disables CA injection entirely (pre-change behavior).
- **Generated `engine.toml`** (`fleet.engine_debug`,
  `fleet.engine_log_format`, `fleet.engine_registry_mirrors`) — the
  supervisor renders a legacy BuildKit-style `engine.toml` and stores it in
  a fleet-wide ConfigMap `dagger-engine-config` (key `engine.toml`), which
  it ensures on every `EnsureStatefulSet` (i.e. on every acquire). The
  ConfigMap is mounted via `subPath` at `/etc/dagger/engine.toml`, which
  the Dagger engine (v0.19+) reads automatically — no extra engine arg or
  env var is needed. Config edits propagate to new pods on the next
  acquire; already-running pods keep the old config until restarted or
  scaled. When the rendered TOML is empty (debug=false, log format `""`,
  no mirrors), no ConfigMap volume/mount is added and any stale ConfigMap
  is deleted best-effort — the pod spec reverts to the pre-change shape.
  By default `engine_log_format: "json"` renders `[log]\n  format = "json"`,
  so every engine gets the mount by default (intended behavior change).

See [ADR-011](design/ADR-011-engine-env-ca-config-injection.md) for the
full rationale and alternatives considered.

---

## Remote shared cache

Self-hosted OCI registry (`registry:2`) storing BuildKit cache blobs. The
Supervisor acts as a reverse proxy in front of the registry(ies): it holds the
registry credentials, validates the engine's cache token, and load-balances
across one or more backend registries. Engines push/pull cache layers per
solve; the client picks the cache ref via `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`:

```
type=registry,ref=cache.supv.example.com/dagger-cache:V0-21-4,mode=max
```

The emitted ref always points at the **Supervisor's cache vhost**
(`cache.public_host`), never the raw registry. `cache.public_host` defaults to
`cache.<host-of-server.public_url>` and must differ from the control-plane host.

In single-backend mode (the default), the Supervisor proxies to
`cache.internal_addr` (or the backend host derived from `cache.registry` when
`cache.internal_addr` is empty). With `cache.registries[]` configured, it
load-balances across all of them instead.

With `cache.ref_per_version: true` (default), the wrapper script
automatically derives the `:V<maj>-<min>-<patch>` tag from
`_EXPERIMENTAL_DAGGER_TAG`, giving each engine version its own cache
namespace and avoiding cross-version cache poisoning.

### Multi-registry cache

To spread cache "charge" across several registries, configure
`cache.registries[]`:

```yaml
cache:
  backend: "registry"
  public_host: "cache.supv.example.com"
  auth_token: ""                       # or set DAGGER_CACHE_CACHE_AUTH_TOKEN
  registries:
    - id: "reg-1"
      internal_addr: "registry-1:5000"
      username: ""
      password: ""
    - id: "reg-2"
      internal_addr: "registry-2:5000"
      username: ""
      password: ""
```

Routing strategy (see ADR-014):

- **Push** (new manifest or blob upload) goes to the **least-charged** healthy
  backend, where charge is the Supervisor's own per-backend manifest-size sum
  from periodic catalog walks.
- **Pull** first consults the persisted Raft-backed routing table
  (`cache_object_routes` / `cache_blob_routes`). On a miss it probes healthy
  backends (least-charged first) and self-heals the table on a hit.
- **Upload sessions** are pinned to one backend for the whole
  `POST → PATCH → PUT` upload lifecycle.

Backend credentials (`username`/`password`) are injected by the Supervisor and
never reach the engine. The engine authenticates to the Supervisor cache proxy
with `DAGGER_CACHE_TOKEN`, which must equal `cache.auth_token` (or, when
`cache.auth_token` is empty, the Supervisor reads the `engine-registry-auth`
K8s secret key `token` — the same secret already mounted into engine pods).

> **TLS SAN requirement:** the control-plane certificate must include
> `cache.public_host` as a SAN (the cache vhost shares the control-plane TLS
> listener). The Supervisor disables the global HTTP read timeout so multi-GB
> blob uploads are not killed; control-API request bodies remain capped
> per-handler (`POST /v1/engines` 1 MiB).

For S3-backed cache instead of OCI:

```yaml
cache:
  backend: "s3"
  s3:
    bucket: "my-dagger-cache"
    region: "us-east-1"
```

> **Note:** cache size/object stats are only implemented for the registry
> backend in this release; an s3-backed cache reports `total_size:-1` and an
> "s3 cache stats not supported" note (see ADR-012).

### Cache auto-clean (GC)

A background sweeper can purge stale cache tags. It is **disabled by default**:

```yaml
cache:
  gc:
    enabled: true                 # master switch
    max_age: "168h"               # purge tags older than 7d
    schedule: "1h"                # sweeper interval
    min_refs_to_keep: 3           # keep the newest N tags per minor version
    protect_active_versions: true # never purge tags for versions with running engines
```

Age is taken from the manifest's `org.opencontainers.image.created` annotation;
tags without it are never purged (conservative). GC never purges cache refs for
versions that still have active engine replicas when
`protect_active_versions` is set, even if `fleet.version_retention` would
otherwise allow their StatefulSets to be deleted.

### Purging cache (admin)

The registry must be started with delete enabled
(`REGISTRY_STORAGE_DELETE_ENABLED=true`) for purge to work. From the MagicCache
page, admins can purge a single version's cache ref or all tags. The underlying
endpoints are `POST /api/v1/cache/purge` and `POST /api/v1/cache/purge-all`
(admin-only); a delete-disabled registry returns `409 "registry delete not
enabled"`.

---

## Pipeline history retention

Pipeline history = trace metadata (Raft FSM) + Loki logs + VictoriaMetrics
metrics. A background sweeper can auto-purge it, and admins can purge manually
from the `/history` page. See ADR-018.

```yaml
history:
  gc:
    enabled: true                 # master switch (disabled by default)
    max_age: "720h"               # purge traces whose last update is older than 30d
    schedule: "1h"                # sweeper interval
```

Age is `COALESCE(started_at, updated_at)`; traces with no known timestamp are
never purged (conservative). **Running traces are protected**: the sweeper and
`purge-all` skip traces whose status is `""` or `"running"`. A manual
per-trace purge is an admin override and is not protected.

### Manual purge (admin)

| Endpoint | Scope |
|---|---|
| `GET /api/v1/history` | Stats: `trace_count`, oldest update, collected time, GC rules. |
| `POST /api/v1/history/purge` | Body `{"trace_id":"<hex>"}`. Purges one trace (metadata + logs + metrics), running or not. Idempotent. |
| `POST /api/v1/history/purge-all` | Purges every trace older than `history.gc.max_age` (running traces protected). |

For each candidate, Loki logs + VictoriaMetrics series are deleted
best-effort **first**; a telemetry failure is logged but does not abort the
trace-metadata deletion, so the trace still disappears from the UI (orphaned
telemetry ages out via backend retention).

### Telemetry backend prerequisites

- **Loki** — `POST /loki/api/v1/delete` is enabled by default: the chart runs
  the Loki **compactor** with `limits_config.deletion_mode: filter-and-delete`,
  `compactor.retention_enabled: true`, and a `delete_request_store`
  (`filesystem` by default). For object-storage deployments, set
  `delete_request_store` to the S3/GCS bucket used for delete requests.
- **VictoriaMetrics** — `delete_series` is admin-only and deletes the entire
  series matching `match[]` (no time range; space is reclaimed lazily during
  background merges). If `-deleteAuthKey` is set on the VM deployment, the
  supervisor's delete request must include that key. The OpenTelemetry
  collector's `transform/logs` processor promotes `trace_id`/`span_id` to
  **log** labels only; the metrics pipeline (`otlp → batch →
  prometheusremotewrite`) has no such transform, and the metrics currently
  emitted (BuildKit cache hit/miss counters, engine metrics) are aggregate
  with no trace association. `{trace_id="..."}` metric deletion is therefore a
  no-op today and activates automatically only if per-trace metrics carrying a
  `trace_id` label are introduced.
- **Tempo** — spans are **not** deleted by this feature. Set `tempo.retention`
  to match (or exceed) `history.gc.max_age` so spans age out alongside the
  supervisor-side purge (see [Telemetry stack](#telemetry-stack)).

### Pipeline disconnect detection

The L4 data-plane tunnel is the *owning* client connection: the Dagger CLI holds
it open for the lifetime of a run. If that tunnel closes before the run's OTLP
finish record arrives (e.g. the CLI was killed), the supervisor marks the
associated `trace_meta` `failed` with `failure_reason: "client connection lost"`
— idempotently, and only when the trace is still non-terminal. Passive UI
viewers (the `/api/v1/traces/:id/live` SSE stream) never trigger this. See
ADR-019.

A background staleness sweeper recovers traces orphaned by a supervisor
restart/crash: it marks `running` traces with no active lease (`InFlight == 0`)
older than `pipeline.stale_sweep.stale_after` as `failed` with
`failure_reason: "client session expired"`.

```yaml
pipeline:
  disconnect_grace: "0s"    # 0s = fail immediately on tunnel close (default).
                            # >0s = linger window; a same-trace reconnect cancels.
  stale_sweep:
    enabled: true           # supervisor-restart / crash recovery sweeper.
    schedule: "1m"
    stale_after: "5m"
```

`GET /api/v1/traces/:id` returns the `failure_reason` field on the merged
`TraceMeta` so the UI can render why a pipeline failed.

---

## Authentication

The supervisor supports multi-user authentication with role-based access
control (RBAC). Users have roles `admin` or `user`; users belong to zero or
more **groups**; groups carry engine-session quotas and project visibility.

### Auth mechanisms

- **Username + password** → JWT (HS256, access 15m / refresh 7d, rotated on
  use). The primary path for human/UI login.
- **GitHub OAuth** (`auth.oauth.enabled: true` with `provider: github`) → JWT.
  The callback is `/api/v1/auth/oauth/github/callback`. `allowed_orgs` restricts
  who may log in (empty = allow all); `default_group` auto-joins new OAuth
  users to a group.
- **Generic OIDC** (`auth.oauth.enabled: true` with `provider: oidc`) → JWT.
  Discovery via `issuer_url` `/.well-known/openid-configuration`; the callback
  is `/api/v1/auth/oauth/oidc/callback`. `allowed_orgs` is matched against the
  `groups_claim` (default `groups`). Covers Dex, Keycloak, Google, Auth0, etc.
- **Per-user API tokens** (`dct_<32 random bytes hex>`) for CI. Each user has
  at most one token; the plaintext is shown once at creation/regeneration.
  Tokens are stored as a SHA-256 hash plus an AES-256-GCM-encrypted ciphertext
  so the **Connect** page can reveal them on demand. Use it as
  `DAGGER_CLOUD_TOKEN`. This is the recommended path for CI. Tokens created
  before the ciphertext column was added are not recoverable — regenerate to
  enable reveal.
- **Legacy flat-file tokens** (`auth.internal.tokens_file`) — DEPRECATED.
  When configured, tokens in the file still authenticate as a synthetic
  `legacy` admin identity (full access, quota bypass) for zero-breakage
  migration. Run `supervisor migrate-tokens` to import them as real users +
  API tokens, then remove the key.

Auth is always enforced — there is no fully-disabled mode. Setting
`auth.internal.enabled: false` disables username/password login and legacy
flat-file tokens; it requires `auth.oauth.enabled: true` with a fully
configured provider, or the supervisor refuses to start. When internal auth is
disabled, `POST /api/v1/auth/login` returns 404 and OAuth is the sole login
path.

### OAuth provider: generic OIDC (Dex example)

```yaml
auth:
  internal:
    enabled: false
  oauth:
    enabled: true
    provider: "oidc"
    issuer_url: "https://dex.example.com"
    client_id: "dagger-cache"
    client_secret: "${OAUTH_CLIENT_SECRET}"
    redirect_url: "https://supv.example.com/api/v1/auth/oauth/oidc/callback"
    allowed_orgs: ["devs"]   # matches the `groups` claim from Dex
    default_group: ""
    scopes: ["openid", "profile", "email", "groups"]
    username_claim: "preferred_username"
    groups_claim: "groups"
```

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
random secret on first boot and persists it in the Raft-backed `meta` store. Set it
explicitly in secret storage (Helm Secret / env) for production before
dropping the auto-generated one.

### API-token encryption key

`auth.token.encryption_key` (at least 32 bytes) encrypts token plaintexts at
rest so the Connect page can reveal them. The value is SHA-256-derived into a
fixed 32-byte AES-256-GCM key before use, so any secret ≥ 32 bytes works. When
empty, the supervisor auto-generates a 32-byte key on first boot and persists
it in the Raft-backed `meta` store (dev mode, with a startup warning). Set it
explicitly via env (`DAGGER_CACHE_AUTH_TOKEN_ENCRYPTION_KEY`) or a K8s Secret
in production so DB compromise alone does not yield token plaintexts — exactly
as with the JWT secret.

### Configuration

See the [Full reference](#full-reference) for all `auth.*`, `database.*`, and
`raft.*` keys. Key env overrides: `DAGGER_CACHE_AUTH_JWT_SECRET`,
`DAGGER_CACHE_DATABASE_DIR`, `DAGGER_CACHE_RAFT_BIND_ADDR`,
`DAGGER_CACHE_AUTH_BOOTSTRAP_ADMIN_PASSWORD`.

### Storage (Raft) & multi-user migration

The supervisor persists all multi-user RBAC state, trace metadata, and the
cache routing tables in a **Hashicorp Raft** replicated state machine (see
ADR-015). Raft always runs — a single-node deployment is just a one-voter
cluster. On first boot with an empty FSM the store starts **fresh**: there is
**no migration path** from the legacy SQLite store, and the `modernc.org/sqlite`
dependency has been removed from the project entirely.

- **Single-node (default):** leave `raft.peers` empty; the node bootstraps with
  itself as the only voter and is always the leader.
- **Multi-node:** the Helm chart ships a `StatefulSet` + headless Service.
  Peers are discovered from the StatefulSet's stable pod DNS names
  (`<sts>-<i>.<headless>.<ns>.svc.cluster.local:8081` for `i=0..replicas-1`) —
  pure DNS arithmetic, no K8s API calls. Each pod advertises its pod FQDN, not
  `127.0.0.1`. Set `raft.replicas` (and the chart's `supervisor.replicaCount`)
  to an **odd number ≥ 3** for quorum fault tolerance; a 2-node cluster loses
  quorum on a single failure.
- **Transport TLS:** the Raft transport is **mTLS** when `raft.tls.enabled` is
  true (the Helm chart default). A self-signed internal CA is generated with
  `goca` and shared across pods via the `<release>-raft-ca` Kubernetes Secret;
  each pod issues itself a per-node leaf certificate (SANs = pod DNS names +
  `127.0.0.1`). Pod-0 writes the CA Secret; the others poll it before issuing
  their leaf. TLS 1.2+, `RequireAndVerifyClientCert`. For non-Helm deploys you
  can pre-provision CA + leaf PEM files via `raft.tls.ca_cert`/`cert`/`key`
  (manual mode) — `raft.tls.enabled` must be set uniformly across all peers.
  The `<release>-raft-ca` Secret contains the internal CA **private key** (any
  pod may issue peer certs), and the engine-client minting CA is likewise
  shared via the `<release>-minting-ca` Secret. Both Secrets hold CA private
  keys and must be RBAC-restricted to the supervisor ServiceAccount.
- **Follower reads:** every node waits until *a* leader exists, then serves
  **stale local reads**. Writes are leader-only via `raft.Apply`; a follower
  returns `ErrNotLeader` (HTTP 503) on writes — clients retry. The leader
  provisions the JWT secret and token-encryption key; followers wait for those
  meta keys to replicate to their local FSM before becoming Ready.
- **Scale-up / scale-down:** the leader runs a `joinLoop` that reconciles the
  cluster membership with the discovered voter list (`raft.AddVoter` /
  `raft.RemoveServer`). Scale-up: bump `replicaCount` + `raft.replicas` and
  rolling-restart. Scale-down: shrink both, rolling-restart, then delete the
  removed pod. A pod that loses its PVC (`<dir>/node-id`) becomes a new node
  that must re-join.
- **Data directory** `database.dir` holds `raft.db` (bolt log + stable store),
  `snapshots/`, `node-id`, and `tls/` (CA + leaf). Persist it on a per-pod PVC.
- The bootstrap-admin flow provisions a fresh admin when the user count is 0,
  so a fresh deployment is immediately usable.

The legacy flat-file token fallback and the `supervisor migrate-tokens`
subcommand remain (they import flat-file tokens, not SQLite data):

1. **Deploy new binary.** The Raft store starts fresh at `database.dir`; the
   bootstrap admin is created. Existing CI keeps working via the legacy
   fallback (runs as `legacy` admin identity — exactly today's full-access
   behavior).
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
- The Raft data directory (`raft.db`, `snapshots/`, `node-id`) holds password
  hashes, token hashes/ciphertexts, the JWT secret, and the token-encryption
  key. `raft.db` and `node-id` are created with `0600` permissions; the
  `snapshots/` directory is tightened to `0700` at startup. Persist the
  directory on a volume owned by the supervisor user.
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

1. Holds a **server cert** — where it comes from depends on `tls.provider`
   (see below).
2. Holds a **minting CA** in `ca.minting_ca_secret`; it signs short-lived
   (`ca.client_cert_ttl`) client certs at lease grant. The minting CA is
   **auto-bootstrapped on first boot** for every provider: ordinal 0 of the
   supervisor StatefulSet generates a goca CA and writes it to the
   `<release>-minting-ca` Secret; the other pods poll the Secret before
   issuing anything. The `tls.ca_path` files are kept as a local cache. The
   Secret contains the CA **private key** (any pod may mint client certs) and
   must be RBAC-restricted to the supervisor ServiceAccount — the chart
   already does this.
3. Pins each minted cert's lease to a specific engine pod via the L4 proxy.

The **Raft transport** is likewise mTLS (chart default) with its own
auto-bootstrapped internal CA (`<release>-raft-ca`) — see
[Storage (Raft)](#storage-raft--multi-user-migration).

### TLS providers

| `tls.provider` | Server certificate source | Minting CA | Notes |
|---|---|---|---|
| `embedded` (default) | Self-signed, issued by the auto-generated minting CA | auto-bootstrapped | Zero config. Server cert SANs cover the data host, cache vhost, and pod names automatically. |
| `cert-manager` | cert-manager `Certificate` (e.g. Let's Encrypt) | auto-bootstrapped | Publicly trusted server cert; the minting CA is still internal. |
| `external` | Operator-managed PEM files (`tls.cert_path`/`tls.key_path`) | auto-bootstrapped | Bring your own keypair (e.g. from an external PKI). |

**Zero certificate generation:** with `embedded` (the default) or
`cert-manager`, you never run `openssl` by hand — the minting CA, the Raft CA,
and the server cert (embedded) are all generated automatically. To bring an
existing minting CA (e.g. migrating a prior deployment), set `ca.crt`/`ca.key`
in the chart values; the supervisor reuses it instead of generating a new one.

### cert-manager (public server certificate)

Install [cert-manager](https://cert-manager.io) and a `ClusterIssuer`, then
point the chart at it. The minting CA is still auto-bootstrapped — it signs
engine client certs and never needs a public issuer.

```bash
# 1. Create a ClusterIssuer once (cluster-wide)
cat <<'EOF' | kubectl apply -f -
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

# 2. Install with the cert-manager provider
helm install dagger-kubernetes oci://ghcr.io/disaster/charts/dagger-kubernetes \
  --version 0.1.0 -f my-values.yaml --namespace dagger-stack \
  --set supervisor.config.tls.provider=cert-manager \
  --set dataIngress.enabled=true \
  --set dataIngress.host=data.your-domain.com \
  --set dataIngress.annotations.cert-manager.io/cluster-issuer=letsencrypt-prod \
  --set dataIngress.tls.secretName=dagger-data-tls
```

The chart renders a cert-manager `Certificate` (`dataCert.enabled: true` +
`dataCert.issuerName`/`issuerKind`) and auto-wires `tls.cert_path`/`key_path`
to the mounted Secret (`/etc/dagger-kubernetes/data-tls/tls.crt` + `.key`).
The `dataIngress` (nginx `ssl-passthrough`) forwards the raw mTLS handshake to
the supervisor, which serves the cert-manager certificate.

> The certificate must include the **cache vhost** (`cache.<public_url host>`)
> as a SAN alongside the data host, because the cache proxy shares the
> control-plane TLS listener. Add both to the `Certificate` `dnsNames` (the
> chart's `dataCert` template only adds `dataIngress.host`; include the cache
> host via a custom `Certificate` or set `ingress.cacheHost` and add it to the
> issuer's request).

For local dev (Docker compose) mTLS is relaxed; in Kubernetes the minting CA
and Raft CA are auto-bootstrapped, and the server cert is either auto-issued
(`embedded`) or cert-manager-managed — no manual certificate provisioning is
required.

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
persistent volumes. **Retention**: the supervisor's history purge does not
delete spans; set `tempo.retention` (Helm `tempo.tempo.retention`) to match or
exceed `history.gc.max_age` so spans age out alongside the supervisor-side
purge of trace metadata, logs, and metrics.

### Grafana Loki
Log aggregation backend. Stores OTLP logs. Exposes the Loki HTTP API on port
3100. Deployed in SingleBinary mode (sufficient for up to ~20 GB/day). For
production, switch to `SimpleScalable` with S3/GCS object storage.
**Deletion**: enabled by default — the chart runs the Loki compactor with
`limits_config.deletion_mode: filter-and-delete`, `retention_enabled: true`,
and a `delete_request_store` (filesystem by default). For object-storage
deployments, set `delete_request_store` to the S3/GCS bucket used for delete
requests.

### VictoriaMetrics
PromQL-compatible metrics backend. Stores OTLP metrics via the Prometheus
remote write protocol. Exposes the HTTP API on port 8428. Single-server
deployment with persistent volumes. **Deletion**: the history purge calls
`POST /api/v1/admin/tsdb/delete_series`, which is admin-only and deletes the
entire series matching `match[]` (no time range); space is reclaimed lazily
during background merges. If `-deleteAuthKey` is set on the VM deployment, the
supervisor's delete request must include that key.

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
It is always served by the control plane at `/` and single-pipeline views at
`/pipelines/<id>`. No separate configuration is needed.

Features:
- **Pipeline list** — every run identified by a friendly name (`@username · org/repo`,
  or the root-folder/module name when there is no git repo) with status,
  duration, and engine version; the raw trace ID is shown as a secondary
  reference under the name. The list auto-refreshes every 10s while any run is
  in flight, and the per-row duration ticks live every 1s until the run finishes
- **Trace viewer** — compact step view: one row per high-level step (direct
  children of the root span, with Dagger `dagger.io/ui.passthrough` spans
  promoted) showing status and wall-clock duration. Sub-spans are collapsed
  and summarised as a hidden count; click a step to expand. `dagger.io/ui.*`
  boolean span attributes drive the collapse/passthrough grouping.
  The trace viewer header shows an `@username` chip (or `anonymous` for
  legacy/anonymous runs) next to the status badge, and the Details table
  includes a "User" row — so the pipeline owner is always visible on the
  detail view, matching the list view's `@username · org/repo` identity.
- **Live updates** — the viewer subscribes to the `/api/v1/traces/:id/live`
  SSE stream. As the supervisor ingests each OTLP trace/log batch it extracts
  the affected trace IDs and broadcasts a lightweight `trace_update` or
  `logs_update` event; the viewer debounces these into an immediate re-fetch
  of steps/logs so new spans and log lines appear as the pipeline runs. A 5s
  polling fallback remains for resilience.
- **Duration** — shown prominently in the viewer header next to the status and
  in the details table; while a pipeline or step is `running`, the displayed
  duration ticks live every 250ms (Details) / 1s (list) from the
  server-provided `start_time`/`started_at` absolute timestamp, and freezes at
  the final server `duration_ms` once the run finishes. The
  `/api/v1/traces/:id` response returns `duration_ms` in milliseconds (matching
  the list endpoint), with the raw value available as `duration_ns`
- **Log viewer** — log lines correlated by span ID (the collector promotes
  `trace_id` and `span_id` to Loki labels) and rendered inline under the step
  or sub-span that produced them (`GET /api/v1/traces/:id/logs`); logs with no
  recognisable span are grouped under a collapsed "unmatched" section. Logs
  load on open and auto-refresh every few seconds while the pipeline is still
  running. Each log container auto-scrolls to the end when opened and sticks
  to the bottom while new lines stream in; scrolling up unpins, scrolling back
  to the bottom re-pins (with a small hysteresis so jitter does not flap the
  state). Dagger engine verbose progress payloads (base64 protobufs) are
  collapsed to a placeholder rather than rendered as base64.
- **Fleet dashboard** — active engines, replicas per version, session counts
- **MagicCache dashboard** (`/cache`) — cache running state, total size,
  object (layer) count, hit rate (from VictoriaMetrics BuildKit counters),
  per-version cache refs (size, layers, digest, protected flag), auto-clean
  (GC) rules with last/next run, and admin-only purge buttons
- **History dashboard** (`/history`) — pipeline-history trace count + oldest
  update, auto-purge (GC) rules with last/next run summary, and admin-only
  per-trace / purge-all buttons
- **Services status page** (`/services`) — every platform service
  (supervisor, cache, collector, tempo, loki, victoria, fleet) with a
  `ok`/`degraded`/`down`/`unknown` state and a rolled-up overall state
- **Connect page** (`/connect`) — ready-to-copy Dagger CLI environment:
  every required env var (`DAGGER_CLOUD_URL`, `DAGGER_CLOUD_TOKEN`,
  `_EXPERIMENTAL_DAGGER_RUNNER_HOST`, and the always-present
  `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`, plus `_EXPERIMENTAL_DAGGER_TAG` only
  when you pin a version) with one-click copy of bash/zsh exports,
  a `.bashrc` snippet, GitHub Actions `env:`, GitLab CI `variables:`, and a
  "Copy token value" button. The token is masked by default; checking "Show
  token plaintext" reveals it on demand (CI snippets use secret references by
  default).
- **Header status indicator** — a colored dot in the navbar (green/amber/red/
  grey) polling `/api/v1/status` every 10s; clicking navigates to `/services`
- **Cache status** — registry health, cache hit rates

### Pipeline view URL

The stock `dagger` CLI hardcodes the pipeline/trace link it prints to
`https://dagger.cloud/<org>/traces/<id>` (`engine/telemetry/url.go`); it never
reads a server-side field, header, or endpoint to derive that host. A
self-hosted platform therefore cannot redirect the bare CLI's printed link —
the platform supplies the self-hosted URL through the wrapper and a dedicated
endpoint instead:

- **`dagger-cache-ci` wrapper** (`cmd/ci`) wraps `dagger`, extracts the trace
  ID from its stderr, and prints `Pipeline View: <base>/pipelines/<id>` (the
  canonical UI route). The base is resolved with precedence `--ui-url` >
  `server.pipeline_url` (config) > `server.public_url` (config) > `--server`.
- **`GET /api/v1/traces/:id/url`** returns
  `{"trace_id":"<id>","url":"<base>/pipelines/<id>"}`, auth-gated by the same
  visibility rules as the trace detail endpoint (owner/member/admin; unknown
  metadata → admin-only). Clients that already know a trace ID can resolve the
  self-hosted URL without parsing CLI output. The same URL is also returned as
  the `url` field of `GET /api/v1/traces/:id`.

The base URL is `server.pipeline_url` when set, otherwise `server.public_url`.
It must be an absolute `http(s)` URL; only its scheme + host are used (the
path `/pipelines/<id>` is fixed, and any path/query/fragment on the configured
base is dropped, so links stay stable behind proxies and TLS-terminating
ingresses). Set it explicitly to point pipeline-view links at a different
public host than the control plane:

```yaml
server:
  public_url: "https://supv.example.com"            # control plane + API + UI
  pipeline_url: "https://dagger.supv.example.com"   # optional; pipeline-view links only
```

If `server.public_url` is also empty, the supervisor refuses to start (it
cannot derive a pipeline-view URL). See
[ADR-021](design/ADR-021-pipeline-view-url.md).

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

It derives the cache ref (`cache.<public_host>/dagger-cache:V0-21-4`) from
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
- [ ] Use `cert-manager` (`tls.provider: cert-manager`) for a publicly trusted server certificate (optional; `embedded` is fully auto-provisioned but self-signed)
- [ ] Back up the auto-generated `<release>-minting-ca` and `<release>-raft-ca` Secrets (or set `ca.crt`/`ca.key` explicitly to reuse a known CA)
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
