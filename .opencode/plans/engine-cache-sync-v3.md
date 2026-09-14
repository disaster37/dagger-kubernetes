# Plan: S3-backed cache sync (v3) — worker snapshots, CLI cache, and GC

## 1. Overview / problem statement

The v1 and v2 implementations sync the BuildKit worker cache through the OCI
registry. The Dagger CLI cache (`dagger-kubernetes/cli-cache`) also lives in
the OCI registry. The registry-based GC sweeper (`RunGC`/`Purge`) walks the
registry catalog to find and delete stale cache refs.

This plan migrates ALL three concerns to S3:

1. **Worker-cache snapshots** — incremental sync of BuildKit worker dirs
2. **CLI cache** — Dagger CLI tarballs served to CI clients
3. **Cache GC** — automatic cleanup of stale cache refs

The OCI registry becomes optional: deployments that use S3 for all three can
remove the registry entirely. Deployments that prefer the registry can keep
using v2 for worker snapshots and the existing registry-backed CLI cache and
GC.

**Why S3 is a better fit for all three:**

- **Worker snapshots:** Content-store blobs are content-addressed files. S3's
  flat key-value store with prefix listing is a better fit than OCI manifests
  that grow linearly with blob count.
- **CLI cache:** CLI tarballs are immutable, versioned artifacts. S3 objects
  with `If-None-Match` / `ETag` provide natural deduplication. No OCI manifest
  overhead.
- **Cache GC:** S3 objects have built-in `LastModified` timestamps. No need
  for VictoriaMetrics hit counters — the object's own metadata tells us when
  it was last written. S3 lifecycle policies can handle bulk cleanup.

**What changes from v2:**

- **Worker snapshots:** OCI registry → S3 prefix listing. Per-version
  isolation (all pods in the same StatefulSet share one snapshot prefix).
  No per-pod isolation — concurrent pushes to the same prefix are safe.
- **CLI cache:** New `S3CLICache` implementation of `domain.CLICache`. Stores
  tarballs at `s3://<bucket>/cli-cache/<version>/<os>/<arch>/<filename>`.
- **Cache GC:** New `S3CacheGC` sweeper that lists objects under the cache
  prefix, checks `LastModified`, and deletes stale objects. Replaces the
  registry catalog-based GC.
- **Auth:** S3 access key + secret key from Kubernetes secrets, injected as
  env vars (same pattern as `engine-registry-auth`).

**Storage layout:**

```
s3://<bucket>/
  worker-snapshots/<version-slug>/
    meta.tar.gz              # metadata DB + snapshots + leases
    blobs/sha256/<first-two-hex>/<full-hex>  # individual content blobs
  cli-cache/<version>/<os>/<arch>/<filename>  # Dagger CLI tarballs
  cache/                                      # BuildKit remote cache (existing, unchanged)
```

**What stays the same:**

- Helper env contract (§4) — expanded with S3 credentials and endpoint.
- Config surface — new `cache.sync.s3_*` and `cli.s3_*` keys, but existing
  keys unchanged.
- K8sProvider integration — init container + sidecar, same volumes/mounts.
- Best-effort semantics — failures never block the pod.
- Periodic push interval and quiesce-wait — unchanged.
- Restore skip when worker dir already has content — unchanged.

---

## 2. Design decisions (with rationale)

### D1 — S3 client library: `minio-go/v7`

**Choice: `github.com/minio/minio-go/v7`** — the de facto standard Go S3
client. It works with ANY S3-compatible store (MinIO, SeaweedFS, Garage, Ceph
RGW, AWS S3). The library choice does not lock us into MinIO specifically.

Rationale:
- `minio-go/v7` is the most widely used Go S3 client (12k+ stars, active
  maintenance).
- It supports custom endpoints (not just AWS), which is essential for
  self-hosted S3-compatible stores.
- It provides `PutObject`, `GetObject`, `StatObject` (HEAD), `ListObjects`,
  and `RemoveObject` — all the operations we need.
- The `ListObjects` API returns `ObjectInfo` with `LastModified`, which we
  need for GC staleness checks.
- It handles credential chaining (env vars, IAM, static keys) and supports
  custom transport for timeouts.

### D2 — Snapshot format: prefix listing, no index object

**Choice: Option 3 (prefix listing, no index object).** S3 LIST replaces the
manifest. Each blob upload is independently atomic. The metadata tarball is
the only mutable object.

Rationale:
- **No manifest means no manifest size ceiling.** The v2 manifest grows
  linearly with blob count. S3 LIST has no such limit — it returns paginated
  results regardless of how many objects exist under a prefix.
- **Each blob is independently atomic.** An S3 `PutObject` either succeeds or
  fails; there is no cross-blob consistency to maintain. This eliminates the
  "manifest references a GC'd blob" failure mode from v2.
- **The metadata tarball is the only mutable object.** It is overwritten on
  each push. If the overwrite fails, the previous `meta.tar.gz` remains
  intact — same safety property as the v2 manifest PUT.
- **No OCI layer descriptors.** We don't need to track media types, sizes, or
  digests for individual blobs. The blob's key IS its content address
  (`sha256:<hex>`), and the file content IS the blob. No metadata duplication.

### D3 — Storage layout

```
s3://<bucket>/worker-snapshots/<version-slug>/
  meta.tar.gz
  blobs/sha256/<first-two-hex>/<full-hex>
```

Rationale:
- **`<version-slug>/`** groups snapshots by engine version (same as v1/v2
  per-version tags). Cross-version restore is still a non-goal.
- **No per-pod prefix.** All pods in the same StatefulSet (same Dagger engine
  version) share one snapshot prefix. Concurrent pushes are safe: blob uploads
  are idempotent (content-addressed), and the metadata tarball is
  last-writer-wins (same as v1/v2 manifest PUT).
- **Prunes propagate naturally.** If pod A prunes and syncs, the new
  `meta.tar.gz` reflects the pruned state. Pod B pulling this gets the pruned
  cache. This is desirable — all pods in the same StatefulSet should have
  consistent cache state.
- **`meta.tar.gz`** is the metadata tarball (everything under
  `/var/lib/dagger/worker/` except `content/blobs/`). It is the only object
  that is overwritten on each push.
- **`blobs/sha256/<first-two-hex>/<full-hex>`** mirrors the BuildKit content
  store layout on disk. This makes the download path trivial: the expected
  local path is `content/blobs/sha256/<first-two-hex>/<full-hex>`, and the S3
  key is `blobs/sha256/<first-two-hex>/<full-hex>`. No mapping needed.

### D4 — Auth: S3 access key + secret key from Kubernetes secrets

**Choice: S3 access key + secret key from Kubernetes secrets, injected as env
vars on the init/sidecar containers.** Same pattern as the existing
`engine-registry-auth` secret.

Rationale:
- The existing `engine-registry-auth` secret already holds registry
  credentials. For S3, we need a separate secret (or additional keys in the
  same secret) with `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.
- The K8sProvider already renders env vars from secrets for the engine
  container (`engineEnv`, `secretEnvVar`). The same pattern applies to the
  sync containers.
- `minio-go/v7` reads `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from
  the environment by default, or they can be passed explicitly via
  `minio.Options`.
- The S3 endpoint is also passed as an env var (`CACHE_SYNC_S3_ENDPOINT`),
  since self-hosted S3-compatible stores have custom endpoints.

**Config surface (new keys in `cache.sync`):**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `cache.sync.s3_endpoint` | string | `""` | S3-compatible endpoint (e.g., `minio.example.com:9000`). Required when `cache.backend` is `s3`. |
| `cache.sync.s3_bucket` | string | `""` | S3 bucket name. Defaults to `cache.s3.bucket` if empty. |
| `cache.sync.s3_region` | string | `"us-east-1"` | S3 region (required by AWS S3; ignored by MinIO). |
| `cache.sync.s3_use_ssl` | bool | `true` | Use HTTPS for the S3 endpoint. |
| `cache.sync.s3_access_key` | string | `""` | S3 access key (env var `CACHE_SYNC_S3_ACCESS_KEY`). |
| `cache.sync.s3_secret_key` | string | `""` | S3 secret key (env var `CACHE_SYNC_S3_SECRET_KEY`). |

The K8sProvider resolves these from the supervisor config and injects them as
env vars on the sync containers. The access key and secret key are sourced
from a Kubernetes secret (not literal values in the pod spec).

### D5 — Cleanup: S3 lifecycle policies + self-cleanup on startup

> **Superseded (2026-09-14):** startup self-cleanup was removed. It deleted the
> entire version prefix, so concurrent pods of the same version wiped each
> other's snapshots and a pod killed before its next push lost the shared
> snapshot. Blobs are content-addressed (idempotent) and `meta.tar.gz` is
> overwritten atomically, so orphaned blobs are harmless; a bucket lifecycle
> policy reclaims them instead. The text below is the historical decision
> record.

**Choice: Rely on S3 bucket lifecycle policies for old snapshots. The
sidecar also cleans up its own old blobs on startup (delete previous blobs
under the version prefix before pushing new ones).**

Rationale:
- **Lifecycle policies** are the standard S3 mechanism for automatic cleanup.
  Operators configure a rule like "delete objects under
  `worker-snapshots/` older than 7 days." This handles pods that were
  deleted without a graceful shutdown (no final cleanup).
- **Self-cleanup on startup** ensures that old blobs don't accumulate across
  restarts. Before the first periodic push, the sidecar lists objects under
  the version prefix and deletes them. This is a fast operation (a few dozen
  objects at most).
- The sidecar does NOT clean up on shutdown (SIGTERM handler). The final push
  overwrites `meta.tar.gz` and uploads new blobs, but old blobs from previous
  pushes are left for the next startup's self-cleanup or the lifecycle policy.

### D6 — Concurrent writes during push: same as v2

**Choice: blobs created after the content store walk completes are NOT
included in the current push. They will be picked up by the next periodic
push.**

Rationale: Same as v2 D6. The snapshot is a point-in-time capture. The
periodic push ensures convergence.

### D7 — Blob GC during walk: skip silently

**Choice: if a blob file disappears between the walk and the upload (BuildKit
GC deleted it), skip it silently.**

Rationale: Same as v2 D5. Content-store blobs are immutable once written, but
BuildKit GC can delete unreferenced blobs at any time.

### D8 — Corrupt metadata DB on restore: discard and start fresh

**Choice: if the metadata tarball extracts successfully but `metadata.db` is
corrupt (BoltDB fails to open), log a WARN, remove the entire worker dir, and
start with an empty cache.**

Rationale: Same as v2 D8.

### D9 — Backend selection: registry vs S3

**Choice: The supervisor selects the backend per subsystem based on
`cache.backend`: `"registry"` → v2 worker snapshots + registry CLI cache +
registry GC; `"s3"` → v3 worker snapshots + S3 CLI cache + S3 GC.**

Rationale:
- This avoids a big-bang migration. Existing registry-backed deployments
  continue to work. New S3-backed deployments get the v3 benefits.
- The v2 code path (`PushIncremental`/`PullIncremental`, `RegistryCLICache`,
  registry-based `RunGC`/`Purge`) is kept intact.
- The S3 code path is additive: new `S3SnapshotStore`, `S3CLICache`, and
  `S3CacheGC` implementations alongside the existing registry ones.

### D10 — CLI cache migration to S3

**Choice: Implement `S3CLICache` as a new implementation of
`domain.CLICache`. Store tarballs at
`s3://<bucket>/cli-cache/<version>/<os>/<arch>/<filename>`.**

Rationale:
- The `CLICache` interface (`Has`, `Get`, `Put`, `Dir`) is already clean and
  backend-agnostic. Adding an S3 implementation requires no interface changes.
- S3 objects are immutable by key. The key includes version, OS, and arch, so
  each artifact has a unique, deterministic key. `Has` becomes an S3 `StatObject`
  (HEAD). `Get` becomes an S3 `GetObject`. `Put` becomes an S3 `PutObject`.
- The `sha256Hex` verification in `Put` is preserved: the tarball is read into
  memory (CLI tarballs are ~50 MB, acceptable), the hash is verified, then
  uploaded to S3.
- The `Dir()` method returns `""` (same as `RegistryCLICache`), indicating
  the cache is remote (no local directory).
- No OCI manifest, no empty-JSON config blob, no layer descriptors. Just a
  flat file at a deterministic key.

**Storage layout for CLI cache:**

```
s3://<bucket>/cli-cache/<version>/<os>/<arch>/<filename>
```

Example: `s3://my-bucket/cli-cache/v0.21.8/linux/amd64/dagger_v0.21.8_linux_amd64.tar.gz`

**Config surface (new keys in `cli`):**

| Key | Type | Default | Meaning |
|---|---|---|---|
| `cli.s3_bucket` | string | `""` | S3 bucket for CLI cache. Defaults to `cache.s3.bucket` if empty. |
| `cli.s3_prefix` | string | `"cli-cache"` | S3 key prefix for CLI tarballs. |

The S3 client (endpoint, region, SSL, credentials) is shared with the worker
snapshot store — configured once via `cache.sync.s3_*`.

### D11 — S3 cache GC mechanism

**Choice: Implement `S3CacheGC` as a sweeper that lists objects under the
BuildKit cache prefix, checks `LastModified`, and deletes objects older than
`MaxAge`. Replaces the registry catalog-based `RunGC`/`Purge`.**

Rationale:
- The existing GC (`RunGC`) works by: catalog → list tags → check
  VictoriaMetrics for last-used time → delete old manifests. This is
  registry-specific.
- For S3, the approach is simpler: LIST objects under the cache prefix
  (e.g., `s3://<bucket>/cache/`), check each object's `LastModified`, and
  delete objects older than `MaxAge`. No VictoriaMetrics needed — the S3
  object's own `LastModified` is the authoritative timestamp.
- The BuildKit remote cache (`:cache` tag in the registry, or the S3 cache
  prefix) is the source of truth for cache refs. The GC sweeps stale refs
  from this prefix.
- The worker snapshot prefix (`worker-snapshots/`) and CLI cache prefix
  (`cli-cache/`) are excluded from GC — they have their own cleanup
  mechanisms (lifecycle policies + self-cleanup for worker snapshots; CLI
  cache is versioned and immutable).
- The `Purge` (manual purge all) operation becomes: LIST all objects under
  the cache prefix → delete all. Same `maxPurgeAllTags` cap (1000 objects)
  for safety.
- The `GCRules` API response is updated to reflect S3-backed GC (no registry
  catalog dependency).

**GC storage layout:**

```
s3://<bucket>/cache/   # BuildKit remote cache refs — swept by GC
```

The GC sweeper is a background goroutine (same as the existing
`StartGCSweeper`) that runs on the configured `Schedule`. It uses the same
`MaxAge` config key (`cache.gc.max_age`).

**S3 lifecycle policy as defense-in-depth:**

Even with the GC sweeper, operators should configure an S3 lifecycle policy
on the bucket to delete objects older than N days. This handles:
- Objects that the GC sweeper misses (e.g., supervisor was down).
- Objects in prefixes the GC sweeper doesn't cover (e.g., old worker
  snapshots from deleted pods).
- The lifecycle policy is a bucket-level configuration, not code.

---

## 3. Data structures & signatures

### 3.1 `internal/domain/cache_sync.go` (modify)

Add S3-related constants and a helper function:

```go
const (
    // WorkerSnapshotsPrefix is the S3 key prefix for worker snapshots.
    WorkerSnapshotsPrefix = "worker-snapshots"

    // metaTarballName is the metadata tarball object name under each version prefix.
    metaTarballName = "meta.tar.gz"

    // blobsPrefix is the S3 key prefix for content blobs under each version prefix.
    blobsPrefix = "blobs/sha256/"
)

// WorkerSnapshotS3Prefix returns the S3 key prefix for a version's snapshots,
// e.g. "worker-snapshots/v0-20-0/".
func WorkerSnapshotS3Prefix(version string) string {
    return WorkerSnapshotsPrefix + "/" + VersionSlug(version) + "/"
}
```

Add `ProbeBlob` to `CacheSnapshotClient` (needed by v2, already implemented
by `RegistryStatsClient`):

```go
type CacheSnapshotClient interface {
    CLIRegistryClient

    // ProbeBlob checks whether a blob exists in the repository (HEAD request).
    ProbeBlob(ctx context.Context, repo, digest string) (bool, error)

    // UploadBlobStream uploads a blob whose sha256 digest and byte size are
    // known up front, streaming body in a single monolithic PUT.
    UploadBlobStream(ctx context.Context, repo, digest string, size int64, body io.Reader) error
}
```

### 3.2 `internal/domain/config.go` (modify)

Add S3 sync config to `CacheSyncConfig`:

```go
type CacheSyncConfig struct {
    Enabled     bool          `mapstructure:"enabled"`
    OnStart     bool          `mapstructure:"on_start"`
    OnStop      bool          `mapstructure:"on_stop"`
    Interval    time.Duration `mapstructure:"interval"`
    QuiesceWait time.Duration `mapstructure:"quiesce_wait"`

    // S3-specific settings (only used when cache.backend is "s3").
    S3Endpoint  string `mapstructure:"s3_endpoint"`
    S3Bucket    string `mapstructure:"s3_bucket"`
    S3Region    string `mapstructure:"s3_region"`
    S3UseSSL    bool   `mapstructure:"s3_use_ssl"`
    S3AccessKey string `mapstructure:"s3_access_key"`
    S3SecretKey string `mapstructure:"s3_secret_key"`
}
```

### 3.3 `internal/repository/s3_snapshot.go` (create)

New file with the S3-backed snapshot store:

```go
// S3SnapshotStore pushes/pulls BuildKit worker-dir snapshots to/from an
// S3-compatible object store. All pods in the same StatefulSet (same version)
// share one snapshot prefix. Concurrent pushes are safe: blob uploads are
// idempotent, and the metadata tarball is last-writer-wins.
type S3SnapshotStore struct {
    client *minio.Client
    bucket string
    prefix string // e.g. "worker-snapshots/v0-20-0/"
    logger *logrus.Logger
}

func NewS3SnapshotStore(client *minio.Client, bucket, version string, logger *logrus.Logger) *S3SnapshotStore

// Push walks the content store, uploads missing blobs to S3, creates a
// metadata tarball, and uploads it as meta.tar.gz.
func (s *S3SnapshotStore) Push(ctx context.Context, workerDir, tmpDir string) error

// Pull downloads the meta.tar.gz and missing content blobs from the version
// prefix into dstDir. Returns ok=false when no snapshot exists.
func (s *S3SnapshotStore) Pull(ctx context.Context, dstDir string) (bool, error)

// CleanupSelf deletes all objects under this version prefix. Called on startup
// before the first push.
func (s *S3SnapshotStore) CleanupSelf(ctx context.Context) error
```

### 3.4 `internal/repository/s3_cli_cache.go` (create)

New file with the S3-backed CLI cache:

```go
// S3CLICache stores verified CLI tarballs on an S3-compatible object store so
// every supervisor pod in a multi-node Raft cluster can serve cached binaries.
type S3CLICache struct {
    client *minio.Client
    bucket string
    prefix string // e.g. "cli-cache"
    logger *logrus.Logger
}

// NewS3CLICache returns a cache backed by the supplied S3 client.
func NewS3CLICache(client *minio.Client, bucket, prefix string, logger *logrus.Logger) *S3CLICache

// Has reports whether the artifact exists (S3 StatObject).
func (c *S3CLICache) Has(ctx context.Context, version, osName, arch string) (bool, error)

// Get downloads the artifact from S3 to a temp file and returns the local path.
func (c *S3CLICache) Get(ctx context.Context, version, osName, arch string) (string, bool)

// Put uploads the tarball to S3 after verifying sha256Hex.
func (c *S3CLICache) Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (string, error)

// Dir returns "" for S3-backed cache.
func (c *S3CLICache) Dir() string
```

The S3 key for a CLI artifact is:
```
<prefix>/<version>/<os>/<arch>/<filename>
```
Example: `cli-cache/v0.21.8/linux/amd64/dagger_v0.21.8_linux_amd64.tar.gz`

### 3.5 `internal/service/s3_cache_gc.go` (create)

New file with the S3-backed cache GC sweeper:

```go
// S3CacheGC sweeps stale BuildKit cache refs from an S3 bucket. It lists
// objects under the cache prefix, checks LastModified, and deletes objects
// older than MaxAge.
type S3CacheGC struct {
    client    *minio.Client
    bucket    string
    prefix    string // e.g. "cache" (the BuildKit remote cache prefix)
    gcCfg     domain.GCConfig
    logger    *logrus.Logger
    metricsObs *observ.Metrics

    mu       sync.Mutex
    lastGC   *domain.GCRunSummary
    lastGCAt time.Time
    nextGCAt time.Time
}

func NewS3CacheGC(client *minio.Client, bucket, prefix string, gcCfg domain.GCConfig, logger *logrus.Logger, obs *observ.Metrics) *S3CacheGC

// RunGC lists objects under the cache prefix and deletes those older than MaxAge.
func (g *S3CacheGC) RunGC(ctx context.Context) (*domain.GCRunSummary, error)

// Purge deletes ALL objects under the cache prefix (capped at maxPurgeAll).
func (g *S3CacheGC) Purge(ctx context.Context) (*domain.PurgeResult, error)

// GCRules returns the current GC configuration and last-run summary.
func (g *S3CacheGC) GCRules() *domain.GCRules

// StartGCSweeper starts a background goroutine that runs RunGC on Schedule.
// Returns a stop function.
func (g *S3CacheGC) StartGCSweeper(ctx context.Context) func()
```

### 3.6 `internal/repository/worker_sync.go` (modify)

The existing `worker_sync.go` (from v2) contains the incremental push/pull
logic for the OCI registry path. This file is kept intact. The new S3 logic
lives in `s3_snapshot.go`.

### 3.7 `internal/repository/worker_archive.go` (modify)

Add `TarGzipSubset` (from v2 plan §3.3):

```go
// TarGzipSubset is like TarGzipDir but excludes entries whose path (relative
// to baseDir/subDir) has any prefix in excludePrefixes. Used to create the
// metadata tarball that skips the content store.
func TarGzipSubset(ctx context.Context, baseDir, subDir string, w io.Writer, excludePrefixes ...string) (digest string, size int64, err error)
```

### 3.8 `cmd/api/cache_sync.go` (modify)

Add S3 env vars and update the push/restore logic:

```go
const (
    // ... existing env vars ...

    envCacheSyncS3Endpoint  = "CACHE_SYNC_S3_ENDPOINT"
    envCacheSyncS3Bucket    = "CACHE_SYNC_S3_BUCKET"
    envCacheSyncS3Region    = "CACHE_SYNC_S3_REGION"
    envCacheSyncS3UseSSL    = "CACHE_SYNC_S3_USE_SSL"
    envCacheSyncS3AccessKey = "CACHE_SYNC_S3_ACCESS_KEY"
    envCacheSyncS3SecretKey = "CACHE_SYNC_S3_SECRET_KEY" // #nosec G101
    envCacheSyncBackend     = "CACHE_SYNC_BACKEND" // "registry" or "s3"
)
```

Add fields to `cacheSyncEnv`:

```go
type cacheSyncEnv struct {
    // ... existing fields ...

    backend     string // "registry" or "s3"
    s3Endpoint  string
    s3Bucket    string
    s3Region    string
    s3UseSSL    bool
    s3AccessKey string
    s3SecretKey string
}
```

Update `pushWorkerSnapshot` to dispatch based on backend:

```go
func pushWorkerSnapshot(ctx context.Context, env *cacheSyncEnv, store *repository.WorkerSnapshotStore, s3Store *repository.S3SnapshotStore, logger *logrus.Logger) error {
    switch env.backend {
    case "s3":
        return pushWorkerSnapshotS3(ctx, env, s3Store, logger)
    default:
        return pushWorkerSnapshotRegistry(ctx, env, store, logger)
    }
}
```

### 3.9 `internal/repository/k8s_provider.go` (modify)

Add S3 fields and `Backend` to `K8sCacheSyncConfig`:

```go
type K8sCacheSyncConfig struct {
    // ... existing fields ...

    Backend string // "registry" or "s3"

    // S3-specific (only set when Backend is "s3").
    S3Endpoint  string
    S3Bucket    string
    S3Region    string
    S3UseSSL    bool
    S3AccessKey string // resolved from secret
    S3SecretKey string // resolved from secret
}
```

Update `syncEnv` to include S3 vars and backend:

```go
func (p *K8sProvider) syncEnv(version, role string) []corev1.EnvVar {
    env := []corev1.EnvVar{
        // ... existing env vars ...
        {Name: "CACHE_SYNC_BACKEND", Value: p.cfg.CacheSync.Backend},
    }
    if p.cfg.CacheSync.Backend == "s3" {
        env = append(env,
            corev1.EnvVar{Name: "CACHE_SYNC_S3_ENDPOINT", Value: p.cfg.CacheSync.S3Endpoint},
            corev1.EnvVar{Name: "CACHE_SYNC_S3_BUCKET", Value: p.cfg.CacheSync.S3Bucket},
            corev1.EnvVar{Name: "CACHE_SYNC_S3_REGION", Value: p.cfg.CacheSync.S3Region},
            corev1.EnvVar{Name: "CACHE_SYNC_S3_USE_SSL", Value: strconv.FormatBool(p.cfg.CacheSync.S3UseSSL)},
            // Access key and secret key from Kubernetes secrets:
            secretEnvVar("CACHE_SYNC_S3_ACCESS_KEY", "engine-s3-auth", "accessKey"),
            secretEnvVar("CACHE_SYNC_S3_SECRET_KEY", "engine-s3-auth", "secretKey"),
        )
    }
    // ...
}
```

### 3.10 `cmd/api/main.go` (modify)

Update `cacheSyncConfig` to handle S3 backend:

```go
func cacheSyncConfig(cfg *domain.Config, backends []domain.RegistryBackend, logger *logrus.Logger) repository.K8sCacheSyncConfig {
    sync := cfg.Cache.Sync
    if !sync.Enabled {
        return repository.K8sCacheSyncConfig{}
    }

    out := repository.K8sCacheSyncConfig{
        Image:       cfg.Fleet.EngineCacheSyncImage,
        Repo:        domain.WorkerSnapshotsRepo,
        Interval:    sync.Interval,
        QuiesceWait: sync.QuiesceWait,
        Enabled:     true,
        OnStart:     sync.OnStart,
        OnStop:      sync.OnStop,
        Backend:     cfg.Cache.Backend,
    }

    switch cfg.Cache.Backend {
    case "registry":
        if len(backends) > 0 {
            out.RegistryAddr = backends[0].InternalAddr
            out.RegistryUser = backends[0].Username
            out.RegistryPass = backends[0].Password
        }
    case "s3":
        out.S3Endpoint = sync.S3Endpoint
        out.S3Bucket = sync.S3Bucket
        if out.S3Bucket == "" {
            out.S3Bucket = cfg.Cache.S3.Bucket
        }
        out.S3Region = sync.S3Region
        out.S3UseSSL = sync.S3UseSSL
        // Access key and secret key are resolved from secrets at render time.
    default:
        logger.Warn("cache.sync.enabled requires cache.backend=registry or s3; worker-cache sync disabled")
        return repository.K8sCacheSyncConfig{}
    }

    if cfg.Fleet.EngineCacheSyncImage == "" {
        logger.Warn("fleet.engine_cache_sync_image is empty; worker-cache sync disabled")
        return repository.K8sCacheSyncConfig{}
    }

    return out
}
```

---

## 4. Helper env contract

| Env var | Default | Used by | Backend |
|---|---|---|---|
| `CACHE_SYNC_BACKEND` | `"registry"` | both | both |
| `CACHE_SYNC_REGISTRY_ADDR` | (required) | both | registry |
| `CACHE_SYNC_REGISTRY_USER` | `""` | both | registry |
| `CACHE_SYNC_REGISTRY_PASS` | `""` | both | registry |
| `CACHE_SYNC_REPO` | `dagger-cache/worker-snapshots` | both | registry |
| `CACHE_SYNC_TAG` | `worker-v2-<version-slug>` | both | registry |
| `CACHE_SYNC_S3_ENDPOINT` | (required) | both | s3 |
| `CACHE_SYNC_S3_BUCKET` | (required) | both | s3 |
| `CACHE_SYNC_S3_REGION` | `"us-east-1"` | both | s3 |
| `CACHE_SYNC_S3_USE_SSL` | `"true"` | both | s3 |
| `CACHE_SYNC_S3_ACCESS_KEY` | (from secret) | both | s3 |
| `CACHE_SYNC_S3_SECRET_KEY` | (from secret) | both | s3 |
| `CACHE_SYNC_BASE_DIR` | `/var/lib/dagger` | both | both |
| `CACHE_SYNC_WORKER_SUBDIR` | `worker` | both | both |
| `CACHE_SYNC_TMP_DIR` | `/tmp` | serve | both |
| `CACHE_SYNC_INTERVAL` | `"10m"` (`"0"` disables) | serve | both |
| `CACHE_SYNC_QUIESCE_WAIT` | `"10s"` | serve | both |
| `CACHE_SYNC_CONCURRENCY` | `"8"` | serve | registry |

The `CACHE_SYNC_CONCURRENCY` env var is only used by the registry backend
(controls concurrent `ProbeBlob` + `UploadBlobStream` goroutines). The S3
backend uploads blobs sequentially (S3 `PutObject` is fast and the
bottleneck is disk I/O, not network round-trips).

---

## 5. Files to create / modify

**Create:**
- `internal/repository/s3_snapshot.go` — S3-backed snapshot store: `Push`,
  `Pull`, `CleanupSelf`, `listBlobs`, `uploadBlob`, `downloadBlob`.
- `internal/repository/s3_snapshot_test.go` — unit tests for the S3 snapshot
  store with a mock S3 client.
- `internal/repository/s3_cli_cache.go` — S3-backed CLI cache: `Has`, `Get`,
  `Put`, `Dir`.
- `internal/repository/s3_cli_cache_test.go` — unit tests for the S3 CLI
  cache with a mock S3 client.
- `internal/service/s3_cache_gc.go` — S3-backed cache GC sweeper: `RunGC`,
  `Purge`, `GCRules`, `StartGCSweeper`.
- `internal/service/s3_cache_gc_test.go` — unit tests for the S3 GC sweeper
  with a mock S3 client.

**Modify:**
- `internal/domain/cache_sync.go` — add `ProbeBlob` to `CacheSnapshotClient`;
  add S3 prefix constants and helper functions.
- `internal/domain/config.go` — add S3 fields to `CacheSyncConfig`; add S3
  fields to `CLIConfig`.
- `internal/domain/cli.go` — add `MediaTypeOCIRawBlob` constant (from v2).
- `internal/repository/worker_archive.go` — add `TarGzipSubset`; refactor
  shared tar logic into an internal helper.
- `internal/repository/worker_snapshot.go` — add `PushIncremental` and
  `PullIncremental` methods (from v2); keep old `Push`/`Pull` as deprecated.
- `internal/repository/worker_sync.go` — create with v2 incremental push/pull
  logic: `walkContentStore`, `probeAndUploadMissing`, `downloadMissingBlobs`,
  `buildMultiLayerManifest`.
- `internal/repository/k8s_provider.go` — add S3 fields and `Backend` to
  `K8sCacheSyncConfig`; update `syncEnv` to include S3 vars and
  `CACHE_SYNC_BACKEND`.
- `cmd/api/cache_sync.go` — add S3 env vars; add `backend` field to
  `cacheSyncEnv`; update `pushWorkerSnapshot` to dispatch on backend; add
  `pushWorkerSnapshotS3` and `restoreWorkerSnapshotS3`.
- `cmd/api/main.go` — update `cacheSyncConfig` to handle S3 backend; pass S3
  config to `K8sCacheSyncConfig`; wire `S3CLICache` when backend is `s3`;
  wire `S3CacheGC` when backend is `s3`.
- `config/loader.go` — add defaults for `cache.sync.s3_*` and `cli.s3_*`
  keys; add validation for S3 backend prerequisites.
- `config/config.app.yaml.sample` — document `cache.sync.s3_*` and
  `cli.s3_*` keys.
- `deploy/helm/dagger-kubernetes/values.yaml` — add `cache.sync.s3_*` and
  `cli.s3_*` values.
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — render S3 sync
  and CLI cache config.
- `docs/README.md` — update worker-cache sync, CLI cache, and GC sections to
  describe S3 backend.
- `go.mod` — add `github.com/minio/minio-go/v7` dependency.

**No change needed:**
- `internal/service/cache_stats.go` — the existing `CacheStatsService` is
  registry-specific. When backend is `s3`, the `S3CacheGC` replaces it for
  GC/purge. Stats (MagicCache dashboard) for S3 is out of scope (same as the
  existing `s3UnsupportedMessage`).
- `Dockerfile`, `dagger/main.go`, `DAGGER.md` — no new binary or CI changes.

---

## 6. Step-by-step implementation order

1. **Dependency:** add `github.com/minio/minio-go/v7` to `go.mod` and run
   `go mod tidy`.

2. **Domain — constants + interface:**
   - Add `MediaTypeOCIRawBlob` to `internal/domain/cli.go`.
   - Add `ProbeBlob` to `CacheSnapshotClient` in `internal/domain/cache_sync.go`.
   - Add S3 prefix constants and helper function (`WorkerSnapshotS3Prefix`,
     `metaTarballName`, `blobsPrefix`).
   - Add `WorkerSnapshotTagV2` function (from v2).

3. **Domain — config:**
   - Add S3 fields to `CacheSyncConfig` in `internal/domain/config.go`.
   - Add S3 fields to `CLIConfig` (`S3Bucket`, `S3Prefix`).

4. **Config loader:**
   - Add `v.SetDefault` for `cache.sync.s3_*` and `cli.s3_*` keys in
     `config/loader.go`.
   - Add validation: when `cache.backend` is `s3` and `cache.sync.enabled`,
     require `cache.sync.s3_endpoint` and a non-empty bucket.
   - Add validation: when `cache.backend` is `s3` and `cli.enabled`, require
     a non-empty bucket (from `cli.s3_bucket` or `cache.s3.bucket`).
   - Update `config/config.app.yaml.sample`.

5. **Repository — `TarGzipSubset`:**
   - Refactor `worker_archive.go` to extract shared tar logic into an
     internal `tarGzipWalk` helper.
   - Implement `TarGzipSubset` using the helper with exclude-prefix filtering.
   - Add tests in `worker_archive_test.go`.

6. **Repository — v2 incremental push/pull (`worker_sync.go`):**
   - Implement `walkContentStore`, `probeAndUploadMissing`,
     `downloadMissingBlobs`, `buildMultiLayerManifest`.
   - Add `PushIncremental` and `PullIncremental` to `WorkerSnapshotStore`.

7. **Repository — S3 snapshot store (`s3_snapshot.go`):**
   - Implement `NewS3SnapshotStore`.
   - Implement `Push`: walk content store → upload missing blobs via
     `PutObject` → `TarGzipSubset` (excluding `content/blobs/`) →
     `PutObject` for `meta.tar.gz`.
   - Implement `Pull`: STAT `meta.tar.gz` → `GetObject` for `meta.tar.gz` →
     `UntarGzipDir` → LIST blobs under version prefix → download missing
     blobs via `GetObject`.
   - Implement `CleanupSelf`: LIST version prefix → `RemoveObject` for each.
   - Add tests in `s3_snapshot_test.go` with a mock S3 client.

8. **Repository — S3 CLI cache (`s3_cli_cache.go`):**
   - Implement `NewS3CLICache`.
   - Implement `Has`: S3 `StatObject` (HEAD) on the deterministic key.
   - Implement `Get`: S3 `GetObject` → write to temp file.
   - Implement `Put`: verify `sha256Hex` → S3 `PutObject`.
   - Implement `Dir`: return `""`.
   - Add tests in `s3_cli_cache_test.go` with a mock S3 client.

9. **Service — S3 cache GC (`s3_cache_gc.go`):**
   - Implement `NewS3CacheGC`.
   - Implement `RunGC`: LIST objects under cache prefix → filter by
     `LastModified` < `now - MaxAge` → `RemoveObject` for each.
   - Implement `Purge`: LIST objects under cache prefix → `RemoveObject` for
     each (capped at `maxPurgeAll`).
   - Implement `GCRules`: return config + last-run summary.
   - Implement `StartGCSweeper`: background ticker goroutine.
   - Add tests in `s3_cache_gc_test.go` with a mock S3 client.

10. **K8sProvider:**
    - Add `Backend`, `S3Endpoint`, `S3Bucket`, `S3Region`, `S3UseSSL` fields
      to `K8sCacheSyncConfig`.
    - Update `syncEnv` to include `CACHE_SYNC_BACKEND` and S3 env vars
      (including secret refs for access key and secret key).
    - Update tests in `k8s_provider_test.go`.

11. **CLI — helper:**
    - Add S3 env var constants and fields to `cacheSyncEnv`.
    - Update `loadCacheSyncEnv` to parse S3 vars and `CACHE_SYNC_BACKEND`.
    - Add `newS3SnapshotStore` helper.
    - Update `runCacheSyncRestore` to dispatch on backend.
    - Update `runCacheSyncServe` to dispatch on backend.
    - Add `pushWorkerSnapshotS3` and `restoreWorkerSnapshotS3`.
    - Add `CleanupSelf` call on sidecar startup (before first periodic push).
    - Update tests in `cache_sync_test.go`.

12. **Wiring (`cmd/api/main.go`):**
    - Update `cacheSyncConfig` to handle S3 backend.
    - Pass S3 config to `K8sCacheSyncConfig`.
    - When `cache.backend` is `s3` and `cli.enabled`: create `S3CLICache`
      instead of `RegistryCLICache`.
    - When `cache.backend` is `s3` and `cache.gc.enabled`: create
      `S3CacheGC` and start its sweeper instead of the registry-based
      `CacheStatsService.StartGCSweeper`.
    - The S3 client is created once and shared across all three subsystems
      (snapshot store, CLI cache, GC).

13. **Helm:**
    - Add `supervisor.config.cache.sync.s3_*` and
      `supervisor.config.cli.s3_*` values to `values.yaml`.
    - Render S3 config in `configmap.yaml`.
    - Add `engine-s3-auth` secret template (or document that operators must
      create it).

14. **Docs:**
    - Update `docs/README.md` worker-cache sync section to describe S3
      backend, v3 format, per-version storage layout, and lifecycle policy
      recommendations.
    - Update CLI cache section to describe S3 backend option.
    - Update GC section to describe S3 GC sweeper and lifecycle policy
      recommendations.

15. **CI gate:**
    - `go build ./... && go vet ./... && go test ./...` plus
      `dagger call -m ./dagger --src . lint`.
    - Full `ci export` if Docker daemon available.
    - Mandatory local redeploy per `AGENTS.local.md`.

---

## 7. Edge cases, error handling & validation

### 7.1 Push edge cases

- **Empty content store:** No blobs to upload. Only `meta.tar.gz` is pushed.
  This is valid — a fresh engine with no cached blobs.
- **Blob deleted by GC during walk:** `os.Open` fails with `os.ErrNotExist`.
  Log DEBUG, skip the blob. The S3 `PutObject` is never attempted.
- **S3 PutObject fails (network error):** Log WARN, skip the blob. The next
  periodic push will retry. The metadata tarball is still pushed (it does not
  depend on blob uploads).
- **Metadata tarball upload fails:** The previous `meta.tar.gz` remains
  intact (S3 overwrite is atomic at the object level). The sidecar logs a
  WARN and continues.
- **S3 endpoint unreachable:** The entire push fails. Same as v1/v2 — a
  failed push logs a warning and the sidecar continues.
- **Disk full during tar:** `TarGzipSubset` writes to a temp file on the
  emptyDir. If the disk is full, the write fails and the push is skipped.
- **Very large blob counts (100,000+):** S3 LIST is paginated (1000 objects
  per page). The walk is O(blobs) directory traversal. No manifest size
  ceiling.

### 7.2 Pull edge cases

- **No snapshots exist (first boot):** `meta.tar.gz` does not exist. Restore
  is a no-op.
- **Metadata tarball missing (deleted by lifecycle policy):** STAT returns
  `NoSuchKey`. Restore is a no-op.
- **Content blob missing from S3 (deleted by lifecycle policy):**
  `GetObject` returns `NoSuchKey`. Log DEBUG, skip that blob. The engine
  will re-download it from the remote cache on next use.
- **Content blob already exists locally:** Check by computing the expected
  file path (`content/blobs/sha256/<first-two-hex>/<full-hex>`) and calling
  `os.Stat`. If the file exists, skip the download.
- **Corrupt metadata DB after untar:** Attempt a basic BoltDB open to verify
  integrity. If it fails, remove the worker dir and return `ok=false`.
- **Disk full during restore:** `GetObject` writes to disk. If the disk is
  full, the write fails and `Pull` returns an error. The partial extraction
  is discarded.

### 7.3 Cleanup edge cases

- **Self-cleanup on startup fails:** Log WARN, continue. Old blobs will be
  cleaned by the lifecycle policy.
- **Lifecycle policy not configured:** Old snapshots accumulate. This is
  an operational concern, not a code bug. Document the recommendation.

### 7.4 CLI cache edge cases

- **S3 StatObject fails (network error):** `Has` returns `false, error`.
  Caller treats as "not found" (same as registry `ManifestExists` failure).
- **S3 GetObject fails mid-download:** Temp file is removed. `Get` returns
  `("", false)`.
- **S3 PutObject fails:** `Put` returns an error. The CLI service logs and
  the client retries or falls back to upstream download.
- **Checksum mismatch in Put:** The sha256 of the downloaded tarball doesn't
  match the expected value. `Put` returns `ErrCLIChecksumMismatch` before
  any S3 upload.
- **Concurrent Put of same artifact:** S3 `PutObject` is atomic. The last
  writer wins, but since the content is identical (verified by sha256), the
  result is the same.

### 7.5 GC edge cases

- **S3 LIST pagination:** `ListObjects` returns paginated results (1000 per
  page). The GC must iterate through all pages.
- **S3 RemoveObject fails (network error):** Log WARN, continue to next
  object. The failed object will be retried on the next GC tick.
- **S3 RemoveObject fails (permission denied):** Log WARN, continue. The
  operator must fix the IAM policy.
- **GC runs while objects are being written:** The GC checks `LastModified`
  against `now - MaxAge`. Objects being written have a recent
  `LastModified` and are not deleted. This is safe.
- **GC sweeper is disabled (`gc.enabled: false`):** `StartGCSweeper` is not
  called. Operators rely on S3 lifecycle policies.
- **Purge cap (`maxPurgeAll`):** The Purge operation stops after deleting
  `maxPurgeAll` objects (1000). This prevents accidental mass deletion.
  Operators must run Purge multiple times to clear a large cache.

### 7.6 Validation rules

- `CACHE_SYNC_BACKEND` must be `"registry"` or `"s3"`.
- When backend is `s3`, `CACHE_SYNC_S3_ENDPOINT` and `CACHE_SYNC_S3_BUCKET`
  must be non-empty.
- `CACHE_SYNC_S3_USE_SSL` must be `"true"` or `"false"`.
- Content blob digests must match `sha256:<64 hex>` (validated before any S3
  operation).
- Metadata tarball must not be empty (a worker dir always has at least
  `metadata.db`).

---

## 8. Testing plan

### 8.1 Unit tests (stdlib `testing`, table-driven)

**`s3_snapshot_test.go` (create):**
- `TestS3PushPullRoundTrip`: create a worker dir with metadata + content
  blobs, push to mock S3, pull into a fresh dir, verify tree equality.
- `TestS3PullNoSnapshots`: empty bucket → `ok=false`.
- `TestS3CleanupSelf`: push blobs, call `CleanupSelf`, verify prefix is empty.
- `TestS3PushSkipsGCdBlob`: blob deleted between walk and upload → skipped
  silently.
- `TestS3PullSkipsMissingBlob`: blob referenced in meta but deleted from
  S3 → skipped silently.
- `TestS3PullSkipsExistingBlob`: blob already exists locally → not
  re-downloaded.
- `TestS3ConcurrentPushIdempotent`: two pods push to the same prefix
  concurrently → both succeed, blobs are not duplicated.

**`s3_cli_cache_test.go` (create):**
- `TestS3CLICacheHasExisting`: `StatObject` returns object info → `Has` returns true.
- `TestS3CLICacheHasMissing`: `StatObject` returns `NoSuchKey` → `Has` returns false.
- `TestS3CLICacheGet`: `GetObject` returns tarball bytes → written to temp file.
- `TestS3CLICacheGetMissing`: `GetObject` returns `NoSuchKey` → returns `("", false)`.
- `TestS3CLICachePut`: verify sha256, upload to S3, verify object exists.
- `TestS3CLICachePutChecksumMismatch`: wrong sha256 → error before upload.
- `TestS3CLICacheKeyFormat`: verify the S3 key is
  `<prefix>/<version>/<os>/<arch>/<filename>`.

**`s3_cache_gc_test.go` (create):**
- `TestS3CacheGCRunGC`: create objects with various `LastModified` times,
  run GC, verify only old objects are deleted.
- `TestS3CacheGCRunGCEmpty`: no objects → no errors.
- `TestS3CacheGCPurge`: create objects, run Purge, verify all deleted.
- `TestS3CacheGCPurgeCap`: create more than `maxPurgeAll` objects, verify
  cap is enforced.
- `TestS3CacheGCGCRules`: verify config and last-run summary are returned.
- `TestS3CacheGCStartSweeper`: start sweeper, wait for at least one tick,
  verify GC ran.

**`worker_archive_test.go` (extend):**
- `TestTarGzipSubsetExcludesPrefix`: create a dir with `content/blobs/` and
  other files; verify `content/blobs/` entries are absent.
- `TestTarGzipSubsetEmptyResult`: exclude everything; verify the tarball
  contains only the root directory entry.
- `TestTarGzipSubsetNestedPrefix`: verify prefix matching is path-aware.

**`worker_sync_test.go` (create, from v2):**
- `TestWalkContentStore`, `TestProbeAndUploadMissing`,
  `TestDownloadMissingBlobs`, `TestBuildMultiLayerManifest`,
  `TestPushIncrementalPullIncrementalRoundTrip`.

**`worker_snapshot_test.go` (extend):**
- Update `stubSnapshotClient` to implement `ProbeBlob`.
- `TestPushIncrementalMultiLayer`, `TestPullIncrementalSkipsExistingBlobs`.

**`cache_sync_test.go` (extend):**
- `TestLoadCacheSyncEnvS3`: verify S3 env vars are parsed correctly.
- `TestLoadCacheSyncEnvBackend`: verify backend dispatch.
- `TestCacheSyncConfigS3`: verify S3 config is populated when backend is
  `"s3"`.

**`k8s_provider_test.go` (extend):**
- Verify S3 env vars are set on sync containers when backend is `"s3"`.
- Verify `CACHE_SYNC_BACKEND` is set.
- Verify no `CACHE_SYNC_POD_NAME` env var is set (no per-pod isolation).

### 8.2 Integration tests

- Extend the existing integration test to cover the S3 path using a real S3
  endpoint (or a MinIO container started in the test).
- Test concurrent pushes from multiple pods to the same version prefix —
  verify blob idempotency and metadata tarball last-writer-wins.
- Test prune propagation: pod A prunes and pushes, pod B pulls and gets the
  pruned cache.

### 8.3 Mandatory local validation (AGENTS.local.md)

Same as v1/v2 §8:
1. Build and push the dev image.
2. Helm upgrade with captured values.
3. Rollout restart supervisor.
4. Agent checks: pods Ready, healthz/readyz 200, API endpoints.
5. Trigger engine fleet, scale to zero, verify S3 objects appear, scale back
   up, verify restore logs.
6. Human verification of live UI.

---

## 9. Documentation changes

- `docs/README.md`: update "Worker-cache sync (warm start)" section:
  - Describe S3 backend option and v3 format.
  - Document storage layout (`worker-snapshots/<version-slug>/`).
  - Document that all pods in the same StatefulSet share one snapshot prefix.
  - Add `cache.sync.s3_*` to the env var / config key table.
  - Recommend S3 bucket lifecycle policies for cleanup.
  - Note that v2 (registry) is still supported.
- `docs/README.md`: update "CLI cache" section:
  - Describe S3 backend option for CLI tarball storage.
  - Document S3 key format (`cli-cache/<version>/<os>/<arch>/<filename>`).
  - Add `cli.s3_*` to the config key table.
- `docs/README.md`: update "Cache GC" section:
  - Describe S3 GC sweeper (LIST + `LastModified` + delete).
  - Document that S3 lifecycle policies are recommended as defense-in-depth.
  - Note that the registry-based GC is still supported for registry backends.
- `config/config.app.yaml.sample`: add `cache.sync.s3_*` and `cli.s3_*`
  blocks with comments.
- `deploy/helm/dagger-kubernetes/values.yaml`: add `cache.sync.s3_*` and
  `cli.s3_*` values with `@param` documentation.
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml`: render S3 sync
  and CLI cache config.
- No `DAGGER.md` change (no dagger/ or CI changes).

---

## 10. Out of scope / follow-ups

- **S3 bucket lifecycle policy automation.** The Helm chart could optionally
  create a lifecycle policy on the S3 bucket (via MinIO admin APIs or AWS
  CloudFormation). Out of scope for v3 — operators configure this manually.
- **S3 multipart upload for large blobs.** `minio-go/v7` supports multipart
  uploads via `PutObject` with automatic part sizing. This is already handled
  by the library; no special code needed unless we want to tune part sizes.
- **Prometheus metrics for S3 sync and GC.** Track blobs uploaded, bytes
  uploaded, metadata tarball size, push duration, GC objects deleted, GC
  bytes freed. Same as v1/v2 out-of-scope.
- **Compression for content blobs.** Content-store blobs may already be
  compressed (container image layers). Re-compressing them would waste CPU.
  Out of scope.
- **Removing old v1 `Push`/`Pull` methods.** After all pods are upgraded to
  v2/v3, the deprecated methods can be removed. Tracked as a follow-up
  cleanup.
- **Cross-backend migration (registry → S3).** Operators who want to migrate
  from the registry backend to S3 must accept a cold-start period (no
  snapshots to restore). The remote BuildKit cache (`:cache` tag or S3 cache
  prefix) is unaffected. Out of scope.
- **S3 versioning for `meta.tar.gz`.** S3 object versioning could provide a
  history of metadata tarballs, allowing restore from an older snapshot if
  the latest is corrupt. Out of scope — the periodic push provides natural
  recovery (next push overwrites with a fresh tarball).
- **MagicCache dashboard for S3.** The existing `CacheStatsService` is
  registry-specific (catalog, tags, manifests). An S3 equivalent (LIST
  objects, aggregate sizes) is out of scope for v3. The MagicCache dashboard
  shows `"s3 cache stats not supported in this release"` for S3 backends
  (existing behavior).
- **Removing the OCI registry entirely.** This plan makes the registry
  optional (all three subsystems work with S3), but the registry code paths
  are kept intact. Removing them is a follow-up cleanup after all deployments
  have migrated.
