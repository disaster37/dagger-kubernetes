# ADR-034: Admin image-cache management (list / prune / prune-all)

**Status:** Accepted  
**Date:** 2026-09-15

## Context

With the local image cache deployed as one Zot mirror per upstream
(ADR-033), operators need a first-class way to inspect what is cached and to
evict entries — for example to force a re-fetch of a mutable tag, to reclaim
space, or to clear a poisoned mirror. Before this ADR the platform only had the
per-version **engine local-cache** purge (ADR-032) and the removed remote-cache
admin surface; there was no way to see or manage the image mirrors.

Two reuse opportunities shaped the design:

1. The OCI Distribution v2 client used by the old remote-cache code already
   implements exactly the mirror surface needed (`Ping`, `Catalog`, `Tags`,
   `ManifestSize`, `DeleteManifest`) with the right error mapping
   (catalog-disabled, delete-disabled, manifest-not-found) and CWE-20/CWE-918
   digest/path hardening. Zot serves that same API.
2. The engine local-cache purge (ADR-032) established the **"call the
   subsystem's own API over HTTP/TCP"** principle instead of manipulating its
   storage or workload.

## Decision

### D1 — Reuse the OCI Distribution v2 client

`internal/repository/registry_client.go` is kept and repurposed as the
mirror client: the type is renamed `RegistryStatsClient` → `DistributionClient`
(constructors `NewDistributionClient` / `NewDistributionClientWithAuth`), the
domain interface is `domain.DistributionClient`
(`Host`, `Ping`, `Catalog`, `Tags`, `ManifestSize`, `DeleteManifest`), and the
CLI-cache / router-only methods are deleted. `ManifestSize`'s returned digest is
reused for the prune tag→digest step (one GET per manifest), so there is no
separate `TagDigest` method. The client is injected into the service through a
factory (`func(addr string) domain.DistributionClient`) so `service` stays free
of the `repository` import.

### D2 — Read-only mirror config surface

The chart deploys the mirrors, so it also tells the supervisor where they are:
a new admin-visible/read-only `image_cache.mirrors` block
(`{id, host, upstream, internal_addr, backend}`, `id` = the chart-derived host
slug). `config/loader.go` validates non-empty `id`/`host`/`internal_addr`,
`backend ∈ {s3,pvc}`, and rejects duplicate ids. The block carries **no S3
credentials** and no prefix: prune never touches the mirror's storage directly.

### D3 — API-only list / prune / prune-all

New admin-only routes (mirroring the fleet purge gating):

- `GET  /api/v1/image-cache` → lists every mirror's repositories/tags
  (`Ping → Catalog → Tags → ManifestSize`) with per-mirror bounds (30s probe
  budget, 1000 repos/tags caps) and **per-mirror errors in the 200 body** —
  an unreachable or catalog-disabled mirror never fails the request.
- `POST /api/v1/image-cache/prune` → body
  `{mirror_id, refs:[{repository, tag?|digest?}]}`; a tag is resolved to its
  digest via the manifest API, a supplied digest is validated (`sha256:<hex>`),
  and each ref gets its own result item. Bounded concurrency (4). A missing
  manifest is **already pruned** (idempotent).
- `POST /api/v1/image-cache/prune-all` → body `{mirror_id}` (empty = all
  mirrors); `Catalog → Tags → digest → DELETE` for every tag-reachable
  manifest, bounded concurrency (4), aggregate
  `manifests_pruned`/`repositories_processed`/`errors` plus per-item `failed[]`
  and a truncation/untagged caveat `Message`.

Pruning issues **only OCI Distribution v2 API calls** against the live mirror
Service: **no workload scaling, no PVC deletion, no storage wipe, no rollout
restart**, and therefore **no `apps/deployments` RBAC**. Behavior is identical
on the `s3` and `pvc` backends (the backend only labels the mirror in the UI).
The `ErrRegistryDeleteDisabled` → 409 and `ErrRegistryCatalogDisabled` → 409
mappings are kept as defensive paths even though Zot allows delete and serves
`_catalog` by default.

Error mapping: unknown mirror → 404, malformed body/ref/digest → 400,
delete/catalog disabled → 409, unreachable mirror → 502, internal → 500.
Per-item failures travel in the result body, not as HTTP errors.

### D4 — Blob reclamation is Zot's online GC

`DELETE /v2/<name>/manifests/<digest>` unlinks the manifest and its tag
immediately. The referenced blobs become unreferenced and Zot's **online GC**
reclaims their bytes after `storage.gcDelay` on the next `storage.gcInterval`
cycle — no offline procedure, no CronJob, no supervisor involvement. The API
`Message` and the UI state this explicitly ("manifest unlinked now; blob bytes
reclaimed automatically within ~`gcDelay`+`gcInterval`"). Untagged/orphaned
manifests and interrupted-upload blobs are not enumerable by tag and are
likewise reclaimed automatically once unreferenced.

### D5 — UI and clarity pass

A new admin `/image-cache` page (reusing the removed Sync-cache nav slot)
renders one card per mirror: repository→tag table with checkboxes (digest, size,
layer count), "Prune selected"/"Prune all" with confirm dialogs, per-mirror
loading/error state, and inline per-item results. For multi-arch tags (OCI
image index / Docker manifest list), `GET /api/v1/image-cache` reports
`size_bytes`/`layer_count` from a representative child platform manifest
(linux/amd64 preferred; attestation and `unknown`-platform entries skipped),
while `digest` remains the top-level index digest so prune still unlinks the
tag (see the Consequences). The existing per-version
engine purge button on the Runners page is relabelled **"Purge local cache"**
(kept per-version, per-pod granularity is a non-goal).

## Alternatives considered

- **Delete and rewrite the OCI client** — rejected: the existing client already
  has the Distribution v2 catalog/tags/manifest/delete semantics with the right
  error mapping and digest/path hardening; a new client would duplicate it.
- **Wipe the mirror's storage directly (S3 prefix / PVC) or scale the mirror
  down and delete its PVC** — rejected: it takes the mirror offline, has a
  cross-mirror blast radius, and requires `apps/deployments` RBAC and
  backend-specific code. The API-only approach is safe on a live mirror and
  works identically for `s3`/`pvc`.
- **Offline `registry garbage-collect` (Distribution)** — rejected with the
  Distribution mirror itself (ADR-033): Zot's online GC makes reclamation
  automatic without a maintenance window.
- **Per-pod engine local purge framing** — the engine purge (ADR-032) is
  unchanged; image-cache prune is a distinct concern (mirror blobs, not engine
  BuildKit cache).

## Consequences

- New domain types/interfaces in `internal/domain/image_cache.go`, a service
  `internal/service/image_cache.go`, a handler `internal/handler/image_cache.go`,
  three admin routes, UI types/client/page, and integration tests against a fake
  OCI Distribution v2 server.
- The supervisor needs **no new Kubernetes RBAC** (`pods/log` stays removed, no
  `apps/deployments`).
- The former pull-through TTL and the registry delete-enable flag are gone;
  there is no TTL — invalidation is explicit via prune.
- Known caveat: prune-all cannot reach untagged/orphaned manifests by tag; Zot
  GC reclaims their bytes automatically once unreferenced.
- For multi-arch tags (OCI image index / Docker manifest list),
  `GET /api/v1/image-cache` reports `size_bytes`/`layer_count` from a
  representative child platform manifest (linux/amd64 preferred; attestation
  and `unknown`-platform entries skipped), while `digest` remains the top-level
  index digest so prune still unlinks the tag. If no child resolves, size/layer
  count are `-1` (unknown).
