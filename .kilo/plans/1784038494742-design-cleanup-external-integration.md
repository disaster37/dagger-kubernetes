# Plan — Battle the Design: External Tools Integration Cleanup

Scope: harden the Supervisor's external-integration surface across five areas
flagged in design review. Driven by contradictions found in the current code
(see "Current-state bugs" per section). No level-of-effort estimates.

## Resolved decisions

| # | Area | Decision |
|---|------|----------|
| 1 | Server endpoints | Drop `ui_url`. Keep `control_addr`, `data_addr`, `data_hostname`, `public_url`. `data_hostname` stays a separate key (TCP LB ≠ HTTP Ingress; bind `:8443` ≠ public `:443`). UI is served from control plane, so `ui_url == public_url`. |
| 2 | Telemetry | **Keep all 4 URLs** (`collector_url`, `tempo_url`, `loki_url`, `victoria_url`) but wire the two that are currently dead (`loki_url`, `prometheus_url`) into real UI panels. **Prometheus is replaced by VictoriaMetrics** (PromQL-compatible drop-in: same `/api/v1/query` + `/query_range`, accepts remote_write). Keep the bundled compose/k8s stack. |
| 3 | Cache traversal | Supervisor becomes a **Host-routed reverse proxy on the control plane `:8080`** fronting the registry backend. Validates `DAGGER_CLOUD_TOKEN` + lease, then proxies to internal `registry:2`/S3. Registry never directly exposed; one token for everything. |
| 4 | CA / TLS | `tls.provider`: `embedded` (goca, **default**) | `cert-manager` | `external`. Fixes the in-memory-CA-regenerates-every-restart bug. |
| 5 | UI | Core component. Replace Vite+Vue with **Nuxt SSG** (`nuxt generate`), bake assets into Go binary via `embed.FS`. Remove `ui` config block entirely. |

---

## 1. Server endpoints — drop `ui_url`

**Current-state bug:** none (just redundancy). `ui_url` is configured and passed
to `api.ServerConfig.UIURL` but the UI is served from the control plane at `/`,
so `ui_url` only exists for CI wrapper link generation.

Tasks:
- `internal/config/config.go`: remove `UIURL` from `ServerConfig`; remove the
  `server.ui_url` default.
- `internal/api/server.go`: remove `UIURL` from `api.ServerConfig`; remove the
  field from the `cmd/supervisor/main.go` constructor call.
- `cmd/dagger-cache.sh` + `ci-integrations/*`: default `DAGGER_CACHE_UI` to
  `DAGGER_CACHE_SERVER` (== `public_url`) when unset. Document the fallback.
- `config.app.yaml.sample`, `docs/README.md`: remove `server.ui_url`; note
  UI is at `public_url`.
- **Keep** `control_addr`, `data_addr`, `data_hostname`, `public_url` — these
  are structurally required (control = Hertz HTTP; data = raw mTLS TCP L4;
  `data_hostname` is the public LB target returned to the CLI as
  `data_hostname:443`, not derivable from the `:8443` bind).

## 2. Telemetry — wire the dead URLs

**Current-state bug:** `loki_url` and `prometheus_url` are in
`config.TelemetryConfig` but **never reach `api.ServerConfig`** (only
`TempoURL` is plumbed, and even `TempoURL` isn't used by a visible handler).
Two of the four "integrations" do nothing today. This section also **renames
`prometheus_url` → `victoria_url`** and swaps the backend to VictoriaMetrics.

Tasks:
- `internal/config/config.go`: keep `TelemetryConfig` with 4 URLs, **rename
  `PrometheusURL`/`prometheus_url` → `VictoriaURL`/`victoria_url`**. Update the
  default `http://prometheus:9090` → `http://victoria:8428` (VictoriaMetrics
  default listen port).
- `internal/api/server.go` `ServerConfig`: add `LokiURL`, `VictoriaURL`
  (alongside existing `TempoURL`, `CollectorURL`); wire from
  `cmd/supervisor/main.go`.
- `internal/telemetry/`: add `LogsClient` (queries Loki
  `/loki/api/v1/query_range` for a trace's logs) and `MetricsClient`
  (queries VictoriaMetrics `/api/v1/query` + `/api/v1/query_range` — PromQL,
  API-identical to Prometheus, just different host:port). Mirror the existing
  `SpanTreeReconstructor` (Tempo) shape.
- `internal/api/server.go`: add handlers
  `GET /api/v1/logs/<traceID>` and `GET /api/v1/metrics` (fleet + cache stats
  already partially exist via `handleFleetInfo`/`handleCacheInfo` — expose a
  unified metrics query proxy). Route them through the existing
  `withMiddleware`.
- `internal/api/traces.go`: confirm `SpanTreeReconstructor` is instantiated with
  `TempoURL` and registered on `/api/v1/traces/` (currently `handleTracesRoutes`
  is wired but the reconstructor construction is not visible — verify and
  connect).
- `ui/` (Nuxt, see §5): add Logs and Metrics panels consuming the new
  endpoints; hide a panel when its backend URL is empty (graceful degradation
  even though we keep 4 URLs).
- `deploy/docker/docker-compose.yaml`: replace the `prometheus` service with
  `victoria` (`victoriametrics/victoria-metrics` image, port `8428`, storage
  volume). Update the OTel collector `prometheusremotewriter` exporter to point
  at `http://victoria:8428/api/v1/write` (remote_write), and/or have Victoria
  scrape the Supervisor `/metrics` via its `vmagent`/scrape config. Update
  Grafana datasource from Prometheus → VictoriaMetrics.
- `deploy/k8s/telemetry.yaml`: replace the Prometheus Deployment/Service with
  VictoriaMetrics (single binary, port 8428, PVC for `vmstorage` data). Provide
  a `ServiceMonitor`-free scrape config (VictoriaMetrics `vmagent` scraping
  `supervisor-control` `/metrics`, or remote_write from the collector). Update
  any Grafana datasource ConfigMap.
- `docs/README.md` "Telemetry & UI": correct the claim that all four are used;
  document the logs/metrics endpoints; note VictoriaMetrics replaces
  Prometheus and is PromQL-compatible.

## 3. Cache traversal — Supervisor fronts the registry

**Current-state bug (security):** engines push/pull cache **directly** from the
publicly-exposed `cache.reg/dagger-cache:Vx` using a *separate* auth token
injected by `cache.Backend.BuildEngineJSON`. The registry is exposed and auth
is a parallel token to `DAGGER_CLOUD_TOKEN`.

Tasks:
- `internal/cache/cache.go`: extend `Backend` with the **internal** registry
  address (e.g. `cache-registry.dagger-cache.svc:5000`) plus a **public cache
  host** (the Supervisor vhost, e.g. `cache.supv.example.com`).
  - `BuildCacheConfig(v, mode)` → emit
    `type=registry,ref=<public-cache-host>/<repo>:<tag>,mode=<mode>`
    (engines now target the Supervisor, not the raw registry).
  - `BuildEngineJSON(token)` → inject the Supervisor cache host with the
    **`DAGGER_CLOUD_TOKEN`** (same token as control plane), not a separate
    registry token.
- `internal/api/server.go`: add a **Host-routed reverse proxy** on the existing
  control-plane listener (`:8080`). Match on `Host == cfg.Cache.PublicHost`
  (or a configured cache vhost); validate `Authorization: Basic <token>` via the
  **same** `extractToken` + token-file check used by `handleEngines`; optionally
  cross-check an active lease; then `httputil.NewSingleHostReverseProxy`
  to the internal registry URL. Registry clients expect the API at the host
  root — **do not path-prefix**. Unknown hosts fall through to the normal mux
  (UI + control API).
- `internal/auth`: (currently empty package) implement a shared `TokenValidator`
  used by both `handleEngines` and the cache proxy so auth stays one path.
- `deploy/k8s/cache-registry.yaml`: change the registry `Service` to
  **`ClusterIP` only** (no public exposure). Add an Ingress / external-dns entry
  for `cache.supv.example.com` → `supervisor-control` Service so the Supervisor
  vhost is reachable.
- `deploy/docker/docker-compose.yaml`: point the cache vhost at the Supervisor.
- `cmd/dagger-cache.sh` + `ci-integrations/*`: derive cache ref from the
  Supervisor cache host (not the raw registry).
- `docs/README.md` "Remote shared cache": rewrite to describe the proxied flow
  and single-token auth; note S3 mode is proxied the same way via an S3 gateway
  (or keep S3 direct if user opts out — flag as open).
- Tests: extend `test/integration_test.go` to cover token-rejected → 401 and
  token-valid → proxied to stubbed backend.

## 4. CA / TLS — provider model (embedded goca default)

**Current-state bug:** `cmd/supervisor/main.go` calls `ca.NewMintingCA`
(in-memory) on **every start** — a brand-new CA key + 10-yr self-signed cert is
generated each restart, **invalidating all previously-minted client certs**.
`ca.minting_ca_secret`, `tls.server_cert_secret`, and `NewMintingCAFromPEM` are
**dead code** (never called). The server cert is the CA's own cert
(`mintingCA.TLSCertificate()`), not a separate server cert.

Tasks:
- `internal/config/config.go`: add `TLSConfig.Provider` (`embedded` |
  `cert-manager` | `external`, default `embedded`). Add goca import path
  `github.com/disaster37/goca` to `go.mod`.
- `internal/ca/ca.go`: introduce a `Provider` interface:
  ```go
  type Provider interface {
      MintingCA() (*MintingCA, error)          // existing API
      ServerTLSCert() (tls.Certificate, error) // dedicated server cert, NOT the CA cert
  }
  ```
  Implementations:
  - `EmbeddedProvider` (goca): on first start, create root CA (goca `New`) +
    server cert (goca `IssueCertificate` with the data-plane SAN). **Persist**
    to a store (K8s Secret when in-cluster, file under `$CAPATH`/PVC for Docker
    dev). On subsequent starts, **load** existing CA. Watch CA NotAfter; renew
    (re-issue server cert; rotate root with cross-signing/rollover) before
    expiry. ← **Open item:** verify goca's root-CA rotation support; if goca
    only issues leaf certs, implement a Supervisor-side renewal loop that
    re-issues the server cert and, near CA expiry, creates a new root + re-stages.
  - `CertManagerProvider`: read `tls.crt`/`tls.key` from a Secret maintained by
    a cert-manager `Certificate` (referenced by name). Supervisor does not
    generate; it only mounts. Document the required `Issuer`/`Certificate` CRDs
    in the helm chart.
  - `ExternalProvider`: current bring-your-own-secret path
    (`tls.server_cert_secret` + `ca.minting_ca_secret`), using the existing
    `NewMintingCAFromPEM`. Now actually wired (today it isn't).
- `cmd/supervisor/main.go`: select provider by `cfg.TLS.Provider`; pass
  `Provider` to `api.NewServer`. Stop calling bare `ca.NewMintingCA`.
- `internal/api/server.go`: `Start` uses `provider.ServerTLSCert()` for the
  data-plane listener (a real server cert, not the CA cert).
- `deploy/k8s/supervisor.yaml`: default to `embedded`; document the
  cert-manager alternative (RBAC for reading Secrets, or mounted secret).
- `docs/README.md` "TLS & client certificates": replace the manual
  "provision TLS + minting-CA secrets" instructions with the zero-config default
  + the cert-manager opt-in.

## 5. UI — core, Nuxt SSG, embedded, no config

**Current-state bug:** none functionally, but UI is optional (`ui.enabled`)
and lives on a configurable `spa_dir` — against the "core component" decision.

Tasks:
- `ui/`: replace Vite+Vue with **Nuxt 3**. `nuxt.config.ts` with
  `ssr: true` + `nitro: { preset: 'static' }` (SSG via `nuxt generate`).
  Pages: pipeline/trace view (existing), fleet, cache, logs, metrics
  (consuming the §2 endpoints). Keep `vue-router`/`pinia`/`echarts` deps as
  needed via Nuxt modules.
- `ui/package.json`: scripts `dev`, `generate` (`nuxt generate`), `preview`;
  swap deps to `nuxt` + retained UI libs.
- `Dockerfile` `ui-builder` stage: `RUN npm run generate` (was `npm run build`);
  output dir `ui/.output/public` → copy into the Go binary build.
- `internal/api/server.go`: serve UI from **`embed.FS`** (no `spa_dir`).
  `//go:embed all:ui-dist` (or a generated asset dir) → `http.FileServer(http.FS(...))`
  at `/`, with SPA fallback to `index.html` for client routes. Remove the
  `ui.enabled` gate (always on).
- `internal/config/config.go`: remove `UIConfig` (`Enabled`, `SPADir`) and the
  `ui.*` defaults. Remove `UI` field from `Config`.
- `internal/api/server.go` `ServerConfig`: remove any UI fields.
- `docs/README.md`: "Telemetry & UI" — document Nuxt build + embed; remove
  `ui.enabled`/`spa_dir` references.
- Build verification: `go build ./...` must succeed with embedded assets;
  `cd ui && npm run generate` must produce `.output/public`.

---

## Cross-cutting

- `config.app.yaml.sample`: full rewrite to match final schema (no `ui_url`,
  no `ui` block, new `tls.provider`, cache vhost key, telemetry with
  `victoria_url` instead of `prometheus_url`, documented as fully wired).
- `helm/dagger-kubernetes/`: sync `values.yaml` + templates to the new schema
  (cache `ClusterIP`, embedded TLS default, cert-manager opt-in, Nuxt image
  build, no UI flags, VictoriaMetrics replaces Prometheus in telemetry
  templates + Grafana datasource).
- `docs/README.md` config reference table: regenerate from the new sample.
- `test/integration_test.go`: add cache-proxy auth (401 + proxied-200) and
  CA-persistence (restart keeps same CA fingerprint) cases.

## Open items (resolve during implementation)

1. **goca root-CA rotation:** confirm `github.com/disaster37/goca` supports
   root renewal/rollover; if not, implement a Supervisor-side renewal loop and
   document the rollover window.
2. **S3 cache behind the proxy:** OCI registry proxy is straightforward HTTP;
   S3 backend may need an S3 gateway or remain direct. Decide whether S3 stays
   direct (token still injected) or is also proxied.
3. **Cache vhost host config:** add `cache.public_host` (e.g.
   `cache.supv.example.com`) to `CacheConfig` for the Host-match in the proxy.

## Validation

- `go build ./...` and `go test ./...` green (incl. new cache-proxy + CA
  persistence tests).
- `cd ui && npm run generate` produces static output; embedded UI renders at
  `/` with trace/logs/metrics panels.
- `deploy/docker` `docker compose up` runs zero-config: embedded CA persists
  across Supervisor restart (same client cert still valid), cache pushes go
  through the Supervisor vhost with the cloud token, telemetry panels populate
  from all four backends (Tempo traces, Loki logs, VictoriaMetrics metrics).
- `deploy/k8s` `kubectl apply` runs with `tls.provider: embedded` by default;
  cert-manager path documented and manually verified once.

## Out of scope

- Migrating the control API off Hertz.
- Replacing the data-plane L4 mTLS proxy model (raw TCP stays).
- New CI providers beyond GitHub/Jenkins/Drone.
