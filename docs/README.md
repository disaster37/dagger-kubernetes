# Dagger Cache

A self-hosted, **Dagger-Cloud-compatible** platform that gives you remote
shared cache, auto-scaling engine fleets, a live pipeline UI, and drop-in CI
integration — without sending your builds or telemetry to a third party.

The Supervisor (`cmd/supervisor`) provides three functions:

1. **Control Plane** (Hertz HTTPS) — `POST /v1/engines` provisions an engine
   pod for the requested Dagger version and returns a lease + certificate.
2. **Data Plane** (mTLS L4 proxy) — pins the client's TLS connection to the
   specific engine replica pod that holds its lease.
3. **OTLP Ingest** — forwards Dagger CLI telemetry to the local stack
   (Tempo / Loki / Prometheus) and powers the pipeline UI.

The Dagger CLI talks to the Supervisor exactly as it would talk to Dagger
Cloud: same `DAGGER_CLOUD_URL` / `DAGGER_CLOUD_TOKEN` env vars, same
`dagger-cloud://self` runner host, same cache-config env var.

---

## Table of contents

- [Quick start](#quick-start)
  - [Docker (local dev)](#docker-local-dev)
  - [Kubernetes](#kubernetes)
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
- [Telemetry & UI](#telemetry--ui)
- [CI integrations](#ci-integrations)
  - [GitHub Actions](#github-actions)
  - [Jenkins](#jenkins)
  - [Drone](#drone)
- [Client wrapper script](#client-wrapper-script)
- [Operations](#operations)
- [Contract drift monitoring](#contract-drift-monitoring)
- [Development](#development)

---

## Quick start

### Docker (local dev)

The fastest way to get a running stack (Supervisor + OTel Collector + Tempo +
Loki + Prometheus + Grafana + a local OCI cache registry):

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
| Loki           | 3100 | logs API                               |
| Prometheus     | 9090 | metrics API                            |
| Grafana        | 3000 | anonymous login enabled                |
| Cache registry | 5000 | `registry:2`, stores BuildKit blobs    |

The compose file configures the Supervisor entirely through
`DAGGER_CACHE_*` environment variables, so no `config.app.yaml` is mounted
in dev mode.

### Kubernetes

```bash
# 1. Create namespace + RBAC, the cache registry, the telemetry stack,
#    and the Supervisor Deployment/Service/Ingress.
kubectl apply -f deploy/k8s/

# 2. Create the config from the sample, then load it as a ConfigMap.
cp config.app.yaml.sample config.app.yaml
# edit hostnames, OAuth creds, allowed versions, etc.
kubectl create configmap supervisor-config \
  --from-file=config.app.yaml -n dagger-cache

# 3. Provision TLS + minting-CA secrets (see TLS section below).
kubectl create secret tls supervisor-tls \
  --cert=tlscert.pem --key=tlskey.pem -n dagger-cache
# ... and a minting CA secret named supervisor-minting-ca

# 4. Create a token for your CI clients.
TOKEN=$(openssl rand -hex 32)
echo "$TOKEN" > tokens
kubectl create secret generic supervisor-tokens \
  --from-file=tokens -n dagger-cache
```

The Supervisor reads `/etc/dagger-cache/config.app.yaml` (see
`deploy/k8s/supervisor.yaml`). Health/readiness probes hit `/healthz` and
`/readyz` on the control port.

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
./cmd/dagger-cache.sh call github.com/your-org/ci@v1.0.0 build
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
                  ┌──────────────────────┘             │             └──► OTel Collector ─► Tempo/Loki/Prom
                  │                                    │
                  ▼                                    ▼
   ┌─────────────────────────────┐      ┌────────────────────────────────┐
   │  Engine fleet (K8s)         │      │  Cache (OCI registry / S3)      │
   │  per-version StatefulSet    │◄─────│  registry:2 or S3 bucket        │
   │  dagger-engine-v0-21-4      │push/│  ref: cache.reg/...:V0-21-4    │
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
   Prometheus (metrics). The pipeline UI reads those backends directly.

---

## Configuration

### Files

| File                      | Purpose                                            |
|---------------------------|----------------------------------------------------|
| `config.app.yaml`         | Live config checked into the repo (example values). |
| `config.app.yaml.sample`  | Fully-commented reference. Copy → edit → deploy.   |

The Supervisor's `--config` flag points at the file to load (default
`config.app.yaml`; in the container this is typically mounted as
`/etc/dagger-cache/config.app.yaml`, see
`deploy/k8s/supervisor.yaml`). The `config.app.yaml` shipped here is a
**minimal** example: it only lists deployment-specific values; every other
option falls back to the compiled-in defaults in
`internal/config/config.go`.

To start from scratch:

```bash
cp config.app.yaml.sample config.app.yaml
$EDITOR config.app.yaml
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

See [`config.app.yaml.sample`](../config.app.yaml.sample) for every key with
inline comments. The sections below summarise the most important ones.

| Section      | Key (representative)        | Default                          | Notes                                            |
|--------------|-----------------------------|----------------------------------|--------------------------------------------------|
| `server`     | `control_addr`              | `:8080`                          | Hertz HTTPS control API.                          |
|              | `data_addr`                 | `:8443`                          | mTLS L4 data proxy.                               |
|              | `data_hostname`             | `data.supv.example.com`          | Public data-plane hostname.                       |
|              | `public_url`                | `https://supv.example.com`       | Public control/UI URL.                            |
| `auth.internal` | `enabled`                | `true`                           | Static bearer-token auth.                         |
|              | `tokens_file`               | `/etc/dagger-cache/tokens`       | One token per line.                               |
| `auth.oauth` | `enabled`                   | `false`                          | OAuth (GitHub) for UI login.                      |
|              | `provider`                  | `github`                         |                                                   |
|              | `allowed_orgs`              | —                                | Restrict login to members of these orgs.          |
| `telemetry`  | `collector_url`             | `http://otel-collector:4318`     | OTLP/HTTP.                                         |
|              | `tempo_url` / `loki_url` / `victoria_url` | `http://tempo:3200` etc. | Backend query APIs.                               |
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
go build -o dagger-cache-ci ./cmd/dagger-cache-ci    # CLI helper
go build -o supervisor ./cmd/supervisor                # server
./supervisor --config=config.app.yaml
```

Or via the Docker image:

```bash
docker build -t dagger-cache/supervisor:latest -f deploy/docker/Dockerfile .
docker run -p 8080:8080 -p 8443:8443 \
  -v "$PWD/config.app.yaml:/etc/dagger-cache/config.app.yaml:ro" \
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

The fleet provider is currently a stub (in-memory) for testing and
development. Production Kubernetes integration is a future milestone; today
the stub provider manages simulated engine StatefulSets per version. The
`fleet` configuration section controls the autoscaler behavior for this
provider.

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

Two independent mechanisms:

- **Internal (static bearer tokens)** — `auth.internal.tokens_file` holds
  one token per line. The CLI presents it as `DAGGER_CLOUD_TOKEN`, which the
  data plane validates before minting a client cert. This is the default and
  the recommended path for CI.
- **OAuth (GitHub)** — `auth.oauth` is for human/UI login only. Set
  `enabled: true`, supply `client_id`/`client_secret` via env, and list the
   orgs allowed to log in via `allowed_orgs`. The redirect URL must match
   `<public_url>/auth/callback`.

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

## Telemetry & UI

The telemetry stack is optional but recommended — it powers the pipeline UI.
Defaults point at the compose service names, which match
`deploy/docker/docker-compose.yaml` and `deploy/k8s/telemetry.yaml`:

- **OTel Collector** (`collector_url`) — receives OTLP from the Dagger CLI.
- **Tempo** (`tempo_url`) — traces.
- **Loki** (`loki_url`) — logs.
- **VictoriaMetrics** (`victoria_url`) — metrics (PromQL-compatible).
- **Grafana** (compose only) — dashboards on port 3000.

The UI is an embedded Vite SPA (packaged in `ui-dist/` via `//go:embed`). It
is always served by the control plane at `/` and trace links like
`/traces/<id>`. No separate configuration is needed.

To export the Supervisor's *own* OTLP (e.g. to the same collector), set
`otel.otlp_endpoint`. Leave it empty to disable.

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

`cmd/dagger-cache.sh` wires up the standard env vars and prints the
pipeline-view link after the run:

```bash
export DAGGER_CACHE_SERVER=https://supv.example.com
export DAGGER_CACHE_UI=https://ui.supv.example.com
export DAGGER_CLOUD_TOKEN=<token>
export DAGGER_TAG=v0.21.4          # optional

./cmd/dagger-cache.sh call github.com/your-org/ci@v1.0.0 build
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
  wired to the K8s probes in `deploy/k8s/supervisor.yaml`.
- **Backups:** the cache registry's PV (`registry-data` volume in compose)
  is the durable asset; back it up if you care about cold-start cache hits.

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
[`internal/api`](../internal/api) (control handlers and L4 data-plane proxy) and the
[`test/integration_test.go`](../test/integration_test.go) contract tests
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
```

Integration tests (`test/integration_test.go`) exercise the full
provision → lease → data-plane flow against stubbed fleet/cache/CA
providers, so they run without a cluster.

## License

See [LICENSE](../LICENSE).
