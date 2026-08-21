# Dead Code, Deprecated API, Config/Helm/Docs Audit & Resource Sizing Cleanup Plan

**Date:** 2026-08-20
**Module:** `github.com/disaster/dagger-kubernetes`
**Scope:** A read-only research + planning deliverable. No code is changed by this document. It itemizes dead/unused code, deprecated paths, config/Helm/docs drift, and a resource-sizing recommendation for a ~10-user standard profile, then prescribes an ordered, verifiable implementation sequence.

**Method:** Full-tree read of `cmd/`, `config/`, `internal/domain`, `internal/service`, `internal/repository`, `internal/handler`, `internal/observ`, plus the Helm chart (`deploy/helm/dagger-kubernetes`), `docs/`, top-level `README.md`, `CONTRIBUTING.md`, `AGENTS.md`, `AGENTS.local.md`, and `scripts/`. Cross-checked by symbol-level grep.

---

## Section A — Dead / unused code (itemized)

Legend for **Category**: **(a)** truly dead (no non-test reference) — safe to delete; **(b)** referenced only by tests — delete together with the corresponding test updates; **(c)** exported API potentially used by module consumers — flag, do not delete silently; **(d)** kept for interface compliance / layering — keep.

| # | File | Symbol | Lines | Cat | Evidence | Action |
|---|------|--------|-------|-----|----------|--------|
| A1 | `internal/repository/stub_provider.go` | `ReplicaState`, `ReplicaStateRunning`, `ReplicaStateDraining` | 12–17 | a | `grep` finds only the definitions; zero references anywhere (prod or test). | Delete. |
| A2 | `internal/repository/metrics_store.go` | `func (c *MetricsClient) InstantQuery(...)` | 41–52 | a | No callers. `CacheHitRate` uses `instantScalar` directly. | Delete. |
| A3 | `internal/repository/metrics_store.go` | `func (c *MetricsClient) RangeQuery(...)` | 54–68 | a | No callers. | Delete. |
| A4 | `internal/repository/metrics_store.go` | `func (c *MetricsClient) doQuery(...)` | 70–72 | a | Only called by A2/A3. `doQueryCtx` is still used by `instantScalar` (keep). | Delete. |
| A5 | `internal/repository/live_hub.go` | `func (h *LiveHub) BroadcastSpanUpdate(...)` | 84–87 | b | Only `internal/repository/telemetry_test.go:139,159`. | Delete + update tests. |
| A6 | `internal/repository/live_hub.go` | `func (h *LiveHub) ClientCount(...)` | 114–118 | b | Only `internal/repository/telemetry_test.go:100,105,120,121,131,132`. | Delete + update tests. |
| A7 | `internal/repository/cache_routes_repo.go` | `func (r *CacheRoutesRepo) BackendCharge(...)` | 82–85 | b | Only `cache_routes_repo_test.go:113–127`. (`AllCharges` is used by `RegistryRouter.RefreshCharges` — keep.) | Delete + update tests. |
| A8 | `internal/repository/cache_routes_repo.go` | `func (r *CacheRoutesRepo) DeleteRoutesForBackend(...)` | 97–100 | b | Only `cache_routes_repo_test.go:155–173` and `fsm_test.go:502`. | Delete + update tests + FSM support (see A8b). |
| A8b | `internal/repository/fsm.go` | `kindDeleteRoutesForBackend` (36), `cmdDeleteRoutesForBackend` (152–154), `case kindDeleteRoutesForBackend` (388–392), `fsmState.deleteRoutesForBackend` (850–862) | — | b | Reachable only via A8. | Delete together with A8. |
| A9 | `internal/service/session.go` | `func (s *Store) Count()` | 174–178 | b | Only `session_test.go:65–66`. | Delete + update test. |
| A10 | `internal/service/session.go` | `func (s *Store) ListByVersion(...)` | 195–206 | a | Zero references anywhere. | Delete. |
| A11 | `internal/service/fleet.go` | `func (m *Manager) ScaleToZero(...)` | 212–230 | a | Zero references anywhere. | Delete. |
| A12 | `internal/service/version.go` | `func (r *Resolver) NeedsRefresh()` | 105–109 | a | Zero references anywhere. | Delete. |
| A13 | `internal/service/version.go` | `func (r *Resolver) SetReleases(...)` | 98–103 | b | Only `connect_service_test.go:278,294`. Production constructs `NewResolver(floor, allowlist, nil)` and never populates releases. | Delete; rewrite tests to use `NewResolver(floor, allowlist, releasesMap)` directly. |
| A14 | `internal/handler/logs.go` | `handleLogsRoutes` route `GET /api/v1/logs/:traceID` | 30–36; registered `server.go:532` | legacy | Duplicate of `GET /api/v1/traces/:traceID/logs` (`traces.go:117`). Referenced by `server_test.go:41`, `middleware_test.go:63`. | Flag for removal (remove route + `handleLogsRoutes`, keep shared `queryAndWriteTraceLogs`), update tests. Requires UI check (see E9). |
| A15 | `internal/handler/server.go` | `handleAdminVersions` route `GET /v1/versions` | 854–865; registered `server.go:470` | legacy | Not part of the Dagger cloud contract (`POST /v1/engines` is). Returns `[]` in practice because `AllReleases()` is always empty (releases never populated). | Flag; remove or make it return the static `version.allowlist`. |
| A16 | `internal/handler/server.go` | `routeKind`, `routeOther/Manifest/UploadStart/UploadComplete` | 141–148 | d | Used by `routeCacheRequest`/`recordCacheRoute`. Keep. | Keep. |

### Layering violation (service → repository) — flag, do not delete

`internal/service` imports `internal/repository` in three non-test files, contradicting the documented rule in `AGENTS.md` §“Project structure” (“service … imports domain, observ”) and the dependency arrow `handler → service → domain ← repository`:

| File | Line | Imported symbols |
|------|------|------------------|
| `internal/service/cache_stats.go` | 17 | `repository.MetricsClient`, `repository.RegistryStatsClient`, `repository.ErrRegistryCatalogDisabled`, `repository.ErrManifestNotFound` |
| `internal/service/registry_router.go` | 13 | `repository.RegistryStatsClient`, `repository.CacheRoutesRepo` |
| `internal/service/history_purge.go` | 14 | `repository.MetricsClient` |

This is a **category (d) / architectural** issue, not dead code. Recommended fix (as a separate, larger changeset, not bundled with the dead-code sweep):

```go
// internal/domain/metrics.go (NEW) — stdlib only
package domain

import "context"

// CacheMetricsClient is the slice of the VictoriaMetrics client the service
// layer needs (cache hit-rate + per-trace series deletion). Implemented by
// repository.MetricsClient.
type CacheMetricsClient interface {
    CacheHitRate(ctx context.Context) (hit, miss float64, err error)
    DeleteTraceSeries(ctx context.Context, traceID string) error
}
```

`repository.MetricsClient` already satisfies it (methods at `metrics_store.go:154` and `:168`). Then `CacheStatsService.metrics` and `HistoryPurgeService.metrics` change type from `*repository.MetricsClient` to `domain.CacheMetricsClient`.

For `registry_router.go`/`cache_stats.go`, introduce a small service-local interface over the `*repository.RegistryStatsClient` methods used (`Ping`, `Catalog`, `Tags`, `ManifestSize`, `ManifestCreated`, `ProbeManifest`, `ProbeBlob`, `DeleteManifest`) so the router depends on the interface, not the concrete repository type. (`repository.CacheRoutesRepo` is already referenced only behind the router's own `routes` field; see the interface extraction below.)

```go
// internal/service/registry_client.go (NEW)
type registryClient interface {
    Host() string
    Ping(ctx context.Context) error
    Catalog(ctx context.Context) ([]string, error)
    Tags(ctx context.Context, repo string) ([]string, error)
    ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error)
    ManifestCreated(ctx context.Context, repo, tag string) (time.Time, error)
    ProbeManifest(ctx context.Context, repo, ref string) (bool, error)
    ProbeBlob(ctx context.Context, repo, digest string) (bool, error)
    DeleteManifest(ctx context.Context, repo, digest string) error
}
```

If this refactor is out of scope for the current cleanup, leave it as an explicitly acknowledged finding (Section I) and add a `// TODO(layering)` note; do **not** attempt to hide it.

---

## Section B — Deprecated API / legacy-path usage

| # | Location | What | Deprecated in favor of | Migration |
|---|----------|------|------------------------|-----------|
| B1 | `cmd/api/main.go:399–402, 405–406` (multi-node TLS warnings) and `:697–698` (`validateMigrateTokensSingleNode`) | String literal concatenation with `+` | AGENTS.md §“Strings”: “NEVER concatenate with +; ALWAYS use fmt.Sprintf” | Replace with `fmt.Sprintf` (or a single `%s`-joined raw string). |
| B2 | `config/loader.go:30`, `domain/config.go:56–59`, `config/config.app.yaml:23`, sample `:34–37`, `internal/service/auth.go`, `internal/service/legacy_import.go`, `cmd/api/main.go` `migrate-tokens` | `auth.internal.tokens_file` legacy flat-file bearer fallback (`legacy` admin identity) | Per-user API tokens (`dct_…`) / JWT / OAuth; `supervisor migrate-tokens` import path (ADR-010) | **Keep** — it is an explicit migration shim (docs + AGENTS.local.md §7 note the tokens Secret was removed). Do not delete; remove only in a future breaking release. |
| B3 | `deploy/k8s/*.yaml` (`supervisor.yaml`, `namespace-rbac.yaml`, `cache-registry.yaml`, `telemetry.yaml`) | Standalone raw manifests: `kind: Deployment` (no raft StatefulSet), `supervisor-tokens` Secret, `emptyDir` DB, no raft/StatefulSet/mTLS | Helm chart (`deploy/helm/dagger-kubernetes`) | Flag as legacy; either delete (they are referenced only by top-level `README.md` §Layout and `CONTRIBUTING.md`, not by `docs/README.md`) or add a “deprecated — use Helm” banner. Recommend delete + fix README/CONTRIBUTING references. |
| B4 | `deploy/docker/*` (`docker-compose.yaml`, `Dockerfile`, `data/tokens`, otel/tempo/prometheus yaml) | Local dev compose stack; `DAGGER_CACHE_*`-era leftovers in comments | Helm (production) / compose for dev only | Keep (still the documented dev quick-start) but fix the stale `# DEPRECATED: migrate with supervisor migrate-tokens` comment in `docker-compose.yaml:34` if the tokens path is removed. |
| B5 | `internal/handler/logs.go` + `server.go:532` | `GET /api/v1/logs/:traceID` legacy alias | `GET /api/v1/traces/:traceID/logs` | Remove route + handler (keep `queryAndWriteTraceLogs`), update `server_test.go:41`, `middleware_test.go:63` (see A14). |
| B6 | `internal/handler/server.go:470` | `GET /v1/versions` legacy admin endpoint | N/A (not part of Dagger contract); static `version.allowlist` | Remove, or repoint at static allowlist (see A15). |
| B7 | No deprecated **stdlib or third-party** API usage found. `os.ReadFile`/`os.WriteFile` (no `ioutil`), urfave/cli v2 current flags, viper `AutomaticEnv`+`SetEnvKeyReplacer`, hertz native SSE, prometheus `client_golang` current API. `gorilla/websocket` and `hertz-contrib/websocket` are **indirect** deps in `go.mod` (not imported) — `go mod tidy` may prune them but they are pulled transitively. | — | — | No action; note only. |

---

## Section C — Configuration settings audit (`config/loader.go` vs `internal/domain/config.go` vs `config.app.yaml` / sample)

### C1. Every key has a `SetDefault` — PASS
All fields across `ServerConfig`, `AuthConfig` (incl. `Internal/OAuth/JWT/Token/BootstrapAdmin/Cookie/CORS`), `TelemetryConfig`, `CacheConfig` (incl. `S3/GC`), `HistoryConfig`, `PipelineConfig`, `FleetConfig`, `CAConfig`, `TLSConfig`, `VersionConfig`, `CIConfig`, `OTelConfig`, `DatabaseConfig`, `RaftConfig`/`RaftTLSConfig`, plus `lease_ttl`, `log_level`, `log_format` have a matching `v.SetDefault(...)` in `Load()`. **No missing defaults.**

### C2. Config fields never READ by any consumer (dead config) — FAIL (3)

| Field (struct) | mapstructure | loader default | Evidence |
|----------------|--------------|----------------|----------|
| `TLSConfig.ServerCertSecret` (`domain/config.go:275`) | `server_cert_secret` | `loader.go:146` `supervisor-tls` | `grep ServerCertSecret` → only definition + loader; **zero Go reads**. Helm configmap still renders `server_cert_secret: <fullname>-tls` (`configmap.yaml:148`) which the binary ignores. |
| `FleetConfig.VersionRetention` (`domain/config.go:244`) | `version_retention` | `loader.go:118` `24h` | Copied into `Manager.versionRetention` (`fleet.go:22,43`) but **never read** in `Sweep`/`sweepVersion` or anywhere. Docs claim “a version that has had zero replicas for version_retention is garbage-collected (StatefulSet + PVs removed)” — **not implemented**. |
| `FleetConfig.MinReplicasPerVersion` (`domain/config.go:245`) | `min_replicas_per_version` | `loader.go:119` `0` | Copied into `Manager.minReplicasPerVersion` (`fleet.go:23,44`) but **never read**. Docs/README claim a warm-pool floor (“keep one warm engine per version”) — **not implemented**. |

**Fix options (must be a deliberate decision, not silent):**
- **(a) Implement** `version_retention` (delete 0-replica StatefulSets older than TTL in `Sweep`) and `min_replicas_per_version` (keep N warm replicas on `Acquire`/`Sweep`) — real feature work, out of scope here; or
- **(b) Remove** the fields from `domain/config.go`, `loader.go`, sample, Helm values (`supervisor.config.fleet.minReplicasPerVersion`, `versionRetention`), and fix docs to stop promising the behavior.
- **(Recommended)** Do (b) for the cleanup, and open a follow-up feature plan if warm pools/version GC are wanted. Do **not** leave the keys documented as functional.

For `server_cert_secret`: remove the field + loader default + sample comment + Helm `configmap.yaml:148` line (it is fully superseded by `tls.provider` auto-wiring; sample already labels it “legacy; unused”).

### C3. Sample vs loader — PASS with one fix
`config/config.app.yaml.sample` lists every key, type, default and comment and matches `loader.go` (including `pipeline.disconnect_grace`, `pipeline.stale_sweep.*`, `raft.tls.*`, `ci.*`, `otel.otlp_endpoint`). After applying C2(b), remove the corresponding sample keys (`server_cert_secret`, `fleet.version_retention`, `fleet.min_replicas_per_version`).

### C4. Env prefix — PASS
`DAGGER_KUBERNETES` + `SetEnvKeyReplacer(".", "_")` + `AutomaticEnv()` are correct. Docs table (`docs/README.md:329–341`) is accurate.

### C5. Helm configmap orphan key — FAIL
`deploy/helm/dagger-kubernetes/templates/configmap.yaml:16` renders `pipeline_url: ""` under `server:`, but `domain.ServerConfig` has no `pipeline_url` field (ADR-021 dropped it). Viper silently ignores it. **Remove the line.**

### C6. `config/config.app.yaml` (minimal example) — PASS (note)
Intentionally minimal (server/auth/database/raft/cache/history/pipeline/version only); all other keys fall back to compiled defaults as documented. Note: `client_id: "${OAUTH_CLIENT_ID}"` / `client_secret: "${OAUTH_CLIENT_SECRET}"` are literal strings (Viper does not shell-expand `${…}`); the sample comments already say to use env vars. No change required.

---

## Section D — Helm chart values audit

| # | Value / template | Issue | Detail | Fix |
|---|---|---|---|---|
| D1 | `templates/hpa.yaml:11–13` | **Broken target** | `scaleTargetRef.kind: Deployment` + `name: <fullname>` — the workload is a **StatefulSet** (`statefulset.yaml`). Enabling `supervisor.autoscaling.enabled` produces an HPA pointing at a non-existent Deployment. | Change `kind` to `StatefulSet` (and keep `name: <fullname>`). |
| D2 | `templates/configmap.yaml:16` | Dead key | `server.pipeline_url: ""` not in `domain.ServerConfig` (ADR-021). | Remove line. |
| D3 | `values.yaml` `grafana:` (572–578) | **Default mismatch** | No `grafana.enabled: true`; Chart.yaml condition `grafana.enabled` ⇒ Grafana **disabled** by default. Chart README “Tool toggles” (line 423) and “Required tools” (line 104) claim `true`; `docs/README.md:149` implies deployed. | Either add `grafana.enabled: true` (and resources) or fix both READMEs to say disabled-by-default. |
| D4 | `values.yaml` `tempo.tempo.retention: 48h` (501) vs `supervisor.config.history.gc.maxAge: 720h` (156) | **Retention inconsistency** | Spans age out at 48h while history purge keeps 30d; contradicts chart README “set tempo.retention to match (or exceed) history.gc.maxAge”. | Set `tempo.retention` default to `720h` (or document 48h as a dev default explicitly). |
| D5 | Subchart resources (collector/registry/tempo/loki/victoria) | **Missing limits** | All five set only `requests` (see values.yaml 425–428, 484–487, 507–510, 523–526, 567–570). | Add `limits` (see Section F table). |
| D6 | `supervisor.config.fleet.enginePrivileged: true` (199) | **Security divergence** | Chart forces privileged engines; binary default (`loader.go:132`) and sample (`engine_privileged: false`) are false. | Keep if BuildKit requires it, but document the privilege rationale in values/README and confirm it cannot be avoided. |
| D7 | `supervisor.config.fleet.engineDockerConfig` (223) | **Opaque effect** | Rendered into the `engine-registry-auth` Secret key `.dockerconfigjson` (`secret.yaml:51`); `k8s_provider.go` mounts that Secret at `/etc/dagger` but never references `.dockerconfigjson` as an imagePullSecret or documents how engines consume it. | Either wire it as an imagePullSecret on engine pods (real feature) or drop the value + Secret key and document removal. |
| D8 | `scripts/update-helm-docs.sh` | **Does not generate the values table** | Only rewrites `--version` markers. The README `## Parameters` section (line 520) is **empty** — the `@param` annotations in `values.yaml` were never rendered into README.md. | Run `helm-docs` (or add it to the script) to generate the parameters table, or delete the empty `## Parameters` heading. |
| D9 | Values with consumers — PASS | All `nameOverride`, `fullnameOverride`, `namespace`, `supervisor.*`, `service.*`, `ingress.*`, `dataIngress.*`, `dataCert.*`, `serviceMonitor.*`, `auth.*`, `tls.*`, `supervisor.config.telemetry.*` (fallback-only), and subchart sections are consumed by at least one template/helper. | — | No action. |

---

## Section E — Documentation updates required

| # | File | Section | What is stale / wrong | Fix |
|---|------|---------|----------------------|-----|
| E1 | `docs/README.md` | 107, 114, 131, 1061 | OCI chart path `oci://ghcr.io/disaster/charts/dagger-kubernetes` — wrong org (`disaster`) | Change to `disaster37` to match chart README + `Chart.yaml` `home`/`sources` (`github.com/disaster37/dagger-kubernetes`). |
| E2 | `docs/README.md` | 1129–1130 | “Grafana … Default credentials: `admin` / `admin`” | Chart README (339–341) says `grafana.adminPassword` defaults `""` → auto-generated random 40-char password in `<release>-grafana` Secret. Align. |
| E3 | `docs/README.md` | 1327–1328 | “append to `tokens_file` (or the `supervisor-tokens` Secret)” | The chart no longer renders a `<release>-tokens` Secret (AGENTS.local.md §7 note). Point to per-user API tokens instead. |
| E4 | `docs/README.md` | 1032, 1354 | “set `ca.crt`/`ca.key` in the chart values” | Actual Helm keys are `tls.caCrt` / `tls.caKey`. Fix key names. |
| E5 | `docs/README.md` | 426–438 (fleet table), 500–502, 1359; chart README 279–288 | `fleet.min_replicas_per_version` (“Autoscaler floor”) and `fleet.version_retention` (“garbage-collected … PVs removed”) describe behavior that is **not implemented** (Section C2). | Remove or mark “not implemented” until C2 is resolved. |
| E6 | `docs/README.md` + chart README | Fleet/autoscaling | HPA/`autoscaling` doc does not note the HPA targets a Deployment while the chart ships a StatefulSet (D1). | Note the D1 fix (HPA→StatefulSet) or remove HPA doc if unsupported. |
| E7 | `docs/README.md` | 470–485, 1394; top-level `README.md` §Layout (26–27); `CONTRIBUTING.md:186–187` | `deploy/k8s/` raw manifests are legacy (Deployment, no raft). | Either delete `deploy/k8s/` and update references, or mark deprecated in favor of Helm. |
| E8 | `docs/README.md` | Grafana datasources (505–506, 1126–1130) | States Grafana datasources are auto-provisioned, implying Grafana is deployed; it is disabled by default (D3). | Align with D3 decision. |
| E9 | `docs/README.md` + UI | Legacy logs route | `/api/v1/logs/:traceID` (A14) is undocumented; verify the SPA does not call it before removal. | Grep `ui/src` for `/api/v1/logs/`; if unused, remove route + no doc change; if used, migrate UI to `/api/v1/traces/:id/logs`. |
| E10 | `docs/README.md` | 55–80 (Docker quick-start) | Compose exposes Loki host port `3101→3100`; compose file uses `DAGGER_CACHE_*`-era comments (B4). | Verify against `deploy/docker/docker-compose.yaml`; fix any `DAGGER_CACHE_*`/`DAGGER_KUBERNETES_*` drift. |
| E11 | `internal/domain/user.go:31` | Comment “Stored in SQLite.” | SQLite replaced by Raft (ADR-015). | Change to “Stored in the Raft FSM.” |
| E12 | `internal/repository/raft_store.go:60` | Comment “now actually used (was rejected before)” | Leftover edit note. | Remove the parenthetical. |

---

## Section F — Helm resources sizing (10-user standard profile)

Findings first:

- **Missing limits everywhere in subcharts.** Only the supervisor sets `limits` (500m / 512Mi). Collector, registry, Tempo, Loki, VictoriaMetrics set `requests` only → burstable with unbounded ceiling; a runaway Loki/VM can OOM the node.
- **Supervisor request is low** for a control plane that also does TLS termination for the mTLS data plane + OCI cache reverse-proxy streaming + Raft + bcrypt/JWT. `100m/128Mi` request and `512Mi` limit can throttle cache blob uploads (memory is bounded by streaming, but 512Mi is tight for many concurrent sessions).
- **Engines are the heavy workload** (BuildKit); default `500m/2 core` limit, `1Gi/8Gi` mem, `50Gi` PVC per engine is reasonable per-pod, but `50Gi × up to 3 replicas × N versions` is a lot of storage for a small cluster.
- **Probes:** supervisor readiness (`/readyz`) returns 503 when the cache/telemetry rollup is down (`status.go:55–67`), which can deadlock first boot if the registry subchart is not yet ready (readiness never flips Ready). Liveness always 200 (safe). Engines get a TCP readiness probe only (no liveness/startup).

### Recommended per-container table (10 users)

| Container | requests.cpu | requests.mem | limits.cpu | limits.mem | Justification |
|-----------|-------------|--------------|-----------|-----------|---------------|
| supervisor | `250m` | `256Mi` | `1000m` | `1Gi` | Go control plane + mTLS data-plane termination + OCI reverse-proxy (streamed) + Raft + bcrypt. `250m/256Mi` covers idle+burst; `1Gi` headroom for many concurrent blob streams and the embedded UI. |
| engine (per pod) | `500m` | `1Gi` | `4000m` | `8Gi` | BuildKit/Dagger engine is CPU+memory heavy; `2→4` cores and `8Gi` ceiling is the safe default for concurrent solves. Keep 8 sessions/replica. |
| opentelemetry-collector | `100m` | `256Mi` | `500m` | `512Mi` | OTLP fan-out; modest CPU, bounded memory for batch/transform processors. |
| registry | `100m` | `128Mi` | `500m` | `512Mi` | I/O bound `registry:2`; limit guards blob-upload buffering spikes. |
| tempo | `250m` | `512Mi` | `2000m` | `2Gi` | Compaction + trace query are CPU/mem intensive even for 10 users. |
| loki (SingleBinary) | `250m` | `512Mi` | `2000m` | `2Gi` | Ingestion + compactor; SingleBinary is memory-hungry. |
| victoria (single) | `100m` | `256Mi` | `1000m` | `1Gi` | PromQL + remote-write ingest; single server is light for 10 users. |
| grafana (if enabled) | `100m` | `128Mi` | `200m` | `256Mi` | Dashboards only. |

Storage: keep engine PVC `50Gi` (reduce to `30Gi` for small clusters), registry `50Gi`, Tempo/Loki/Victoria `20Gi` each (dev) — fine for 10 users; increase Tempo/Loki to `100Gi` for long retention. Supervisor raft PVC `2Gi` is ample.

**Probe fix (recommended):** keep `/healthz` as the liveness (always 200); add `failureThreshold` and consider making `/readyz` **not** gate on cache/telemetry (or only on raft leadership + control server), so a down registry does not block pod readiness on first boot.

---

## Section G — Test impact & coverage

Deletions in Section A require these test edits (standard `testing` package, table-driven, `logrus.New()`→`io.Discard`, per AGENTS.md):

| Deleted symbol | Test file to change | Change |
|----------------|---------------------|--------|
| `ReplicaState*` (A1) | none | no test references; safe delete. |
| `InstantQuery`/`RangeQuery`/`doQuery` (A2–A4) | none | no test references; safe delete. |
| `BroadcastSpanUpdate` (A5) | `internal/repository/telemetry_test.go:139,159` | switch to `hub.Broadcast(...)` or delete the two lines. |
| `ClientCount` (A6) | `internal/repository/telemetry_test.go:100–132` (`TestLiveHubClientCounts`) | delete the test, or re-implement counting via `Subscribe`/`Unsubscribe` bookkeeping if still valuable. |
| `BackendCharge` (A7) | `internal/repository/cache_routes_repo_test.go:113–153` | keep the `AllCharges` half of the test, drop the `BackendCharge` half. |
| `DeleteRoutesForBackend` (A8/A8b) | `cache_routes_repo_test.go:155–190`, `fsm_test.go:502` | delete `TestDeleteRoutesForBackend` and the `kindDeleteRoutesForBackend` case. |
| `Store.Count` (A9) | `internal/service/session_test.go:65–66` | use `len(s.List())` or drop the assertion. |
| `ListByVersion` (A10) | none | safe delete. |
| `ScaleToZero` (A11) | none | safe delete. |
| `NeedsRefresh` (A12) | none | safe delete. |
| `SetReleases` (A13) | `internal/service/connect_service_test.go:278,294` | construct a fresh `service.NewResolver(floor, allowlist, map[string][]string{...})` instead of mutating. |
| `handleLogsRoutes` (A14) | `server_test.go:41`, `middleware_test.go:63` | remove the route registration; keep `handleTracesLogs` coverage. |
| `handleAdminVersions` (A15) | `server_test.go:50,171` | update/remove the `/v1/versions` case. |

**Coverage maintenance:** AGENTS.md mandates 100% coverage per package. Deleting code *reduces* the denominator; do not let the removals leave uncovered branches. After each package edit run:

```bash
go test -cover ./internal/repository/ ./internal/service/ ./internal/handler/ ...
```

and add/extend table-driven tests for any behavior still exercised but now uncovered. Integration tests (`tests/integration/*`) do not reference any to-be-deleted symbol (verified: they use `MaxReplicasPerVersion`/`ReplicaIdleTTL` only, which remain).

---

## Section H — Ordered implementation steps

**Phase 0 — verify baseline**
```bash
cd /projects/dagger-cache
go build ./...
go vet ./...
go test ./...
helm lint deploy/helm/dagger-kubernetes
helm template dagger-kubernetes deploy/helm/dagger-kubernetes --namespace test
```

**Phase 1 — dead code (lowest risk first)**
1. `internal/repository/stub_provider.go` — delete `ReplicaState*` (A1).
2. `internal/repository/metrics_store.go` — delete `InstantQuery`, `RangeQuery`, `doQuery` (A2–A4).
3. `internal/service/session.go` — delete `ListByVersion` (A10), `Count` (A9) + fix `session_test.go`.
4. `internal/service/fleet.go` — delete `ScaleToZero` (A11).
5. `internal/service/version.go` — delete `NeedsRefresh` (A12), `SetReleases` (A13) + fix `connect_service_test.go`.
6. `internal/repository/live_hub.go` — delete `BroadcastSpanUpdate` (A5), `ClientCount` (A6) + fix `telemetry_test.go`.
7. `internal/repository/cache_routes_repo.go` — delete `BackendCharge` (A7), `DeleteRoutesForBackend` (A8) + fix tests + `fsm.go` (`kindDeleteRoutesForBackend`, `cmdDeleteRoutesForBackend`, case, `fsmState.deleteRoutesForBackend`) + `fsm_test.go`.
8. Legacy routes — remove `handleLogsRoutes` (A14) and `GET /v1/versions` (A15) after UI grep confirms no `ui/src` usage; update `server_test.go`/`middleware_test.go`.

**Phase 2 — config cleanup (Section C)**
9. `internal/domain/config.go` — remove `TLSConfig.ServerCertSecret`, `FleetConfig.VersionRetention`, `FleetConfig.MinReplicasPerVersion`.
10. `config/loader.go` — remove the three `v.SetDefault(...)` lines.
11. `config/config.app.yaml.sample` — remove the three keys + comments (also `server_cert_secret` note).
12. `internal/service/fleet.go` — remove the now-unused `versionRetention`/`minReplicasPerVersion` struct fields and constructor params (and `ManagerConfig` fields); update all `ManagerConfig{...}` literals in `cmd/api/main.go`, `internal/service/fleet_test.go`, `internal/handler/test_helper_test.go`, `tests/integration/*`.
13. `deploy/helm/.../values.yaml` — remove `supervisor.config.fleet.minReplicasPerVersion` and `versionRetention`; `templates/configmap.yaml` — drop `pipeline_url: ""` (C5) and `server_cert_secret` (C2).

**Phase 3 — Helm fixes (Section D)**
14. `templates/hpa.yaml` — `kind: StatefulSet` (D1).
15. `values.yaml` — add subchart `limits` per Section F table; decide `grafana.enabled` (D3); set `tempo.retention` to `720h` (D4); document `enginePrivileged` (D6); resolve `engineDockerConfig` (D7).
16. Regenerate/complete the chart `README.md` parameters table (D8).

**Phase 4 — docs (Section E)**
17. Apply E1–E12 in `docs/README.md`, chart README, top-level `README.md`, `CONTRIBUTING.md`, `internal/domain/user.go`, `internal/repository/raft_store.go`.

**Phase 5 — string-concat compliance (B1)**
18. `cmd/api/main.go` — replace `+` concatenations with `fmt.Sprintf` (or single raw strings).

**Phase 6 — verify + (per AGENTS.local.md §6) redeploy**
```bash
gofmt -w ./... && goimports -w ./...
go build ./... && go vet ./... && go test ./...
go test -cover ./internal/service/ ./internal/repository/ ./internal/handler/ ./internal/domain/ ./config/
helm lint deploy/helm/dagger-kubernetes
helm template dagger-kubernetes deploy/helm/dagger-kubernetes --namespace test
```
Then follow `AGENTS.local.md` §4–§5 (build image → push → capture values → `helm upgrade` with `--set supervisor.config.raft.replicas=1` → rollout restart → agent + human verification).

---

## Section I — Risks / things NOT to delete

1. **`auth.internal.tokens_file` / `TokenValidator` / `migrate-tokens`** — active migration shim; deleting breaks legacy CI tokens (B2). Keep.
2. **Interface-compliance stubs** — `var _ domain.FleetProvider = (*StubProvider)(nil)`, `var _ domain.MintingCA = ...`, `var _ raft.FSM = ...`, `var _ domain.CacheBackend = (*Cache)(nil)` etc. are intentional and must remain even though the concrete types are only used in tests (`StubProvider`). Do **not** delete `StubProvider` — it is the fallback when no K8s clientset exists (`cmd/api/main.go:975–977`).
3. **`resolveLiveHub` / typed-nil guard in `NewServer`** (`server.go:344–349, 352–360`) — deliberate; do not “simplify” into a typed-nil panic.
4. **Generated files** (`dagger/internal/dagger/*.gen.go`, `dagger/*.gen.go`, `dagger/dagger.gen.go`) — regenerated GraphQL bindings; `XXX_GraphQLType` etc. are required interface methods, not dead code. Never hand-edit.
5. **`service → repository` layering** (A-layering) — do **not** fix by moving types around in the same changeset as the dead-code sweep; keep it a separate, reviewed refactor (or an explicitly deferred TODO).
6. **`fleet.min_replicas_per_version` / `fleet.version_retention`** — remove only if the “warm pool” and “version GC” features are genuinely not desired; otherwise implement them (they are documented, user-facing behavior).
7. **`enginePrivileged: true`** — likely required by BuildKit; verify before changing, and never silently drop it (security + functionality tradeoff).
8. **`.kilo/`, `.kube/`, `kilo.json`, `coverage.out`, `out/`** — non-source artifacts at repo root. `coverage.out`/`out/` are gitignored; `.kilo/` (old plan dumps) and `.kube/` (a kubeconfig dir) are **not** in `.gitignore` and `.kube/` may contain cluster credentials — review and gitignore/remove rather than treat as dead code. (AGENTS.local.md is already gitignored.)
9. **`deploy/docker/`** — keep as the documented dev path even though it is legacy; only fix stale comments.
10. **Do not touch `AGENTS.local.md`** — machine-specific, gitignored, source-of-truth for the live cluster.

---

## Section J — Open questions (unresolvable by static analysis)

1. **Grafana enabled?** `values.yaml` has no `grafana.enabled`, yet AGENTS.local.md §9 lists Grafana credentials and the live cluster serves `/grafana`. Is Grafana deployed by this chart (via a captured override) or a separate stack? → decides D3/E8.
2. **`fleet.min_replicas_per_version` / `version_retention`** — were these intended features or aspirational config? → decides C2 (remove vs implement).
3. **`engineDockerConfig`** — how are engines expected to consume `.dockerconfigjson`? Not referenced as imagePullSecret. → decides D7.
4. **Legacy routes** (`/v1/versions`, `/api/v1/logs/:traceID`) — does the Vue UI or any external consumer call them? Grep `ui/src` (and CI integrations) before removal. → decides A14/A15.
5. **`deploy/k8s/`** — is anyone still deploying from raw manifests, or can they be deleted? → decides B3/E7.
6. **`service → repository` layering** — defer to a dedicated refactor, or include in this changeset? (Recommended: defer.) → decides scope.
7. **Coverage gate** — `coverage.out` suggests a CI coverage threshold; confirm the exact `go test -cover` gate used by `dagger/` pipeline before enforcing 100% in Phase 6.
