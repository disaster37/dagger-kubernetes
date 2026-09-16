# Plan: Incremental layer-based worker-cache sync (v2)

## 1. Overview / problem statement

The current implementation (`engine-cache-sync.md` v1, deployed) syncs the entire
`/var/lib/dagger/worker/` directory as a single gzip tarball pushed as one OCI
blob. Every 10-minute sync re-uploads multi-GB even when 99% of blobs are
unchanged. This plan replaces that monolithic approach with incremental,
layer-based sync that uploads only new content-store blobs.

**What changes from v1:**
- Push: walk the BuildKit content store (`content/blobs/sha256/`), probe each
  blob in the registry, upload only missing ones, then push a small metadata
  tarball (everything except `content/blobs/`) plus a multi-layer manifest.
- Pull: fetch the multi-layer manifest, restore the metadata tarball, then
  download only content blobs not already present locally.
- Temp disk: drops from multi-GB to a few MB (only the metadata tarball needs
  temp space).
- Manifest: changes from single-layer to multi-layer (metadata tarball + N
  content blobs).
- Tag: changes from `worker-<version-slug>` to `worker-v2-<version-slug>` for
  clean migration.

**What stays the same:**
- Helper env contract (§4 of v1) — no new env vars needed.
- Config surface — no new config keys.
- K8sProvider integration — init container + sidecar, same volumes/mounts.
- Concurrency model — per-version tag, last-writer-wins manifest PUT.
- Best-effort semantics — failures never block the pod.
- Stats/GC skip of the snapshot repo — unchanged.

---

## 2. Design decisions (with rationale)

### D1 — Media type for raw content blobs: `application/octet-stream`

**Choice: `application/octet-stream`** for content-store blob layers (layers
1..N in the manifest).

Rationale:
- The content-store blobs are raw, uncompressed, content-addressed files. They
  are NOT gzip tarballs. Using `MediaTypeOCILayerGzip`
  (`application/vnd.oci.image.layer.v1.tar+gzip`) would be semantically wrong
  and could confuse OCI tooling that expects to decompress such layers.
- `application/octet-stream` is the standard media type for opaque binary blobs
  in OCI registries. The existing `UploadBlobStream` already sends
  `Content-Type: application/octet-stream` on the PUT.
- The metadata tarball (layer 0) keeps `MediaTypeOCILayerGzip` — it IS a gzip
  tarball.
- Add a new constant `MediaTypeOCIRawBlob = "application/octet-stream"` to
  `internal/domain/cli.go` alongside the existing media type constants.

### D2 — Manifest shape: metadata tarball at layer 0, content blobs at layers 1..N

**Choice: layer 0 = metadata tarball, layers 1..N = raw content blobs.**

Rationale:
- The metadata tarball is always needed first (it contains `metadata.db` and
  other internals). Putting it at layer 0 makes the pull logic simple: always
  download layer 0, then conditionally download layers 1..N.
- Content blobs are ordered by digest for deterministic manifests (same input
  → same manifest). This also makes the manifest diffable for debugging.
- The empty-JSON config descriptor is unchanged (same `emptyJSONDigest` /
  `emptyJSONSize` constants from `cli_cache_registry.go`).

### D3 — Tag migration: `worker-v2-<version-slug>`

**Choice: use a new tag prefix `worker-v2-`** so old and new manifests coexist.

Rationale:
- The new manifest format (multi-layer with raw blob layers) is incompatible
  with the old single-layer format. An old `Pull` would try to untar a raw
  content blob as a gzip tarball and fail.
- Using a different tag means old pods continue to push/pull the old format
  until they are upgraded, and new pods use the new format. No migration script
  needed.
- The old tags (`worker-<version-slug>`) can be cleaned up by the existing GC
  after all pods are upgraded.
- The `WorkerSnapshotTag` function is renamed to `WorkerSnapshotTagV2` (or a
  new function is added) to produce `worker-v2-<version-slug>`. The old
  function is kept for reference but no longer called.

### D4 — Blob walking: sequential with buffered channel for probe/upload

**Choice: walk the content store sequentially, feed digests into a buffered
channel, process with a fixed-size worker pool (default 8 goroutines).**

Rationale:
- Walking `content/blobs/sha256/` is fast (directory traversal, not file reads).
  The bottleneck is the network round-trip per `ProbeBlob` call.
- A worker pool of 8 concurrent probes keeps the registry from being
  overwhelmed while providing good throughput. The pool size should be
  configurable via an env var (`CACHE_SYNC_CONCURRENCY`, default 8) for tuning.
- Sequential walk + concurrent probe is simpler than concurrent walk and avoids
  directory iterator races.
- For very large blob counts (50,000+), the channel buffer should be sized to
  the worker count (not the total blob count) to avoid memory pressure. Workers
  pull from the channel; the walker blocks when the channel is full, providing
  natural backpressure.

### D5 — Blob GC during walk: skip silently

**Choice: if a blob file disappears between the walk and the upload (BuildKit GC
deleted it), skip it silently — do not include it in the manifest.**

Rationale:
- Content-store blobs are immutable once written, but BuildKit GC can delete
  unreferenced blobs at any time. A file that existed during `WalkDir` may be
  gone by the time we `os.Open` it.
- The snapshot is best-effort. Missing a few recently-GC'd blobs is harmless —
  the engine will re-download them from the remote cache on next use.
- Log a DEBUG-level message when a blob is skipped due to file-not-found, so
  operators can monitor GC churn if needed.

### D6 — Concurrent engine writes during push: new blobs after walk starts are omitted

**Choice: blobs created after the walk completes are NOT included in the current
push. They will be picked up by the next periodic push.**

Rationale:
- This is the same "dirty snapshot" semantics as v1. The snapshot is a
  point-in-time capture of what existed when the walk started.
- Trying to capture "everything up to manifest PUT" would require a
  compare-and-swap or lock, which is rejected (would block the engine).
- The periodic push (default 10 min) ensures convergence.

### D7 — Orphaned blobs in registry: accepted, cleaned by registry GC

**Choice: do not delete blobs from the registry that are no longer referenced by
the latest manifest. The registry's own GC will clean unreferenced blobs.**

Rationale:
- OCI registries typically run GC to remove unreferenced blobs. The bundled
  `registry:2` does this.
- Deleting blobs from the registry would require tracking which blobs were in
  the previous manifest vs. the new one — complex and error-prone.
- Orphaned blobs consume storage but are harmless. If storage becomes an issue,
  the registry GC interval can be tuned.

### D8 — Corrupt metadata DB on restore: discard and start fresh

**Choice: if the metadata tarball extracts successfully but `metadata.db` is
corrupt (BoltDB fails to open), log a WARN, remove the entire worker dir, and
start with an empty cache.**

Rationale:
- A corrupt `metadata.db` means BuildKit will fail to start. The only safe
  recovery is to discard the cache and cold-start.
- This is the same behavior as v1's "discard partial extraction on failed
  untar."
- The restore init container already checks if the worker dir is empty before
  pulling; after discarding, it will be empty, so the next pod restart will
  attempt a fresh restore.

### D9 — Manifest size: acceptable up to ~50,000 layers

**Choice: a single manifest with up to 50,000 layers (~5 MB JSON) is acceptable.
If it becomes a bottleneck, the follow-up is an OCI image index with batched
sub-manifests.**

Rationale:
- 50,000 layers × ~100 bytes per layer descriptor = ~5 MB. This is fetched once
  per restore (not per sync), so the overhead is negligible.
- The `maxRegistryBody` constant (16 MiB) already accommodates this.
- If blob counts grow beyond 100,000, the manifest approaches 10 MB — still
  acceptable but worth monitoring. The plan's §10 notes the image-index
  follow-up.

---

## 3. Data structures & signatures

### 3.1 `internal/domain/cli.go` (modify)

Add a new media type constant:

```go
// MediaTypeOCIRawBlob is the media type for raw, content-addressed blobs
// (BuildKit content-store files) stored as individual OCI layers. Unlike
// MediaTypeOCILayerGzip, these are NOT gzip-compressed tarballs.
MediaTypeOCIRawBlob = "application/octet-stream"
```

### 3.2 `internal/domain/cache_sync.go` (modify)

Add `ProbeBlob` to `CacheSnapshotClient`:

```go
type CacheSnapshotClient interface {
    CLIRegistryClient

    // ProbeBlob checks whether a blob exists in the repository (HEAD request).
    // Returns (true, nil) for 200, (false, nil) for 404/405.
    ProbeBlob(ctx context.Context, repo, digest string) (bool, error)

    // UploadBlobStream uploads a blob whose sha256 digest and byte size are
    // known up front, streaming body in a single monolithic PUT.
    UploadBlobStream(ctx context.Context, repo, digest string, size int64, body io.Reader) error
}
```

Add a new tag function:

```go
// WorkerSnapshotTagV2 returns the per-version v2 snapshot tag, e.g.
// "worker-v2-v0-20-0". The "v2" prefix distinguishes the multi-layer
// incremental-sync format from the legacy single-layer format.
func WorkerSnapshotTagV2(version string) string {
    return "worker-v2-" + VersionSlug(version)
}
```

### 3.3 `internal/repository/worker_archive.go` (modify)

Add a new function for subset tarballs:

```go
// TarGzipSubset is like TarGzipDir but excludes entries whose path (relative
// to baseDir/subDir) has any prefix in excludePrefixes. Used to create the
// metadata tarball that skips the content store.
//
// Example: TarGzipSubset(ctx, "/var/lib/dagger", "worker", w, "content/blobs/")
// tars everything under worker/ except content/blobs/.
func TarGzipSubset(ctx context.Context, baseDir, subDir string, w io.Writer, excludePrefixes ...string) (digest string, size int64, err error)
```

The existing `TarGzipDir` is kept unchanged (it is still used by tests and
could be useful for other purposes). `TarGzipSubset` shares the same
`digestCountWriter` and tar-writing logic, refactored into a shared internal
helper.

### 3.4 `internal/repository/worker_snapshot.go` (modify)

Replace `Push` and `Pull` signatures:

```go
// PushIncremental walks the content store, probes+uploads missing blobs,
// creates a metadata tarball (excluding content/blobs/), and publishes a
// multi-layer manifest.
//
// workerDir is the absolute path to the worker directory (e.g.
// /var/lib/dagger/worker). tmpDir is where the metadata tarball temp file
// is written.
func (s *WorkerSnapshotStore) PushIncremental(ctx context.Context, tag, workerDir, tmpDir string) error

// PullIncremental fetches the multi-layer manifest for tag, restores the
// metadata tarball (layer 0) into dstDir, then downloads only content blobs
// (layers 1..N) that are not already present locally.
//
// dstDir is the base directory (e.g. /var/lib/dagger) — the metadata tarball
// extracts "worker/..." under it. Returns ok=false when no snapshot exists.
func (s *WorkerSnapshotStore) PullIncremental(ctx context.Context, tag, dstDir string) (bool, error)
```

The old `Push` and `Pull` methods are kept for backward compatibility during
the migration but marked as deprecated. They can be removed in a follow-up
after all pods are on v2.

### 3.5 `cmd/api/cache_sync.go` (modify)

Replace `pushWorkerSnapshot` with `pushWorkerSnapshotIncremental`:

```go
// pushWorkerSnapshotIncremental performs an incremental push: walks the
// content store, probes+uploads missing blobs, creates a metadata tarball,
// and publishes a multi-layer manifest.
func pushWorkerSnapshotIncremental(ctx context.Context, env *cacheSyncEnv, store *repository.WorkerSnapshotStore, logger *logrus.Logger) error
```

The `runCacheSyncRestore` and `runCacheSyncServe` functions are updated to:
- Use `WorkerSnapshotTagV2` instead of `WorkerSnapshotTag` for the tag.
- Call `PushIncremental` / `PullIncremental` instead of `Push` / `Pull`.
- Pass `workerDir` and `tmpDir` explicitly to `PushIncremental`.

Add a new env var for concurrency tuning:

```go
envCacheSyncConcurrency = "CACHE_SYNC_CONCURRENCY"  // default 8
```

### 3.6 `internal/repository/k8s_provider.go` (modify)

Update `syncEnv` to use `domain.WorkerSnapshotTagV2(version)` instead of
`domain.WorkerSnapshotTag(version)` for the `CACHE_SYNC_TAG` env var.

Add `CACHE_SYNC_CONCURRENCY` env var to the serve role (not needed for
restore, which only pulls).

No other K8sProvider changes needed — the container spec, volumes, mounts,
and security context are identical.

---

## 4. Helper env contract (same as v1, plus one new var)

| Env var | Default | Used by |
|---|---|---|
| `CACHE_SYNC_REGISTRY_ADDR` | (required) | both |
| `CACHE_SYNC_REGISTRY_USER` | `""` | both |
| `CACHE_SYNC_REGISTRY_PASS` | `""` | both |
| `CACHE_SYNC_REPO` | `dagger-cache/worker-snapshots` | both |
| `CACHE_SYNC_TAG` | `worker-v2-<version-slug>` | both |
| `CACHE_SYNC_BASE_DIR` | `/var/lib/dagger` | both |
| `CACHE_SYNC_WORKER_SUBDIR` | `worker` | both |
| `CACHE_SYNC_TMP_DIR` | `/tmp` | serve |
| `CACHE_SYNC_INTERVAL` | `"10m"` (`"0"` disables) | serve |
| `CACHE_SYNC_QUIESCE_WAIT` | `"10s"` | serve |
| `CACHE_SYNC_CONCURRENCY` | `"8"` | serve |

The `CACHE_SYNC_CONCURRENCY` env var controls the number of concurrent
`ProbeBlob` + `UploadBlobStream` goroutines during push. It is only needed
for the serve role (restore downloads sequentially, which is fine for a
one-time operation).

---

## 5. Files to create / modify

**Create:**
- `internal/repository/worker_sync.go` — new file with the incremental
  push/pull logic: `walkContentStore`, `probeAndUploadMissing`,
  `downloadMissingBlobs`, `buildMultiLayerManifest`.

**Modify:**
- `internal/domain/cli.go` — add `MediaTypeOCIRawBlob` constant.
- `internal/domain/cache_sync.go` — add `ProbeBlob` to `CacheSnapshotClient`;
  add `WorkerSnapshotTagV2`.
- `internal/repository/worker_archive.go` — add `TarGzipSubset`; refactor
  shared tar logic into an internal helper.
- `internal/repository/worker_snapshot.go` — add `PushIncremental` and
  `PullIncremental` methods; keep old `Push`/`Pull` as deprecated.
- `cmd/api/cache_sync.go` — update `pushWorkerSnapshot` →
  `pushWorkerSnapshotIncremental`; use `WorkerSnapshotTagV2`; add
  `CACHE_SYNC_CONCURRENCY` env var; update `runCacheSyncRestore` and
  `runCacheSyncServe`.
- `internal/repository/k8s_provider.go` — update `syncEnv` to use
  `WorkerSnapshotTagV2`; add `CACHE_SYNC_CONCURRENCY` to serve env.
- `internal/repository/worker_snapshot_test.go` — add tests for
  `PushIncremental`/`PullIncremental` with multi-layer manifests.
- `internal/repository/worker_archive_test.go` — add tests for `TarGzipSubset`.
- `cmd/api/cache_sync_test.go` — update tests for v2 tag and incremental
  push/pull.
- `internal/repository/k8s_provider_test.go` — update tag assertion to
  `worker-v2-<slug>`.
- `docs/README.md` — update worker-cache sync section to describe incremental
  sync and v2 tag format.
- `config/config.app.yaml.sample` — no changes needed (no new config keys).

**No change needed:**
- `config/loader.go` — no new config keys.
- `internal/domain/config.go` — no new config fields.
- `internal/service/cache_stats.go` — snapshot-repo skip unchanged.
- `deploy/helm/` — no chart changes (tag is computed in code, not in values).
- `Dockerfile`, `dagger/main.go`, `DAGGER.md` — no new binary or CI changes.

---

## 6. Step-by-step implementation order

1. **Domain — media type + interface:** add `MediaTypeOCIRawBlob` to
   `internal/domain/cli.go`. Add `ProbeBlob` to `CacheSnapshotClient` in
   `internal/domain/cache_sync.go`. Add `WorkerSnapshotTagV2` function.

2. **Repository — `TarGzipSubset`:** refactor `worker_archive.go` to extract
   shared tar logic into an internal `tarGzipWalk` helper. Implement
   `TarGzipSubset` using the helper with exclude-prefix filtering. Add tests
   in `worker_archive_test.go` (exclusion works, empty result when everything
   excluded, nested prefix matching).

3. **Repository — incremental push/pull (`worker_sync.go`):** implement:
   - `walkContentStore(baseDir string) ([]string, error)` — walks
     `content/blobs/sha256/`, returns sorted digest list.
   - `probeAndUploadMissing(ctx, client, repo, digests []string, concurrency int, logger)` —
     worker pool that probes each digest, uploads missing ones via
     `UploadBlobStream`, returns the list of successfully uploaded/skipped
     digests.
   - `downloadMissingBlobs(ctx, client, repo, layers []CLIManifestLayer, dstDir string, logger)` —
     for each layer, check if the blob file exists locally at the correct
     path; if not, download via `GetBlob` and write.
   - `buildMultiLayerManifest(metaDigest string, metaSize int64, blobLayers []CLIManifestLayer) *CLIManifest` —
     constructs the manifest with layer 0 = metadata tarball, layers 1..N =
     content blobs.

4. **Repository — `PushIncremental` / `PullIncremental`:** add methods to
   `WorkerSnapshotStore` in `worker_snapshot.go`. `PushIncremental` calls
   `walkContentStore` → `probeAndUploadMissing` → `TarGzipSubset` →
   `UploadBlobStream` (for metadata tarball) → `buildMultiLayerManifest` →
   `PutManifest`. `PullIncremental` calls `GetManifest` → `GetBlob` (layer 0)
   → `UntarGzipDir` → `downloadMissingBlobs`.

5. **CLI — update helper:** in `cmd/api/cache_sync.go`:
   - Add `envCacheSyncConcurrency` constant and `concurrency` field to
     `cacheSyncEnv`.
   - Replace `pushWorkerSnapshot` with `pushWorkerSnapshotIncremental` that
     calls `store.PushIncremental`.
   - Update `runCacheSyncRestore` to call `store.PullIncremental`.
   - Update `runCacheSyncServe` to call `pushWorkerSnapshotIncremental`.
   - Use `domain.WorkerSnapshotTagV2` for the tag.

6. **K8sProvider — update tag:** in `k8s_provider.go`, change
   `domain.WorkerSnapshotTag(version)` to `domain.WorkerSnapshotTagV2(version)`
   in `syncEnv`. Add `CACHE_SYNC_CONCURRENCY` env var to the serve role.

7. **Tests — update existing:** update `worker_snapshot_test.go` to test
   `PushIncremental`/`PullIncremental` with multi-layer manifests. Update
   `k8s_provider_test.go` tag assertions. Update `cache_sync_test.go` for v2
   tag and incremental flow.

8. **Tests — new:** add tests in `worker_sync_test.go` for:
   - `walkContentStore` with various directory structures.
   - `probeAndUploadMissing` with stub client (some blobs exist, some missing,
     some upload errors).
   - `downloadMissingBlobs` with existing and missing local files.
   - `buildMultiLayerManifest` shape validation.
   - End-to-end `PushIncremental` → `PullIncremental` round-trip.

9. **Docs:** update `docs/README.md` worker-cache sync section to describe
   incremental sync, v2 tag format, and the new `CACHE_SYNC_CONCURRENCY` env
   var.

10. **CI gate:** `go build ./... && go vet ./... && go test ./...` plus
    `dagger call -m ./dagger --src . lint`. Full `ci export` if Docker
    daemon available. Then mandatory local redeploy per `AGENTS.local.md`.

---

## 7. Edge cases, error handling & validation

### 7.1 Push edge cases

- **Empty content store:** `walkContentStore` returns an empty list. The
  manifest has only layer 0 (metadata tarball). This is valid — a fresh
  engine with no cached blobs.

- **Blob deleted by GC during walk:** `os.Open` fails with `os.ErrNotExist`
  when trying to upload. Log DEBUG, skip the blob, do not include it in the
  manifest. The probe/upload worker must handle this gracefully.

- **Blob deleted by GC between probe and upload:** the probe returns 404
  (blob not in registry), but the local file is also gone. Same handling:
  skip silently.

- **ProbeBlob returns error (registry unreachable):** the entire push fails.
  This is the same behavior as v1 — a failed push logs a warning and the
  sidecar continues. The next periodic push will retry.

- **UploadBlobStream fails mid-upload:** the blob is not included in the
  manifest. The registry may have a partial upload that will be GC'd. The
  next periodic push will retry the blob.

- **Metadata tarball upload fails:** the manifest is not published. The
  previous snapshot remains intact. Same as v1.

- **Manifest PUT fails:** same as v1 — previous snapshot intact.

- **Very large blob counts (50,000+):** the manifest JSON is ~5 MB. The
  `maxRegistryBody` limit (16 MiB) is not hit. The `PutManifest` body is
  well within limits. The walk itself is O(blobs) directory traversal, which
  is fast.

- **Concurrent pushes to same tag:** same as v1 — last-writer-wins manifest
  PUT. Blob uploads are idempotent (content-addressed). The only "loss" is
  that non-winning pods' unique blobs are not referenced by the winning
  manifest — they become orphaned and are cleaned by registry GC.

### 7.2 Pull edge cases

- **Manifest not found:** `PullIncremental` returns `ok=false`. Same as v1.

- **Metadata tarball blob missing (manifest references a GC'd blob):**
  `GetBlob` returns `ErrManifestNotFound`. `PullIncremental` returns an
  error. The restore init container logs a warning and starts with an empty
  cache.

- **Content blob missing from registry:** `GetBlob` returns
  `ErrManifestNotFound`. Log DEBUG, skip that blob. The engine will
  re-download it from the remote cache on next use.

- **Content blob already exists locally:** check by computing the expected
  file path (`content/blobs/sha256/<first-two-hex>/<full-hex>`) and calling
  `os.Stat`. If the file exists, skip the download. Do NOT verify the
  content hash (too expensive for large blobs) — trust the file name.

- **Corrupt metadata DB after untar:** the restore init container should
  attempt a basic BoltDB open to verify integrity. If it fails, remove the
  worker dir and return `ok=false` so the engine starts with an empty cache.
  (This is a new validation step not present in v1.)

- **Disk full during restore:** `GetBlob` writes to disk via `io.Copy`. If
  the disk is full, the write fails and `PullIncremental` returns an error.
  The partial extraction is discarded (same as v1).

### 7.3 Validation rules

- `CACHE_SYNC_CONCURRENCY` must be >= 1 and <= 64. Default 8.
- Content blob digests must match `sha256:<64 hex>` (validated by
  `validDigest` before any registry call).
- Manifest layer count must be >= 1 (at least the metadata tarball).
- Metadata tarball must not be empty (a worker dir always has at least
  `metadata.db`).

---

## 8. Testing plan

### 8.1 Unit tests (stdlib `testing`, table-driven)

**`worker_archive_test.go` (extend):**
- `TestTarGzipSubsetExcludesPrefix`: create a dir with `content/blobs/` and
  other files; verify `content/blobs/` entries are absent from the tarball.
- `TestTarGzipSubsetEmptyResult`: exclude everything; verify the tarball
  contains only the root directory entry.
- `TestTarGzipSubsetNestedPrefix`: verify prefix matching is path-aware
  (excluding `content/blobs/` does not exclude `content/other/`).

**`worker_sync_test.go` (create):**
- `TestWalkContentStore`: create a realistic `content/blobs/sha256/` tree;
  verify the returned digest list is sorted and complete.
- `TestWalkContentStoreEmpty`: empty content store returns empty list.
- `TestProbeAndUploadMissing`: stub client where some blobs exist (ProbeBlob
  returns true), some are missing (returns false), some fail (returns error).
  Verify only missing blobs are uploaded.
- `TestProbeAndUploadMissingFileNotFound`: stub client where ProbeBlob returns
  false, but the local file is missing — verify the blob is skipped silently.
- `TestDownloadMissingBlobs`: some blobs exist locally, some don't. Verify
  only missing ones are downloaded.
- `TestBuildMultiLayerManifest`: verify schemaVersion=2, config is empty-JSON,
  layer 0 is metadata tarball with `MediaTypeOCILayerGzip`, layers 1..N are
  raw blobs with `MediaTypeOCIRawBlob`.
- `TestPushIncrementalPullIncrementalRoundTrip`: full end-to-end with a stub
  client. Create a worker dir with metadata + content blobs, push, pull into
  a fresh dir, verify tree equality.

**`worker_snapshot_test.go` (extend):**
- Update `stubSnapshotClient` to implement `ProbeBlob`.
- `TestPushIncrementalMultiLayer`: verify the manifest has N+1 layers (1
  metadata + N content blobs).
- `TestPullIncrementalSkipsExistingBlobs`: pre-create some blob files in the
  destination; verify they are not re-downloaded.

**`cache_sync_test.go` (extend):**
- Update tag assertions to `worker-v2-<slug>`.
- `TestLoadCacheSyncEnvConcurrency`: verify `CACHE_SYNC_CONCURRENCY` parsing
  and default.

**`k8s_provider_test.go` (extend):**
- Update tag assertions in sync env tests to `worker-v2-<slug>`.
- Verify `CACHE_SYNC_CONCURRENCY` is set in serve env.

### 8.2 Integration tests

- Extend the existing integration test that stands up a minimal OCI registry
  to test multi-layer manifest push/pull with real HTTP.
- Test concurrent pushes to the same tag (last-writer-wins, no corruption).

### 8.3 Mandatory local validation (AGENTS.local.md)

Same as v1 §8:
1. Build and push the dev image.
2. Helm upgrade with captured values.
3. Rollout restart supervisor.
4. Agent checks: pods Ready, healthz/readyz 200, API endpoints.
5. Trigger engine fleet, scale to zero, verify `worker-v2-<version>` tag
   appears in registry, scale back up, verify restore logs.
6. Human verification of live UI.

---

## 9. Documentation changes

- `docs/README.md`: update "Worker-cache sync (warm start)" section:
  - Describe incremental sync (only new blobs uploaded).
  - Document v2 tag format (`worker-v2-<version-slug>`).
  - Add `CACHE_SYNC_CONCURRENCY` to the env var table.
  - Note that old `worker-<version-slug>` tags are from v1 and will be GC'd.
- `config/config.app.yaml.sample`: no changes (no new config keys).
- `deploy/helm/`: no changes (tag is computed in code).
- No `DAGGER.md` change (no dagger/ or CI changes).

---

## 10. Out of scope / follow-ups

- **OCI image index for very large blob counts:** if blob counts exceed
  100,000, the manifest approaches 10 MB. The follow-up is to publish an OCI
  image index (`application/vnd.oci.image.index.v1+json`) with batched
  sub-manifests of ~10,000 layers each. The pull logic would fetch the index,
  then fetch each sub-manifest. This is a pure additive change — the
  multi-layer manifest format is forward-compatible with an index.

- **BoltDB integrity check on restore:** the plan mentions a basic `bolt.Open`
  check after untar. This is a nice-to-have; if `metadata.db` is corrupt,
  BoltDB will fail to open and the engine will fail to start regardless. The
  explicit check just provides a better error message.

- **Prometheus metrics for incremental sync:** track blobs probed, blobs
  uploaded, bytes uploaded, metadata tarball size, push duration. Same as v1
  out-of-scope.

- **Compression for content blobs:** content-store blobs may already be
  compressed (container image layers). Re-compressing them would waste CPU
  and could even increase size. Out of scope.

- **Removing old v1 `Push`/`Pull` methods:** after all pods are upgraded to
  v2 and old `worker-<version>` tags are GC'd, the deprecated methods can be
  removed. Tracked as a follow-up cleanup.

- **Secret-ref injection for registry credentials:** same as v1 out-of-scope.
