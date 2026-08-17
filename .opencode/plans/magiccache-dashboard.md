# Plan: MagicCache Dashboard — cache status, stats, services status, GC, and purge

**Module:** `github.com/disaster/dagger-kubernetes` · **Root:** `/projects/dagger-cache`
**Goal:** Make the web UI show rich MagicCache information: cache running state, all-services status page, real-time header status indicator, cache size + object count + purge, and auto-clean (GC) rules + "will it auto-clean".

All conventions from `AGENTS.md` apply: `fmt.Sprintf` (never `+`), `%w` error wrapping, logrus structured logging, table-driven stdlib `testing` with 100% coverage target, `gofmt`/`goimports` (local prefix `github.com/disaster/dagger-kubernetes`), Hertz for HTTP, Viper for config, dependency rule `handler → service → domain ← repository` (`domain` stdlib-only; `observ` is the cross-cutting exception).

---

## 1. File inventory (new + modified)

### Backend (Go)
| File | Status | Purpose |
|---|---|---|
| `internal/domain/cache.go` | MODIFY | Add stats/purge/GC structs + `CacheStatsProvider`, `CachePurger` interfaces; add `InternalAddr` to `S3Ref`? no — add `GCConfig` to `CacheConfig` (here). |
| `internal/domain/status.go` | NEW | `ServiceStatus`, `ServiceState`, `PlatformStatus`, `StatusProvider` interface (stdlib only). |
| `internal/domain/config.go` | MODIFY | Add `GCConfig` struct + `GC GCConfig` field on `CacheConfig`. |
| `internal/repository/registry_client.go` | NEW | OCI Distribution v2 client (catalog/tags/manifests/delete) via stdlib `net/http`. |
| `internal/repository/registry_client_test.go` | NEW | Table-driven unit tests (httptest server). |
| `internal/repository/metrics_store.go` | MODIFY | Already has `MetricsClient`; no change needed except confirming it is injectable. (No edit required if signature sufficient; otherwise add a `CacheHitRate(ctx) (hit, miss float64, err error)` helper.) |
| `internal/service/cache.go` | MODIFY | Add `InternalAddr string` field to `Cache` struct (used by status probe). |
| `internal/service/cache_stats.go` | NEW | `CacheStatsService` implementing `CacheStatsProvider` + `CachePurger`; TTL cache; GC sweeper. |
| `internal/service/cache_stats_test.go` | NEW | Unit tests with stub registry client + stub metrics client. |
| `internal/service/status.go` | NEW | `StatusService` implementing `StatusProvider`; aggregates supervisor/cache/telemetry/fleet. |
| `internal/service/status_test.go` | NEW | Unit tests with stubs. |
| `internal/handler/cache.go` | NEW | `handleCacheInfo` (rich), `handleCachePurge`, `handleCachePurgeAll`, `handleCacheGC` (optional). |
| `internal/handler/cache_test.go` | NEW | Handler tests (auth gating, admin gating, error shapes). |
| `internal/handler/status.go` | NEW | `handlePlatformStatus`; upgrade `handleHealthz`/`handleReadyz` to real aggregation. |
| `internal/handler/status_test.go` | NEW | Handler tests. |
| `internal/handler/server.go` | MODIFY | Add `Deps` fields (`CacheStatsProvider`, `CachePurger`, `StatusProvider`, `MetricsClient`); register new routes; remove inline `handleCacheInfo`. |
| `internal/handler/server_test.go` | MODIFY | Update `newTestEngine` to register new routes; update `TestHandleCacheInfo` for new shape. |
| `internal/handler/test_helper_test.go` | MODIFY | Wire new deps into `newTestEnv` (stub stats/status providers). |
| `internal/observ/metrics.go` | MODIFY | Add `CacheSizeBytes`, `CacheObjectCount`, `CachePurgeTotal`, `GCRunTotal` collectors. |
| `cmd/api/main.go` | MODIFY | Construct `RegistryStatsClient`, `MetricsClient`, `CacheStatsService`, `StatusService`; inject into `Deps`; start GC sweeper goroutine. |
| `config/loader.go` | MODIFY | Add `cache.gc.*` Viper defaults. |
| `config/config.app.yaml` | MODIFY | Add `cache.gc.*` section (keep in sync with sample). |
| `config/config.app.yaml.sample` | MODIFY | Add `cache.gc.*` section with comments. |
| `config/loader_test.go` | MODIFY | Add GC defaults assertions. |

### Frontend (Vue 3 + TS)
| File | Status | Purpose |
|---|---|---|
| `ui/src/api/types.ts` | MODIFY | Add `CacheInfo`, `CacheVersionRef`, `PurgeRequest`, `PurgeResult`, `GCRules`, `GCRunSummary`, `ServiceStatus`, `ServiceState`, `PlatformStatus`. |
| `ui/src/api/client.ts` | MODIFY | Update `fetchCacheInfo` return type; add `purgeCache`, `purgeAllCache`, `fetchPlatformStatus`. |
| `ui/src/stores/status.ts` | NEW | Pinia store polling `/api/v1/status` every 10s; exposes rollup + per-service. |
| `ui/src/components/StatusIndicator.vue` | NEW | Header badge (all-ok/degraded/down). |
| `ui/src/App.vue` | MODIFY | Add `<StatusIndicator />` in navbar. |
| `ui/src/magiccache/MagicCache.vue` | MODIFY (rewrite) | Stats, per-version refs, GC rules, purge buttons. |
| `ui/src/views/Services.vue` | NEW | Services/status page. |
| `ui/src/router/index.ts` | MODIFY | Add `/services` route. |
| `ui/src/style.css` | MODIFY | Add `.status-dot`, `.status-ok/.status-degraded/.status-down/.status-unknown` classes. |

### Docs / ADRs / config
| File | Status | Purpose |
|---|---|---|
| `docs/design/ADR-012-magiccache-dashboard.md` | NEW | ADR for cache stats, services status, GC, purge. |
| `docs/design/index.md` | MODIFY | Add ADR-012 row. |
| `docs/README.md` | MODIFY | Update MagicCache + services-status sections. |
| `config/config.app.yaml.sample` | MODIFY | `cache.gc.*` section (per AGENTS.md sync rule). |

### Tests
| File | Status |
|---|---|
| `internal/repository/registry_client_test.go` | NEW |
| `internal/service/cache_stats_test.go` | NEW |
| `internal/service/status_test.go` | NEW |
| `internal/handler/cache_test.go` | NEW |
| `internal/handler/status_test.go` | NEW |
| `tests/integration/cache_status_test.go` | NEW |
| `config/loader_test.go` | MODIFY |

---

## 2. Data structures

### Go — `internal/domain/cache.go` (additions)

```go
// CacheStats is the rich cache payload returned by GET /api/v1/cache.
type CacheStats struct {
    Backend       string            `json:"backend"`        // "registry" | "s3"
    Registry      string            `json:"registry"`       // registry host (or s3 bucket)
    Running       bool              `json:"running"`        // registry reachable / s3 configured
    Reachable     bool              `json:"reachable"`      // last probe succeeded
    TotalSize     int64             `json:"total_size"`     // bytes; -1 when unknown
    ObjectCount   int64             `json:"object_count"`   // layer/blob count; -1 when unknown
    Versions      []CacheVersionRef `json:"versions"`       // per-version refs (registry backend)
    HitRate       *float64          `json:"hit_rate"`       // 0..1; nil when no data
    HitCount      int64             `json:"hit_count"`      // from VictoriaMetrics; 0 when no data
    MissCount     int64             `json:"miss_count"`
    CollectedAt   string            `json:"collected_at"`   // RFC3339 UTC
    Message       string            `json:"message,omitempty"` // human note (e.g. "s3 stats unsupported", "catalog disabled")
    GC            GCRules           `json:"gc"`
}

// CacheVersionRef describes one per-version cache ref (registry backend).
type CacheVersionRef struct {
    Version     string `json:"version"`      // e.g. "v0.21.4"
    Tag         string `json:"tag"`           // e.g. "v0-21-4"
    Ref         string `json:"ref"`           // full ref "cache.reg/dagger-cache:v0-21-4"
    Size        int64  `json:"size"`          // sum of layer sizes (bytes); -1 unknown
    LayerCount  int64  `json:"layer_count"`   // number of layers; -1 unknown
    Digest      string `json:"digest"`        // manifest digest (sha256:...); "" when unavailable
    Protected   bool   `json:"protected"`     // true when version has active fleet replicas
    LastUsedAt  string `json:"last_used_at,omitempty"` // RFC3339; "" when unknown
}

// GCRules describes the auto-clean configuration and last/next run.
type GCRules struct {
    Enabled              bool          `json:"enabled"`
    MaxAge               string        `json:"max_age"`            // duration string e.g. "168h"
    Schedule             string        `json:"schedule"`           // duration string e.g. "1h"
    MinRefsToKeep        int           `json:"min_refs_to_keep"`
    ProtectActiveVersions bool         `json:"protect_active_versions"`
    LastRunAt            string        `json:"last_run_at,omitempty"`       // RFC3339
    LastRunSummary       *GCRunSummary `json:"last_run_summary,omitempty"`
    NextRunAt            string        `json:"next_run_at,omitempty"`      // RFC3339 (estimated)
}

type GCRunSummary struct {
    StartedAt   string `json:"started_at"`    // RFC3339
    FinishedAt  string `json:"finished_at"`  // RFC3339
    PurgedTags  int    `json:"purged_tags"`
    FreedBytes  int64  `json:"freed_bytes"`
    Skipped     int    `json:"skipped"`        // protected or below min_refs
    Errors      int    `json:"errors"`
    Message     string `json:"message,omitempty"`
}

// PurgeRequest is the body of POST /api/v1/cache/purge.
type PurgeRequest struct {
    Version string `json:"version"` // engine version e.g. "v0.21.4"; "" invalid
    Tag      string `json:"tag"`      // optional explicit tag; defaults to derived from version
}

// PurgeResult is the response of purge endpoints.
type PurgeResult struct {
    Purged        int      `json:"purged"`
    FreedBytes    int64    `json:"freed_bytes"`
    Versions      []string `json:"versions"`       // versions affected
    AlreadyPurged int      `json:"already_purged"`  // tags that were already absent
    Message       string   `json:"message,omitempty"`
}

// CacheStatsProvider reports cache stats (size, objects, hit rate, GC rules).
type CacheStatsProvider interface {
    Stats(ctx context.Context) (*CacheStats, error)
    GCRules() GCRules
}

// CachePurger purges cache refs.
type CachePurger interface {
    Purge(ctx context.Context, req PurgeRequest) (*PurgeResult, error)
    PurgeAll(ctx context.Context) (*PurgeResult, error)
}
```

### Go — `internal/domain/status.go` (new)

```go
package domain

import "context"

// ServiceState is the rollup health of a single platform service.
type ServiceState string

const (
    ServiceOK       ServiceState = "ok"
    ServiceDegraded ServiceState = "degraded"
    ServiceDown     ServiceState = "down"
    ServiceUnknown  ServiceState = "unknown"
)

// ServiceStatus is one row in the services/status view.
type ServiceStatus struct {
    Name       string      `json:"name"`        // "supervisor" | "cache" | "collector" | "tempo" | "loki" | "victoria" | "fleet"
    Category   string      `json:"category"`   // "control" | "cache" | "telemetry" | "fleet"
    State      ServiceState `json:"state"`
    Message    string      `json:"message,omitempty"`
    Configured bool        `json:"configured"` // false when the URL/feature is not configured (then state=unknown)
    CheckedAt  string      `json:"checked_at"` // RFC3339 UTC
}

// PlatformStatus is the aggregated response of GET /api/v1/status.
type PlatformStatus struct {
    State    ServiceState    `json:"state"`     // rollup: down if any down; degraded if any degraded & none down; else ok
    Services []ServiceStatus `json:"services"`
    CheckedAt string         `json:"checked_at"`
}

// StatusProvider aggregates platform service health.
type StatusProvider interface {
    Status(ctx context.Context) (*PlatformStatus, error)
}
```

### Go — `internal/domain/config.go` (additions)

```go
// In CacheConfig:
type CacheConfig struct {
    Backend       string   `mapstructure:"backend"`
    Registry      string   `mapstructure:"registry"`
    PublicHost    string   `mapstructure:"public_host"`
    InternalAddr  string   `mapstructure:"internal_addr"`
    S3            S3Config `mapstructure:"s3"`
    RefPerVersion bool     `mapstructure:"ref_per_version"`
    GC            GCConfig `mapstructure:"gc"`   // NEW
}

// GCConfig governs the cache auto-clean background sweeper.
type GCConfig struct {
    Enabled               bool          `mapstructure:"enabled"`
    MaxAge                time.Duration `mapstructure:"max_age"`
    Schedule              time.Duration `mapstructure:"schedule"`
    MinRefsToKeep         int           `mapstructure:"min_refs_to_keep"`
    ProtectActiveVersions bool          `mapstructure:"protect_active_versions"`
}
```

### TS — `ui/src/api/types.ts` (additions)

```ts
export interface CacheVersionRef {
  version: string
  tag: string
  ref: string
  size: number
  layer_count: number
  digest: string
  protected: boolean
  last_used_at?: string
}

export interface GCRunSummary {
  started_at: string
  finished_at: string
  purged_tags: number
  freed_bytes: number
  skipped: number
  errors: number
  message?: string
}

export interface GCRules {
  enabled: boolean
  max_age: string
  schedule: string
  min_refs_to_keep: number
  protect_active_versions: boolean
  last_run_at?: string
  last_run_summary?: GCRunSummary
  next_run_at?: string
}

export interface CacheInfo {
  backend: string
  registry: string
  running: boolean
  reachable: boolean
  total_size: number
  object_count: number
  versions: CacheVersionRef[]
  hit_rate: number | null
  hit_count: number
  miss_count: number
  collected_at: string
  message?: string
  gc: GCRules
}

export interface PurgeRequest {
  version: string
  tag?: string
}

export interface PurgeResult {
  purged: number
  freed_bytes: number
  versions: string[]
  already_purged: number
  message?: string
}

export type ServiceState = 'ok' | 'degraded' | 'down' | 'unknown'

export interface ServiceStatus {
  name: string
  category: string
  state: ServiceState
  message?: string
  configured: boolean
  checked_at: string
}

export interface PlatformStatus {
  state: ServiceState
  services: ServiceStatus[]
  checked_at: string
}
```

---

## 3. Function / method signatures

### `internal/repository/registry_client.go` (new)

```go
package repository

type RegistryStatsClient struct {
    host       string        // e.g. "localhost:5000" (cache.internal_addr) or derived from cache.registry
    httpClient *http.Client // 10s timeout
}

func NewRegistryStatsClient(host string) *RegistryStatsClient

// Catalog returns the list of repositories. Returns ErrRegistryCatalogDisabled
// on 404/403, ErrRegistryUnreachable on transport error.
func (c *RegistryStatsClient) Catalog(ctx context.Context) ([]string, error)

// Tags returns the tags for a repository.
func (c *RegistryStatsClient) Tags(ctx context.Context, repo string) ([]string, error)

// ManifestSize fetches the manifest for repo:tag and returns (digest, sizeBytes, layerCount).
// sizeBytes is the sum of layer + config descriptor sizes. Returns ErrManifestNotFound on 404.
func (c *RegistryStatsClient) ManifestSize(ctx context.Context, repo, tag string) (digest string, size int64, layers int64, err error)

// DeleteManifest deletes a manifest by digest. Returns ErrRegistryDeleteDisabled on 405/403.
func (c *RegistryStatsClient) DeleteManifest(ctx context.Context, repo, digest string) error

// Ping probes registry reachability (GET /v2/). Returns nil if reachable.
func (c *RegistryStatsClient) Ping(ctx context.Context) error
```

Sentinel errors (defined in `registry_client.go`, wrapped with `%w`):
```go
var ErrRegistryUnreachable     = errors.New("registry unreachable")
var ErrRegistryCatalogDisabled = errors.New("registry catalog disabled")
var ErrManifestNotFound        = errors.New("manifest not found")
var ErrRegistryDeleteDisabled = errors.New("registry delete not enabled")
```

### `internal/repository/metrics_store.go` (addition — optional helper)

```go
// CacheHitRate queries VictoriaMetrics for cache hit/miss counters over the last
// window. Returns (hit, miss, err); err non-nil when victoria unconfigured or no data.
// PromQL (best-effort; metric names emitted by BuildKit/engine):
//   sum(increase(buildkit_cache_hits_total[5m])) / sum(increase(buildkit_cache_requests_total[5m]))
func (c *MetricsClient) CacheHitRate(ctx context.Context) (hit, miss float64, err error)
```
If the BuildKit metric names are uncertain, the helper returns `(0, 0, ErrNoData)` and the handler surfaces `hit_rate: null`. The exact PromQL is isolated in one constant so it can be tuned post-deployment without touching logic.

### `internal/service/cache_stats.go` (new)

```go
package service

type CacheStatsService struct {
    cache        *Cache                 // existing service.Cache (now with InternalAddr)
    registry     *repository.RegistryStatsClient
    metrics      *repository.MetricsClient // may be nil
    fleet        domain.FleetProvider       // to mark protected versions (may be nil)
    gcCfg        domain.GCConfig
    logger       *logrus.Logger
    metricsObs   *observ.Metrics           // may be nil

    mu           sync.Mutex
    cached       *domain.CacheStats
    cachedAt     time.Time
    cacheTTL     time.Duration             // 15s

    purgeMu      sync.Mutex                // serializes purge / GC
    lastGC       *domain.GCRunSummary
    lastGCAt     time.Time
    nextGCAt     time.Time
}

func NewCacheStatsService(
    cache *Cache,
    registry *repository.RegistryStatsClient,
    metricsClient *repository.MetricsClient,
    fleet domain.FleetProvider,
    gcCfg domain.GCConfig,
    logger *logrus.Logger,
    obs *observ.Metrics,
) *CacheStatsService

// Stats implements domain.CacheStatsProvider. Returns cached payload when fresh,
// else re-probes (registry catalog + manifests + metrics) with a 30s overall budget.
func (s *CacheStatsService) Stats(ctx context.Context) (*domain.CacheStats, error)

// GCRules implements domain.CacheStatsProvider.
func (s *CacheStatsService) GCRules() domain.GCRules

// Purge implements domain.CachePurger. Validates version, derives tag, deletes manifest.
// Idempotent: missing tag → AlreadyPurged++ and no error.
func (s *CacheStatsService) Purge(ctx context.Context, req domain.PurgeRequest) (*domain.PurgeResult, error)

// PurgeAll implements domain.CachePurger. Purges every tag in every catalog repo.
func (s *CacheStatsService) PurgeAll(ctx context.Context) (*domain.PurgeResult, error)

// RunGC is the sweeper entry point (called by the ticker in main.go). Eligible:
// tags older than gcCfg.MaxAge, not protected (active fleet version when
// ProtectActiveVersions), keeping at least MinRefsToKeep most-recent per version.
func (s *CacheStatsService) RunGC(ctx context.Context) (*domain.GCRunSummary, error)

// StartGCSweeper launches the background ticker goroutine; returns stop func.
// Used by main.go. No-op when gcCfg.Enabled == false.
func (s *CacheStatsService) StartGCSweeper(ctx context.Context) (stop func())
```

### `internal/service/status.go` (new)

```go
package service

type StatusService struct {
    cfg         *domain.Config
    cache        *Cache
    registry     *repository.RegistryStatsClient // may be nil (s3 backend)
    fleetManager *Manager                        // may be nil
    logger       *logrus.Logger
}

func NewStatusService(cfg *domain.Config, cache *Cache, registry *repository.RegistryStatsClient, fleet *Manager, logger *logrus.Logger) *StatusService

// Status implements domain.StatusProvider. Probes each service with a 5s per-probe
// timeout, returns aggregated PlatformStatus. Never returns an error unless context
// cancelled (always returns a status payload so the UI can render).
func (s *StatusService) Status(ctx context.Context) (*domain.PlatformStatus, error)
```

### `internal/handler/cache.go` (new) and `internal/handler/status.go` (new)

```go
// cache.go
func (s *Server) handleCacheInfo(ctx context.Context, c *app.RequestContext)   // GET /api/v1/cache  (requireAuth)
func (s *Server) handleCachePurge(ctx context.Context, c *app.RequestContext)  // POST /api/v1/cache/purge      (adminOnly)
func (s *Server) handleCachePurgeAll(ctx context.Context, c *app.RequestContext)// POST /api/v1/cache/purge-all  (adminOnly)

// status.go
func (s *Server) handlePlatformStatus(ctx context.Context, c *app.RequestContext) // GET /api/v1/status (requireAuth)
func (s *Server) handleHealthz(ctx context.Context, c *app.RequestContext)        // upgraded: real aggregation (no auth — kube probe)
func (s *Server) handleReadyz(ctx context.Context, c *app.RequestContext)          // upgraded: real aggregation (no auth — kube probe)
```

### `internal/handler/server.go` (Deps additions)

```go
type Deps struct {
    // ... existing fields ...
    CacheStatsProvider domain.CacheStatsProvider
    CachePurger        domain.CachePurger
    StatusProvider     domain.StatusProvider
    MetricsClient       *repository.MetricsClient // for direct PromQL proxy already exists; reused
}
```
`Server` struct gains corresponding fields. `NewServer` copies them.

### Config defaults (`config/loader.go` additions)

```go
v.SetDefault("cache.gc.enabled", false)
v.SetDefault("cache.gc.max_age", "168h")          // 7d
v.SetDefault("cache.gc.schedule", "1h")
v.SetDefault("cache.gc.min_refs_to_keep", 3)
v.SetDefault("cache.gc.protect_active_versions", true)
```

---

## 4. HTTP API contract

All JSON errors reuse `ErrorResponse{Message string}` via `writeError(c, status, msg)`.

| Method | Route | Auth | Request body | Success response | Status codes |
|---|---|---|---|---|---|
| GET | `/api/v1/cache` | `requireAuth` | — | `domain.CacheStats` | 200; 500 (probe failure still returns 200 with `running:false` — only 500 on internal error) |
| POST | `/api/v1/cache/purge` | `adminOnly` | `PurgeRequest{version, tag?}` | `PurgeResult` | 200; 400 (invalid version); 404 (tag not found & not already-purged); 409 (registry delete disabled); 500 |
| POST | `/api/v1/cache/purge-all` | `adminOnly` | — | `PurgeResult` | 200; 409 (delete disabled); 500 |
| GET | `/api/v1/status` | `requireAuth` | — | `PlatformStatus` | 200 (always, even when services down) |
| GET | `/healthz` | none | — | `{"state":"ok"}` or `{"state":"degraded"}` | 200 always (liveness) |
| GET | `/readyz` | none | — | `{"state":"ok"}` or `{"state":"down","services":[...]}` | 200 when ok/degraded; 503 when down |

**Notes:**
- `GET /api/v1/cache` returns 200 with `running:false`, `reachable:false`, `total_size:-1`, `object_count:-1`, `message:"registry unreachable"` when the registry is down — the UI renders "cache not running" rather than erroring.
- `POST /api/v1/cache/purge` validation: `version` must parse via `domain.Parse` and be in the version resolver allowlist (reuse `s.versionResolver.IsAllowed`); `tag` optional, defaults to `version.Slug()`. Reject empty/unknown with 400 `writeError(c, StatusBadRequest, "invalid version")`.
- Purge is idempotent: deleting a manifest that is already absent counts as `already_purged++` and returns 200 (not 404). 404 is reserved for "version tag never existed in the catalog".
- Registry delete disabled (HTTP 405/403 from `DELETE`) → 409 `writeError(c, StatusConflict, "registry delete not enabled")`.
- `purge-all` requires admin and is serialized with the same `purgeMu` as `purge` and `RunGC` (no concurrent purge + GC).

---

## 5. Real-time updates design

**Decision: polling at 10s** for both the header global status indicator and the services/status view.

**Justification:**
1. Status probes are expensive (TCP dials to 4+ telemetry endpoints, registry catalog fetch, fleet `AllFleetInfo`). Running them per SSE subscriber would multiply load; a single background prober broadcasting via SSE adds a goroutine + a new hub topic for marginal latency gain.
2. The existing `Runners.vue` already polls `/api/v1/fleet` every 10s and users accept that cadence — consistency matters.
3. SSE (`internal/repository/live_hub.go`) is reserved for truly live data (trace spans/logs) where sub-second updates matter; status changes are slow.
4. The `CacheStatsService` already TTL-caches stats at 15s, so a 10s poll is cheap (cache hit most of the time).

**Backend fan-out:** none required. No new SSE endpoint. A future `GET /api/v1/status/live` SSE endpoint driven by a background prober is documented in ADR-012 as out-of-scope.

**Frontend:** `ui/src/stores/status.ts` (Pinia) polls `fetchPlatformStatus()` every 10s via `setInterval`, started in `App.vue` `onMounted` (only when authenticated), cleared on `onUnmounted`/logout. Exposes `state` (rollup) + `services` + `lastError`. The `StatusIndicator` component reads `state`; `Services.vue` reads `services`.

---

## 6. Cache size & object count mechanism

**Registry backend (fully implemented):**
1. `RegistryStatsClient.Ping(ctx)` → `GET http://<host>/v2/` (the distribution base endpoint). Reachable ⇒ `running:true`.
2. `Catalog(ctx)` → `GET /v2/_catalog` → `{"repositories":["dagger-cache"]}`. On 404/403 → `ErrRegistryCatalogDisabled`; stats fall back to `total_size:-1, object_count:-1, message:"catalog disabled"`, but `running:true` (ping succeeded).
3. For each repo, `Tags(ctx, repo)` → `GET /v2/<repo>/tags/list`.
4. For each tag, `ManifestSize(ctx, repo, tag)` → `GET /v2/<repo>/manifests/<tag>` with `Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json`. Parse `layers[]` and `config`, sum `size`, count layers. Returns `(digest, size, layers)`.
5. `total_size` = sum across all tags; `object_count` = sum of layer counts. Per-version `CacheVersionRef` populated from each tag (tag → version via reverse `Slug()` parse: `v0-21-4` → `v0.21.4`).
6. `protected` per version: true when `fleetManager.AllFleetInfo()` returns a `FleetInfo` for that version with `ReadyReplicas > 0` (or `Replicas > 0` when `protect_active_versions`).

**Timeouts:** 10s per HTTP request; overall 30s budget in `Stats()` (context deadline). Partial results returned on timeout (whatever was collected, with `message:"partial: probe timed out"`).

**TTL caching:** `CacheStatsService` caches the full `CacheStats` for 15s (`cacheTTL`). Concurrent `Stats()` calls within the TTL return the cached payload; the first call after expiry re-probes. A background refresh-on-write could be added later; v1 uses simple TTL.

**VictoriaMetrics hit/miss:** `MetricsClient.CacheHitRate(ctx)` runs PromQL when `victoria_url` configured. On no data / unconfigured / query error → `hit_rate: null`, `hit_count:0`, `miss_count:0` (graceful). Division-by-zero guarded: when `hit+miss == 0`, `hit_rate = null`.

**S3 backend (v1: unsupported):** No S3 SDK in `go.mod`; AGENTS.md forbids deviating from the required library list. `Stats()` returns `running: true` when `S3.Bucket != ""`, `total_size:-1, object_count:-1, versions:[], message:"s3 cache stats not supported in this release"`. Documented in ADR-012 as a known limitation; future work would add the AWS SDK v2 minimal.

**Registry unreachable:** `Ping()` transport error ⇒ `running:false, reachable:false`, stats fields `-1`, `message:"registry unreachable"`. UI shows "Cache not running". HTTP still 200.

---

## 7. Purge / clean endpoint design

**`POST /api/v1/cache/purge`** (admin-only):
- Body: `{"version":"v0.21.4","tag":"v0-21-4"}` (`tag` optional).
- Validation:
  - `version` non-empty, parses via `domain.Parse`, and `s.versionResolver.IsAllowed(parsed)` — else 400 `"invalid version"`.
  - `tag` optional; when empty defaults to `parsed.Slug()`. When provided, must match `^[A-Za-z0-9._-]{1,128}$` — else 400 `"invalid tag"`.
- Flow:
  1. Acquire `purgeMu` (serializes with `PurgeAll` and `RunGC`).
  2. `ManifestSize(repo, tag)` to get the digest. On `ErrManifestNotFound` → 404 `"tag not found"`.
  3. `DeleteManifest(repo, digest)`. On `ErrRegistryDeleteDisabled` → 409 `"registry delete not enabled"`.
  4. On success: invalidate the stats cache (`s.cached = nil`), increment `metrics.CachePurgeTotal`, return `PurgeResult{purged:1, versions:[version], freed_bytes:<size>}`.
- Idempotency: a second purge of the same tag → `ManifestSize` returns `ErrManifestNotFound` → counted as `already_purged:1`, returns 200 (not 404). This makes retries safe.
- Concurrency: `purgeMu` ensures no two purges or purge+GC run concurrently.

**`POST /api/v1/cache/purge-all`** (admin-only):
- No body. Iterates all repos × tags from the catalog, deletes each manifest. Aggregates `purged`, `already_purged`, `freed_bytes`, `errors`. Protected versions are **not** skipped on explicit purge-all (admin intent is explicit) — but a `?protect=true` query param can opt into skipping protected versions (default `false`). Returns 409 if the registry has delete disabled (aborts before deleting anything).
- Bounds: cap at 1000 tags to avoid unbounded work; remaining tags require a follow-up call. Returns `message:"truncated at 1000 tags"` when hit.

**Purge during GC:** `purgeMu` is shared, so a manual purge blocks GC and vice-versa. No partial state.

---

## 8. Auto-clean (GC) design

**Config keys** (defaults in `config/loader.go`):
| Key | Default | Meaning |
|---|---|---|
| `cache.gc.enabled` | `false` | Master switch. |
| `cache.gc.max_age` | `168h` (7d) | Purge tags whose manifest is older than this. Age source: manifest `created` annotation when present; else version age (time since the version was first seen in the fleet) — fall back to "unknown age ⇒ skip" when neither is available. |
| `cache.gc.schedule` | `1h` | Sweeper ticker interval. |
| `cache.gc.min_refs_to_keep` | `3` | Always keep at least this many most-recent tags per version (by `last_used_at` or catalog order). |
| `cache.gc.protect_active_versions` | `true` | Never purge tags for versions with active fleet replicas (`ReadyReplicas > 0`). |

**Background sweeper:**
- `CacheStatsService.StartGCSweeper(ctx)` launches a goroutine with `time.NewTicker(gcCfg.Schedule)`. On each tick calls `RunGC(ctx)`. No-op when `!gcCfg.Enabled`. Returns a `stop()` func that stops the ticker and cancels the goroutine.
- Started from `cmd/api/main.go` alongside the existing fleet sweep goroutine (line ~237). `stop()` is deferred.
- `RunGC`:
  1. Acquire `purgeMu`.
  2. Fetch catalog + tags + manifest sizes (reuse the stats probe path).
  3. Fetch active versions from `fleetManager.AllFleetInfo()` (best-effort; on error, treat all as protected to be safe).
  4. For each version's tags, sort by recency, drop the newest `min_refs_to_keep`, then for the remainder: skip if `protect_active_versions` and version is active; skip if age < `max_age` (or age unknown); else `DeleteManifest`.
  5. Record `GCRunSummary{started_at, finished_at, purged_tags, freed_bytes, skipped, errors, message}`; store as `lastGC`, update `lastGCAt`/`nextGCAt = now + schedule`.
  6. Increment `metrics.GCRunTotal` with label `status` = `success`/`error`.
  7. Invalidate stats cache.

**Interaction with fleet `version_retention`:** independent. `fleet.version_retention` governs StatefulSet lifecycle (when to delete a version's STS); `cache.gc` governs cache blob lifecycle. GC protects versions that still have fleet replicas even if `version_retention` would otherwise allow STS deletion — this prevents purging cache for a version an engine pod might still pull. Documented in ADR-012.

**Surfacing to UI:** `CacheStats.gc` (`GCRules`) includes `enabled`, the rules, `last_run_at`, `last_run_summary`, and `next_run_at` (estimated = `lastGCAt + schedule`). The MagicCache view shows "Auto-clean: ON/OFF", the rules table, last run summary, and next estimated run.

---

## 9. "All services running" definition

Services enumerated and probed by `StatusService.Status(ctx)` (5s per-probe timeout via `context.WithTimeout`):

| Service | `name` | `category` | Probe | `configured` |
|---|---|---|---|---|
| Supervisor (control plane) | `supervisor` | `control` | Always `ok` (the process is serving the request). | true |
| Cache / registry backend | `cache` | `cache` | `RegistryStatsClient.Ping(ctx)` when registry backend; `S3.Bucket != ""` check when s3. | `cache.backend != ""` |
| OTel Collector | `collector` | `telemetry` | TCP dial `telemetry.collector_url` host:port (or HTTP GET `/` when URL is http). | `telemetry.collector_url != ""` |
| Tempo | `tempo` | `telemetry` | TCP dial `telemetry.tempo_url`. | `telemetry.tempo_url != ""` |
| Loki | `loki` | `telemetry` | TCP dial `telemetry.loki_url`. | `telemetry.loki_url != ""` |
| VictoriaMetrics | `victoria` | `telemetry` | TCP dial `telemetry.victoria_url`. | `telemetry.victoria_url != ""` |
| Fleet (engine StatefulSets) | `fleet` | `fleet` | `fleetManager.AllFleetInfo()`; `ok` when provider reachable (no error); `degraded` when any version has `ReadyReplicas < Replicas`; `down` when provider error. | always (fleet always configured, may be stub) |

**State semantics:**
- `ok`: probe succeeded (and, for fleet, no degraded replicas).
- `degraded`: fleet has some not-ready replicas but provider reachable; or a telemetry service responded but with a non-200 health endpoint.
- `down`: probe failed (transport error / refused).
- `unknown`: `configured == false` (URL not set) — the service is intentionally absent, not failing.

**Rollup (`PlatformStatus.state`):**
- `down` if any configured service is `down`.
- else `degraded` if any configured service is `degraded`.
- else `ok`.
- Unconfigured services (`unknown`) do not affect the rollup.

**Dedicated view:** new route `/services` → `ui/src/views/Services.vue` (NOT reusing `/cache` — the user asked for a dedicated "services/status" page). The MagicCache page focuses on cache; Services focuses on platform health.

**`/healthz` (liveness):** returns 200 always with `{"state":"ok"|"degraded"}` — kube liveness probe should not restart on a degraded telemetry sidecar. `/readyz` (readiness): returns 200 when rollup is `ok`/`degraded`, 503 when `down` (so kube stops routing traffic when the control plane can't reach its cache). Both call `StatusService.Status` but with a short-circuit: `/healthz` only checks `supervisor` + `cache` (cheap); `/readyz` runs the full probe. To avoid probe storms, `StatusService` caches the last `PlatformStatus` for 5s.

---

## 10. Header global status indicator

**Component:** `ui/src/components/StatusIndicator.vue`, placed in `App.vue` navbar between `.nav-links` and `.nav-user`.

**States & colors:**
- `ok` → green dot + "All systems operational"
- `degraded` → amber dot + "Degraded"
- `down` → red dot + "Service down"
- `unknown`/loading → grey dot + "Checking…"
- Click → navigates to `/services`.

**Store:** `ui/src/stores/status.ts` (Pinia):
```ts
export const useStatusStore = defineStore('status', () => {
  const state = ref<ServiceState>('unknown')
  const services = ref<ServiceStatus[]>([])
  const lastError = ref<string | null>(null)
  const loading = ref(true)
  let timer: number | undefined
  let inFlight = false

  async function refresh(): Promise<void>      // fetchPlatformStatus, guard inFlight
  function start(): void                        // setInterval 10s, called from App.vue onMounted when authed
  function stop(): void                         // clearInterval, called on logout/unmount
  return { state, services, lastError, loading, refresh, start, stop }
})
```
`App.vue` calls `status.start()` in `onMounted` (when `auth.isAuthenticated`) and `status.stop()` in `onUnmounted` + on logout. Refresh interval 10s, matching `Runners.vue`.

---

## 11. Edge cases & error handling

| Case | Handling |
|---|---|
| Registry unreachable | `Stats()` returns `running:false, reachable:false`, sizes `-1`, `message:"registry unreachable"`; HTTP 200; UI shows "Cache not running". |
| Registry catalog disabled (404/403 on `/v2/_catalog`) | `running:true`, sizes `-1`, `message:"catalog disabled"`; versions list empty. |
| Manifest `size` field absent (some registries) | `ManifestSize` falls back to `HEAD /v2/<repo>/blobs/<digest>` and reads `Content-Length`; if that also fails, `size:-1` for that tag, counted in `object_count` but not `total_size`. |
| VictoriaMetrics unconfigured | `CacheHitRate` returns `ErrNoData`; `hit_rate:null`, counts 0. |
| VictoriaMetrics no data (no BuildKit metrics yet) | same as above. |
| No fleet (stub provider / not in k8s) | `AllFleetInfo()` returns `[]` with no error; all versions `protected:false`; fleet service `ok` (provider reachable). If `AllFleetInfo()` errors (k8s down) → fleet service `down`. |
| Empty registry (no tags) | `total_size:0, object_count:0, versions:[]`, `running:true`. |
| Partial service outage | rollup `degraded`; per-service rows show the failing ones. |
| Unauthenticated SSE token expiry | N/A — no SSE for status. Polling uses axios with the existing 401-refresh interceptor. |
| Purge during GC | `purgeMu` serializes; the second caller blocks until the first releases. |
| Purge of protected version (manual) | allowed (admin explicit intent); only GC respects `protect_active_versions`. |
| Division-by-zero on hit-rate | when `hit+miss == 0` → `hit_rate:null`. |
| Stats cache stale during purge | `Purge`/`PurgeAll`/`RunGC` set `s.cached = nil` so the next `Stats()` re-probes. |
| Concurrent `Stats()` calls | `sync.Mutex` around the probe; the second caller waits and returns the freshly computed result (or could return cached — v1 recomputes once, both get the new value). |
| `cache.backend == "s3"` | stats unsupported message; status probe = bucket configured check. |
| `cache.internal_addr` empty | `RegistryStatsClient` derives host from `cache.registry` (strip the repo path after the first `/`). |
| Oversized purge-all | capped at 1000 tags; `message:"truncated"`. |
| GC age unknown | tag skipped, counted in `skipped`. |

---

## 12. Validation

- **Purge payload:** `version` required, parsed + allowlisted; `tag` optional, regex `^[A-Za-z0-9._-]{1,128}$`. Reject with 400 + `ErrorResponse`.
- **Size formatting:** backend always returns raw bytes (int64); the frontend formats with a `formatBytes()` helper (KiB/MiB/GiB). `-1` rendered as "unknown".
- **Date formatting:** all timestamps RFC3339 UTC via the existing `formatTime(t)` helper in `server.go`. Frontend renders with `new Date(t).toLocaleString()`.
- **Bounds on response sizes:** `versions` array capped at 200 entries in `Stats()` (newest first); `message:"truncated"` when exceeded. Purge-all capped at 1000 tags.
- **Version allowlist:** purge reuses `s.versionResolver.IsAllowed` so admins can't purge a version the platform doesn't admit (prevents typos deleting unrelated tags).

---

## 13. Testing strategy

All Go tests: stdlib `testing` only, table-driven, target 100% coverage, loggers to `io.Discard` via `observ.NewTestLogger()`.

### New Go unit tests
| File | Coverage |
|---|---|
| `internal/repository/registry_client_test.go` | `RegistryStatsClient` against `httptest.Server`: catalog ok/disabled(404)/unreachable; tags list; manifest size (with/without layers); delete ok/disabled(405)/not-found; ping ok/fail. Stub registry returns canned OCI manifest JSON. |
| `internal/service/cache_stats_test.go` | `Stats()` cache hit/miss/expiry; registry unreachable → `running:false`; catalog disabled → sizes -1; s3 backend → unsupported message; hit-rate no-data → null; `Purge` validation (bad version, missing tag, delete disabled → 409 sentinel); `PurgeAll` truncation; `RunGC` eligibility (max_age, min_refs, protect_active); `GCRules()` reflects config + last run; `StartGCSweeper` no-op when disabled. Uses stub registry client + stub metrics client + stub fleet provider. |
| `internal/service/status_test.go` | `Status()` rollup (all ok → ok; one down → down; one degraded → degraded); unconfigured service → unknown (no rollup impact); fleet stub with not-ready replicas → degraded; cache s3 backend; timeout per probe. |
| `internal/handler/cache_test.go` | `GET /api/v1/cache` auth gating (401 without token, 200 with); shape assertions; `POST /api/v1/cache/purge` admin-only (403 as user, 200 as admin); 400 invalid version; 404 missing tag; 409 delete disabled; `POST /api/v1/cache/purge-all` admin-only. Uses `ut.PerformRequest` + `newTestEnv`. |
| `internal/handler/status_test.go` | `GET /api/v1/status` auth gating; shape; `GET /healthz` returns 200 with state; `GET /readyz` 200 when ok, 503 when down (inject a failing status provider). |
| `config/loader_test.go` | GC defaults present after `Load` with no file; env override `DAGGER_CACHE_CACHE_GC_ENABLED=true`. |

### Integration tests
| File | Coverage |
|---|---|
| `tests/integration/cache_status_test.go` | Boots a real `handler.Server` on a random port with a stub registry (`httptest.Server` returning a catalog + one manifest) + stub fleet; asserts `GET /api/v1/cache` returns the rich shape with `running:true`, `total_size>0`, one version ref; asserts `GET /api/v1/status` returns `state:ok` and the `cache` service `ok`; asserts `POST /api/v1/cache/purge` as admin succeeds against the stub registry (delete enabled) and returns `purged:1`. Mirrors the `TestProvisionEngineWithAPIToken` wiring pattern. |

### Frontend verification
- `cd ui && npm run typecheck` (vue-tsc) passes with new types.
- `npm run build` succeeds.
- Manual: header indicator shows green when backend healthy, amber/red when a service is down (simulate by stopping the stub registry in an integration test or pointing `cache.registry` at an unreachable host); MagicCache page shows stats, GC rules, purge buttons (admin only); Services page lists all services with states; purge button triggers `POST` and refreshes stats.

---

## 14. Docs

| File | Update |
|---|---|
| `docs/design/ADR-012-magiccache-dashboard.md` (NEW) | Context, decision: layered cache stats via OCI catalog + VictoriaMetrics + status aggregation + GC sweeper + admin-gated purge; polling (not SSE) for status; s3 stats unsupported in v1; registry delete must be enabled for purge; GC protects active fleet versions. Consequences. |
| `docs/design/index.md` | Add ADR-012 row to the table. |
| `docs/README.md` | Update the "Pipeline UI" / "Cache status" section: describe the MagicCache dashboard (size, objects, per-version refs, hit rate, GC rules, purge), the Services status page (`/services`), and the header status indicator. Document `cache.gc.*` config. |
| `config/config.app.yaml.sample` | Add `cache.gc:` block under `cache:` with all 5 keys + comments (per AGENTS.md sync rule). |
| `config/config.app.yaml` | Mirror the sample's `cache.gc` block. |

---

## 15. Build / verify commands

```bash
# Backend
go build ./...
go test ./...
go vet ./...

# Integration tests (black-box)
go test ./tests/integration/...

# Frontend
cd ui
npm ci            # or npm install
npm run typecheck # vue-tsc --noEmit
npm run build     # vite build → ui/dist/

# Embed UI into the Go binary (required for //go:embed all:ui-dist)
cd /projects/dagger-cache
rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist
go build ./...

# Full CI (optional, via the local Dagger module)
dagger call -m ./dagger --src . ci export --path out
dagger call -m ./dagger --src . ui export --path ui-dist
```

**UI embed note:** `internal/handler/ui.go` uses `//go:embed all:ui-dist`. The `internal/handler/ui-dist/` directory is tracked in git (pre-built). After any frontend change, `ui/dist` must be copied into `internal/handler/ui-dist/` before `go build` (the Dockerfile does this in the `ui-builder`→`go-builder` COPY step). The `.gitignore` only ignores the root `/ui-dist/`, not `internal/handler/ui-dist/`.

---

## 16. Phased implementation order

1. **Domain types** — `internal/domain/cache.go` (stats/purge/GC structs + interfaces), `internal/domain/status.go`, `internal/domain/config.go` (`GCConfig`). `go build ./internal/domain`.
2. **Config** — `config/loader.go` defaults + `config/config.app.yaml(.sample)` + `config/loader_test.go`. `go test ./config/`.
3. **Registry client** — `internal/repository/registry_client.go` + test. `go test ./internal/repository/`.
4. **Cache stats service** — `internal/service/cache_stats.go` (+ `InternalAddr` on `service.Cache`) + test. `go test ./internal/service/`.
5. **Status service** — `internal/service/status.go` + test. `go test ./internal/service/`.
6. **Handlers** — `internal/handler/cache.go`, `internal/handler/status.go`; update `server.go` (`Deps`, `Server` fields, routes, remove inline `handleCacheInfo`); update `server_test.go`/`test_helper_test.go`. `go test ./internal/handler/`.
7. **main.go wiring** — construct `RegistryStatsClient`, `MetricsClient`, `CacheStatsService`, `StatusService`; inject into `Deps`; start `StartGCSweeper`. `go build ./cmd/api`.
8. **observ metrics** — add 4 collectors; register in `NewMetrics`. `go test ./internal/observ/`.
9. **Frontend types + client** — `ui/src/api/types.ts`, `ui/src/api/client.ts`. `npm run typecheck`.
10. **Status store + indicator** — `ui/src/stores/status.ts`, `ui/src/components/StatusIndicator.vue`, wire into `App.vue`. `npm run typecheck`.
11. **MagicCache.vue rewrite** — stats, per-version table, GC rules card, purge buttons (admin-gated via `auth.isAdmin`). `npm run typecheck`.
12. **Services.vue + route** — `ui/src/views/Services.vue`, `ui/src/router/index.ts` (`/services`). `npm run typecheck && npm run build`.
13. **Docs/ADR/config-sample** — ADR-012, index, README, config sample. 
14. **Integration test** — `tests/integration/cache_status_test.go`. `go test ./tests/integration/...`.
15. **Embed + final build** — `cp -r ui/dist internal/handler/ui-dist && go build ./... && go test ./...`.

---

## 17. Risks & unknowns (with mitigations)

| Risk / unknown | Mitigation |
|---|---|
| s3 stats unsupported in v1 | Documented in ADR-012 + UI message. Future: add AWS SDK v2 minimal (requires AGENTS.md library-list amendment). |
| OCI registry must enable delete (`REGISTRY_STORAGE_DELETE_ENABLED=true`) for purge | `DeleteManifest` detects 405/403 → 409 "registry delete not enabled"; UI shows the message. Documented in README. |
| Manifest `size` may be absent on some registries | Fall back to `HEAD` blob `Content-Length`; else `size:-1` for that tag. |
| GC `max_age` relies on manifest/tag timestamps not all registries expose | Fall back to version age (fleet first-seen); else skip with `skipped++`. Conservative — never purges when age unknown. |
| VictoriaMetrics hit/miss metric names uncertain (BuildKit/engine) | `CacheHitRate` returns `ErrNoData` on any failure → `hit_rate:null`; PromQL isolated in one constant for post-deploy tuning. |
| Probe storms on `/healthz`/`/readyz` (kube probes every few s) | `StatusService` caches `PlatformStatus` for 5s; `/healthz` short-circuits to supervisor+cache only. |
| Polling 10s may feel stale for the header indicator | Acceptable (matches Runners.vue); documented future SSE enhancement. |
| `purge-all` could delete active-version cache | Admin-only + explicit; `?protect=true` opt-in to skip protected; default `false` (admin intent is explicit). |
| Concurrent `Stats()` probe thundering herd | `sync.Mutex` + 15s TTL; second caller waits for the first. |
| `cache.internal_addr` empty in dev | `RegistryStatsClient` derives host from `cache.registry` (strip repo path). |
| Frontend bundle grows | Negligible (a few small components). |

---

## 18. Open questions (out of scope for v1)

- SSE push for status (`GET /api/v1/status/live`) — documented as future work in ADR-012.
- s3 backend stats — requires AWS SDK v2 (AGENTS.md amendment needed).
- Per-tag `last_used_at` from BuildKit — requires a metrics join; v1 leaves it empty.
- Manual "run GC now" button — could call `RunGC` via `POST /api/v1/cache/gc` (admin); left out of v1 to keep scope tight, but the service method exists and wiring a handler is trivial.
