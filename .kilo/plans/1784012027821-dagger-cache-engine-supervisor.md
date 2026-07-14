# Dagger Self-Hosted Cloud — Remote Shared Cache, Auto-scaling Engines, Live Pipeline UI & CI Integration

A self-hosted, Dagger-Cloud-compatible platform: the stock `dagger` CLI targets your Kubernetes via a Supervisor that dynamically provisions per-version engine fleets (StatefulSet + autoscale + Service), shares a remote registry cache (self-hosted "Magicache", URL+token), **collects OpenTelemetry to render live pipeline DAGs** (Drone-like), and **surfaces Dagger steps natively in GitHub Actions / Jenkins / Drone**.

> All §2 facts verified against the local Dagger source at `/tmp/dagger` (branch `main`), file paths cited.

---

## 1. Goals & non-goals

**Goals**
- Stock `dagger` CLI works unmodified: user sets a URL (Supervisor) + a token; engines, cache, telemetry, and UI are transparent.
- Per version: a **StatefulSet** of engine pods, **autoscaled**, fronted by a **Service**; scale-to-zero / delete when idle.
- **Shared remote cache** (self-hosted Magicache): OCI registry of BuildKit cache blobs, URL+token. Not a PVC.
- **Telemetry & live UI**: collect OTLP traces/logs/metrics the CLI already emits to `DAGGER_CLOUD_URL`; render live pipeline DAGs (Drone-like) with zoomable step logs, state, duration, and live-follow.
- **Vue.js web UI** with **auth (internal token + OAuth)**; config in `config.app.yaml` via **Viper**. Views: pipelines, magiccache, runner fleet.
- **CI integration**: surface Dagger steps natively in GitHub Actions / Jenkins / Drone where possible; always emit a clickable UI link.
- K8s primary; local Docker dev mode.
- Go backend (CloudWeGo **Hertz** + **Kitex**); Vue 3 frontend.

**Non-goals**
- Reimplementing the OSS engine or BuildKit.
- Engine versions < `v0.19.0` via the cloud-compatible path (stock CLI rejects them, §2.6).
- Multi-tenant billing.

---

## 2. Verified constraints (from `/tmp/dagger`)

### 2.1 `dagger-cloud://` runner-host contract
- Registered in `engine/client/drivers/cloud.go`. `Provision(ctx, _ *url.URL, opts)` **ignores the URL host**; the relay address comes from env **`DAGGER_CLOUD_URL`** (`internal/cloud/client.go:46`). Requires `opts.CloudAuth` from **`DAGGER_CLOUD_TOKEN`** (`internal/cloud/auth/auth.go:401-413`). Calls `POST {DAGGER_CLOUD_URL}/v1/engines`, expects **HTTP 201** + `EngineSpec`.

### 2.2 `EngineSpec` (exact — `internal/cloud/client.go:138-189`)
Request: `{image, module, function, exec_cmd, client_id, minimum_engine_version, trace_id}`. Response **201**:
```jsonc
{ "image":"registry.dagger.io/engine:vX.Y.Z",
  "url":"<data-host>:<data-port>",                 // REQUIRED (mTLS data endpoint)
  "cert":{ "certificate_chain":[<DER>], "private_key":<PKCS8 DER> },  // REQUIRED
  "instance_id":"<id>", "location":"...", "org_id":"...", "user_id":"..." }
```
Non-201 → `{"message":"..."}` (`ErrResponse`).

### 2.3 Control-plane auth
CLI sends **`Authorization: Basic base64(token + ":")`** for a `DAGGER_CLOUD_TOKEN` (`client.go:53-55`, `auth.go:331`); `X-Dagger-Org` header when an org is set.

### 2.4 Data plane: mTLS, system-trusted server cert (HARD)
`DaggerCloudConnector.Connect` (`engine/client/drivers/cloud.go:29-70`) dials `tcp://<EngineSpec.url>`, `tls.Client` with the minted client cert (mTLS), `ServerName=<host>`, TLS1.2, **no `RootCAs`, no `InsecureSkipVerify`** → the data-plane server cert **must be trusted by the client OS root store** (Let's Encrypt, or a private CA installed on clients). Over TLS the CLI runs h2c (BuildKit gRPC + `/query` + `/sessionAttachables` + SSE).

### 2.5 Engine can listen on plain TCP h2c
`cmd/engine/main.go`: `--addr tcp://0.0.0.0:<port>` (`main.go:179-211`, `getListener:848-858`). We run pods plain `tcp://` (private, cert-free); the Supervisor terminates mTLS and L4-proxies.

### 2.6 Version selection + floor
Client proposes `image = "registry.dagger.io/engine:" + engine.Tag` (`client.go:197-210`); `engine.Tag` overridable via **`_EXPERIMENTAL_DAGGER_TAG`** (`engine/version.go:66-71`). Runtime gate: engine ≥ **`v0.19.0`** (`engine/version.go:27,125-127`), reported via BuildKit `Info` — not bypassable.

### 2.7 Remote cache = BuildKit cache import/export
- Client reads `CacheOptionsEntry` from env and passes to engine via `ClientMetadata.UpstreamCacheImportConfig/UpstreamCacheExportConfig` (`engine/opts.go:83-86`; `engine/client/client.go:1437-1438`).
- Env vars (`engine/client/client.go:75-86,1334-1399`): `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` (import+export), `_EXPERIMENTAL_DAGGER_CACHE_IMPORT_CONFIG`, `_EXPERIMENTAL_DAGGER_CACHE_EXPORT_CONFIG`.
- Format: `type=<backend>,k=v,k=v;…`. Backends (`internal/buildkit/client/solve.go:458-504`): **`registry`** (`ref=<host>/<repo>`, `mode=max`), `s3`, `local`, `gha`, `azblob`, `inline`.
- Registry auth lives in the **engine** (buildkitd registries config / docker config), not the client. URL (ref) in client env, token in engine pod.

### 2.8 Telemetry: OTLP to `DAGGER_CLOUD_URL` (THE key enabler for the UI)
- `engine/telemetry/cloud.go` `ConfiguredCloudExporters` builds OTLP/HTTP exporters when `DAGGER_CLOUD_TOKEN` (or OAuth) is present:
  - traces → `{DAGGER_CLOUD_URL}/v1/traces`
  - logs → `{DAGGER_CLOUD_URL}/v1/logs`
  - metrics → `{DAGGER_CLOUD_URL}/v1/metrics`
  - Authed with `Authorization: <Basic/Bearer>` + `X-Dagger-Org`.
- Body = OTLP/HTTP protobuf (`ExportTraceServiceRequest` etc.), optional gzip.
- **Implication:** because the client already sets `DAGGER_CLOUD_URL` = our Supervisor, the CLI **automatically POSTs telemetry to us** — no extra env, no CLI changes. We just implement `POST /v1/{traces,logs,metrics}` (token-authed) and forward to an OTel Collector.
- The CLI surfaces a trace URL via `CloudURLCallback` (`engine/client/client.go:102`; set to `Frontend.SetCloudURL` in `internal/cmd/dagger/engine.go:165`); `URLForTrace` (`engine/telemetry/url.go`) hardcodes `https://dagger.cloud/<org>/traces/<traceID>` — **not overrideable**, so our wrapper rewrites/extracts the `traceID` and prints our UI URL (§14).
- Spans are **CI-aware**: `engine/telemetry/labels.go` sets `dagger.io/ci` + provider labels by detecting `GITHUB_ACTIONS`, `JENKINS_HOME`, etc. So spans carry the CI context we can use for the UI + CI integration.

### 2.9 Existing CI integration surface
- Dagger ships a GitHub Action `dagger/dagger-for-github@<v>` (`modules/gha/steps.go:51`). We integrate with / extend it (§24).

---

## 3. Architecture

```
   Client (stock `dagger` CLI)                       Supervisor (Go: Hertz control + TLS data + OTLP ingest + UI API)
   ─────────────────────────                         ───────────────────────────────────────────────────────────────
   DAGGER_CLOUD_URL    = https://supv.example.com    ┌──────────────────────────────────────────────────┐
   DAGGER_CLOUD_TOKEN  = <token>                     │ Control plane (Hertz HTTPS)                       │
   _EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self│  POST /v1/engines  (provision engine)           │
   _EXPERIMENTAL_DAGGER_CACHE_CONFIG=type=registry,  │  POST /v1/{traces,logs,metrics}  (OTLP ingest)   │
        ref=cache.reg/v0.21.4,mode=max               │  GET  /api/v1/...  (UI data API)                 │
   _EXPERIMENTAL_DAGGER_TAG=v0.21.4 (optional)       │  Static + auth for Vue SPA                       │
                                                     └──────┬───────────────┬───────────────┬──────────┘
   1) POST /v1/engines ──────────────────────────────►       │               │               │
   ◄── 201 {url:data.supv:443, cert, instance_id}            │               │               │
                                                            │               │               │
   2) tls+mtls → data.supv:443 ┐                            │               │               │
      h2c session (BuildKit     │ L4 proxy to pinned        │               │               │
      gRPC + /query + ...)     ┘ replica pod :9999          │               │               │
                                                            ▼               ▼               ▼
   3) CLI OTLP POST /v1/{traces,logs,metrics} ─────────► OTel Collector ──► Tempo (traces)   ┐
                                                            └──► Loki (logs) ─►              │ Vue UI (web)
                                                            └──► Prometheus (metrics)        │ reads all
                                                                                              │
   4) Vue SPA (browser, internal/OAuth auth) ──► Hertz /api/v1 + WebSocket (live follow) ◄──┘

   Engine pods (StatefulSet per version, autoscaled) push/pull cache blobs ↘
                                                                            ┌──────────────────────┐
                                                                            │ Remote cache (OCI   │
                                                                            │ registry, token)    │
                                                                            └──────────────────────┘
   5) CI integration: wrapper prints UI trace URL; GHA job-summary/check-runs; Jenkins dynamic stage(); Drone config-extension steps (§24).
```

**Why this shape:** control plane is one Hertz handler returning a cert + data host; data plane is a dumb mTLS byte-pipe to a pinned replica (never parses the Dagger protocol); the **shared cache is the remote registry**; **telemetry is ingested for free** because the CLI already exports OTLP to `DAGGER_CLOUD_URL`; the UI reads Tempo/Loki/Prometheus to render the live DAG.

---

## 4. Glossary
| Term | Meaning |
|---|---|
| **Supervisor** | Our Go service: control (Hertz) + data (TLS+mTLS L4) + OTLP ingest + UI API + fleet lifecycle. |
| **Engine fleet** | Per version `V`: `StatefulSet dagger-engine-<vslug>` (replicas 0..N) + headless `Service`. |
| **Replica** | One engine pod, plain-TCP h2c on `:9999`, disposable local store. |
| **Session pin** | A `dagger` invocation = one session; its minted client cert maps to **one** replica. |
| **Remote cache** | OCI registry storing BuildKit cache blobs; per-version refs; token-authed. |
| **Pipeline view** | UI rendering of the OTel span tree (trace) as a Drone-like live DAG. |
| **Lease** | `certFP → {version, replica pod, lastActivity, inFlight, traceID}` with TTL. |

---

## 5. Control plane (implement exactly)

**`POST /v1/engines`** (Hertz `internal/api/control.go`)
1. Auth (Basic, username=token). Parse `image` tag → version `V` (via `_EXPERIMENTAL_DAGGER_TAG` or CLI default); per-token/org override/allowlist; reject `V < v0.19.0`.
2. Acquire replica for `V` (§9): least-pinned ready replica with capacity; scale up if under cap (wait for readiness); `429` if at cap.
3. Mint client cert (Supervisor CA, TTL `CLIENT_CERT_TTL`). Register lease `certFP → {V, replicaPod, lastActivity, traceID(req.trace_id)}`.
4. Respond **201** `EngineSpec{url:"data.supv.example.com:443", cert, instance_id, image, location:"k8s"}`.

**OTLP ingest** (Hertz `internal/api/telemetry.go`, same token auth):
- `POST /v1/traces` | `/v1/logs` | `/v1/metrics` — accept OTLP/HTTP protobuf (handle `Content-Encoding: gzip`), validate token, then **reverse-proxy the raw body** to the in-cluster OTel Collector (`http://otel-collector:4318/v1/traces|logs|metrics`). No protobuf parsing needed in the Supervisor.
- Tag the request with the token's tenant/org header when forwarding (collector → Tempo/Loki via tenant labels).

**Admin Hertz routes**: `GET /v1/engines`, `POST /v1/engines/:version/{scale,stop}`, `GET /v1/versions`, `POST /v1/cache/purge`, `GET /healthz|/readyz|/metrics`.

---

## 6. Data plane (TLS+mTLS L4 proxy) — `internal/dataplane`
- `tls.Listener`: server cert (system-trusted), `ClientAuth: RequireAndVerifyClientCert`, `ClientCAs: mintingCAPool`, TLS1.2.
- Accept → `PeerCertificates[0]` fingerprint → lease → **pinned replica pod** (resolve name→IP via informer; restart → fast fail → client retries & re-provisions).
- `io.Copy` both ways to `podIP:9999` (plain h2c). No protocol parsing. All session connections share the cert → same pod (state preserved).
- `inFlight`/`lastActivity` per lease.

**`internal/ca`**: minting CA (K8s Secret) → `SerializableCertificate`. Server cert = Let's Encrypt (cert-manager) or private CA on clients (air-gapped) — no custom-CA escape hatch (§2.4).

---

## 7. Remote shared cache — self-hosted Magicache
**Backend: OCI registry (default).** Distribution/`registry:2`, token-authed (htpasswd). Per-version refs `cache.reg/dagger-cache:V<vslug>` (content-addressed blobs shared; manifests version-scoped). Engine pushes (export) + pulls (import) per solve, authed by engine-side registry creds.

**Client env (URL):** `_EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=cache.reg/dagger-cache:V0-21-4,mode=max"` (set by wrapper, §14).

**Engine-side token:** K8s `Secret` (docker config) + `engine.json` `registries` block:
```jsonc
{ "registries": { "cache.reg": { "auth": "<base64(token:)>" } } }
```

**Alternatives:** `type=s3` (bucket+keys, multi-writer-safe); `type=local` over RWX PVC (single-writer unsafe, dev only). A `/var/lib/dagger` PVC is now optional/per-replica/disposable (emptyDir or `volumeClaimTemplates` RWO) — local warm layer, not the shared cache.

**Properties:** true shared cache across engines/versions; no single-writer constraint; survives scale-to-zero/deletion; `POST /v1/cache/purge?version=V` deletes registry tag(s).

---

## 8. (reserved — see §9 fleet)

## 9. Engine fleet lifecycle & autoscaling (per version)
### 9.1 Topology
One `StatefulSet dagger-engine-<vslug>` per active version `V` + headless `Service`. Replica pod: `image registry.dagger.io/engine:v<V>`, `args ["--addr","tcp://0.0.0.0:9999","--root","/var/lib/dagger"]`, `privileged: true`, `terminationGracePeriodSeconds:120`, `engine.json` (+ registry-auth Secret), optional `volumeClaimTemplates` RWO. `persistentVolumeClaimRetentionPolicy: { whenScaled: Retain, whenDeleted: Delete }`.

### 9.2 Acquire replica (per `POST /v1/engines`)
1. Resolve `V`; ensure STS exists. 2. Least-pinned ready replica with capacity; else scale up (cap `MAX_REPLICAS_PER_VERSION`), wait `Ready`; `429` if at cap. 3. Pin cert→replica, return `EngineSpec`. (StatefulSet removes highest ordinal first → never evicts a busy lower ordinal.)

### 9.3 Idle scale-down & deletion
Tick `SWEEP_INTERVAL`(30s). Replica with `pinnedSessions==0 && idle>REPLICA_IDLE_TTL`(5m) → eligible; while highest ordinal eligible, `replicas--`. Whole STS idle >`IDLE_TTL`(5m) → `replicas=0`. Idle `replicas==0` >`VERSION_RETENTION`(24h) → delete STS (+local PVCs); remote cache retained. `MIN_REPLICAS_PER_VERSION`(0) warm floor. Backpressure shrinks TTLs.

### 9.4 Autoscale-up
On Acquire (no capacity); optional HPA on CPU/inflight as belt-and-suspenders.

### 9.5 Hooks & reconciler
`OnSTSCreate/ReplicaReady/Pin/Unpin/ScaleDown/DeleteSTS` → metrics. Reconciler (on Supervisor restart): discover STSes+pods, rebuild informer view, reap orphans, expire orphan leases (`LEASE_TTL`=2m).

---

## 10. Version handling
`internal/version`: normalize `1.21`/`v0.21`/`0.21.4`→`v0.21.4`; minor→latest patch via GitHub Releases (cached 1h)+`versions.yaml`; floor `v0.19.0`. STS slug `v0-21-4`; cache ref `v0.21.4`. `ENGINE_IMAGE_REPO` mirror override; `IfNotPresent`+pre-pull DaemonSet.

---

## 11. Client experience (zero CLI modification)
The `dagger-cache` wrapper (or sourced env) sets everything from `config.app.yaml` (§13):
```sh
export DAGGER_CLOUD_URL=https://supv.example.com
export DAGGER_CLOUD_TOKEN=<token>
export _EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self
export _EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=cache.reg/dagger-cache:V0-21-4,mode=max"
export _EXPERIMENTAL_DAGGER_TAG=v0.21.4   # optional
dagger call github.com/acme/ci@v1.4.0 build --arg platform=linux/amd64
```
The wrapper resolves `DAGGER_TAG`→cache ref tag, and post-run parses the CLI's printed trace URL to extract the `traceID`, printing `https://ui.supv.example.com/traces/<traceID>` (since `URLForTrace` hardcodes dagger.cloud, §2.8). **Legacy path** (`POST /v1/sessions` → `kube-pod://`/`tls://`) for < v0.19.0, not primary.

---

## 12. Telemetry ingestion & live pipeline rendering

### 12.1 Pipeline (collection)
- Supervisor OTLP ingest (§5) → **OTel Collector** (`otelcol-contrib`) → **Tempo** (traces), **Loki** (logs), **Prometheus** (metrics). Tenant label per token/org.
- The OTel span tree **is** the pipeline DAG: Dagger emits nested spans per function call / `withExec` / op (parent/child, name, status, start/duration, attributes). We reconstruct the tree by `traceID` + `parentSpanID`.
- Live-follow: the UI subscribes via WebSocket; the Supervisor fans out span updates by polling Tempo's `/api/traces/<traceID>` (or subscribing to collector live exports) and pushing deltas.

### 12.2 Pipeline view (Vue)
- Render the span tree as a **Drone-like live DAG**: nodes = dagger steps/functions; edges = parent→child; node shows **state** (running/success/failed/canceled), **duration** + elapsed, **progress**; clicking a node **zooms** to that step's **logs** (streamed from Loki by spanID/traceID), **attributes**, **sub-steps** (recurse).
- Top bar: overall status, total elapsed, the pinned engine/replica, version, cache hit/miss indicators (from spans/metrics), and the CI context (`dagger.io/ci` labels, §2.8).
- Live-follow toggle: tail logs as they arrive; auto-scroll.
- Timeline/gantt lane per top-level function; parallel branches shown side-by-side.

### 12.3 Backend API (Hertz)
| Method | Path | Purpose |
|---|---|---|
| GET | `/api/v1/traces` | list recent traces (filters: tenant, version, CI, status, time). |
| GET | `/api/v1/traces/:id` | full span tree (reconstructed from Tempo). |
| GET | `/api/v1/traces/:id/logs` | logs for a trace/step (Loki query). |
| WS  | `/api/v1/traces/:id/live` | live span + log deltas. |
| GET | `/api/v1/cache` | magiccache stats (§15). |
| GET | `/api/v1/fleet` | runner fleet (§16). |

---

## 13. Configuration — Viper + `config.app.yaml`
Single source of truth loaded by **Viper** (`github.com/spf13/viper`): `config.app.yaml` (path overridable via `--config` / `DAGGER_CACHE_CONFIG_FILE`), with env overrides (`DAGGER_CACHE_*`) and `--flags`.

```yaml
# config.app.yaml
server:
  control_addr: ":8080"
  data_addr: ":8443"
  data_hostname: "data.supv.example.com"
  public_url: "https://supv.example.com"        # control plane
  ui_url: "https://ui.supv.example.com"        # printed trace/UI links
auth:
  internal:
    enabled: true
    tokens_file: "/etc/dagger-cache/tokens"     # hashed tokens
  oauth:
    enabled: false
    provider: "github"                          # github | google | oidc
    client_id: "${OAUTH_CLIENT_ID}"
    client_secret: "${OAUTH_CLIENT_SECRET}"
    redirect_url: "https://ui.supv.example.com/auth/callback"
    allowed_orgs: ["acme"]
telemetry:
  collector_url: "http://otel-collector:4318"
  tempo_url: "http://tempo:3200"
  loki_url: "http://loki:3100"
  prometheus_url: "http://prometheus:9090"
cache:
  backend: "registry"
  registry: "cache.reg/dagger-cache"
  s3: { bucket: "", region: "" }
  ref_per_version: true
fleet:
  namespace: "dagger-cache"
  max_replicas_per_version: 3
  max_sessions_per_replica: 8
  replica_idle_ttl: "5m"
  version_retention: "24h"
  min_replicas_per_version: 0
ca:
  minting_ca_secret: "supervisor-minting-ca"
  client_cert_ttl: "2h"
tls:
  server_cert_secret: "supervisor-tls"          # Let's Encrypt via cert-manager
version:
  floor: "v0.19.0"
  allowlist: ["0.19","0.20","0.21"]
ci:
  github:
    job_summary: true
    check_runs: true
  jenkins:
    dynamic_stages: true
  drone:
    config_extension: true
ui:
  enabled: true
  spa_dir: "/opt/dagger-cache/ui"
log_level: "info"
otel:
  otlp_endpoint: ""                             # Supervisor's own traces (optional)
```

---

## 14. Web UI — Vue 3 + auth

### 14.1 Stack
- **Vue 3 + Vite + Pinia + Vue Router + TypeScript**. Live DAG: `vue-flow` (or `dagre`+custom) for the pipeline graph; log viewer with virtual scroll + ANSI support; WebSocket for live-follow. Charts: ECharts (fleet/cache metrics).
- Served as a static SPA by Hertz (`ui.spa_dir`); dev mode proxies to the Vite dev server.

### 14.2 Auth (internal + OAuth)
- **Internal:** username/token login → JWT (short-lived) + refresh token; tokens in `auth.internal.tokens_file` (bcrypt-hashed). Roles: `admin` (fleet/cache/purge), `viewer` (read-only).
- **OAuth:** GitHub/Google/OIDC (config `auth.oauth.*`); on callback exchange code → user info → JWT; allowlist orgs (`allowed_orgs`). Session cookie (httpOnly, secure) + CSRF token for non-WS; WebSocket auth via short-lived ticket query param.
- Middleware: `auth.Required()` on `/api/*`; `/auth/{login,callback,refresh,logout}`.

### 14.3 Views
1. **Pipelines** (default): list of recent traces; click → live DAG (§12.2) with zoom/log/state/duration/follow.
2. **Magiccache**: registry cache dashboard — per-version refs, size, blob count, hit/miss rate (from spans/metrics), last-used, **purge** button, registry storage usage/gc.
3. **Runners (fleet)**: per-version StatefulSets, replicas, ordinal state (ready/running/draining), pinned sessions, CPU/mem, version, cache-ref; manual **scale/stop**; pod logs link to pipeline view.
4. **Settings**: tokens (admin), OAuth status, version allowlist, config view.

### 14.4 Repo layout (additions)
```
ui/                       # Vue 3 SPA (Vite, TS, Pinia)
  src/{views,pipeline,magiccache,fleet,auth,components,stores,api,router}
internal/api/{ui.go,auth_oauth.go,auth_internal.go,telemetry.go,control.go,admin.go}
internal/config/         # Viper loader → typed Config struct (struct tags)
internal/telemetry/       # OTLP proxy, span-tree reconstructor, live fan-out
deploy/k8s/{collector.yaml,tempo.yaml,loki.yaml,prometheus.yaml,ui-ingress.yaml}
```

---

## 15. Magiccache UI (detail)
Backend derives data from: registry catalog API (`/v2/_catalog` + manifest/list tags → sizes), Prometheus metrics (cache hit/miss, layer pull/push durations emitted by BuildKit/engine), and fleet state. `GET /api/v1/cache` → `{versions:[{v, refs:[{name,size,blobs,lastUsed}], totalSize, hitRate, registryUsage}]}`. `POST /v1/cache/purge {version}` → delete tags (registry delete enabled).

---

## 16. Runner fleet UI (detail)
`GET /api/v1/fleet` → `{versions:[{v, stsName, replicas, readyReplicas, ordinals:[{name,ordinal,state,pinnedSessions,cpu,mem,version,startedAt}], cacheRef}]}`. Derived from client-go informers + leases. Actions `POST /api/v1/fleet/:version/scale {replicas}` and `/stop`. Per-ordinal "view logs" deep-links to the pipeline view filtered by that pod.

---

## 17. CI integration — native Dagger-step visibility

**Universal fallback (all CIs):** the wrapper prints the **self-hosted UI trace URL** (`https://ui.supv.example.com/traces/<traceID>`) so CI logs have a clickable live-DAG link. Spans already carry CI labels (`dagger.io/ci`, provider) for grouping in the UI (§2.8).

### 17.1 GitHub Actions
- Integrate with **`dagger/dagger-for-github`** (existing action) as the runner; our wrapper is the `dagger` binary it calls.
- **Job Summary** (`GITHUB_STEP_SUMMARY`): after/while running, write Markdown — a pipeline table/DAG (step, status, duration, cache hit) with per-step deep-links to the UI. Live update by appending as spans arrive (the wrapper polls our `/api/v1/traces/:id`).
- **Log groups** (`::group::<step>::` … `::endgroup::`): fold each Dagger function/sub-step in the job log with live streamed output → native collapsible steps.
- **Check Runs** (PRs): for each top-level Dagger function, create/update a GitHub **check run** (`POST /repos/{owner}/{repo}/check-runs`) with status/conclusion + annotations + a UI link → per-function status in the GitHub Checks UI (closest to dynamic sub-steps GHA offers).
- Limits: GHA steps are static YAML; true runtime sub-steps aren't supported — check runs + summary + log groups are the native ceiling.

### 17.2 Jenkins
- **Scripted-pipeline dynamic `stage()`**: a Jenkins **shared library** (`daggerCache.pipeline {}`) that, for each Dagger function discovered from the trace, wraps execution in `stage("dagger: <fn>") { … }` → renders natively in **Stage View / Blue Ocean** with status + duration + per-stage logs.
- The library calls our UI API to map stages↔spanIDs and to stream logs. Alternatively a small **Jenkins plugin** for a richer custom view, but the shared-library dynamic-stage approach needs no plugin install.
- Limits: Declarative Pipeline stages are static; scripted pipeline is required for dynamic stages.

### 17.3 Drone
- **Config-extension plugin** (`drone-config-extension`): mutate the `.drone.yml` at config time to inject **one Drone step per Dagger function** (parallel/serial) → native per-function steps in the Drone UI with logs.
- Fallback: the `dagger-cache` step prints the UI trace URL and uses Drone's step APIs to annotate. Drone 2.x runtime steps are otherwise static, so the config-extension (pre-build YAML mutation) is the native path.
- Limits: truly runtime dynamic steps aren't supported; config-time injection is the ceiling.

### 17.4 Cross-CI helper
`ci-integrations/` repo folders: `gha/` (composite action + summary writer), `jenkins/shared-library`, `drone/config-extension`, plus a common Go binary `dagger-cache-ci` that: runs `dagger`, extracts `traceID`, and emits the CI-native artifacts (summary/groups/check-runs/stages) by polling our UI API. Each CI ships the same UX: live pipeline in our UI + best-effort native steps.

---

## 18. CloudWeGo usage
- **Hertz**: control plane (`POST /v1/engines`), **OTLP ingest** (`/v1/{traces,logs,metrics}` reverse-proxy), **UI API** (`/api/v1/*` + WebSocket), admin routes, static SPA + auth. Middlewares: request-ID, Basic-auth/JWT, quota, recover, OTel, access log, gzip.
- **Data plane**: plain Go `net`/`crypto/tls` L4 proxy (raw TLS, not HTTP).
- **Kitex** (Protobuf `internal/rpc/supervisor.proto`): internal control↔worker RPC when scaled out (stateless control Deployment → leader-elected worker StatefulSet). Single-replica = one process.

---

## 19. Repository layout
```
github.com/<org>/dagger-cache
  cmd/supervisor/main.go
  internal/
    api/         # Hertz: control.go, telemetry.go, ui.go, auth_internal.go, auth_oauth.go, admin.go, middleware
    dataplane/   # TLS+mTLS L4 proxy, certFP→pinned pod, io.Copy bridge
    ca/          # minting CA + SerializableCertificate
    fleet/       # StatefulSet/Service CRUD, informer view, autoscaler, reconciler, sweeper
    cache/       # remote-registry ref builder, purge, engine registry-auth Secret, engine.json
    session/     # lease store (certFP→replica), pin/unpin, orphan reaper
    telemetry/   # OTLP reverse-proxy, span-tree reconstructor, live fan-out (WS)
    version/ auth/ config/ observ/
    rpc/         # supervisor.proto + Kitex stubs (scaled-out)
  ui/            # Vue 3 SPA (Vite, TS, Pinia, vue-flow, ECharts)
  ci-integrations/{gha,jenkins,drone}/ dagger-cache-ci/
  deploy/k8s/  deploy/docker/  test/
```
Deps: `hertz`, `kitex`, `viper`, `k8s.io/client-go`, `prometheus/client_golang`, `go.uber.org/zap`, `otel`; UI: `vue@3`, `vite`, `pinia`, `vue-router`, `vue-flow`, `echarts`, `axios`, `@websocket`/native. Do **not** import `dagger.io/dagger` at runtime (we mirror its cloud contract).

---

## 20. Deployment — Kubernetes (primary)
- Namespace `dagger-cache`. RBAC (Supervisor SA): `pods`/`statefulsets`/`services` (CRUD), `pods/exec`, `pvc`, `configmaps`/`secrets`.
- **Telemetry stack**: OTel Collector Deployment + Service (`:4318` OTLP/HTTP, `:4317` gRPC); **Tempo** (traces, S3/FS backend), **Loki** (logs, S3/FS), **Prometheus** (metrics). All in-cluster.
- **Remote cache**: `cache-registry` (Distribution) Deployment + `cache.reg` Service + htpasswd Secret + PVC (single registry = single-writer safe).
- **Secrets/ConfigMaps**: `supervisor-minting-ca`, `supervisor-tls` (cert-manager/Let's Encrypt), `cache-registry-auth`, `engine-registry-auth`, `engine-config` (`engine.json`, `versions.yaml`), `config-app` (config.app.yaml).
- **Supervisor Deployment** (2+ behind Ingress `supv.example.com`) + `Service data` (LoadBalancer/Ingress TLS-passthrough at `data.supv.example.com:443`, system-trusted cert). Scaled-out: control Deployment + worker StatefulSet over Kitex.
- **UI**: Supervisor serves the SPA (or separate `ui` Deployment + Ingress `ui.supv.example.com` with same auth).
- **Engine STS template** (on-demand): privileged, `--addr tcp://0.0.0.0:9999`, `engine.json`, registry-auth Secret, optional `volumeClaimTemplates`, `terminationGracePeriodSeconds:120`, dedicated-node tolerations.
- **NetworkPolicy**: engine egress → `cache-registry`+registry mirror+allowed hosts; `:9999` ingress only from Supervisor. UI/Supervisor egress → collector/tempo/loki/prom.
- CI clients need only `DAGGER_CLOUD_URL/TOKEN` + runner-host + cache-config (+ `DAGGER_TAG`); no `kubectl`.

---

## 21. Deployment — Docker (dev mode)
`deploy/docker/docker-compose.yaml`: Supervisor (control `:8080` + data `:8443` self-signed local-trusted CA + UI), local Distribution registry, **OTel Collector + Tempo + Loki + Prometheus** as containers, and `CACHE_PROVIDER=docker` provisioning engine *containers* (one-per-version, scaled by Supervisor via Docker API). Full flow without a cluster; UI live-DAG tested against real traces.

---

## 22. Security
- **Authn**: control plane = `DAGGER_CLOUD_TOKEN` (Basic); data plane = mTLS (minted short-lived cert); telemetry ingest = same token; UI = JWT (internal) or OAuth; cache = registry token. Five concerns, separate secrets, rotation-supported.
- **System-trusted server cert (§2.4, hard):** Let's Encrypt (public) or private CA on clients (air-gapped).
- **Privileged engine pods:** dedicated/tolerated node pool; no hostNetwork; limits; egress NetworkPolicy; secrets by reference.
- **Cache isolation:** per-version refs; content-addressed blobs (no plaintext secrets).
- **Telemetry PII:** scrub span attributes before UI display; tenant isolation in Tempo/Loki (tenant labels); no raw tokens in spans.
- **Supply chain:** pin image digests; reproducible build (`CGO_ENABLED=0`); verified UI build (lockfile).
- **Quotas:** per-token `maxConcurrent`/`maxRuntimePerDay`; per-viewer RBAC.
- **Audit:** every Acquire/Pin/Scale/Purge/PipelineView as structured logs + OTel spans.

---

## 23. Error handling & cleanup
| Failure | Handling |
|---|---|
| Bad token | `401`. |
| Version < v0.19.0 | `400`. |
| Image pull / no capacity | retry/backoff → `503`/`429` `Retry-After`. |
| Pinned replica restarts mid-session | data conn drops → client retries → re-provision → re-pin. |
| OTLP ingest upstream (collector) down | buffer/retry in Supervisor; drop oldest on overflow; UI shows stale. |
| Trace missing in Tempo | UI falls back to live WS; reconstruct as spans arrive. |
| Supervisor crash | `LEASE_TTL` expires orphan leases; reconciler rebuilds view. |
| Registry PVC full | registry GC + LRU `PurgeCache`; `507`. |
| STS stuck scaling | reconcile via `status.*`; force-delete stuck pods. |
Every Acquire has a release path (lease TTL + reconciler = safety net).

---

## 24. Configuration reference (env override of `config.app.yaml`)
| Var | Default | Purpose |
|---|---|---|
| `DAGGER_CACHE_CONFIG_FILE` | `config.app.yaml` | Viper config path. |
| `DAGGER_CACHE_PROVIDER` | `kubernetes` | `kubernetes` \| `docker`. |
| `DAGGER_CACHE_CONTROL_ADDR` | `:8080` | Hertz control. |
| `DAGGER_CACHE_DATA_ADDR` | `:8443` | TLS data plane. |
| `DAGGER_CACHE_DATA_HOSTNAME` | `data.supv.example.com` | in `EngineSpec.url`. |
| `DAGGER_CACHE_UI_URL` | `https://ui.supv.example.com` | printed trace links. |
| `DAGGER_CACHE_COLLECTOR_URL` | `http://otel-collector:4318` | OTLP forward target. |
| `DAGGER_CACHE_CACHE_BACKEND` | `registry` | `registry` \| `s3`. |
| `DAGGER_CACHE_CACHE_REGISTRY` | `cache.reg/dagger-cache` | registry host/repo. |
| `DAGGER_CACHE_MAX_REPLICAS_PER_VERSION` | `3` | autoscale cap. |
| `DAGGER_CACHE_MAX_SESSIONS_PER_REPLICA` | `8` | pin capacity. |
| `DAGGER_CACHE_REPLICA_IDLE_TTL` | `5m` | per-replica idle. |
| `DAGGER_CACHE_VERSION_RETENTION` | `24h` | idle STS before delete. |
| `DAGGER_CACHE_CLIENT_CERT_TTL` | `2h` | minted cert validity. |
| `DAGGER_CACHE_LEASE_TTL` | `2m` | orphan-lease reclaim. |
| `DAGGER_CACHE_FLOOR` | `v0.19.0` | engine version floor. |
| `DAGGER_CACHE_LOG_LEVEL` | `info` | zap. |

---

## 25. Validation & test plan
**Unit**
- `EngineSpec`/`SerializableCertificate` round-trip (stub CA).
- Control handler: Basic-auth, version floor→400, capacity→429, 201 shape, cache-ref builder.
- OTLP ingest: gzip decode + raw forward to a stub collector; token gating.
- Span-tree reconstructor from a Tempo-like fixture → DAG nodes/edges; live fan-out emits deltas.
- Fleet: least-pinned acquire; scale-down removes highest idle ordinal only; reconciler reaps orphans.
- UI auth: internal JWT issue/verify; OAuth callback→JWT; RBAC.

**Integration (kind)**
- Stock `dagger` CLI (cloud env + cache-config + `DAGGER_TAG=v0.21.x`) runs a module: replica ready, pipeline streamed, cache blobs pushed, **trace appears in UI with live DAG + step logs + durations**; second run = cache hit (fewer layer pulls, faster).
- Concurrent same-version sessions → multiple replicas; cache shared via registry.
- Idle sweep → scale-down/scale-to-zero/delete; remote cache + telemetry retained.
- Kill Supervisor mid-session → reconciler reaps orphans within `LEASE_TTL`.
- Request `v0.16.x` → `400`.
- **CI integration**: run the wrapper in a GHA-style job (act/local-runner) → assert job summary, log groups, and (with a fake GitHub API) check-runs appear; Jenkins scripted pipeline shows dynamic `stage()`s; Drone config-extension injects steps.

**Docker dev mode**
- Full flow incl. UI live-DAG against real traces; magiccache + fleet dashboards.

---

## 26. Risks & open questions
1. **System-trusted server cert (§2.4, hard):** Let's Encrypt public, or private CA on clients. Confirm acceptable.
2. **Floor v0.19.0 (§2.6):** older engines unreachable via `dagger-cloud://`; legacy explicit path for them. Confirm if v0.16 truly needed.
3. **Session pinning vs Service:** session connections must hit one replica; data plane pins cert→pod IP; Service is for readiness/lifecycle. Validate pin stability (restart→break→retry).
4. **Concurrent registry-cache writers:** BuildKit `type=registry` export races on the manifest tag (blobs still shared → hits work); per-version refs + `mode=max` mitigate; accept last-writer-wins or add a thin coordinator.
5. **Telemetry volume:** OTLP at scale; size the collector/Tempo/Loki; sampling policy (keep all dagger function spans, sample inner ops) to bound cost.
6. **Trace URL override:** `URLForTrace` hardcodes dagger.cloud; wrapper rewrites via traceID extraction — fragile to CLI output format changes; track across releases.
7. **CI-native step ceilings:** GHA (summary/check-runs/log groups), Jenkins (scripted dynamic stages), Drone (config-extension) are best-effort, not true live DAG; the UI is the canonical rich view.
8. **Privileged engine pods:** dedicated node pool; confirm cluster policy.
9. **Contract drift:** track `internal/cloud/client.go`, `engine/telemetry/cloud.go`, `engine/client/client.go` across releases.

---

## 27. Ordered implementation tasks
1. `go mod init`; scaffold `cmd/supervisor`; **Viper** config loader (`config.app.yaml`→typed struct); logging/metrics/tracing.
2. `internal/ca`: minting CA + `SerializableCertificate`. Unit round-trip.
3. `internal/api` (Hertz): `POST /v1/engines` (Basic-auth, version floor, 201 `EngineSpec`) with stub fleet. Admin routes. Middleware.
4. `internal/cache`: cache-ref builder, purge, engine registry-auth Secret, `engine.json`.
5. `internal/session`: lease store (certFP→replica), pin/unpin, orphan reaper.
6. `internal/fleet`: STS+Service CRUD, informer view, readiness probe, autoscaler, reconciler. Fake-provider unit tests.
7. `internal/dataplane`: TLS+mTLS listener, certFP→pinned-pod, L4 bridge. Unit with `net.Pipe`.
8. `internal/version`: resolver + releases cache + allowlist + floor.
9. Wire control→fleet-acquire→mint→data-pin; lease heartbeat; sweeper.
10. `internal/rpc` Kitex IDL + split control/worker (single-process default).
11. **Telemetry**: `POST /v1/{traces,logs,metrics}` OTLP reverse-proxy → collector. Deploy collector/Tempo/Loki/Prometheus. `internal/telemetry` span-tree reconstructor + WS fan-out. `/api/v1/traces*` + `/api/v1/traces/:id/logs` + `/live`.
12. `deploy/k8s`: ns, RBAC, `cache-registry`, cert-manager, Supervisor Deployment + `data` Service/Ingress, engine STS template, pre-pull DaemonSet, NetworkPolicy, telemetry stack.
13. `deploy/docker`: dev compose (Supervisor + local registry + telemetry stack + `CACHE_PROVIDER=docker`).
14. **UI (Vue 3)**: project scaffold (Vite/TS/Pinia/Router); auth (internal JWT + OAuth callback); Pipelines list + live DAG (vue-flow) + step log zoom + live-follow (WS); Magiccache dashboard; Runners dashboard; Settings.
15. `dagger-cache` wrapper CLI (sets runner-host + cache-config + DAGGER_TAG→ref; extracts traceID → prints UI URL).
16. **CI integrations**: `ci-integrations/gha` (composite action + job summary + log groups + check-runs), `jenkins/shared-library` (dynamic `stage()`), `drone/config-extension` (step injection), common `dagger-cache-ci` binary polling UI API.
17. Integration tests on kind (§25) incl. UI live-DAG + CI-native artifacts.
18. Prometheus metrics + Grafana dashboards; operator runbook (scale/purge/cert & cache-token rotation, telemetry sampling, contract-drift watch).
19. Docs: client setup, operator guide (K8s + Docker + telemetry stack + UI), CI integration per tool, contract-drift runbook.

---
*All §2 facts verified on `/tmp/dagger` (`main`): `internal/cloud/client.go`, `internal/cloud/auth/auth.go`, `engine/client/drivers/{cloud,driver,tls}.go`, `engine/client/client.go` (cache envs §2.7, CloudURLCallback §2.8), `engine/client/buildkit.go`, `internal/buildkit/client/solve.go` (`type=registry` §2.7), `engine/telemetry/{cloud,url}.go` (OTLP-to-DAGGER_CLOUD_URL §2.8), `engine/telemetry/labels.go` (CI detection §2.8), `engine/opts.go`, `engine/version.go`, `engine/consts.go`, `engine/distconsts/consts.go`, `cmd/engine/main.go`, `modules/gha/steps.go` (`dagger-for-github` §2.9).*
