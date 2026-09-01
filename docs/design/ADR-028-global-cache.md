# ADR-028: Single global BuildKit cache (tag `cache`)

**Status:** Accepted  
**Date:** 2026-09-01  
**Supersedes:** ADR-006 §cache.ref_per_version, ADR-012 §per-version refs/stats/purge, ADR-013 §version-tagged cache config, ADR-014 §example ref

## Context

Previously the remote BuildKit cache ref was derived from the Dagger engine
version: `type=registry,ref=cache.host/dagger-cache:V0-21-4,mode=max`. Each
engine version wrote to its own OCI tag, and the platform exposed per-version
cache refs, per-version stats, per-version purge, and a per-version age-based
GC sweeper.

## Decision

Every client, regardless of Dagger CLI/engine version, uses the exact same
cache ref — `cache.host/dagger-cache:cache` (tag is the fixed string `cache`).

### Rationale

- BuildKit cache is content-addressed; cross-version sharing is safe.
- Dagger Cloud itself uses a single cache.
- Per-version refs, stats, and purge lose their subject matter and are removed.
- The cache ref is now version-independent. Engine pinning
  (`_EXPERIMENTAL_DAGGER_TAG`) is a separate concern and is unchanged.

### Consequences

1. **Single cache tag.** The tag is the constant `cache` (unexported `const` in
   `internal/service/cache.go`). No config key.

2. **Removed per-version code.** `CacheVersionRef`, `PurgeRequest`,
   `Version.Slug()`, `Version.CacheRefTag()`, `CacheRefForVersion()`,
   `defaultVersion()`, `parseVersionTag()`, `buildVersionRefs()`,
   `activeVersions()`, `isProtected()`, `gcProcessGroup()`, per-version
   `Purge`, `PurgeAll`, and related constants are deleted.

3. **Single purge endpoint.** `POST /api/v1/cache/purge` (no body) purges
   every tag in the `dagger-cache` repo (the global `cache` tag plus any
   pre-migration legacy version tags), capped at 1000 tags.

4. **Single-ref stats.** `CacheStats.Ref *CacheRef` replaces
   `CacheStats.Versions []CacheVersionRef`. `total_size`/`object_count` still
   sum across all discovered manifests.

5. **GC retained, re-targeted to last-used staleness.** The sweeper stays,
   but judges staleness by the routing-table `LastSeenAt` (touched on every
   manifest pull and push), falling back to manifest creation time. Tags with
   no observation and no creation annotation are never deleted. Legacy
   `vX-Y-Z` tags are swept by creation age. `min_refs_to_keep` and
   `protect_active_versions` are removed.

6. **Connect/env.** `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` is emitted always
   (registry and s3), now version-independent, with the global ref.

7. **UI.** MagicCache page shows one global ref card and a single "Purge
   cache" button. GC card is trimmed (no `min_refs_to_keep` /
   `protect_active_versions` rows).

## Migration

- Existing version-tagged blobs (`v0-21-4`, …) stay in the registry. They are
  ignored by stats (single `cache` tag) and by clients (new `:cache` ref).
  No data migration required.
- Legacy tag cleanup: the admin "Purge cache" action deletes legacy version
  tags, and GC sweeps them by creation age after `cache.gc.max_age`.
- Config migration: `cache.gc.min_refs_to_keep` and
  `cache.gc.protect_active_versions` are removed and ignored by the new binary
  (mapstructure drops unknown keys).
- "Last used" warm-up: on first deploy against a pre-existing registry, the
  routing table is empty, so GC falls back to manifest creation time until the
  first pull/push populates the `cache` route row.
