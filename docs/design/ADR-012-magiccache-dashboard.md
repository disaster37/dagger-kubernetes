# ADR-012: MagicCache dashboard — cache stats, services status, GC, and purge

- **Status:** accepted
- **Date:** 2026-08-14
- **Deciders:** dagger-kubernetes maintainers

## Context

The web UI previously showed only the cache backend name and registry host
(`GET /api/v1/cache` returned a two-field map). Operators had no way to see how
much cache the registry holds, whether the cache backend is reachable, which
engine versions own cache refs, whether the platform services are healthy, or
to reclaim space by deleting stale cache tags. The health endpoints
(`/healthz`, `/readyz`) were static stubs that always returned `200`.

We needed:

1. Rich cache information: size, object count, per-version refs, hit rate, GC
   rules.
2. A dedicated all-services status page and a header status indicator.
3. An admin-gated purge capability and a background auto-clean (GC) sweeper.

## Decision

### 1. Layered cache stats via OCI catalog + VictoriaMetrics

`GET /api/v1/cache` now returns a rich `CacheStats` payload assembled by a new
`CacheStatsService`:

- The OCI Distribution v2 API is probed directly (stdlib `net/http`, no new
  dependency): `GET /v2/` (reachability), `GET /v2/_catalog` (repositories),
  `GET /v2/<repo>/tags/list`, and `GET /v2/<repo>/manifests/<tag>` (layer sizes,
  digest). `total_size` is the sum of layer + config descriptor sizes,
  `object_count` the sum of layer counts.
- BuildKit cache hit/miss counters are queried from VictoriaMetrics via PromQL
  (`buildkit_cache_hits_total` / `buildkit_cache_misses_total`). The PromQL is
  isolated in constants so it can be tuned post-deployment. Any failure yields
  `hit_rate: null` (graceful).
- Per-version `CacheVersionRef`s are derived by reversing the tag slug
  (`v0-21-4` → `v0.21.4`); a version is marked `protected` when the fleet has
  active (ready) replicas for it.
- The full payload is TTL-cached for 15s; concurrent `Stats()` calls share a
  mutex and return the freshly computed result.

### 2. Services status aggregation

A new `StatusService` probes each platform service with a 5s per-probe timeout:
supervisor (always ok), cache (registry ping or s3 bucket check), the four
telemetry backends (TCP dial), and fleet (StatefulSet readiness). It rolls
results into a `PlatformStatus` (`ok`/`degraded`/`down`) with unconfigured
services reported as `unknown` and excluded from the rollup. The result is
cached for 5s to avoid probe storms from kube liveness/readiness probes.

`/healthz` (liveness) returns 200 always, mapping a `down` rollup to
`degraded` so kube never restarts the process over a down sidecar. `/readyz`
(readiness) returns 503 when the rollup is `down`, so kube stops routing
traffic when the cache is unreachable.

### 3. Admin-gated purge + auto-clean (GC) sweeper

`POST /api/v1/cache/purge` (single version) and
`POST /api/v1/cache/purge-all` are admin-only. Purge is idempotent: a missing
tag counts as `already_purged` and returns 200, so retries are safe. The
registry must have delete enabled (`REGISTRY_STORAGE_DELETE_ENABLED=true`);
a 405/403 from `DELETE` maps to 409 "registry delete not enabled".

`cache.gc.*` config governs a background sweeper (`CacheStatsService.RunGC`,
ticker via `StartGCSweeper`): tags older than `max_age` are purged unless the
version has active fleet replicas (`protect_active_versions`), always keeping
the newest `min_refs_to_keep` tags per minor version line. Age comes from the
OCI `org.opencontainers.image.created` annotation; unknown age is never purged
(conservative).

### 4. Polling (not SSE) for status

Status changes are slow and probes are expensive (multiple TCP dials + catalog
fetch). The header indicator and services page poll `/api/v1/status` every 10s,
matching the existing `Runners.vue` fleet cadence. SSE is reserved for truly
live data (trace spans/logs). A future `GET /api/v1/status/live` SSE endpoint
driven by a background prober is documented as out of scope.

### 5. S3 stats unsupported in v1

`cache.backend: "s3"` returns `running:true` (when a bucket is configured) with
`total_size:-1`, `object_count:-1`, and a "s3 cache stats not supported in this
release" message. There is no AWS SDK in `go.mod`, and AGENTS.md forbids
deviating from the required library list. Future work would add the minimal
AWS SDK v2 and amend AGENTS.md.

## Consequences

- Operators can observe and reclaim cache space from the UI without shell
  access to the registry.
- The GC sweeper protects cache refs for versions that still have engine
  replicas, even if `fleet.version_retention` would otherwise allow STS
  deletion — preventing purging cache an engine pod might still pull.

  > **Superseded (2026-08-20):** `fleet.version_retention` was never
  > implemented and the config key has been removed; the GC protection above
  > is unchanged, but the `version_retention` reference is historical only.
- The registry must enable delete for purge/GC; otherwise the UI surfaces a
  clear 409 message and no state changes.
- No new third-party dependencies were introduced (stdlib `net/http` for the
  registry client; existing Hertz/Viper/logrus stack).
