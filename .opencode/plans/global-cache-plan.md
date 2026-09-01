# Plan: Single global BuildKit cache (tag `cache`) — REV 1 (GC retained)

> Revision note: the original plan removed GC entirely. This revision KEEPS a
> periodic GC sweeper that prunes cache objects not used for `cache.gc.max_age`
> (X days), per the user's requirement. All other decisions (global tag `cache`,
> removed per-version refs, single purge endpoint, single-ref stats payload,
> connect/env emission, UI fix, docs, dead-symbol list, test plan, CI gate) are
> unchanged except where they touched GC.

## 1. Context and goals

Today the remote BuildKit cache ref is derived from the Dagger engine version:
`type=registry,ref=cache.host/dagger-cache:V0-21-4,mode=max`. Each engine version
writes to its own OCI tag (`v0-21-4`, `v0-20-0`, …), and the platform exposes
per-version cache refs, per-version stats, per-version purge, and a per-version
age-based GC sweeper.

**Goal:** every client, regardless of Dagger CLI/engine version, uses the exact
same cache ref — `cache.host/dagger-cache:cache` (tag is the fixed string
`cache`). BuildKit cache is content-addressed, so cross-version sharing is safe;
Dagger Cloud itself uses a single cache.

**Consequences that drive the rest of this plan:**

- The cache ref is now version-independent. Engine *pinning*
  (`_EXPERIMENTAL_DAGGER_TAG`) is a separate concern and is unchanged.
- There is exactly **one** cache tag. Per-version refs, per-version stats, and
  per-version purge lose their subject matter and are removed. Manual purge
  remains (a single "purge the global cache" action).
- **GC is kept**, re-targeted from per-version age groups to a single-tag
  staleness rule: if the global `cache` tag has not been *used* for
  `cache.gc.max_age`, delete it (the whole cache). Legacy `vX-Y-Z` tags are also
  swept by creation age during migration. See §2 D4.
- S3 backend is unchanged (S3 has no tag concept; GC is registry-only).

## 2. Design decisions (authoritative)

### D1 — Global tag is a constant, not config
`cacheTag = "cache"` (unexported `const` in `internal/service/cache.go`). No new
config key. The feature spec fixes the tag to exactly `cache`; a config knob
would add surface area for no benefit.

### D2 — Ref formats
- Registry: `type=registry,ref=<host>/<repo>:cache,mode=max`, where
  `<host>/<repo>` is the existing host/repo with the `PublicHost` rewrite
  already implemented in `Cache.BuildCacheConfig` (unchanged logic).
  - `PublicHost == ""` → `<registry>` (e.g. `cache.reg/dagger-cache:cache`)
  - `PublicHost != ""` → `<publicHost>/<repo>` (e.g. `cache.supv.example.com/dagger-cache:cache`)
- S3: `type=s3,bucket=<bucket>,region=<region>,mode=max` — **unchanged** (no tag).

### D3 — Keep or remove version cache code
Remove all per-version cache code (see §5 dead-symbol list). Engine-version
logic that is NOT cache-specific stays:
- `domain.Version`, `Parse`, `Compare`, `MinorKey`, `String`, `IsFullVersion` —
  **KEEP** (version resolver, engine fleet naming, `_EXPERIMENTAL_DAGGER_TAG`).
- `domain.VersionSlug(string)` (in `domain/fleet.go`) — **KEEP** (fleet
  StatefulSet naming, unrelated to cache).
- `domain.Version.Slug()` (method) and `CacheRefTag()` — **REMOVE** (only used
  by the version-tagged cache path).

### D4 — GC semantics: kept, re-targeted to last-used staleness
With a single content-addressed tag there are no per-version tags to group, but
the user requires GC to keep pruning unused cache data. The sweeper stays, with
these revised semantics:

- **Registry API reality (binding):** the OCI Distribution v2 client
  (`repository.RegistryStatsClient` / `domain.RegistryClient`) can only
  `Catalog`, `Tags`, `ManifestSize`, `ManifestCreated` (the
  `org.opencontainers.image.created` annotation), `ProbeManifest`, `ProbeBlob`,
  and `DeleteManifest(by digest)`. There is **no blob-delete endpoint** and
  **no way to enumerate untagged/orphaned manifests** (`tags/list` lists tags
  only). Therefore GC is **tag-level only**: it deletes the manifest a tag
  points to (which drops the tag). **Object-level pruning of orphaned blobs is
  NOT implementable** with the platform's registry APIs; reclaiming those blobs
  remains the operator's job via the registry's own `garbage-collect` job.
- **"Last used" signal (chosen):** the single `cache` tag maps to one routing
  table row `(repo="dagger-cache", tag="cache")`. Its `LastSeenAt` becomes the
  authoritative "last used" timestamp and is updated on **every manifest pull
  and push** through the proxy (a small new touch — see §3). Fallback order:
  1. routing-table `LastSeenAt` (touched on pull+push);
  2. manifest creation annotation (`ManifestCreated`);
  3. zero time → **unknown**.
  A route row's `LastSeenAt` is always ≥ the manifest's creation time for the
  live tag (a push records both; a pull touches only `LastSeenAt`), so the
  precedence is correct.
- **Rule:** an entry is purged when `now - lastUsed > cache.gc.max_age`.
  - `cache` tag: judged by `lastUsed` (routing `LastSeenAt` → created).
  - Legacy `vX-Y-Z` tags (pre-migration, never written by new clients): judged
    by the same precedence — a stale pre-migration route row, else creation
    time — so they are swept automatically after `max_age` too.
- **Never-observed edge case:** when both signals are unknown (no route row AND
  no creation annotation), GC **skips** (never deletes). Justification: matches
  the existing sweeper's "unknown age → never purge (conservative)" rule and
  avoids deleting a freshly-created cache on first deployment.
- **`min_refs_to_keep` and `protect_active_versions` are removed** — both are
  per-version concepts with no meaning for a single global tag. The `fleet`
  dependency of `CacheStatsService` is removed with them.
- Deleting the stale `cache` tag deletes the entire cache (it is the only live
  tag), which is exactly the requested "if not used for X days, delete it".
- Metrics/error handling are identical in spirit to the current sweeper:
  `GCRunTotal{status}` counter, `CachePurgeTotal` per deleted tag,
  `ErrRegistryDeleteDisabled` aborts the run with an error, catalog-disabled
  backends are skipped with a warn log, per-entry delete errors increment
  `summary.Errors`, and a run summary is recorded on every tick (success or
  error) with `last_run_at`/`next_run_at` derived from `schedule`.

### D5 — Purge semantics: single action
One admin endpoint `POST /api/v1/cache/purge` (replaces both `…/purge` and
`…/purge-all`). It purges **every tag in the `dagger-cache` repo** (the global
`cache` tag plus any pre-migration legacy version tags), capped at 1000 tags —
exactly the behavior of the current `PurgeAll`. `PurgeRequest` is removed (no
body). `PurgeResult` drops `versions`, gains `tags`.

### D6 — Stats payload: single ref + GC block
`CacheStats.Versions []CacheVersionRef` → `CacheStats.Ref *CacheRef` (single,
nullable). `total_size`/`object_count` continue to sum across **all** discovered
manifests (registry footprint, incl. any legacy tags). The `GC` block is **kept**
(with the revised `GCRules` fields), because GC still exists.

### D7 — Connect/env
`_EXPERIMENTAL_DAGGER_CACHE_CONFIG` is emitted **always** (registry and s3),
now version-independent, with the global ref. `_EXPERIMENTAL_DAGGER_TAG` is
still emitted **only** when the user pins a version (engine pinning unchanged).

### D8 — Cache proxy
No routing changes to the OCI path (tag `cache` is a valid single path segment).
Two additive changes:
- `routeCacheManifest` classifies GET/HEAD manifests too (so the pull post-process
  can touch `LastSeenAt`);
- on a successful manifest GET/HEAD the handler calls the new
  `TouchManifest(repo, tag)` (see §3) to keep the GC "last used" signal accurate.
The purge endpoint handler changes (§4).

### D9 — UI/dashboard
Connect page is already generic (renders `env_vars` from the server) — no source
change needed; the new value flows through. MagicCache page is rewritten to show
one global ref and a single "Purge cache" action, and the **GC card is kept**
(revised: shows `enabled`, `max_age`, `schedule`, `last_run_at`, `next_run_at`,
and the last-run summary — no `min_refs_to_keep` / `protect_active_versions`
rows). The committed embedded copy `internal/handler/ui-dist/` must be
regenerated (§4).

### D10 — Docs
New ADR-028 documents the change (global tag + retained last-used GC). ADR-006,
ADR-012, ADR-013, ADR-014 are amended. `docs/README.md`, config sample/actual,
and Helm chart values/templates updated. `DAGGER.md` is **not** affected (no
change to `dagger/`, `.github/workflows/`).

## 3. New/updated data structures and signatures (exact)

### `internal/domain/cache.go`
```go
type CacheStats struct {
    Backend     string    `json:"backend"`
    Registry    string    `json:"registry"`
    Running     bool      `json:"running"`
    Reachable   bool      `json:"reachable"`
    TotalSize   int64     `json:"total_size"`
    ObjectCount int64     `json:"object_count"`
    Ref         *CacheRef `json:"ref"` // single global cache ref (registry backend); nil for s3
    HitRate     *float64  `json:"hit_rate"`
    HitCount    int64     `json:"hit_count"`
    MissCount   int64     `json:"miss_count"`
    CollectedAt string    `json:"collected_at"`
    Message     string    `json:"message,omitempty"`
    GC          GCRules   `json:"gc"` // KEPT — revised rules below
}

type CacheRef struct {
    Ref        string `json:"ref"`                  // "<host>/<repo>:cache"
    Tag        string `json:"tag"`                  // "cache"
    Size       int64  `json:"size"`                 // layer+config bytes; -1 unknown
    LayerCount int64  `json:"layer_count"`          // number of layers; -1 unknown
    Digest     string `json:"digest"`               // sha256:...; "" unavailable
    LastUsedAt string `json:"last_used_at,omitempty"` // NOW POPULATED (routing LastSeenAt → created → "")
}

type GCRules struct {
    Enabled        bool          `json:"enabled"`
    MaxAge         string        `json:"max_age"`  // duration string e.g. "168h"
    Schedule       string        `json:"schedule"` // duration string e.g. "1h"
    LastRunAt      string        `json:"last_run_at,omitempty"`      // RFC3339
    LastRunSummary *GCRunSummary `json:"last_run_summary,omitempty"`
    NextRunAt      string        `json:"next_run_at,omitempty"`      // RFC3339 (estimated)
}
// MinRefsToKeep and ProtectActiveVersions are REMOVED.

type GCRunSummary struct { // unchanged
    StartedAt  string `json:"started_at"`
    FinishedAt string `json:"finished_at"`
    PurgedTags int    `json:"purged_tags"`
    FreedBytes int64  `json:"freed_bytes"`
    Skipped    int    `json:"skipped"` // fresh, unknown-age, or missing backend
    Errors     int    `json:"errors"`
    Message    string `json:"message,omitempty"`
}

type PurgeResult struct {
    Purged        int      `json:"purged"`
    FreedBytes    int64    `json:"freed_bytes"`
    AlreadyPurged int      `json:"already_purged"`
    Tags          []string `json:"tags,omitempty"`
    Message       string   `json:"message,omitempty"`
}

type CacheStatsProvider interface {
    Stats(ctx context.Context) (*CacheStats, error)
    GCRules() GCRules // KEPT
}

type CachePurger interface {
    Purge(ctx context.Context) (*PurgeResult, error) // single purge (no body)
}
```
**Delete:** `CacheVersionRef`, `PurgeRequest`. **Keep (revise):** `GCRules`,
`GCRunSummary`, the `GC` field, and `CacheStatsProvider.GCRules()`.

### `internal/domain/config.go`
```go
type GCConfig struct {
    Enabled  bool          `mapstructure:"enabled"`
    MaxAge   time.Duration `mapstructure:"max_age"`
    Schedule time.Duration `mapstructure:"schedule"`
}
```
**Delete** the `MinRefsToKeep` and `ProtectActiveVersions` fields. The
`GC GCConfig` field on `CacheConfig` is **kept**.

### `internal/domain/registry.go`
Add to `CacheRoutesStore`:
```go
TouchManifest(ctx context.Context, repo, tag string) error
```

### `internal/service/cache.go`
```go
const cacheTag = "cache"

type Cache struct {
    Type       string
    Registry   string
    PublicHost string
    S3         domain.S3Ref
}

var _ domain.CacheBackend = (*Cache)(nil)

func (b *Cache) BackendType() string  { return b.Type }
func (b *Cache) RegistryHost() string { return b.Registry }

// CacheRef returns "<host>/<repo>:cache", rewriting the host to PublicHost
// when set (mirrors the old BuildCacheConfig host rewrite).
func (b *Cache) CacheRef() string

// BuildCacheConfig no longer takes a *domain.Version.
func (b *Cache) BuildCacheConfig(mode string) string
```
**Delete:** `CacheRefForVersion`.

### `internal/service/cache_stats.go`
```go
const (
    cacheStatsTTL    = 15 * time.Second
    cacheProbeBudget = 30 * time.Second
    maxPurgeAllTags  = 1000

    s3UnsupportedMessage = "s3 cache stats not supported in this release"
    registryDownMessage  = "registry unreachable"
    catalogDisabledMsg   = "catalog disabled"
)

type CacheStatsService struct {
    cache      *Cache
    router     *RegistryRouter           // may be nil (s3 backend)
    metrics    domain.CacheMetricsClient // may be nil
    gcCfg      domain.GCConfig           // KEPT
    logger     *logrus.Logger
    metricsObs *observ.Metrics           // may be nil

    mu       sync.Mutex
    cached   *domain.CacheStats
    cachedAt time.Time
    purgeMu  sync.Mutex // serializes purge / GC
    gcMu     sync.Mutex // guards lastGC / lastGCAt / nextGCAt
    lastGC   *domain.GCRunSummary
    lastGCAt time.Time
    nextGCAt time.Time
}

func NewCacheStatsService(
    cache *Cache,
    router *RegistryRouter,
    metricsClient domain.CacheMetricsClient,
    gcCfg domain.GCConfig,      // KEPT
    logger *logrus.Logger,
    obs *observ.Metrics,
) *CacheStatsService
```
- **Drop the `fleet domain.FleetProvider` parameter and field** (only
  `protect_active_versions` used it).
- `probe` initializes `Ref: nil`, `GC: s.GCRules()`; after `probeBackends` it
  calls `buildCacheRef(ctx, entries)` and sets `stats.Ref`, `TotalSize`,
  `ObjectCount`. `timedOut` still sets `"partial: probe timed out"`; the
  `truncated` branch is gone.
- `buildCacheRef(ctx context.Context, entries []cacheEntry) (ref *domain.CacheRef, totalSize, objectCount int64)`:
  sums `totalSize`/`objectCount` across all entries; builds the single
  `*CacheRef` from the first entry whose `tag == cacheTag`, with
  `Ref: s.cache.CacheRef()`, `Tag: cacheTag`, `Size`, `LayerCount`, `Digest`,
  and `LastUsedAt: rfc3339(lastUsedAt(ctx, entry))` (empty when unknown).
- `lastUsedAt(ctx context.Context, e cacheEntry) time.Time` — the shared
  staleness signal for both stats and GC:
  1. `s.router.routes.LookupManifest(e.repo, e.tag)`; if `ok` and
     `LastSeenAt` parses as RFC3339, return it;
  2. else `client.ManifestCreated(ctx, e.repo, e.tag)` via
     `s.router.ClientByID(e.backendID)`; return it if non-zero;
  3. else return zero time.
- `Purge(ctx context.Context) (*domain.PurgeResult, error)` — body of the
  current `PurgeAll` (iterate backends, catalog, `purgeBackend`), keeping the
  `maxPurgeAllTags` cap + `"truncated at 1000 tags"` message and the
  `ErrRegistryCatalogDisabled` / `ErrRegistryDeleteDisabled` sentinels.
- `purgeAllBackend` → rename to `purgeBackend` (same behavior).
- **GC (kept, re-targeted):**
  - `GCRules() domain.GCRules` — revised fields; still reads `gcCfg`, and
    `lastGCAt`/`lastGC`/`nextGCAt` under `gcMu`.
  - `RunGC(ctx) (*domain.GCRunSummary, error)` — same outer shape (purgeMu,
    summary, `finish` closure, registry-nil guard, bounded probe context,
    `invalidateCache`), but the version-grouping loop is replaced:
    ```go
    entries := s.gcCollectEntries(probeCtx)
    if err := s.gcSweepEntries(probeCtx, ctx, entries, summary); err != nil {
        return finish("registry delete not enabled", err)
    }
    s.invalidateCache()
    return finish("", nil)
    ```
  - `gcCollectEntries(ctx) []cacheEntry` — **unchanged** (catalog every
    backend, collect tags+manifest metadata, tag `backendID`; skip
    catalog-disabled backends with a warn log).
  - `gcSweepEntries(probeCtx, ctx context.Context, entries []cacheEntry, summary *domain.GCRunSummary) error`
    (replaces `gcProcessGroup`): for each entry:
    ```go
    used := s.lastUsedAt(probeCtx, e)
    if used.IsZero() { summary.Skipped++; continue }        // never observed → keep
    if time.Since(used) < s.gcCfg.MaxAge { summary.Skipped++; continue }
    client, ok := s.router.ClientByID(e.backendID)
    if !ok { summary.Skipped++; continue }
    if err := client.DeleteManifest(probeCtx, e.repo, e.digest); err != nil {
        if errors.Is(err, domain.ErrRegistryDeleteDisabled) { return err }
        if errors.Is(err, domain.ErrManifestNotFound) { summary.Skipped++; continue }
        summary.Errors++; continue
    }
    summary.PurgedTags++; summary.FreedBytes += e.size
    s.deleteManifestRoute(ctx, e.repo, e.tag)
    if s.metricsObs != nil { s.metricsObs.CachePurgeTotal.Inc() }
    ```
    No `sort`/`regexp`/`strings` version logic needed.
  - `recordGC(summary, runErr)` — **unchanged** (stores summary, bumps
    `GCRunTotal{status}`).
  - `StartGCSweeper(ctx) (stop func())` — **unchanged** (respects
    `gcCfg.Enabled` and `gcCfg.Schedule`).

**Delete:** `parseVersionTag`, `activeVersions`, `isProtected`,
`buildVersionRefs`, `gcProcessGroup` (replaced by `gcSweepEntries`), `Purge`
(version variant), `PurgeAll`, and consts `maxCacheVersions`, `truncatedMsg`,
`tagRe`. Remove struct field `fleet`.

### `internal/service/connect_service.go`
- `ConnectEnv` unchanged except:
  - `if cc := s.cache.BuildCacheConfig("max"); cc != ""` (no version arg).
  - Cache env var `Description` → `"Remote shared cache (MagicCache) ref — one global cache shared across all engine versions."`
  - Keep the `_EXPERIMENTAL_DAGGER_TAG` block verbatim (engine pinning).
- **Delete** `defaultVersion()`.

### `internal/observ/metrics.go`
**Keep** `GCRunTotal` (field + registration). Keep `CacheSizeBytes`,
`CacheObjectCount`, `CachePurgeTotal`, `HistoryGCRunTotal`.

### Touch-on-pull (new — feeds the GC "last used" signal)
- `internal/repository/fsm.go`:
  - new command kind `kindTouchManifestRoute`;
  - `cmdTouchManifestRoute{ Repo, Tag, At string }` (RFC3339 `At`);
  - apply case: `s.touchManifestRoute(p.Repo, p.Tag, p.At)` — updates
    `LastSeenAt` on an **existing** route only (no-op when absent, so a touch
    never fabricates a route);
  - `fsmState.touchManifestRoute(repo, tag, at string)`.
- `internal/repository/cache_routes_repo.go`: add
  `func (r *CacheRoutesRepo) TouchManifest(ctx, repo, tag string) error` →
  `applyCtx(ctx, kindTouchManifestRoute, cmdTouchManifestRoute{Repo: repo, Tag: tag, At: nowRFC3339()})`.
- `internal/service/registry_router.go`: add nil-safe
  `func (r *RegistryRouter) TouchManifest(ctx, repo, tag string) error`.
- `internal/handler/server.go`:
  - `routeCacheManifest` returns `routeManifest` for GET/HEAD **and** PUT;
  - `recordManifestRoute` branches: PUT + (201/202) → `RecordManifest` (as
    today); GET/HEAD + 200 → `s.router.TouchManifest(repo, tag)` (best-effort,
    warn-only on error);
  - add `TouchManifest(ctx, repo, tag string) error` to the `cacheRouter`
    interface (and to its test stub).

## 4. Files to modify (exact list)

| File | Action | Change |
|---|---|---|
| `internal/domain/cache.go` | MODIFY | §3 structs; drop `CacheVersionRef`/`PurgeRequest`; keep+revise `GCRules`/`GCRunSummary`/`GC` field/`GCRules()` method. |
| `internal/domain/config.go` | MODIFY | Drop `GCConfig.MinRefsToKeep` + `ProtectActiveVersions`; keep `CacheConfig.GC`. |
| `internal/domain/registry.go` | MODIFY | Add `TouchManifest` to `CacheRoutesStore`. |
| `internal/domain/version.go` | MODIFY | Delete `Slug()` and `CacheRefTag()`. |
| `internal/domain/version_test.go` | MODIFY | Delete `TestVersionSlug`. |
| `internal/service/cache.go` | MODIFY | §3: `cacheTag`, `CacheRef()`, `BuildCacheConfig(mode)`; delete `CacheRefForVersion`. |
| `internal/service/cache_stats.go` | MODIFY | §3: single-ref stats (with `LastUsedAt`) + single `Purge` + re-targeted `RunGC`/`gcSweepEntries`/`lastUsedAt`; delete per-version + fleet symbols. |
| `internal/service/registry_router.go` | MODIFY | Add `TouchManifest`. |
| `internal/service/connect_service.go` | MODIFY | §3: version-independent cache config; delete `defaultVersion()`. |
| `internal/repository/fsm.go` | MODIFY | Add `kindTouchManifestRoute` + `cmdTouchManifestRoute` + apply case + `touchManifestRoute`. |
| `internal/repository/cache_routes_repo.go` | MODIFY | Add `TouchManifest`. |
| `internal/observ/metrics.go` | MODIFY | Keep `GCRunTotal` (no change from today). |
| `internal/handler/cache.go` | MODIFY | `handleCachePurge` → no body decode, calls `Purge(ctx)`; delete `handleCachePurgeAll` + `cacheTagRe` + `regexp` import; drop `ErrValidation` branch in `writePurgeError`. |
| `internal/handler/server.go` | MODIFY | Route table: drop `…/purge-all`; keep `POST /api/v1/cache/purge`. Add `TouchManifest` to `cacheRouter`; classify GET/HEAD manifests; touch on pull. |
| `cmd/api/main.go` | MODIFY | `NewCacheStatsService(cacheBackend, router, metricsClient, cfg.Cache.GC, logger, metrics)` (drop `provider`); **keep** `StartGCSweeper` + `stopGC` wiring. |
| `cmd/ci/main.go` | MODIFY | Replace version-tagged cache block with: `_EXPERIMENTAL_DAGGER_TAG` only when `version != ""`; `_EXPERIMENTAL_DAGGER_CACHE_CONFIG=type=registry,ref=<cache-registry>:cache,mode=max` only when `c.IsSet("cache-registry")`; update `--cache-registry` usage text. |
| `config/loader.go` | MODIFY | Delete the `min_refs_to_keep` + `protect_active_versions` `SetDefault` lines; keep `enabled`/`max_age`/`schedule`. |
| `config/config.app.yaml` | MODIFY | `cache.gc:` block (lines 108–113) → keep `enabled`/`max_age`/`schedule`, drop `min_refs_to_keep`/`protect_active_versions`. |
| `config/config.app.yaml.sample` | MODIFY | Update `registry` comment (no longer per-version); `gc:` block → keep 3 keys, drop 2. |
| `deploy/helm/dagger-kubernetes/values.yaml` | MODIFY | Drop `cache.gc.minRefsToKeep` + `cache.gc.protectActiveVersions` @param comments + values (lines 226–236); keep `enabled`/`maxAge`/`schedule` + `history.gc`. |
| `deploy/helm/dagger-kubernetes/templates/configmap.yaml` | MODIFY | `cache.gc` render block (lines 123–130) → drop the two removed keys. |
| `deploy/helm/dagger-kubernetes/README.md` | MODIFY | Regenerate via `scripts/update-helm-docs.sh` (drops the two rows). |
| `scripts/dagger-kubernetes.sh` | MODIFY | Emit `CACHE_REF="${CACHE_REGISTRY}:cache"` + `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` unconditionally; `_EXPERIMENTAL_DAGGER_TAG` only when `DAGGER_TAG` set; delete `VSLUG`; fix auto-discovery comment. |
| `ci-integrations/gha/dagger-kubernetes.sh` | MODIFY | Same as `scripts/dagger-kubernetes.sh`. |
| `ci-integrations/jenkins/vars/daggerKubernetes.groovy` | MODIFY | `if (magicCache)` → `cacheConfig = "type=registry,ref=${cacheRegistry}:cache,mode=max"` (drop `version` requirement + `vslug`). |
| `ci-integrations/drone/config-extension.sh` | NO CHANGE | Already never emits cache config (doc-only). |
| `ui/src/api/types.ts` | MODIFY | §4.1. |
| `ui/src/api/client.ts` | MODIFY | §4.1. |
| `ui/src/magiccache/MagicCache.vue` | MODIFY | §4.1. |
| `internal/handler/ui-dist/**` | MODIFY (regenerate) | Rebuild UI + copy `ui/dist` → `internal/handler/ui-dist` (committed embed source). |
| `internal/service/cache_test.go` | MODIFY | §6. |
| `internal/service/connect_service_test.go` | MODIFY | §6. |
| `internal/service/cache_stats_test.go` | MODIFY | §6 (largest rewrite). |
| `internal/repository/fsm_test.go` | MODIFY | §6 (touch-route cases). |
| `internal/repository/cache_routes_repo_test.go` | MODIFY | §6 (`TouchManifest`). |
| `internal/handler/cache_test.go` | MODIFY | §6. |
| `internal/handler/connect_test.go` | MODIFY | §6. |
| `internal/handler/test_helper_test.go` | MODIFY | §6 (stubs; router stub gains `TouchManifest`). |
| `tests/integration/cache_status_test.go` | MODIFY | §6. |
| `tests/integration/cache_proxy_test.go` | MODIFY | §6 (tag `cache`; routing unchanged). |
| `docs/README.md` | MODIFY | §7. |
| `docs/design/ADR-006-oci-registry-cache-backend.md` | MODIFY (amend) | §7. |
| `docs/design/ADR-012-magiccache-dashboard.md` | MODIFY (amend) | §7. |
| `docs/design/ADR-013-connect-env-menu.md` | MODIFY (amend) | §7. |
| `docs/design/ADR-014-registry-proxy-token-loadbalancing.md` | MODIFY (amend) | §7 (example tag). |
| `docs/design/ADR-028-global-cache.md` | CREATE | §7. |
| `docs/design/index.md` | MODIFY | Add ADR-028 row. |

### 4.1 UI specifics
`ui/src/api/types.ts`:
- Replace `CacheVersionRef` with `CacheRef { ref, tag, size, layer_count, digest, last_used_at? }`.
- `CacheInfo`: `versions: CacheVersionRef[]` → `ref: CacheRef | null`; **keep** `gc: GCRules`.
- **Keep** `GCRunSummary`; **keep+trim** `GCRules` (drop `min_refs_to_keep`,
  `protect_active_versions`). Delete `PurgeRequest`.
- `PurgeResult`: `versions` → `tags: string[]`.

`ui/src/api/client.ts`:
- `purgeCache()` (no arg) → `POST /api/v1/cache/purge`; delete `purgeAllCache`.
- Remove now-unused `PurgeRequest` import.

`ui/src/magiccache/MagicCache.vue`:
- `emptyCache()`: `ref: null`, keep `gc` initializer
  (`{ enabled: false, max_age: '', schedule: '' }`).
- Replace "Cache Versions" table with a "Global cache" card showing `info.ref`
  (ref, tag, size, layers, digest, `last_used_at`) or "No cache yet." when
  `info.ref` is null.
- **Keep** the "Auto-clean (GC)" card, trimmed: show `enabled`, `max_age`,
  `schedule`, `last_run_at`, `next_run_at`, and the `last_run_summary`
  (purged_tags/freed_bytes/skipped/errors). Remove the
  "Keep (most recent per minor)" and "Protect active versions" rows and the
  `min_refs_to_keep`/`protect_active_versions` references in `gcSummary`.
- Admin card: single "Purge cache" button → `purgeCache()`; confirm text
  "Purge the global cache? This removes all cache blobs."; message uses
  `res.tags`/`res.already_purged`.

Rebuild: `cd ui && npm ci && npm run build` then copy `ui/dist/*` into
`internal/handler/ui-dist/` (mirror the Dockerfile `COPY` step). Both `ui/dist`
(build output) and `internal/handler/ui-dist` (committed embed source) must be
committed.

## 5. Dead symbols (golangci `unused` will fail CI if left)

**KEEP (revise, do not delete):**
- `domain.GCRules` (drop `MinRefsToKeep`, `ProtectActiveVersions`)
- `domain.GCRunSummary` (unchanged)
- `domain.GCConfig` (drop `MinRefsToKeep`, `ProtectActiveVersions`)
- `domain.CacheStats.GC` field; `domain.CacheStatsProvider.GCRules()`
- `service.CacheStatsService.RunGC`, `StartGCSweeper`, `GCRules()`, `recordGC`,
  `gcCollectEntries`, `gcMu`/`lastGC`/`lastGCAt`/`nextGCAt`
- `observ.Metrics.GCRunTotal` (field + registration)
- `service.cacheEntry`

**DELETE:**
- `domain.Version.Slug()` and `domain.Version.CacheRefTag()`
- `service.Cache.CacheRefForVersion()`
- `service.ConnectService.defaultVersion()`
- `service.parseVersionTag()`, `service.buildVersionRefs()`,
  `service.activeVersions()`, `service.isProtected()`
- `service.gcProcessGroup()` (replaced by `gcSweepEntries`)
- `service.Purge` (version variant) and `service.PurgeAll` (replaced by single `Purge(ctx)`)
- `service` consts `maxCacheVersions`, `truncatedMsg`, `tagRe`
- `domain.CacheVersionRef`, `domain.PurgeRequest`
- `handler.cacheTagRe`, `handler.handleCachePurgeAll`
- test-only: `stubFleetProvider` (cache_stats_test.go) once the fleet-param GC
  tests are removed

**RENAME:**
- `service.purgeAllBackend` → `service.purgeBackend`

## 6. Test plan

### Unit tests (change)
- `internal/service/cache_test.go`: replace `TestCacheRefForVersion` with
  `TestCacheRef` (`cache.reg/dagger-cache:cache`, and `PublicHost` rewrite).
  Rewrite `TestBuildCacheConfig*` to drop the version arg; expected
  `type=registry,ref=…:cache,mode=max`. S3 + unknown-backend tests unchanged
  except the call signature.
- `internal/service/connect_service_test.go`: update all
  `registryCache().BuildCacheConfig(<version>, "max")` → `BuildCacheConfig("max")`.
  `TestConnectEnvNoVersionLatestRelease` asserts `_EXPERIMENTAL_DAGGER_TAG`
  empty + cache config equals `registryCache().BuildCacheConfig("max")`.
  Env-var counts stay 4 (no version) / 5 (version pinned).
- `internal/service/cache_stats_test.go`: rewrite.
  - Stats: keep/adapt `TestCacheStatsRegistryOK` (assert `stats.Ref` non-nil,
    `Ref.Tag=="cache"`, `Ref.Ref=="cache.supv.example.com/dagger-cache:cache"`,
    and `stats.GC` rules present), cache-hit/expiry, unreachable,
    catalog-disabled, s3-unsupported, hit-rate, multi-backend, mark-down.
  - Purge: drop version; single `Purge(ctx)`. `TestPurgeAll`→`TestPurge`,
    `TestPurgeAllTruncated`→`TestPurgeTruncated`,
    `TestPurgeAllCatalogDisabled`→`TestPurgeCatalogDisabled`,
    `TestPurgeRegistryNil`. Delete `TestPurgeInvalidVersion`/`TestPurgeInvalidTag`.
  - GC (re-written for last-used semantics):
    - `TestRunGCPurgesStaleCacheTag` — route `LastSeenAt` older than `max_age`
      → the `cache` tag's manifest is deleted; `PurgedTags==1`, `FreedBytes` set.
    - `TestRunGCKeepsFreshCacheTag` — recent `LastSeenAt` → skipped, not deleted.
    - `TestRunGCCreationFallback` — no route row, stale creation annotation →
      deleted (fallback path).
    - `TestRunGCPurgesLegacyTagsByCreation` — legacy `v0-21-4` with stale
      creation annotation and no route → deleted.
    - `TestRunGCNeverObservedSkips` — no route row AND no creation annotation →
      `PurgedTags==0`, `Skipped>=1`.
    - `TestGCRulesReflectConfigAndLastRun` — revised fields (assert
      `MinRefsToKeep`/`ProtectActiveVersions` are gone; `max_age`/`schedule`/
      `last_run_at`/`next_run_at` present).
    - `TestStartGCSweeperDisabled` / `TestStartGCSweeperEnabled` — keep.
    - `TestRunGCRegistryNil` / `TestRunGCDeleteDisabled` — keep.
  - `newStatsService` drops the `fleet` param and the `defaultGC()` helper drops
    the two removed fields. Delete `stubFleetProvider`.
- `internal/repository/fsm_test.go` / `cache_routes_repo_test.go`: add
  `TestTouchManifestRoute` — updates `LastSeenAt`, preserves `CreatedAt`, no-op
  when the route is absent.
- `internal/handler/cache_test.go`: `TestHandleCachePurgeAdminOnly` uses empty
  body (no `version`); delete `TestHandleCachePurgeInvalidVersion`; keep
  delete-disabled (409) test; fold `…PurgeAllAdminOnly` into the single purge
  endpoint test.
- `internal/handler/connect_test.go`: `TestConnectEnvDefaultMasked` →
  `cache.BuildCacheConfig("max")`.
- `internal/handler/test_helper_test.go`: `stubCacheStatsProvider` keeps
  `GCRules()`; `stubCachePurger` becomes single `Purge(ctx)`; the `cacheRouter`
  stub gains `TouchManifest`.

### Integration tests (change)
- `tests/integration/cache_status_test.go`: `registryStub` serves tag `cache`
  (paths `/v2/dagger-cache/manifests/cache`, `/tags/list` → `["cache"]`).
  `TestCacheStatusAndPurgeIntegration`: assert `stats.Ref != nil &&
  stats.Ref.Tag == "cache"` (instead of `Versions`) and `stats.GC` present;
  purge request body becomes empty (`POST /api/v1/cache/purge`);
  `newCacheStatusTestEnv` wires
  `NewCacheStatsService(cacheBackend, router, nil, gcCfg, logger, metrics)`
  (no provider; `gcCfg` from defaults).
- `tests/integration/cache_proxy_test.go`: change manifest push/pull paths from
  `manifests/v0-21-4` to `manifests/cache`. Routing assertions unchanged
  (proxy is tag-agnostic). Add one assertion that a manifest GET/HEAD touch
  does not break routing (route still resolves afterward).

### CI gate (must stay green)
Full gate (Docker daemon available):
```bash
dagger call -m ./dagger --src . ci export --path out
```
Minimum when no Docker daemon:
```bash
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
```
Note: `golangci-lint` (effective latest) runs `unused` — the §5 deletions are
mandatory; the kept GC symbols must retain call sites (they do: `RunGC`,
`StartGCSweeper`, `GCRules`, `recordGC`, `gcCollectEntries`, `gcSweepEntries`,
`GCRunTotal`, `GCRunSummary`, `GCConfig`, `GCRules`). `go test -race` in the
Dagger `Test` step runs the rewritten tests.

## 7. Docs updates

### `docs/README.md`
- §Client setup (~L194–200): drop "always tagged per engine version"; example →
  `export _EXPERIMENTAL_DAGGER_CACHE_CONFIG="type=registry,ref=cache.supv.example.com/dagger-cache:cache,mode=max"`.
- §Connect page (~L216–220): cache env var is the single global ref (not
  version-targeted); `_EXPERIMENTAL_DAGGER_TAG` still pins the engine.
- Config table (~L430): `registry` description → "…always tagged `:cache` (single global cache)". `gc.*` rows (L437–441) → drop `min_refs_to_keep`/`protect_active_versions`, keep `enabled`/`max_age`/`schedule` with updated descriptions.
- §Remote shared cache (~L604–622): `:cache` tag; "one global cache shared across
  all engine versions"; remove the version-slug derivation paragraph.
- §Cache auto-clean GC (~L682–699): rewrite — GC is a tag-level staleness
  sweeper: the global `cache` tag is deleted when not *used* (pulled or pushed)
  for `cache.gc.max_age`; "used" is the supervisor's own last-seen observation
  (falling back to manifest creation time). Legacy `vX-Y-Z` tags are swept by
  creation age. Never-observed tags are never deleted. Orphaned blob
  reclamation is NOT done by the supervisor — run the registry's
  `garbage-collect` job (delete must be enabled).
- §Purging cache (~L701–708): single `POST /api/v1/cache/purge` (admin).
- §MagicCache feature list (~L1405): "single global cache ref" instead of
  "per-version cache refs".
- §Connect page feature list (~L1415–1417): cache config is the global ref.
- §GHA magic cache (~L1478–1505): no `version` requirement; `:cache`.
- §Jenkins magic cache (~L1529–1559): `magicCache` no longer requires `version`;
  `:cache`.
- §Drone magic cache (~L1749–1775): `:cache`; delete "tag must match version
  slug" line.
- §Client wrapper script (~L1828–1832): derives `:cache` (not from `DAGGER_TAG`).

### ADRs
- **NEW `docs/design/ADR-028-global-cache.md`**: decision + rationale
  (content-addressed, cross-version safe, matches Dagger Cloud), removal of
  per-version refs/stats/purge, **retention of a re-targeted last-used GC**,
  purge semantics, migration note (legacy `vX-Y-Z` tags remain but are
  ignored/overwritten by new clients; purge clears them and GC sweeps them by
  creation age).
- **ADR-006**: strike "version-tagged refs" + `cache.ref_per_version`; state the
  single global `:cache` tag.
- **ADR-012**: per-version refs/protect-active-versions are superseded by the
  single global cache; stats now emit one `ref`; GC is now a single-tag
  last-used staleness rule (`min_refs_to_keep`/`protect_active_versions` removed).
- **ADR-013** §5: cache config is version-independent (no "effective version" tag).
- **ADR-014**: update the example ref `:v0-19-0` → `:cache`.
- **`docs/design/index.md`**: add ADR-028 row.

## 8. Migration / rollout

- Existing version-tagged blobs (`v0-21-4`, …) stay in the registry. They are
  ignored by stats (single `cache` tag) and by clients (new `:cache` ref). No
  data migration required.
- **Legacy tag cleanup:** the admin "Purge cache" action deletes legacy version
  tags (it iterates all tags in the repo), and — new in this revision — GC also
  sweeps legacy tags by creation age after `cache.gc.max_age`, so operators
  reclaim old space automatically once GC is enabled.
- Config migration: `cache.gc.min_refs_to_keep` and
  `cache.gc.protect_active_versions` are removed and ignored by the new binary
  (mapstructure drops unknown keys). `cache.gc.enabled`/`max_age`/`schedule`
  are unchanged. Operators should drop the two removed keys from their config;
  no startup failure either way.
- "Last used" warm-up note: on first deploy against a pre-existing registry, the
  routing table is empty, so GC falls back to manifest creation time until the
  first pull/push populates the `cache` route row. No tag is deleted while its
  age is unknown.
- Deployment (per AGENTS.local.md §4–6): rebuild image (includes UI),
  `helm upgrade` (capture values first), `rollout restart`, run §5.1 agent checks
  (`/healthz`, `/readyz`, authed `/api/v1/cache`), then §5.2 human verification of
  the MagicCache + Connect pages.

## 9. Out of scope

- Dagger CLI binary provisioning cache (`cli.cache_repo`, `repository/cli_cache_registry.go`) — separate from BuildKit cache.
- History purge/GC (unchanged).
- S3 cache stats implementation (still "not supported"; S3 has no tag, so GC is registry-only).
- Registry-level blob/GC and orphaned-blob reclamation (documented operator `garbage-collect` job).
- Any change to the cache proxy routing/load-balancing logic (only the additive touch-on-pull + purge endpoint).
- `DAGGER.md` (no `dagger/` or CI-workflow changes).

## 10. Suggested implementation order

1. `domain` (cache.go, config.go, version.go, registry.go) + version_test.go.
2. `service/cache.go`, `cache_stats.go`, `registry_router.go`, `connect_service.go`.
3. `repository` (fsm.go `kindTouchManifestRoute`, cache_routes_repo.go `TouchManifest`).
4. `cmd/api/main.go`, `cmd/ci/main.go`, `config/loader.go`, config YAML.
5. `handler` (cache.go, server.go touch-on-pull) + handler test stubs.
6. Unit tests (service, repository, handler).
7. Helm chart (values.yaml, configmap.yaml, README regen).
8. `ci-integrations/` + `scripts/`.
9. UI (`types.ts`, `client.ts`, `MagicCache.vue`) + rebuild `ui-dist`.
10. Integration tests.
11. Docs (README, ADRs, index).
12. Run CI gate (§6), then redeploy + verify per AGENTS.local.md.
