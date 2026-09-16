# Cleanup: remove dead remote-cache / registry-proxy code + add admin image-cache management

## 1. Overview / problem statement

The platform's cache architecture changed three times, leaving a large amount of
remote-cache/registry code compiled, wired, and tested but never exercised:

1. The OCI docker-registry cache backend was replaced by S3.
2. Dagger 0.21.x removed `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`, so nothing writes
   the S3 `cache/` prefix anymore.
3. The worker-cache sync was removed and a local image mirror was added;
   engine-cache purge is now purely local. **This revision swaps the mirror
   software from CNCF Distribution (`registry:3`) to Zot** (an OCI-only registry
   with online GC and on-demand pull-through sync) — the deployed mirror image
   + chart/config change, but the supervisor-side OCI Distribution v2 API
   contract (list/prune/prune-all) is unchanged.

Consequence: the registry cache proxy, the cache stats/purge/GC services, the
FSM cache-route persistence, the "Sync cache" UI page, and their
config/Helm/docs are all dead. This plan removes them while preserving the
still-live S3 CLI cache, the image mirror, the local engine-cache purge, the
status endpoint, and Raft replay/snapshot compatibility.

**New work folded in (operator feedback):** the OCI Distribution v2 API client
(`registry_client.go`) is **kept and repurposed** as the mirror client for a new
**admin "Image cache" feature** that lists/prunes the local image mirror(s).
Zot implements the same OCI Distribution v2 API surface, so the client and all
supervisor-side tests are unchanged. It
also adds a **"Prune local cache"** clarity pass on the Runners page (the
existing per-version purge stays; no per-pod button is added — rationale in
D14). The removed "Sync cache" nav slot is reused for the new "Image cache"
page.

**Pruning principle (this revision):** the earlier validated engine local-cache
purge (ADR-032) already works by calling the engine's **own API** over
HTTP/TCP — an in-process session `POST /query` carrying
`{ engine { localCache { prune(useDefaultPolicy: false) } } }` — with no
workload scaling. That design is **unchanged** (D14). This revision applies the
**same principle to the mirror**: every prune (selected and all) goes through the
mirror's **own OCI Distribution v2 API** (`/v2/...`, served by Zot) over
HTTP/TCP. There is no
workload scaling, no PVC deletion, no storage wipe, and no rollout restart
anywhere in this plan.

**Mirror mechanism (this revision):** the deployed mirror is now **Zot**
(one Deployment + Service + JSON-config ConfigMap per upstream, address
`<release>-<slug>-mirror.<ns>.svc:5000`), serving each upstream via
`extensions.sync` **on-demand pull-through** (`onDemand: true`) instead of
Distribution's `REGISTRY_PROXY_REMOTEURL`. Zot's **online GC** (`storage.gc`,
`gcDelay`, `gcInterval`) reclaims unreferenced blob bytes automatically after a
manifest DELETE, replacing the offline stop-the-world `registry garbage-collect`
that Distribution v2/v3 would have required. The supervisor-side admin feature
(D7–D14) is unchanged: it speaks the same OCI Distribution v2 API, so the
client, config surface, API routes, UI, and tests carry over. Distribution
v2/v3 (offline GC only) and Harbor (heavy full stack) are recorded as rejected
alternatives in ADR-033.

## 2. Design decisions (with rationale)

### D1 — FSM: keep decode-only no-op command handlers, drop state maps + snapshot fields
The cache-route commands (`kindUpsertManifestRoute`=15, `kindDeleteManifestRoute`=16,
`kindUpsertBlobRoute`=18, `kindRecordUpload`=19, `kindDeleteUpload`=20,
`kindReapUploads`=21, `kindTouchManifestRoute`=25) are persisted as `command.Kind`
bytes in the Raft log, and the live cluster restores snapshots on start
(`no_snapshot_restore_on_start: false`) with a Raft state carried over from the
registry-cache era. Removing the kind dispatch entirely would make any replayed
old entry hit `applyCommand`'s `"unknown raft command kind %d"` fallthrough and
log an apply error on every follower restart that replays a compacted-log tail.

Decision:
- **Keep the `commandKind` constants** (their numeric IDs must never be renumbered).
- **Collapse their apply cases into decode-only no-ops** (`return nil, nil, true`
  in `applyTraceCacheCommand`; `return nil, true` for `kindTouchManifestRoute` in
  `applySessionCommand`) that discard the payload without unmarshalling it.
- **Remove the payload types** (`cmdDeleteManifestRoute`, `cmdUpsertBlobRoute`,
  `cmdDeleteUpload`, `cmdReapUploads`, `cmdTouchManifestRoute`) — no longer needed.
- **Remove the state maps** (`cacheObjectRoutes`, `cacheBlobRoutes`,
  `cacheUploadSessions`) and their read/write helpers (`manifestRouteKey`,
  `upsertManifestRoute`, `touchManifestRoute`, `upsertBlobRoute`, `recordUpload`,
  `reapUploads`, `lookupManifestRoute`, `lookupBlobRoute`, `lookupUpload`,
  `allCharges`).
- **Remove the snapshot fields** (`ObjectRoutes`, `BlobRoutes`, `Uploads`), the
  `blobRouteEdge` type, and their encode/decode loops.

Rationale: `encoding/json` **ignores unknown keys on decode**, so old snapshots
that still contain `object_routes`/`blob_routes`/`uploads` restore cleanly (the
cache-route data is silently dropped, which is fine — nothing reads it). There
is **no snapshot version field** to bump (the snapshot is a bare JSON document).
Old log entries replay as no-ops instead of erroring. The constants stay
referenced by the no-op `case`, so `golangci-lint`'s `unused` linter stays green.
This mirrors the existing precedent for the removed `kindDeleteRoutesForBackend`
(slot 17), extended with a comment documenting the reserved block.

### D2 — UI: remove the "Sync cache" (MagicCache) page entirely; reuse its nav slot for "Image cache"
The page displayed the remote cache's stats/purge/GC, none of which exists.
Grep confirms nothing else links to `/cache` (only `router/index.ts` + `App.vue`).
Remove the page, its route, the nav link, `fetchCacheInfo`/`purgeCache`, and the
`CacheInfo`/`CacheRef`/`GCRules`/`PurgeResult` types. Keep `EngineCachePurgeResult`
/`EnginePodPurgeResult` and `purgeEngineCache` (used by the Runners page for the
live local purge). The vacated `/cache` route + nav slot is **replaced** by the
new `/image-cache` "Image cache" page (D13), not left empty.

### D3 — Status: drop the "cache" service row
`StatusService.probeCache` reports the remote cache's health (registry ping / S3
bucket presence). With the remote cache gone and `service.Cache` removed, the row
is meaningless. Remove `probeCache` plus the `cache`/`router` fields and
constructor params; the status payload becomes supervisor + telemetry + fleet.
The UI `Services.vue` iterates the services list generically, so no UI change is
needed (verify). Re-adding an "image mirror" status row is a separate feature and
explicitly out of scope here.

### D4 — Connect env: remove the `cache_backend` field
`ConnectEnvSnapshot.CacheBackend` is hardcoded to `"s3"` and displayed by
`Connect.vue`. It is now misleading (no remote cache backend exists). Remove the
domain field, the hardcoded assignment, the UI display, and the test assertion.

### D5 — `imageCache.registries[].name`: remove (unused)
The `name` field is documented but never read — `_helpers.tpl` derives the slug
from `host` (`imageCacheSlug`). **Remove** `name` (do not wire it; `host` already
uniquely identifies a mirror). Update the `@param` doc, the commented example, and
the Helm README row. Note: the new supervisor-facing `image_cache.mirrors[]` config
(D8) uses `id` = the derived slug, **not** the removed `name`.

### D6 — Keep `WithReadTimeout(0)` + `WithStreamBody(true)`; update stale comments
Both server options were added for the cache proxy's multi-GB blob uploads, but
reverting them is a behavioral change for the OTLP ingest path (body buffering)
and is not dead code. Keep them and rewrite the comments (in `configure` and on
`readBoundedBody`/`maxControlBody`) so they no longer reference the cache vhost.

### D7 — REUSE the OCI client as the mirror client (do NOT delete it)
The mirror listing/pruning feature needs exactly what `registry_client.go`
already implements: `Ping`, `Catalog`, `Tags`, `ManifestSize` (returns the tag's
digest + size + layer count), and `DeleteManifest`. Decision:

- **Keep `internal/repository/registry_client.go`** (filename unchanged; header
  doc updated to "OCI Distribution v2 client for the local image mirrors (Zot
  serves this API)").
- **Rename the type** `RegistryStatsClient` → **`DistributionClient`**;
  constructors `NewRegistryStatsClient` → `NewDistributionClient`,
  `NewRegistryStatsClientWithAuth` → `NewDistributionClientWithAuth`.
- **Rename the domain interface** `domain.RegistryClient` → **`domain.DistributionClient`**,
  trimmed to the mirror surface:
  `Host`, `Ping`, `Catalog`, `Tags`, `ManifestSize`, `DeleteManifest`.
  `ManifestSize`'s returned `digest` is reused for the prune tag→digest step
  (no separate `TagDigest` method; one GET per manifest).
- **Delete the dead CLI-cache / router-only methods** (their only callers die
  with the CLI registry cache and the registry router): `ProbeManifest`,
  `ProbeBlob`, `ManifestCreated`, `BlobSize`, `UploadBlob`, `UploadBlobStream`,
  `PutManifest`, `GetManifest`, `GetBlob`, `ManifestExists`, and
  `errUploadDigestMismatch`. Also drop the `var _ domain.CLIRegistryClient`
  assertion (the interface is deleted) and the `BlobSize` size-sum fallback in
  `ManifestSize` (descriptor sizes absent ⇒ `size = -1`).
- **Keep** the sentinels `ErrRegistryUnreachable`, `ErrRegistryCatalogDisabled`,
  `ErrManifestNotFound`, `domain.ErrRegistryDeleteDisabled`,
  `domain.ErrRegistryCatalogDisabled`, `domain.ErrManifestNotFound` (moved from
  the deleted `domain/cache.go` into the new `domain/image_cache.go`), the
  `validDigest`/`digestRe`/`readBounded`/`manifestAccept`/`manifest`/`descriptor`
  helpers, and `WithTimeout`/`Host`/`baseURL`/`do`/`discard`.

Rationale: this is the cheapest correct path — the client already implements the
Distribution v2 catalog/tags/manifest/delete semantics (which Zot serves) with
the right error
mapping (catalog-disabled 404/403, delete-disabled 405/403, manifest-not-found
404) and the CWE-20/CWE-918 digest/path hardening the mirror feature needs.
Deleting it and writing a new one would duplicate all of that.

### D8 — How the supervisor learns the mirror endpoints (new config surface)
The chart deploys one Deployment+Service per upstream
(`<release>-<slug>-mirror.<ns>.svc:5000`). The supervisor learns them via a new
admin-visible/read-only config block, rendered by the chart:

```yaml
image_cache:
  mirrors:
    - id: "docker-io"                                          # imageCacheSlug(host)
      host: "docker.io"
      upstream: "https://registry-1.docker.io"
      internal_addr: "<release>-docker-io-mirror.<ns>.svc:5000"
      backend: "s3"           # "s3" | "pvc" — informational (UI label); prune is identical
```

- `domain.Config` gains `ImageCache ImageCacheConfig` (`mapstructure:"image_cache"`).
- `ImageCacheMirror{ID, Host, Upstream, InternalAddr, Backend}`;
  `ImageCacheConfig{Mirrors []ImageCacheMirror}`. (No `S3`/`S3Prefix` — prune-all
  uses the Distribution API only and never touches the mirror's storage directly.)
- `config/loader.go`: default `image_cache.mirrors=[]`; new
  `validateImageCacheConfig` (each mirror: non-empty `id`/`host`/`internal_addr`,
  `backend ∈ {s3,pvc}`, duplicate `id` rejected).
- Helm `configmap.yaml` renders the block only when `.Values.imageCache.enabled`,
  from the existing `imageCacheRegistries` + `imageCacheMirrorAddress` helpers and
  `imageCache.storage.backend`. Read-only for admins — it mirrors what the chart
  already deploys. (The mirror's own S3 storage config — `imageCache.storage.s3.*`
  — lives only in the Zot mirror's `config.json` (D15); the supervisor never sees
  it and prune-all never touches it.)

### D9 — Listing (GET /api/v1/image-cache)
- Per mirror: `Ping` (`GET /v2/`) → `Catalog` (`GET /v2/_catalog`) → for each
  repo `Tags` (`GET /v2/<name>/tags/list`) → for each tag `ManifestSize` (size +
  digest + layer count).
- **Bounds**: a per-mirror probe budget (e.g. `30s`) and a per-request
  `REGISTRY_CATALOG_MAXENTRIES`-aware cap — the client already bounds body reads
  to `maxRegistryBody` (16 MiB); the service caps repos-per-mirror (e.g. 1000) and
  tags-per-repo (e.g. 1000) and stops early on context cancellation (partial
  results are still returned with `Message` noting the truncation).
- **Errors are per-mirror, never a hard failure**: an unreachable mirror, a
  catalog-disabled mirror (`ErrRegistryCatalogDisabled` — kept as a defensive
  mapping; Zot serves `GET /v2/_catalog` by default, so it is not expected), or
  an empty mirror all surface as a
  `mirrors[].error` string + `reachable:false` / empty `repositories[]` in the
  200 response. `reachable` reflects the last `Ping`.

### D10 — Prune selected images (`POST /api/v1/image-cache/prune`)
- **Delete is always enabled with Zot**: unlike Distribution, Zot has no
  `REGISTRY_STORAGE_DELETE_ENABLED` gate — `DELETE /v2/<name>/manifests/<digest>`
  is available out of the box (no auth configured). The client keeps the
  405/403 → `domain.ErrRegistryDeleteDisabled` → HTTP 409 mapping as a defensive
  path (only reachable if a future authz config denies anonymous delete).
- **Tag→digest step**: for a `ref` with `tag`, call `ManifestSize` (which HEADs/GETs
  the manifest and returns the `Docker-Content-Digest`-derived digest); for a `ref`
  with `digest`, validate `sha256:<hex>` and use it directly.
- **Semantics**: `DELETE /v2/<name>/manifests/<digest>` unlinks the manifest (and
  its tag reference). Deleted **by digest** removes that exact manifest revision;
  deleted **by tag** removes whatever manifest the tag currently points at (after
  tag→digest resolution). Blobs referenced by the deleted manifest become
  unreferenced immediately; Zot's **online GC** then reclaims their bytes
  automatically after `gcDelay` (default `2h`) on the next `gcInterval` cycle
  (default `1h`) — no offline procedure (see D11). Prune-selected therefore
  **removes the cached image (forces re-fetch on the next request) and, after the
  GC delay, reclaims the blob bytes**; this timing is documented in the UI/API.
  A manifest already absent
  (`ErrManifestNotFound`) counts as `already pruned` (idempotent), not an error.
- **Per-item results**: each ref gets its own `ImageCachePruneItem`; failures are
  reported per-item (the request does not fail wholesale). Bounded concurrency
  (e.g. 4 manifests deleted concurrently per mirror).

### D11 — Prune all (`POST /api/v1/image-cache/prune-all`)
Prune-all is the **same "call the service's own API" principle as the engine
local-cache purge** (D14/ADR-032), applied to the mirror: it issues **only OCI
Distribution v2 API calls** against the live mirror Service (Zot serves this
API) — **no workload
scaling, no PVC deletion, no storage wipe, no rollout restart, and therefore no
`apps/deployments` RBAC**. Identical behavior on `s3` and `pvc` backends (the
backend is irrelevant to the API; it only labels the mirror in the UI).

Algorithm (bounded concurrency, e.g. 4 manifests at a time, per mirror):
1. `Ping` (`GET /v2/`) — unreachable mirror ⇒ per-mirror error (502).
2. `Catalog` (`GET /v2/_catalog`) to enumerate repositories; `Tags` (`GET
   /v2/<name>/tags/list`) per repository to enumerate tags.
3. For each tag, resolve its digest via `ManifestSize` (the manifest HEAD/GET
   returns `Docker-Content-Digest` — the existing client already surfaces it).
4. `DeleteManifest` (`DELETE /v2/<name>/manifests/<digest>`) for every resolved
   digest.
5. Aggregate: `manifests_pruned`, `repositories_processed`, per-item errors, and
   a `Message`. A manifest already absent (`ErrManifestNotFound`, 404) counts as
   **already pruned** (idempotent), not an error.

Edge handling:
- **Untagged/orphaned manifests and blobs** that `_catalog`/`tags/list` cannot
  enumerate (e.g. a manifest pushed then its tag overwritten, or an interrupted
  upload's blobs) are **not** reachable by tag and therefore not deleted by
  prune-all; Zot's online GC reclaims their bytes automatically once they are
  unreferenced (see below). Documented as a known caveat.
- **Thousands of manifests**: bounded concurrency + per-request timeouts + a
  repo/tag cap; if enumeration or deletion is truncated (cap/timeout/context
  cancellation) the partial result is returned with a `Message` noting truncation.
- **Manifest deleted mid-enumeration** (e.g. concurrent prune-selected): the
  later `DELETE` returns 404 ⇒ "already pruned", never an error.
- **Delete-disabled mirror**: Zot has no delete-disabled flag — `DELETE` is
  always allowed (no auth configured). The 405/403 ⇒ `ErrRegistryDeleteDisabled`
  ⇒ 409 mapping is kept as a defensive path only.

**Blob reclamation (Zot online GC, verified against v2.1.21):** Zot has
**online (inline) garbage collection** — the registry stays up during GC; there
is no stop-the-world/read-only step. Evidence: Zot's admin configuration guide
states "Garbage collection in zot is an inline feature meaning that it is **not**
necessary to take the registry offline." Config keys (under `storage`, JSON
`config.json`):

- `gc: true` — enable background GC.
- `gcDelay: "2h"` — minimum age of an unreferenced blob before it is reclaimed
  (the tunable delay after a manifest `DELETE`; default `1h` when unset — the
  chart pins it explicitly).
- `gcInterval: "1h"` — how often the GC scheduler runs.
- `gcTimeWindow: "01:00-08:00"` (optional) — restrict GC to a daily off-peak
  window (changes require a server restart).
- `dedupe: true` — content dedupe (single copy per blob across manifests).
- `retention` (optional, out of scope for v1) — tag/referrer retention policies.

Reclamation trigger + expected delay: `DELETE /v2/<name>/manifests/<digest>`
unlinks the manifest immediately; the referenced blobs become unreferenced and
the next GC run (≤ `gcInterval`) removes any blob whose unreferenced age exceeds
`gcDelay`. The API/UI therefore explain prune as "manifest unlinked now; blob
bytes reclaimed automatically within ~`gcDelay`+`gcInterval`". Untagged/orphaned
blobs (from overwritten tags or interrupted uploads) are likewise reclaimed
automatically by the same GC — there is **no** offline `registry garbage-collect`
procedure and no Helm CronJob to add. (The supervisor client is untouched: this
is purely the mirror's own background behavior.)

### D12 — API surface (admin-only)
Routes (all `adminOnly`, mirroring the existing fleet purge):
- `GET  /api/v1/image-cache` → `ImageCacheInfo`
- `POST /api/v1/image-cache/prune` → body `ImageCachePruneRequest` → `ImageCachePruneResult`
- `POST /api/v1/image-cache/prune-all` → body `ImageCachePruneAllRequest` → `ImageCachePruneAllResult`

Error mapping (handler): unknown mirror → 404; malformed body/ref/digest → 400;
`ErrRegistryDeleteDisabled` → 409; `ErrRegistryCatalogDisabled` (prune-all by API)
→ 409; per-mirror unreachable → 502; internal → 500. Per-item/prune failures are
carried in the result body (not HTTP errors) wherever the request itself was valid.
Exact Go structs + JSON shapes in §3.

### D13 — UI: new "Image cache" nav page (replaces the removed Sync-cache slot)
A new **`/image-cache`** page (`ui/src/imagecache/ImageCache.vue`), admin-gated in
the nav (like Users/Groups), rendered from the removed `/cache` slot:
- one card per mirror (`id`, `host`, `backend`, `reachable` + per-mirror
  loading/error state);
- a table of repositories → tags with checkboxes (repository, tag, digest,
  size) and "Prune selected" + "Prune all" buttons with confirm dialogs;
- prune results shown inline (per-item outcomes + freed count).
Types/client functions added in §3.

### D14 — Runners page: keep the per-version "Purge cache" button, no per-pod button
The per-version local-cache purge button already exists (`ui/src/fleet/Runners.vue`,
`POST /api/v1/fleet/:version/purge-cache`). **Decision: keep it and relabel it to
"Purge local cache"** to disambiguate from the new image-cache prune; it stays
**per-version** (fan-out to all pods). **Do NOT add per-pod granularity** in this
changeset: (a) the result already reports per-pod outcomes (failed pods are listed
individually); (b) per-pod prune is a rarely-needed operation and the fan-out
already calls the pruner per pod IP; (c) it is additive scope with its own
`?pod=`/per-pod-button UI+handler+service surface. The `?pod=<podName>` variant is
trivial to add later (the pruner already works per pod IP) — recorded as a
non-goal, not a blocker.

### D15 — Helm changes for the mirror feature (Zot)

`templates/image-cache.yaml` is **rewritten** from the env-driven Distribution
Deployment to a Zot Deployment mounting a chart-rendered `config.json`
ConfigMap; the Service (`<release>-<slug>-mirror`, port 5000) and the optional
`pvc` PersistentVolumeClaim are structurally unchanged. Topology stays **one Zot
Deployment + Service per upstream** (rationale recorded in ADR-033): each mirror
syncs a single upstream with no `destination`/`stripPrefix` rewriting, so the
local repo path maps 1:1 to the upstream repo path (transparent BuildKit
pull-through).

- **Image**: `ghcr.io/project-zot/zot:v2.1.21` (full-featured; multi-arch
  linux/amd64 + linux/arm64; Apache-2.0). Base is
  `gcr.io/distroless/base-nossl-debian13`, a **non-root** static binary (UID
  `65532`, entrypoint `/usr/bin/zot`, `EXPOSE 5000`). Pod `securityContext`:
  `runAsNonRoot: true`, `runAsUser: 65532`, `fsGroup: 65532` (writable PVC),
  `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`. Port stays 5000.
- **ConfigMap** `<name>-zot-config` renders `config.json` (Zot reads a JSON/YAML
  file, not env vars). Rendered shape (s3 backend, `docker-io` example):
  ```json
  {
    "distSpecVersion": "1.1.1",
    "storage": {
      "rootDirectory": "/var/lib/registry",
      "dedupe": true,
      "gc": true,
      "gcDelay": "2h",
      "gcInterval": "1h",
      "storageDriver": {
        "name": "s3",
        "rootdirectory": "/docker-io",
        "region": "us-east-1",
        "regionendpoint": "<release>-minio.<ns>.svc:9000",
        "bucket": "image-cache",
        "forcepathstyle": true,
        "secure": false,
        "skipverify": false
      }
    },
    "http": { "address": "0.0.0.0", "port": "5000", "compat": ["docker2s2"] },
    "log": { "level": "warn" },
    "extensions": {
      "sync": {
        "enable": true,
        "registries": [
          {
            "urls": ["https://registry-1.docker.io"],
            "onDemand": true,
            "tlsVerify": true,
            "preserveDigest": true,
            "maxRetries": 3,
            "retryDelay": "15m"
          }
        ]
      }
    }
  }
  ```
  - `http.compat: ["docker2s2"]` + `preserveDigest: true` keep upstream
    Docker-manifest digests (no OCI conversion / digest drift), so the
    supervisor's `ManifestSize`/prune-by-digest and any `@digest` pulls keep
    working. Extensions not listed (`search`, `ui`, `mgmt`, `metrics`, `scrub`,
    `lint`, `trust`) are **disabled by omission** — only `sync` is enabled.
  - **Health/readiness**: `GET /v2/` (OCI ping, the client's `Ping`) plus Zot's
    `/livez`/`readyz`/`startupz`; chart sets `readinessProbe: GET /readyz` and
    `livenessProbe: GET /livez` on port 5000.
  - **Storage — s3 (default)**: `storage.storageDriver` (`name: "s3"`) with
    `regionendpoint` = the in-cluster MinIO Service (reuse the existing
    `imageCacheS3Endpoint` helper), `bucket` = `image-cache`, `rootdirectory` =
    `/<slug>` (per-upstream prefix, equivalent to the removed
    `REGISTRY_STORAGE_S3_ROOTDIRECTORY=/<slug>`), `forcepathstyle: true`,
    `secure: false`. **Credentials are not embedded in the ConfigMap**: omit
    `accesskey`/`secretkey` and rely on the AWS SDK default credential chain,
    injecting `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env vars via
    `secretKeyRef` to the existing `engine-s3-auth` Secret (keys
    `accessKey`/`secretKey`) — mirroring the current Distribution env injection.
    `storage.rootDirectory` (local dedupe/metadata cache DB, rebuilt at startup
    by scanning S3) uses an `emptyDir` on `s3`; on `pvc` it is the blob store.
  - **Storage — pvc**: omit `storageDriver`; `storage.rootDirectory:
    /var/lib/registry` on the per-mirror PVC (Recreate strategy retained).
  - **Rendering rules**: `gcTimeWindow` and `sync.pollInterval` are omitted from
    the JSON when empty (Zot treats them as unset); `gc.enabled: false` omits
    `storage.gc` entirely.
- **TTL/proxy semantics (replaces `REGISTRY_PROXY_TTL`)**: Zot on-demand sync has
  no TTL. The first request for an uncached repo/tag fetches it from upstream and
  serves it; subsequent requests are served from cache. Invalidation is
  **explicit** — prune (this feature) unlinks the manifest and the next request
  re-fetches. `pollInterval` (periodic refresh) is **not** set by default and must
  not be used for Docker Hub (rate limits / no upstream catalog). `imageCache.ttl`
  is removed.
- **Private upstreams**: Zot supports private upstreams via
  `extensions.sync.credentialsFile` (a mounted JSON Secret mapping upstream →
  {username,password}) or Docker-credentials auto-discovery, plus
  `credentialHelper` (e.g. `"ecr"`). For v1 the presets are public and
  `credentialsFile` is **not** rendered; private/custom registries (mapping the
  existing `imageCache.registries[].username/passwordSecretRef` onto a mounted
  credentialsFile Secret) are a documented follow-up.
- `templates/configmap.yaml`: render the `image_cache.mirrors` block (D8) when
  `imageCache.enabled` (no `image_cache.s3`). Unchanged from the prior revision.
- `templates/rbac.yaml`: keep the `pods/log` removal; **no** `apps/deployments`
  addition — prune-all needs no k8s API access.
- `values.yaml` (new `imageCache` schema — replaces the Distribution keys):
  ```yaml
  imageCache:
    enabled: false
    image: { repository: "ghcr.io/project-zot/zot", tag: "v2.1.21", pullPolicy: IfNotPresent }
    logLevel: "warn"
    storage:
      backend: "s3"                              # s3 | pvc
      s3: { bucket: "image-cache", region: "us-east-1", endpoint: "", forcePathStyle: true, secure: false }
      pvc: { storageClass: "", size: "20Gi" }
    gc: { enabled: true, delay: "2h", interval: "1h", timeWindow: "" }   # storage.gc/gcDelay/gcInterval/gcTimeWindow
    dedupe: true                                 # storage.dedupe
    sync: { onDemand: true, preserveDigest: true, tlsVerify: true, pollInterval: "", maxRetries: 3, retryDelay: "15m" }
    resources: { requests: { cpu: "100m", memory: "128Mi" }, limits: { cpu: "500m", memory: "512Mi" } }
    nodeSelector: {}
    tolerations: []
    securityContext: { runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532 }
    presets: { ... }                             # unchanged keys
    registries: []                               # unchanged shape (creds → future credentialsFile)
  ```
  Removed keys: `imageCache.ttl` (no TTL) and `imageCache.registries[].name`
  (D5). There is **no** `REGISTRY_STORAGE_DELETE_ENABLED` equivalent (Zot always
  allows delete). `@param` docs updated for every new key; `image_cache.mirrors[]`
  (D8) is chart-computed, not user-facing.

## 3. Data structures / signatures

### 3.1 New domain types — `internal/domain/image_cache.go`
```go
// Sentinels (moved from the deleted domain/cache.go):
var ErrRegistryDeleteDisabled  = errors.New("registry delete not enabled")
var ErrRegistryCatalogDisabled = errors.New("registry catalog disabled")
var ErrManifestNotFound        = errors.New("manifest not found")

// DistributionClient is the mirror-facing OCI Distribution v2 slice.
type DistributionClient interface {
    Host() string
    Ping(ctx context.Context) error
    Catalog(ctx context.Context) ([]string, error)
    Tags(ctx context.Context, repo string) ([]string, error)
    // ManifestSize returns (digest, sizeBytes, layerCount). digest is used by
    // the prune path for tag→digest resolution; size/layerCount by listing.
    ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error)
    DeleteManifest(ctx context.Context, repo, digest string) error
}

type ImageCacheService interface {
    List(ctx context.Context) (*ImageCacheInfo, error)
    Prune(ctx context.Context, mirrorID string, refs []ImageCachePruneRef) (*ImageCachePruneResult, error)
    PruneAll(ctx context.Context, mirrorID string) (*ImageCachePruneAllResult, error)
}
```
Config types (in `internal/domain/config.go`):
```go
type ImageCacheConfig struct {
    Mirrors []ImageCacheMirror `mapstructure:"mirrors"`
}
type ImageCacheMirror struct {
    ID           string `mapstructure:"id"`             // slug, e.g. "docker-io"
    Host         string `mapstructure:"host"`           // upstream host, e.g. "docker.io"
    Upstream     string `mapstructure:"upstream"`       // upstream base URL
    InternalAddr string `mapstructure:"internal_addr"`  // "<release>-<slug>-mirror.<ns>.svc:5000"
    Backend      string `mapstructure:"backend"`        // "s3" | "pvc" (informational)
}
```

### 3.2 API payloads — `internal/domain/image_cache.go`
```go
type ImageCacheRepository struct { Repository string `json:"repository"` }
type ImageCacheTag struct {
    Tag        string `json:"tag"`
    Digest     string `json:"digest"`       // sha256:...
    SizeBytes  int64  `json:"size_bytes"`   // -1 unknown
    LayerCount int64  `json:"layer_count"`  // -1 unknown
}
type ImageCacheMirrorInfo struct {
    ID           string                 `json:"id"`
    Host         string                 `json:"host"`
    Upstream     string                 `json:"upstream"`
    Backend      string                 `json:"backend"`
    Reachable    bool                   `json:"reachable"`
    Repositories []ImageCacheRepository `json:"repositories"`
    Error        string                 `json:"error,omitempty"`
}
type ImageCacheInfo struct {
    Mirrors     []ImageCacheMirrorInfo `json:"mirrors"`
    CollectedAt string                 `json:"collected_at"` // RFC3339 UTC
}

type ImageCachePruneRef struct {
    Repository string `json:"repository"`
    Tag        string `json:"tag,omitempty"`
    Digest     string `json:"digest,omitempty"` // sha256:...; alternative to tag
}
type ImageCachePruneRequest struct {
    MirrorID string                `json:"mirror_id"`
    Refs     []ImageCachePruneRef  `json:"refs"`
}
type ImageCachePruneItem struct {
    Repository string `json:"repository"`
    Tag        string `json:"tag,omitempty"`
    Digest     string `json:"digest,omitempty"`
    Pruned     bool   `json:"pruned"`
    Error      string `json:"error,omitempty"`
}
type ImageCachePruneResult struct {
    MirrorID string                `json:"mirror_id"`
    Items    []ImageCachePruneItem `json:"items"`
    Pruned   int                   `json:"pruned"`
    Errors   int                   `json:"errors"`
    Message  string                `json:"message,omitempty"`
}

type ImageCachePruneAllRequest struct {
    MirrorID string `json:"mirror_id"` // empty = all mirrors
}
type ImageCachePruneAllItem struct {
    Repository string `json:"repository"`
    Tag        string `json:"tag,omitempty"`
    Digest     string `json:"digest,omitempty"`
    Pruned     bool   `json:"pruned"`
    Error      string `json:"error,omitempty"`
}
type ImageCachePruneAllMirror struct {
    MirrorID              string                  `json:"mirror_id"`
    ManifestsPruned       int                     `json:"manifests_pruned"`
    RepositoriesProcessed int                     `json:"repositories_processed"`
    Errors                int                     `json:"errors"`
    Failed                []ImageCachePruneAllItem `json:"failed,omitempty"` // per-item failures
    Message               string                  `json:"message,omitempty"` // truncation/untagged caveat
    Error                 string                  `json:"error,omitempty"`   // whole-mirror failure (unreachable/409)
}
type ImageCachePruneAllResult struct {
    Mirrors []ImageCachePruneAllMirror `json:"mirrors"`
    Message string                     `json:"message,omitempty"`
}
```

### 3.3 Infrastructure interfaces (service → repository)
**None.** Prune-all, prune, and list all use the injected
`domain.DistributionClient` factory (D7) to dial the mirror Service over HTTP —
no k8s API client, no S3 client, and no new repository adapters are added. The
service holds only the `newClient func(addr string) domain.DistributionClient`
factory.

### 3.4 Service — `internal/service/image_cache.go`
```go
func NewImageCacheService(
    mirrors []domain.ImageCacheMirror,
    newClient func(addr string) domain.DistributionClient, // wired to repository.NewDistributionClient
    logger *logrus.Logger,
) *ImageCacheService
```
Client construction is injected (same pattern as the old `NewRegistryRouter`'s
`newClient`), keeping `service` free of the repository import. No wiper or
deployment-controller dependencies exist anymore.

### 3.5 UI — `ui/src/api/types.ts` + `ui/src/api/client.ts`
```ts
export interface ImageCacheTag { tag: string; digest: string; size_bytes: number; layer_count: number }
export interface ImageCacheRepository { repository: string; tags: ImageCacheTag[] }
export interface ImageCacheMirrorInfo { id: string; host: string; upstream: string; backend: string; reachable: boolean; repositories: ImageCacheRepository[]; error?: string }
export interface ImageCacheInfo { mirrors: ImageCacheMirrorInfo[]; collected_at: string }
export interface ImageCachePruneRef { repository: string; tag?: string; digest?: string }
export interface ImageCachePruneResult { mirror_id: string; items: { repository: string; tag?: string; digest?: string; pruned: boolean; error?: string }[]; pruned: number; errors: number; message?: string }
export interface ImageCachePruneAllItem { repository: string; tag?: string; digest?: string; pruned: boolean; error?: string }
export interface ImageCachePruneAllResult { mirrors: { mirror_id: string; manifests_pruned: number; repositories_processed: number; errors: number; failed?: ImageCachePruneAllItem[]; message?: string; error?: string }[]; message?: string }
```
Client functions: `fetchImageCacheInfo()`, `pruneImageCache(mirrorId, refs)`,
`pruneAllImageCache(mirrorId?)`.

## 4. File-by-file change list

### 4.1 DELETE (entire files)

**Go — service**
- `internal/service/cache.go` + `internal/service/cache_test.go`
- `internal/service/cache_stats.go` + `internal/service/cache_stats_test.go`
- `internal/service/registry_router.go` + `internal/service/registry_router_test.go`
- `internal/service/s3_cache_gc.go` + `internal/service/s3_cache_gc_test.go`

**Go — repository**
- `internal/repository/cli_cache_registry.go` + `internal/repository/cli_cache_registry_test.go`
- `internal/repository/cache_routes_repo.go` + `internal/repository/cache_routes_repo_test.go`

**Go — handler**
- `internal/handler/cache.go` + `internal/handler/cache_test.go`
- `internal/handler/cache_proxy_test.go`

**Go — domain**
- `internal/domain/cache.go` — its three sentinels move to `image_cache.go`;
  `CacheStats`/`CacheRef`/`GCRules`/`GCRunSummary`/`PurgeResult`/`CacheStatsProvider`/
  `CachePurger`/`CacheBackend`/`S3Ref`/`S3CachePrefix`/`CacheRoute`/`CacheUploadSession`
  all deleted. **`internal/domain/registry.go` is DELETED** (its `RegistryClient`
  interface is replaced by the new `DistributionClient` in `image_cache.go`, and its
  `CacheRoutesStore` interface is dead).

**Go — integration**
- `tests/integration/cache_proxy_test.go`

**UI**
- `ui/src/magiccache/MagicCache.vue` (delete the `ui/src/magiccache/` directory)

### 4.2 MODIFY (cleanup — from the prior plan, still valid)

**`internal/domain/config.go`**
- Delete `RegistryBackend`, `SecretRef`, `GCConfig`.
- Trim `CacheConfig` to `{ S3 S3Config }`. Remove `Registry`, `PublicHost`,
  `InternalAddr`, `AuthToken`, `Registries`, `GC`. Keep `S3Config`.
- Add `ImageCache ImageCacheConfig` + `ImageCacheMirror` (D8).

**`internal/domain/connect.go`** — remove `CacheBackend string`.

**`internal/domain/metrics.go`** — rename `CacheMetricsClient` → `TraceSeriesDeleter`,
keeping only `DeleteTraceSeries(ctx, traceID) error` (drop `CacheHitRate`); update doc.

**`internal/domain/cli.go`** — remove `CLIRegistryClient`, `CLIManifest`,
`CLIManifestConfig`, `CLIManifestLayer`, `MediaTypeOCIImageManifest`,
`MediaTypeOCILayerGzip`, `MediaTypeOCIEmptyJSON`, `MediaTypeOCIRawBlob`. Keep
`AssetFilename`, the CLI sentinel errors, `CLIArtifact`, `CLICache`, `CLIReleaseIndex`,
`CLIUpstream`.

**`config/loader.go`** — remove `v.SetDefault` for `cache.registry`, `cache.public_host`,
`cache.internal_addr`, `cache.auth_token`, `cache.registries`, `cache.gc.*`; keep
`cache.s3.*` (drop the "S3 cache GC" wording). Add `image_cache.mirrors` default +
`validateImageCacheConfig` (D8).

**`internal/repository/fsm.go`** (D1) — keep `commandKind` constants + reserved
comment; delete payload types + cache-route cases as no-ops; remove state maps +
helpers.

**`internal/repository/fsm_snapshot.go`** (D1) — remove `blobRouteEdge`,
`ObjectRoutes`/`BlobRoutes`/`Uploads` + encode/decode loops.

**`internal/repository/fsm_test.go`** (D1) — remove cache-route cases + snapshot
round-trip assertions; keep user/group/token/project/trace/meta/session tests; add
the reserved-kind no-op replay test (§6).

**`internal/repository/metrics_store.go`** — remove `CacheHitRate`, `instantScalar`,
`ErrNoData`, `cacheHitsPromQL`, `cacheMissesPromQL`. Keep `NewMetricsClient`,
`doQueryCtx`, `DeleteSeries`, `DeleteTraceSeries`. Update
`var _ domain.TraceSeriesDeleter = (*MetricsClient)(nil)`.

**`internal/service/status.go` + `status_test.go`** (D3) — remove `cache`/`router`
fields, `probeCache`, params from `NewStatusService`; drop the registry/s3/bogus
tests + `newStatusServiceWithRegistry`; keep raft-clean tests.

**`internal/service/history_purge.go` + `history_purge_test.go`** — `CacheMetricsClient`
→ `TraceSeriesDeleter`.

**`internal/service/test_helper_test.go`** — remove `newRegistryClient`.

**`internal/service/connect_service.go` + `connect_service_test.go`** (D4) — remove
the `CacheBackend: "s3"` line + assertion.

**`internal/handler/server.go`** — remove the cache-router interface, `routeKind`,
`errBackendAuth`, `cacheProxyBackendIDKey`, OCI path regexes, `validDigest`,
`validOCIPathSegment`; `Deps.CacheBackend`/`CacheStatsProvider`/`CachePurger`/`Router`;
`Server.cacheBackend`/`router`/`cacheToken`/`cacheStats`/`cachePurger` +
`ServerConfig.CacheHost`/`CacheScheme`/`CacheToken`; the cache-proxy branch in
`buildProxies`; `cacheHostMiddleware` + the two cache routes; all the `routeCache*`/
`record*Route`/`cacheProxy*`/`requireCacheAuth`/`extractCacheToken`/`basicAuthHeader`/
`uploadUUIDFromResponse` functions; prune now-unused imports (`crypto/subtle`,
`encoding/base64`, `encoding/hex` if orphaned, and the hertz `app/client` +
`reverseproxy` imports — verify `reverseproxy` is still used by `buildProxies` for
otel/victoria before removing). Update `maxControlBody`/`readBoundedBody`/
`WithReadTimeout(0)`/`WithStreamBody(true)` comments (D6). **Add** the three
image-cache routes (§3) and a `Deps.ImageCache domain.ImageCacheService` +
`Server.imageCache` field.

**`internal/handler/metrics.go`** — remove the `"cache": "/api/v1/cache"` help entry.

**`internal/handler/server_test.go` + `test_helper_test.go`** — remove cache routes
+ `TestHandleCacheInfo`; drop `CacheStatsProvider`/`CachePurger`/`stubCacheStatsProvider`/
`stubCachePurger`; add the image-cache stub + route tests.

**`internal/observ/metrics.go`** — remove `CacheSizeBytes`, `CacheObjectCount`,
`CachePurgeTotal`, `GCRunTotal` + their construction/registration. Keep history/CLI/
engine/OTel metrics.

**`cmd/api/main.go` + `main_test.go`** — remove `validateCacheConfig`/`hostOf`,
`cacheHost`/`cacheScheme`/`cacheToken`, `service.Cache`, `cacheStatsSvc`, `s3CacheGC`,
`cachePurger`, the GC sweeper block, and the `Deps`/`ServerConfig` cache wiring. Keep
`s3Client` (CLI cache), `metricsClient` (history purge), `engineCachePurger`. **Add**
the image-cache wiring (§6 order). Delete `TestValidateCacheConfig`/`hostOf` subtest.

**Integration tests (MODIFY, not delete)** — `api_test.go`, `cli_test.go`,
`ci_steps_test.go`, `engine_cache_purge_test.go`, `oauth_oidc_test.go`,
`pipeline_url_test.go`, `rbac_test.go`: remove the `cacheBackend := &service.Cache{...}`
construction + `CacheBackend:` dep line. `cache_status_test.go`: delete
`TestCacheStatusAndPurgeIntegration` + `registryStub` + cache wiring; keep
`TestStatusRaftNotCleanSupervisorDownIntegration`, `stubRaftCleanState`, and the
simplified env.

**Helm** — `values.yaml` (remove `supervisor.config.cache.publicHost`/`authToken`/`gc.*`,
keep `cache.s3.*`; remove `imageCache.registries[]` `name` and `imageCache.ttl`;
add the Zot `imageCache.*` schema from D15), `_helpers.tpl` (remove
`cachePublicHost` + `cacheRegistry`), `configmap.yaml`
(remove cache.registry/public_host/auth_token + gc; keep cache.s3; add `image_cache.mirrors`
block), `ingress.yaml` (remove `$cacheHost` + TLS SAN loop + extra rule), `rbac.yaml`
(remove `pods/log` only), `image-cache.yaml` (rewrite as the Zot Deployment +
`<name>-zot-config` ConfigMap + Service + optional PVC per D15), `README.md`
(update rows).

**UI** — `router/index.ts` (remove `/cache`; add `/image-cache`), `App.vue`
(remove "Sync cache"; add admin-gated "Image cache"), `api/client.ts` (remove
`fetchCacheInfo`/`purgeCache` + `CacheInfo`/`PurgeResult` imports; add image-cache
functions), `api/types.ts` (remove `CacheRef`/`GCRules`/`CacheInfo`/`PurgeResult`; add
image-cache types; remove `cache_backend`), `views/Connect.vue` (remove `cache_backend`),
`fleet/Runners.vue` (relabel button to "Purge local cache").

### 4.3 KEEP / ADAPT — `internal/repository/registry_client.go` (+ `registry_client_test.go`)
Kept and adapted per D7: rename type+constructors, trim dead methods, update the
`domain.DistributionClient` assertion, keep the mirror surface + hardening helpers.
`registry_client_test.go` is trimmed to Ping/Catalog/Tags/ManifestSize/DeleteManifest
(catalog-disabled, delete-disabled, manifest-not-found, invalid-digest, oversized-body)
and the CLI-cache upload/manifest tests are deleted.

### 4.4 ADD (new files)
- `internal/domain/image_cache.go` (+ `image_cache_test.go` for validation/JSON)
- `internal/service/image_cache.go` + `internal/service/image_cache_test.go`
- `internal/handler/image_cache.go` + `internal/handler/image_cache_test.go`
- `ui/src/imagecache/ImageCache.vue`
- `tests/integration/image_cache_test.go`

## 5. Implementation order

Grouped so the tree compiles at each boundary; run the gate (§8) at the end.

1. **Repository** — adapt `registry_client.go`/`registry_client_test.go` (D7) and
   delete `cli_cache_registry.go`, `cache_routes_repo.go` (+tests); modify `fsm.go`,
   `fsm_snapshot.go`, `fsm_test.go`, `metrics_store.go`.
2. **Domain** — delete `cache.go`/`registry.go`; trim `config.go`, `connect.go`,
   `metrics.go`, `cli.go`; add `image_cache.go` (types + sentinels + interfaces).
3. **Observ** — remove cache metrics.
4. **Service** — delete `cache.go`, `cache_stats.go`, `registry_router.go`,
   `s3_cache_gc.go` (+tests); modify `status.go`/`status_test.go`, `history_purge.go`
   /`history_purge_test.go`, `connect_service.go`/`connect_service_test.go`,
   `test_helper_test.go`; add `image_cache.go`/`image_cache_test.go`.
5. **Handler** — remove cache routes + proxy machinery + tests; add `image_cache.go`
   + `image_cache_test.go`; update `server.go`, `metrics.go`, `server_test.go`,
   `test_helper_test.go`.
6. **Wiring** — `cmd/api/main.go` + `main_test.go`: drop cache wiring; build
   `imageCacheSvc` (per-mirror `NewDistributionClient` over `internal_addr`),
   inject `Deps.ImageCache`.
7. **Integration tests** — update the seven modified files; delete
   `cache_proxy_test.go`; prune `cache_status_test.go`; add `image_cache_test.go`
   (fake OCI Distribution v2 server (the Zot contract): list→prune→prune-all
   round-trip across multiple
   repos/tags, partial failure, empty mirror, untagged-blob caveat).
8. **UI** — router, App.vue, client.ts, types.ts, Connect.vue, Runners.vue relabel,
   delete MagicCache.vue, add ImageCache.vue.
9. **Helm + RBAC** — values.yaml, _helpers.tpl, configmap.yaml, ingress.yaml,
   rbac.yaml, image-cache.yaml, README.md.
10. **Docs** — config.app.yaml.sample, docs/README.md, ADR notes (§7).
11. **Dead-symbol sweep (§9) + verification greps (§10) + CI gate (§8).**

## 6. Edge cases / error handling / validation

- **Old Raft snapshots/logs** (D1): decode-only no-op handlers keep replay clean;
  snapshot fields drop safely (JSON ignores unknown keys). No migration/snapshot
  bump. Re-run a follower restart on the cluster to confirm no "unknown raft
  command kind" errors.
- **Stale API callers**: `GET /api/v1/cache` and `POST /api/v1/cache/purge` 404
  after removal — intended. Confirm no client/CI script still calls them.
- **UI deep links to `/cache`**: route removed; SPA catch-all still renders (verify).
  The new `/image-cache` route is admin-gated like `/admin/*`.
- **Helm values left in live deployments**: stale `supervisor.config.cache.*` keys
  are ignored by `mapstructure`; clean them from the captured values file before
  upgrade (or leave — harmless). `helm template` must still succeed.
- **`pods/log` removal safety**: no Go code reads pod logs (grep-verified); logs flow
  through Loki. Confirm the informers only need get/list/watch on pods.
- **Mirror listing**: empty mirror → `reachable:true` with empty `repositories`;
  unreachable mirror → `reachable:false` + `error`; catalog disabled →
  `reachable:true` + `error: "catalog disabled"`. Never a hard 500.
- **Prune with a digest vs a tag** (D10): digest prunes that exact revision; tag
  resolves via `ManifestSize` then deletes the resolved digest; absent manifest is
  "already pruned". Invalid `sha256:<hex>` digest → 400.
- **Delete-disabled mirror**: Zot has no delete-disabled flag (DELETE is always
  allowed, no auth configured); the `ErrRegistryDeleteDisabled` 409 mapping is kept
  as a defensive path only (a future authz config could 401/403).
- **Prune-all idempotency/safety**: empty mirror (catalog has no repos, or repos
  have no tags) → `manifests_pruned:0` no-op; a manifest already deleted (404) →
  counted "already pruned", not an error. No storage wipe/scale/restart, so there
  is no cross-mirror blast radius and no outage.
- **Prune-all truncation**: repos/tags capped (e.g. 1000/1000) + per-request
  timeouts + bounded concurrency; if truncated, the partial result is returned with
  a `Message` noting truncation.
- **Prune-all untagged/orphaned manifests+blobs**: not reachable by tag; prune-all
  leaves them; Zot's online GC reclaims their bytes automatically once unreferenced
  (≤ `gcDelay`+`gcInterval`) — documented as a caveat in the API `Message` + UI.
- **`image_cache.mirrors` empty**: the three endpoints return `{mirrors: []}` (list)
  / 404 unknown-mirror (prune) / `{mirrors: []}` (prune-all) — never a crash.
- **`imageCache.registries[].name`** removal: backward-compatible (`_helpers.tpl`
  never read it).
- **`cache.s3.*`**: retained, still required for the CLI cache (`cli.enabled`);
  `validateCLIConfig`'s S3 checks unchanged.
- **Status UI**: `Services.vue` renders one fewer row; grep to confirm nothing
  special-cases `svc.name === "cache"`.

## 7. Documentation changes

- `config/config.app.yaml.sample`: `cache:` reduced to `s3` only; drop
  `registry`/`public_host`/`internal_addr`/`auth_token`/`registries`/`gc`; add an
  `image_cache:` section documenting `mirrors[]` (admin-visible/read-only,
  chart-rendered).
- `docs/README.md`: rewrite "Remote shared cache" (drop the `cache/` prefix, GC
  sweeper, admin remote purge, "Sync cache dashboard" note); keep CLI cache, "Local
  engine cache", "Purging the local cache (admin)"; **rewrite "Local image cache
  (registry mirror)"** as a Zot section: one Zot mirror per upstream
  (`extensions.sync` on-demand pull-through, `onDemand: true`,
  `preserveDigest: true`), JSON `config.json` delivery, S3 `storageDriver` /
  PVC `rootDirectory` storage, online GC (`storage.gc`/`gcDelay`/`gcInterval`)
  semantics, and a new "Managing the image cache (admin)" subsection
  (list/prune/prune-all; prune unlinks the manifest immediately and blob bytes are
  reclaimed automatically within ~`gcDelay`+`gcInterval` — **no** offline GC
  procedure and no delete-enable flag). Update the preset mapping table (host →
  mirror address stays `<release>-<slug>-mirror.<ns>.svc:5000`) with the Zot
  `urls` upstream column, and the config table rows (`cache.*` drop
  `gc.*`/`registry`/`public_host`; add `image_cache.*` and the Zot
  `imageCache.*` mirror keys). Update the architecture diagram annotation
  (registry mirror → Zot).
- `docs/design/`: follow the "Superseded" note convention. Update ADR-012's note to
  state the Sync cache page was removed and replaced by the Image cache page;
  **update ADR-033** to record Zot (online GC, on-demand sync) instead of
  Distribution, with the rejected alternatives (Distribution v2/v3 offline-only
  stop-the-world GC; Harbor full-stack weight) and the online-GC motivation —
  retitled "Local image cache (Zot on-demand mirror)"; **add a new ADR-034
  "Admin image-cache management (list/prune/prune-all)"** recording D7–D15 (reuse
  of the Distribution-v2 API client, the config surface, the API-only prune-all
  with no storage wipe/scale/restart, and Zot's online-GC reclamation timing).
  ADR-006/014/028 already superseded — leave as-is unless a note contradicts this.

## 8. CI gate + redeploy validation (per AGENTS.local.md)

**Gate (mandatory before finishing):**
```bash
# minimum when no Docker daemon:
go build ./... && go vet ./... && go test ./...
dagger call -m ./dagger --src . lint
# full gate when a Docker daemon is available:
dagger call -m ./dagger --src . ci export --path out
```
Also build the UI and run `helm template`/`helm lint` (covered by the Dagger CI
matrix).

**Redeploy (AGENTS.local.md §4/§5):**
```bash
export KUBECONFIG=/home/user/.kube/home
docker build -t docker.io/disaster/dagger-kubernetes:dev .
docker push docker.io/disaster/dagger-kubernetes:dev
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml
# remove now-ignored cache.* keys (publicHost/authToken/gc); keep raft.clusterDomain,
# dns.nameserver, dataplane.tls.provider, noSnapshotRestoreOnStart.
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

**Live validation (agent):**
1. Pods Ready (3/3 supervisor + MinIO 1/1 + image-cache mirrors 1/1 when enabled;
   each Zot mirror Ready with `/readyz` 200).
2. Probes `/healthz` + `/readyz` 200 via port-forward.
3. API smoke: `/api/v1/status` 200 (no `cache` row), `/api/v1/connect/env` 200
   (no `cache_backend`), `/api/v1/fleet` 200; `GET /api/v1/cache` 404.
4. **Image cache**: pull an image through a mirror (e.g. `alpine:3.20`), then
   `GET /api/v1/image-cache` shows the repo+tag+size; `POST /api/v1/image-cache/prune`
   for one tag returns `pruned:1` and a re-list shows it gone; `POST
   /api/v1/image-cache/prune-all` returns aggregated totals (`manifests_pruned`,
   `repositories_processed`, `errors`) and a re-pull succeeds (re-fetches from
   upstream on demand). Confirm the mirror pod did **not** restart during
   prune-all (`kubectl get pod -o jsonpath` `restartCount`/`startTime` unchanged),
   and that the Zot `config.json` (ConfigMap `<name>-zot-config`) contains
   `"gc": true` with the configured `gcDelay`/`gcInterval`, `"onDemand": true`,
   and `"compat": ["docker2s2"]`. Then verify **blob reclamation**: after the GC
   interval elapses (`gcDelay` + `gcInterval`), confirm the S3 `image-cache`
   bucket (per-`<slug>` prefix) no longer holds the pruned image's unreferenced
   blobs (object count/size drops; `storage.dedupe` keeps shared layers).
5. UI: login; Services page (no cache row); no `/cache` nav link; Connect page
   (no `cache_backend`); Runners page shows "Purge local cache" and it still works;
   the new "Image cache" page (admin) lists mirrors + prunes.
6. Local engine purge still works: `POST /api/v1/fleet/<version>/purge-cache`.
7. Supervisor logs free of `unknown raft command kind` + fatal errors.
8. Human verification (§5.2) of the UI.

## 9. Dead-symbol sweep (keeps `golangci-lint` green)

After removal, `grep -rn` for each symbol below and delete any leftover reference.
`unused` (U1000) only flags unexported symbols; the grep sweep also catches exported
dead symbols.

Risks to re-check post-sweep:
- unexported FSM payload types/snapshot helpers (must be fully deleted).
- `basicAuthHeader`, `validOCIPathSegment`, `validDigest` (handler), the OCI regexes,
  `routeKind`, `errBackendAuth`, `cacheProxyBackendIDKey` (handler, unexported).
- `newRegistryClient`, `newTestRouter`, `newStatusServiceWithRegistry`,
  `stubCacheStatsProvider`, `stubCachePurger`, `stubCacheRouter` (test helpers).
- Trimmed client methods `ProbeManifest`, `ProbeBlob`, `ManifestCreated`, `BlobSize`,
  `UploadBlob`, `UploadBlobStream`, `PutManifest`, `GetManifest`, `GetBlob`,
  `ManifestExists`, `errUploadDigestMismatch` must be gone from `registry_client.go`.
- New image-cache symbols must each be referenced (no orphans): `ImageCacheService`,
  `NewImageCacheService`, `DistributionClient`/`NewDistributionClient`/
  `NewDistributionClientWithAuth`, the handler funcs, and the UI functions.

## 10. Verification greps

**Should be 0 matches** (Go):
`CacheStatsService`, `NewCacheStatsService`, `RegistryRouter`, `NewRegistryRouter`,
`S3CacheGC`, `NewS3CacheGC`, `CacheRoutesRepo`, `NewCacheRoutesRepo`,
`RegistryCLICache`, `NewRegistryCLICache`, `CacheBackend`, `CacheStatsProvider`,
`CachePurger`, `CacheRef`, `CacheStats`, `GCRules`, `GCRunSummary`, `PurgeResult`,
`CacheRoute`, `CacheUploadSession`, `S3Ref`, `S3CachePrefix`, `RegistryBackend`,
`SecretRef`, `GCConfig`, `CLIRegistryClient`, `CLIManifest`,
`MediaTypeOCIImageManifest`, `MediaTypeOCILayerGzip`, `MediaTypeOCIEmptyJSON`,
`MediaTypeOCIRawBlob`, `CacheMetricsClient`, `CacheHitRate`, `instantScalar`,
`ErrNoData`, `cacheHitsPromQL`, `cacheMissesPromQL`, `CacheSizeBytes`,
`CacheObjectCount`, `CachePurgeTotal`, `GCRunTotal`, `ErrNoBackend`,
`ErrRouteNotFound`, `ErrInvalidOCIPath`, `handleCacheInfo`, `handleCachePurge`,
`/api/v1/cache`, `cacheProxy`, `cacheToken`, `cacheRouter`, `cacheHostMiddleware`,
`serveCacheHost`, `routeCacheRequest`, `writeCacheRouteError`, `requireCacheAuth`,
`extractCacheToken`, `cacheProxyDirector`, `cacheProxyModifyResponse`,
`isOCIUploadLocation`, `isConfiguredCacheTarget`, `cacheProxyBackendIDKey`,
`cacheScheme`, `rewriteUploadLocation`, `routeCacheList`, `routeCacheManifest`,
`routeCacheUploadStart`, `routeCacheUploadResume`, `routeCacheBlobPull`,
`routeLeastCharged`, `recordCacheRoute`, `recordManifestRoute`,
`recordUploadStartRoute`, `recordUploadCompleteRoute`, `uploadUUIDFromResponse`,
`basicAuthHeader`, `validOCIPathSegment`, `validDigest`, `routeKind`, `errBackendAuth`,
`rePing`, `reManifest`, `reBlobUpload`, `reBlobUploadUUID`, `reBlob`, `reTags`,
`reCatalog`, `reDigest`, `invalidCacheRoute`, `CacheHost`, `CacheScheme`,
`CacheToken`, `validateCacheConfig`, `hostOf`, `probeCache`, `cacheObjectRoutes`,
`cacheBlobRoutes`, `cacheUploadSessions`, `manifestRouteKey`, `upsertManifestRoute`,
`touchManifestRoute`, `upsertBlobRoute`, `recordUpload`, `reapUploads`,
`lookupManifestRoute`, `lookupBlobRoute`, `lookupUpload`, `allCharges`,
`cmdDeleteManifestRoute`, `cmdUpsertBlobRoute`, `cmdDeleteUpload`, `cmdReapUploads`,
`cmdTouchManifestRoute`, `ObjectRoutes`, `BlobRoutes`, `Uploads`, `blobRouteEdge`,
`newRegistryClient`, `newTestRouter`, `newStatusServiceWithRegistry`,
`stubCacheStatsProvider`, `stubCachePurger`, `stubCacheRouter`,
`MirrorStorageWiper`, `MirrorDeploymentController`, `NewS3MirrorWiper`,
`NewMirrorDeploymentController`, `WipePrefix`, `RolloutRestart`, `ScaleTo`,
`S3Prefix`.
Also the renamed symbols must be gone under their old names: `RegistryStatsClient`,
`NewRegistryStatsClient`, `NewRegistryStatsClientWithAuth`, `RegistryClient`.

**Now KEPT (must appear, under their new names)** — not in the 0-match list:
`DistributionClient`, `NewDistributionClient`, `NewDistributionClientWithAuth`,
`domain.DistributionClient`, `ErrRegistryUnreachable`, `ErrRegistryDeleteDisabled`,
`ErrRegistryCatalogDisabled`, `ErrManifestNotFound` (now in `image_cache.go`),
`ImageCache*`, `NewImageCacheService`, `handleImageCacheInfo`,
`handleImageCachePrune`, `handleImageCachePruneAll`, `/api/v1/image-cache`.

**Config/Helm (0 matches in Go + templates + sample + docs):**
`cache.registry`, `cache.public_host`, `cache.internal_addr`, `cache.auth_token`,
`cache.registries`, `cache.gc`, `cachePublicHost`, `cacheRegistry`, `pods/log`,
`image_cache.s3`, `DAGGER_KUBERNETES_IMAGE_CACHE_S3_`, `s3_prefix`, `apps/deployments`,
`imageCache.ttl`, and every Distribution env key
`REGISTRY_PROXY_REMOTEURL`, `REGISTRY_PROXY_TTL`, `REGISTRY_PROXY_USERNAME`,
`REGISTRY_PROXY_PASSWORD`, `REGISTRY_STORAGE`, `REGISTRY_STORAGE_DELETE_ENABLED`,
`REGISTRY_STORAGE_MAINTENANCE_READONLY`, `REGISTRY_LOG_LEVEL`, `REGISTRY_STORAGE_S3_*`.
(`image_cache.mirrors` and the Zot `imageCache.*` keys — `imageCache.image`,
`imageCache.storage.s3`, `imageCache.gc.*`, `imageCache.sync.*`, `imageCache.dedupe`,
`imageCache.securityContext` — are expected present, the new feature.)

**UI (0 matches):** `MagicCache`, `magiccache`, `/cache`, `fetchCacheInfo`,
`purgeCache`, `CacheInfo`, `CacheRef`, `GCRules`, `PurgeResult`, `cache_backend`.

**FSM kinds retained for compatibility** (only in the reserved const block + no-op
`case`, no payload/state usage): `kindUpsertManifestRoute`, `kindDeleteManifestRoute`,
`kindUpsertBlobRoute`, `kindRecordUpload`, `kindDeleteUpload`, `kindReapUploads`,
`kindTouchManifestRoute`.

## 11. Rollout / commit

Branch `feat/s3-cache-backend`, PR #13. The completed sync-removal + image-mirror
work is the uncommitted base; commit the cleanup **on top** of it.

**Migration note (Zot swap is clean):** the Distribution-based mirror was never
enabled on the live cluster — `imageCache.enabled` defaults to `false` and the
captured values (AGENTS.local.md §7) carry no `imageCache.*` overrides — so there
is no live Distribution deployment and no cache data to migrate. The swap is a
pure chart/template change; enabling `imageCache` afterwards starts Zot fresh.

- **Commit 1:** "remove dead remote-cache / registry-proxy code and Sync cache UI"
  — §4.1/§4.2/§4.3 cleanup + §9/§10 sweep + §7 docs (code + UI + Helm + ADR notes).
  Keep it a single logical, revertible commit.
- **Commit 2:** "add admin image-cache management (list/prune/prune-all) + Zot
  mirror" — §4.4 new
  files + Helm Zot `image_cache` rendering + ADR-033 update + ADR-034 + docs. A
  separate concern from the
  deletion so each is independently reviewable/revertable.

Build/push/upgrade/rollout: §8.

## 12. Open questions / risks

- **No new RBAC:** prune-all, prune, and list all dial the mirror Service over the
  OCI Distribution v2 API directly (HTTP/TCP) — the supervisor needs **no** k8s
  API verbs
  beyond the existing set (minus the `pods/log` removal). No `apps/deployments`.
- **On-demand pull-through — VERIFIED (positive):** Zot's `extensions.sync` with
  `onDemand: true` (no `content` filter) is a synchronous pull-through equivalent of
  Distribution's `REGISTRY_PROXY_REMOTEURL`: "When an image is requested by the
  user, pull it from the upstream registries and serve it to the user." The Zot
  mirroring article confirms the first request triggers the upstream fetch and is
  served in the same request. Verified against Zot v2.1.21 docs + `examples/README.md`
  (sync section) and `articles/mirroring`. (Caveat: Zot is OCI-only — Docker digests
  are preserved only with `http.compat: ["docker2s2"]` + `preserveDigest: true`,
  both of which this plan sets.)
- **Zot online GC — VERIFIED (positive):** `storage.gc: true` + `gcDelay`
  (unreferenced-blob minimum age) + `gcInterval` (run frequency) + optional
  `gcTimeWindow` give inline/online GC; the admin guide states it is **not**
  necessary to take the registry offline. So `DELETE manifest` (prune-selected and
  prune-all) unlinks immediately and blob bytes are reclaimed automatically within
  ~`gcDelay`+`gcInterval`. No offline `registry garbage-collect` procedure and no
  Helm CronJob are added. Orphaned/untagged blobs are reclaimed by the same GC;
  documented as a caveat in the API `Message` + UI.
- **Blob reclamation timing** (only remaining nuance): the UI/API must explain that
  space is freed on the *next* GC cycle after `gcDelay`, not synchronously with the
  prune call. Chart defaults `gcDelay: "2h"`, `gcInterval: "1h"` are pinned
  explicitly so the behavior is deterministic. `storage.dedupe: true` means shared
  layers survive until their last referencing manifest is gone.
- **Per-pod local purge (non-goal):** confirmed out of scope; the fan-out already
  prunes per pod IP. A `?pod=` variant is a trivial follow-up if the operator wants it.
- **Connect env `cache_backend` removal (D4)** and **status "cache" row removal (D3)**
  remain as recommended; confirm no external CI wrapper keys off either.
- **Private upstreams (follow-up):** Zot supports them via
  `extensions.sync.credentialsFile` (mounted JSON Secret) or Docker-credentials
  auto-discovery / `credentialHelper` (e.g. `ecr`). v1 renders no `credentialsFile`
  (presets are public); wiring `imageCache.registries[].username/passwordSecretRef`
  onto a credentialsFile Secret is a documented follow-up.
