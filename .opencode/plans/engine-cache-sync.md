# Plan: BuildKit local-cache synchronization for engine pods

## 1. Overview / problem statement

Each Dagger engine pod is a StatefulSet (`dagger-engine-<version-slug>`) with a
per-pod PVC `dagger-kubernetes` mounted at `/var/lib/dagger`. BuildKit stores its
local cache under `/var/lib/dagger/worker/` (BoltDB `metadata.db`, content store,
snapshots, leases). Today that local cache is **lost** whenever a pod gets a
*fresh* PVC: scale-out to a new replica, node migration, PVC re-provision, or a
new engine version. Every such pod cold-starts (no cache hits) until it re-fills
its worker dir, which wastes CI time and re-downloads layers.

This feature syncs the local worker cache through the existing OCI registry so a
pod can **warm-start** from a sibling pod's cache:

1. **On start** — restore `/var/lib/dagger/worker/` from a shared snapshot
   (only when the worker dir is empty/fresh).
2. **On stop** — push the local cache back as a snapshot for the next pod.
3. **Periodic** — every `interval`, push a snapshot so a crash (non-graceful
   stop) or a long-running pod doesn't leave the shared cache stale, and so
   long-running pods converge.
4. **Concurrency-safe** — concurrent pod stops must not corrupt the snapshot.

The sync is **best-effort and additive**: it warms the *local* cache; the
existing remote BuildKit cache (`:cache` tag, ADR-028) is unchanged and remains
the source of truth for content. A corrupt/missing snapshot only means a cold
start.

---

## 2. Design decisions (with rationale)

### D1 — What gets synced: a single gzip tarball of `/var/lib/dagger/worker/` pushed as one OCI blob

**Choice: (a) tar the whole `worker/` dir, gzip it, upload as a single OCI
layer + manifest** — the same pattern as `internal/repository/cli_cache_registry.go`.

Rationale:
- **(c) BuildKit native gRPC cache export is rejected.** The project's registry
  client (`RegistryStatsClient`) is OCI Distribution v2 only, and `buildctl`
  / the BuildKit export gRPC surface is not available inside the engine pod we
  control. Implementing it would require a BuildKit-aware client — out of scope.
- **(b) subdir-only export is rejected.** The content store blobs are immutable
  and safe to copy, but the metadata index (`metadata.db`) is what actually
  turns warm content into cache hits. Pushing only subdirs would warm content
  but not the index, giving little hit benefit for more complexity.
- **(a) is chosen** because it is simple, matches the existing CLI-cache
  artifact pattern (single-layer OCI manifest + empty-JSON config), and a full
  worker dir is the only thing BuildKit needs to warm-start.

**Consistency window (must be explicit).** BuildKit's `metadata.db` is BoltDB,
which is crash-safe: writes are copy-on-write and the meta page is committed
last, so a torn read recovers to the last committed transaction. Content-store
blobs are immutable. Consequence:

- **Periodic sync while the engine is running captures a "dirty" snapshot.**
  This is accepted and documented as best-effort. Worst case on restore: a few
  of the most recent cache entries are missing (BoltDB rolls back), which is
  harmless for a cache. We never lock or pause the engine (that would break
  running pipelines).
- **On-stop sync quiesces the engine first** (see D4) so the common case is a
  clean snapshot.

The tarball is written to a **temp file on a node-backed emptyDir** (not the
PVC, which the sidecar mounts read-only) so we capture one consistent snapshot
per cycle and never buffer multi-GB data in memory (the existing
`UploadBlob` reads the whole body into memory — we must NOT reuse it for large
snapshots; see D7).

### D2 — Where the sync logic lives: init container (restore) + sidecar (periodic + on-stop), both running a new `supervisor cache-sync` subcommand

- **(c) Supervisor coordinating from outside is rejected.** The Supervisor has
  no access to another pod's PVC; the data movement must happen inside the pod.
- **A sidecar + init container is chosen** over init+preStop because:
  - `restore` must run strictly *before* the engine starts → an **init
    container** guarantees that ordering.
  - `push` (periodic + final) must run *alongside* the engine for its whole
    lifetime → a **sidecar container**.
  - The engine image is not fully under our control; we cannot rely on it for
    tar/auth logic. Our own image (the supervisor image) already has the Go
    binary + CA certs and needs no extra system tools (`archive/tar` +
    `compress/gzip` come from the stdlib).

**The helper is a subcommand of the existing `supervisor` binary**
(`supervisor cache-sync restore` / `supervisor cache-sync serve`), not a new
binary. This reuses the already-built, already-shipped supervisor image (no
Dockerfile change, no `dagger/main.go` change, no DAGGER.md change) and matches
the existing `supervisor migrate-tokens` subcommand precedent.

**Authentication:** the helper talks **directly to the in-cluster backend
registry** over plain HTTP using the backend's basic-auth credentials, exactly
like `RegistryStatsClient` already does:

- Registry address = `cache.registries[0].internal_addr` (or the single-backend
  `cache.internal_addr`). Username/password from that backend (empty for the
  bundled unauthenticated `registry:2`). This is the same "first backend" choice
  the CLI cache already makes (`cacheBackends[0]` in `cmd/api/main.go`).
- Direct backend access (not the public cache vhost) avoids the public-ingress
  hairpin for potentially multi-GB snapshots, avoids TLS/CA complexity for the
  helper, and is an internal control-plane concern (cache convergence), not
  client traffic.
- Credential plumbing: the Supervisor resolves backend passwords
  (`resolveRegistryBackendSecrets`, which runs *before* `createProvider`) and
  passes address+creds into `K8sProviderConfig.CacheSync`, which renders them as
  env vars on the init/sidecar containers. Security caveat: credentials appear
  as literal env vars on the engine pod spec — acceptable for now (the common
  bundled registry is unauthenticated); a secret-ref follow-up is noted in §10.

### D3 — Concurrency: per-version snapshot ref, last-writer-wins; pulls are read-only

- **Ref:** repository `dagger-cache/worker-snapshots` (a well-known domain
  constant `WorkerSnapshotsRepo`), tag `worker-<version-slug>` (e.g.
  `worker-v0-20-0`). One tag per engine version.
- **Per-version, not global:** a worker dir is version-sensitive (BoltDB schema
  + snapshot format can differ across BuildKit/engine versions). The remote
  `:cache` tag already handles cross-version sharing safely via
  content-addressing; the worker snapshot is a *local* warm-start and is kept
  per-version to avoid format incompatibilities. Cross-version restore is a
  documented non-goal.
- **Concurrent stops:** multiple pods pushing the same tag is safe at the
  registry level — blob uploads are content-addressed (deduplicated), and the
  manifest PUT for a tag is atomic and last-writer-wins. No lock, no
  compare-and-swap is needed: each snapshot is a full point-in-time copy, so
  **any** winner is a valid warm-start; the only "loss" is that non-winning
  pods' unique entries aren't merged (fine for a cache).
- **Concurrent pulls:** read-only, no coordination needed.
- **No ref explosion:** one manifest per version; stale versions are naturally
  cleaned by the existing cache GC (creation-age based, see D5).

### D4 — Periodic sync: timer lives in the sidecar (`cache-sync serve`); best-effort dirty snapshot

- The sidecar runs a `time.Ticker` at `cache.sync.interval` (0 = disabled).
- Periodic pushes do **not** pause the engine; they accept the dirty snapshot
  (D1). This is strictly best-effort convergence.
- **On stop**, the sidecar traps SIGTERM, sleeps `cache.sync.quiesce_wait`
  (default 10s) to let the engine flush+exit on its own SIGTERM, then does the
  final push. We deliberately avoid `shareProcessNamespace` (which SIGKILLs
  siblings when PID 1 exits) — the quiesce grace is a pragmatic bound, and any
  residual dirtiness is covered by BoltDB crash recovery.
- The final push must complete within the pod's `terminationGracePeriodSeconds`
  (engine default 120s). A truncated push cannot update the manifest (blob PUT
  never finishes), so the previous snapshot stays intact — safe. Operators with
  very large caches should raise `fleet.engine_termination_grace_seconds`.

### D5 — Config surface

New `cache.sync` section (see §3 for exact structs):

| Key | Type | Default | Meaning |
|---|---|---|---|
| `cache.sync.enabled` | bool | `true` | Master switch for worker-cache sync. |
| `cache.sync.on_start` | bool | `true` | Restore snapshot on start (init container). |
| `cache.sync.on_stop` | bool | `true` | Final push on stop (sidecar SIGTERM handler). |
| `cache.sync.interval` | duration | `"10m"` | Periodic push cadence; `0` disables periodic. |
| `cache.sync.quiesce_wait` | duration | `"10s"` | Grace before the final on-stop push. |

Plus `fleet.engine_cache_sync_image` (string, default `""`) — the image holding
the `supervisor` binary used by the init/sidecar containers. The Helm chart
renders it from the supervisor image; non-Helm users set it explicitly. If sync
is enabled but the image is empty, the Supervisor logs a WARN and skips the
init/sidecar (graceful degradation; never blocks the engine pod).

The snapshot **repo and tag are hardcoded domain constants** (not config keys),
mirroring `service.cacheTag = "cache"`. Keeping them constants avoids drift
between the helper and the stats/GC skip (D5 note) and reduces config surface.
(`cache.sync.ref` from the brief is intentionally not a config key.)

The helper reads its inputs from env vars rendered by `K8sProvider` (see §4).

### D6 — Error handling

- **Restore (on start):** best-effort. Snapshot missing or registry
  unreachable → log WARN, exit 0, pod starts with an empty cache. A pull is
  bounded by a timeout (default 5m). A partial/corrupt untar is discarded (we
  untar into the live dir only after a fully successful blob download).
- **Push (periodic/on-stop):** best-effort. Errors are logged; the sidecar
  never crashes on a failed push, and an on-stop push failure never blocks pod
  termination (exit 0). A short retry (e.g. 2 retries) covers transient
  transport errors; total push work is bounded by the remaining termination
  grace so SIGKILL truncation cannot corrupt the old snapshot.
- **Registry unreachable at start/stop:** covered by the above (best-effort,
  no hard failure).
- **Digest mismatch** (e.g. engine wrote between tar and upload): the registry
  rejects the PUT; the sidecar logs and skips that cycle. Never retry-loop a
  mismatch (data changed, retrying won't help).

### D7 — Streaming upload (no in-memory buffering)

The existing `RegistryStatsClient.UploadBlob` buffers the whole body in memory
(`io.Copy` into a `bytes.Buffer`) — unusable for multi-GB snapshots. Add a
**streaming** variant (monolithic PUT with a precomputed digest + known size):

- Push path: tar+gzip to a temp file on the emptyDir while teeing through a
  `sha256` hasher to compute `(digest, size)` in one pass, then stream the temp
  file to `UploadBlobStream` with `Content-Length` set.
- Pull path: `GetBlob` already streams (`io.ReadCloser`); `UntarGzipDir`
  streams from it directly (no temp file needed on restore).

---

## 3. Data structures & signatures

### `internal/domain/config.go` (modify)

```go
// CacheSyncConfig governs the BuildKit local worker-cache snapshot sync.
type CacheSyncConfig struct {
	Enabled     bool          `mapstructure:"enabled"`
	OnStart     bool          `mapstructure:"on_start"`
	OnStop      bool          `mapstructure:"on_stop"`
	Interval    time.Duration `mapstructure:"interval"`     // periodic push; 0 = disabled
	QuiesceWait time.Duration `mapstructure:"quiesce_wait"` // grace before final on-stop push
}

type CacheConfig struct {
	// ...existing...
	Sync CacheSyncConfig `mapstructure:"sync"`
}
```

`FleetConfig` gains:
```go
EngineCacheSyncImage string `mapstructure:"engine_cache_sync_image"`
```

### `internal/domain/cache_sync.go` (create)

```go
const (
	WorkerSnapshotsRepo   = "dagger-cache/worker-snapshots"
	workerSnapshotTagPre  = "worker-"
)

// WorkerSnapshotTag returns the per-version snapshot tag, e.g. "worker-v0-20-0".
func WorkerSnapshotTag(version string) string { return workerSnapshotTagPre + VersionSlug(version) }

// CacheSnapshotClient is the slice of the OCI client the worker snapshot needs.
// Implemented by *repository.RegistryStatsClient. Reuses domain.CLIManifest as
// the single-layer OCI artifact manifest shape (identical structure).
type CacheSnapshotClient interface {
	ManifestExists(ctx context.Context, repo, tag string) (bool, error)
	GetManifest(ctx context.Context, repo, tag string) (*CLIManifest, error)
	GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error)
	UploadBlobStream(ctx context.Context, repo, digest string, size int64, body io.Reader) error
	PutManifest(ctx context.Context, repo, tag string, manifest *CLIManifest) error
}
```

### `internal/repository/registry_client.go` (modify)

```go
// UploadBlobStream uploads a blob whose sha256 digest and byte size are known
// up front, streaming body in a single monolithic PUT (no in-memory buffering).
// Follows OCI Distribution v2: POST /v2/<repo>/blobs/uploads/ -> PUT <location>?digest=<d>.
func (c *RegistryStatsClient) UploadBlobStream(ctx context.Context, repo, digest string, size int64, body io.Reader) error
```
Behavior: reject non-`sha256:<hex>` digests (`validDigest`), initiate upload,
PUT with `Content-Type: application/octet-stream` + `Content-Length: size`,
accept `201/200`, verify returned digest when present. Errors wrap
`ErrRegistryUnreachable`.

### `internal/repository/worker_archive.go` (create)

```go
// TarGzipDir writes a gzip tarball of <baseDir>/<subDir> to w, archiving
// entries under the "<subDir>/" prefix (so extraction into baseDir restores
// the subtree). Returns the sha256 hex digest and byte count of the compressed
// stream (computed while writing). Streaming: O(1) memory.
func TarGzipDir(ctx context.Context, baseDir, subDir string, w io.Writer) (digest string, size int64, err error)

// UntarGzipDir extracts a gzip tarball produced by TarGzipDir into dstDir,
// streaming from r. Safe against path traversal (rejects absolute and "../"
// entries).
func UntarGzipDir(ctx context.Context, dstDir string, r io.Reader) error
```

### `internal/repository/worker_snapshot.go` (create)

```go
// WorkerSnapshotStore pushes/pulls whole worker-dir snapshots as single-layer
// OCI artifacts (mirrors RegistryCLICache's manifest shape).
type WorkerSnapshotStore struct {
	client domain.CacheSnapshotClient
	repo   string // domain.WorkerSnapshotsRepo
	logger *logrus.Logger
}

func NewWorkerSnapshotStore(client domain.CacheSnapshotClient, repo string, logger *logrus.Logger) *WorkerSnapshotStore

// Push uploads the tarball stream (already digest+size known) and publishes the
// manifest for version's snapshot tag.
func (s *WorkerSnapshotStore) Push(ctx context.Context, version string, r io.Reader, digest string, size int64) error

// Pull downloads the snapshot for version and untars it into dstDir. Returns
// ok=false when no snapshot exists. Streams, never buffers the whole blob.
func (s *WorkerSnapshotStore) Pull(ctx context.Context, version string, dstDir string) (ok bool, err error)
```
`Push` uses the same empty-JSON config descriptor constants as
`cli_cache_registry.go` and `domain.MediaTypeOCILayerGzip` for the layer.

### `internal/repository/k8s_provider.go` (modify)

```go
type K8sCacheSyncConfig struct {
	Image        string
	RegistryAddr string
	RegistryUser string
	RegistryPass string
	Repo         string
	Interval     time.Duration
	QuiesceWait  time.Duration
	Enabled      bool
	OnStart      bool
	OnStop       bool
}
```
Added as `CacheSync K8sCacheSyncConfig` on `K8sProviderConfig`. The tag is
computed per-version inside `buildStatefulSet` via
`domain.WorkerSnapshotTag(version)` (not stored in the config).

New methods on `K8sProvider`:
```go
func (p *K8sProvider) syncInitContainer(version string) corev1.Container            // "cache-restore"
func (p *K8sProvider) syncSidecarContainer(version string) corev1.Container         // "cache-sync"
func (p *K8sProvider) syncEnv(version string, role string) []corev1.EnvVar          // shared env builder
```
Integration points in `buildStatefulSet`:
- append the restore init container when `OnStart` (in addition to any existing
  `ca-init`).
- append the sidecar to `Containers`.
- add the `cache-sync-tmp` emptyDir volume + sidecar mount when `OnStop ||
  Interval > 0`.

### `internal/service/cache_stats.go` (modify)

Add a `snapshotRepo string` field to `CacheStatsService` (set from
`domain.WorkerSnapshotsRepo`), and skip that repo in `collectEntries` /
`gcCollectEntries` / `Purge` so multi-GB snapshots do not inflate the
MagicCache `total_size`/`object_count` and are not swept as cache refs. Add a
small `func (s *CacheStatsService) skipRepo(repo string) bool`.

### `cmd/api/cache_sync.go` (create)

```go
// cacheSyncCommand returns the top-level "cache-sync" CLI command.
func cacheSyncCommand() *cli.Command // subcommands: restore, serve

func runCacheSyncRestore(c *cli.Context) error
func runCacheSyncServe(c *cli.Context) error
```
Both read config from env vars (see §4). `restore` = pull + untar iff worker dir
empty; `serve` = ticker loop + SIGTERM final push.

---

## 4. Helper env contract (rendered by `K8sProvider` on init + sidecar)

| Env var | Default | Used by |
|---|---|---|
| `CACHE_SYNC_REGISTRY_ADDR` | (required) | both |
| `CACHE_SYNC_REGISTRY_USER` | `""` | both |
| `CACHE_SYNC_REGISTRY_PASS` | `""` | both |
| `CACHE_SYNC_REPO` | `dagger-cache/worker-snapshots` | both |
| `CACHE_SYNC_TAG` | `worker-<version-slug>` | both |
| `CACHE_SYNC_BASE_DIR` | `/var/lib/dagger` | both |
| `CACHE_SYNC_WORKER_SUBDIR` | `worker` | both |
| `CACHE_SYNC_TMP_DIR` | `/tmp` | serve (tarball temp) |
| `CACHE_SYNC_INTERVAL` | `"10m"` (`"0"` disables) | serve |
| `CACHE_SYNC_QUIESCE_WAIT` | `"10s"` | serve |

Container spec details:
- **init `cache-restore`** (only when `OnStart`): image `CacheSync.Image`,
  command `["/usr/local/bin/supervisor","cache-sync","restore"]`, mounts
  `volumeDaggerKubernetes` at `/var/lib/dagger` (read-write), securityContext
  `runAsUser: 0` + `Privileged` matching the engine (the PVC is root-owned).
- **sidecar `cache-sync`** (when `OnStop || Interval > 0`): same image/command
  with `serve`, mounts `volumeDaggerKubernetes` read-only at `/var/lib/dagger`,
  emptyDir `cache-sync-tmp` at `/tmp`, same securityContext. No probes; it is
  auxiliary to the engine's readiness.

---

## 5. Files to create / modify

**Create**
- `internal/domain/cache_sync.go`
- `internal/repository/worker_archive.go`
- `internal/repository/worker_snapshot.go`
- `internal/repository/worker_archive_test.go`
- `internal/repository/worker_snapshot_test.go`
- `cmd/api/cache_sync.go`

**Modify**
- `internal/domain/config.go` — `CacheSyncConfig`, `Sync` field, `EngineCacheSyncImage`.
- `config/loader.go` — defaults + validation for `cache.sync.*` and `fleet.engine_cache_sync_image`.
- `config/config.app.yaml.sample` — document `cache.sync.*` and `fleet.engine_cache_sync_image`.
- `internal/repository/registry_client.go` — `UploadBlobStream`.
- `internal/repository/k8s_provider.go` — `K8sCacheSyncConfig`, init+sidecar, volumes/mounts/env.
- `internal/repository/k8s_provider_test.go` — new tests for the rendered spec.
- `internal/service/cache_stats.go` — snapshot-repo skip (stats/GC/purge).
- `internal/service/cache_stats_test.go` — skip tests.
- `cmd/api/main.go` — register `cache-sync` command; extend `createProvider` to accept resolved cache backends and populate `K8sProviderConfig.CacheSync`; pass `snapshotRepo` to `NewCacheStatsService`.
- `deploy/helm/dagger-kubernetes/values.yaml` — `cache.sync.*` + `fleet.cacheSyncImage`.
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — render both.
- `docs/README.md` — feature + config table + operational caveats.

**No change needed:** `Dockerfile`, `dagger/main.go`, `DAGGER.md` (the helper is
a subcommand of the existing binary; no new binary/image and no dagger changes).

---

## 6. Step-by-step implementation order

1. **Domain:** add `CacheSyncConfig` + `Sync` to `CacheConfig`, add
   `EngineCacheSyncImage` to `FleetConfig`; create `cache_sync.go` (constants,
   `WorkerSnapshotTag`, `CacheSnapshotClient`).
2. **Config loader:** add `v.SetDefault` for the six `cache.sync.*` keys and
   `fleet.engine_cache_sync_image`; add `validateCacheSyncConfig` (fail/disable
   gracefully when `enabled` but `backend != "registry"`; `interval >= 0`,
   `quiesce_wait >= 0`). Update `config.app.yaml.sample`.
3. **Repository — streaming upload:** add `UploadBlobStream` to
   `registry_client.go`.
4. **Repository — archive:** implement `worker_archive.go` (`TarGzipDir` /
   `UntarGzipDir`) + tests (round-trip, traversal rejection, digest determinism).
5. **Repository — snapshot store:** implement `worker_snapshot.go` + tests with
   a stub `CacheSnapshotClient` (round-trip push/pull, missing-snapshot ok=false,
   upload error, manifest error).
6. **Service — stats skip:** add `snapshotRepo` to `CacheStatsService`, skip in
   `collectEntries`/`gcCollectEntries`/`Purge`; update constructor callers +
   tests.
7. **CLI — helper:** implement `cmd/api/cache_sync.go` (`restore` and `serve`
   using `TarGzipDir`/`UntarGzipDir` + `WorkerSnapshotStore`), register in
   `cmd/api/main.go`.
8. **K8sProvider:** add `K8sCacheSyncConfig` + `CacheSync` field; render init
   `cache-restore` and sidecar `cache-sync` + emptyDir + env; add tests.
9. **Wiring (`cmd/api/main.go`):** change `createProvider` signature to also
   receive `cacheBackends`; build `K8sCacheSyncConfig` from `cfg.Cache.Sync` +
   `cacheBackends[0]` + `cfg.Fleet.EngineCacheSyncImage`; pass
   `domain.WorkerSnapshotsRepo` to `NewCacheStatsService`.
10. **Helm:** add `supervisor.config.cache.sync.*` + `fleet.cacheSyncImage`
    values and render them in `configmap.yaml`.
11. **Docs:** `docs/README.md`.
12. **CI gate:** `go build ./... && go vet ./... && go test ./...` +
    `dagger call -m ./dagger --src . lint` (and full `ci export` if a Docker
    daemon is available). Then the mandatory local redeploy (§8).

---

## 7. Edge cases, error handling & validation rules

- **Fresh vs retained PVC (restore correctness).** On scale-down the PVC is
  *retained* (`WhenScaled: Retain`) and reattached on scale-up, so the worker
  dir is already warm. The restore init container must **skip pull+untar when
  `/var/lib/dagger/worker` already has content** (preserve the newer local
  cache). Restore only runs on a truly empty/missing worker dir (fresh PVC /
  node migration).
- **Snapshot missing on first boot.** `Pull` returns `ok=false`; log + exit 0.
- **Concurrent stop → same tag.** Atomic last-writer-wins manifest PUT; any
  winner is a valid snapshot (D3). No lock.
- **Truncated push (SIGKILL mid-upload).** Manifest never updated → previous
  snapshot intact. Orphaned in-progress blobs are reaped by the registry GC.
- **Digest mismatch on dirty periodic push.** Registry rejects the PUT; log +
  skip; do not retry-loop.
- **Path traversal in untar.** `UntarGzipDir` rejects absolute and `../` paths.
- **Security contexts.** The sync containers run as root (PVC is root-owned by
  the privileged engine); mirror the engine's `Privileged` flag.
- **Temporary disk.** The sidecar's tarball lives on a node-backed emptyDir; the
  node needs free disk ≈ worker-dir size. Documented caveat.
- **Termination-grace budget.** Final push must finish within
  `engine_termination_grace_seconds` minus `quiesce_wait`; raise the former for
  large caches.
- **Disabled sync image.** `cache.sync.enabled` + empty
  `fleet.engine_cache_sync_image` → WARN and omit init/sidecar (never break the
  engine pod).
- **Non-registry backend.** `cache.backend == "s3"` + `sync.enabled` → WARN and
  disable sync (snapshots require an OCI registry).

---

## 8. Testing plan

**Unit (stdlib `testing`, table-driven, `logrus` with `io.Discard`):**
- `worker_archive_test.go`: tar→untar round-trip preserves files/permissions;
  empty dir; nested paths; path-traversal rejection; digest+size determinism
  (same input → same digest); large-ish input streams without buffering.
- `worker_snapshot_test.go`: stub `CacheSnapshotClient` (mirrors
  `stubCLIRegistryClient` in `cli_cache_registry_test.go`) — push then pull
  round-trip; missing snapshot → `ok=false`; upload error; manifest-put error;
  tag format (`WorkerSnapshotTag("v0.20.0") == "worker-v0-20-0"`).
- `registry_client_test.go` (extend): `UploadBlobStream` initiates upload +
  streams PUT with correct digest/size/length; 404/5xx → error; invalid digest
  → error. Reuse the existing `httptest`-based `testClient` pattern.
- `k8s_provider_test.go` (extend): with `CacheSync` enabled, assert the
  `cache-restore` init container + `cache-sync` sidecar are present with the
  correct image/command/env/mounts/securityContext; disabled → absent;
  `OnStart=false` → no init container; `OnStop=false && interval==0` → no sidecar.
- `cache_stats_test.go` (extend): snapshot repo excluded from stats/GC/purge;
  normal repos unaffected.

**Integration (`tests/integration/`, real Hertz servers, `freeAddr(t)`):**
- Stand up a minimal in-memory OCI Distribution v2 registry (`httptest`/Hertz
  handler implementing `/v2/<repo>/blobs/uploads`, `.../blobs/<digest>`,
  `.../manifests/<tag>`) on `freeAddr(t)`; drive `WorkerSnapshotStore.Push` +
  `Pull` end-to-end, asserting a second store instance reads back identical
  bytes and that concurrent `Push` to the same tag leaves a valid manifest
  (last-writer-wins).
- No hardcoded ports; shut down servers via `t.Cleanup` + timed `Shutdown`.

**Mandatory local validation (AGENTS.local.md §4–§6):**
1. `docker build -t docker.io/disaster/dagger-kubernetes:dev .` and push.
2. `helm get values` to capture live values; add
   `supervisor.config.cache.sync.*` and ensure `fleet.cacheSyncImage` renders
   `docker.io/disaster/dagger-kubernetes:dev` (verify in `configmap.yaml` output).
3. `helm upgrade --install ... -f <captured>.yaml --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes`.
4. `rollout restart` the supervisor StatefulSet and `rollout status`.
5. Agent checks: pods Ready; `/healthz`/`/readyz` 200; authed
   `/api/v1/status` and `/api/v1/cache`; supervisor logs free of fatal errors.
6. Trigger an engine fleet (run a pipeline or provision an engine), then scale
   it to zero; confirm a `worker-<version>` tag appears in the registry
   (`docker-registry` service), then scale back up and confirm the new pod's
   restore logs "restored snapshot" (or "skipped: existing cache").
7. Human verification of the live UI (MagicCache, Runners, Pipelines).

---

## 9. Documentation changes

- `docs/README.md`: new "Worker-cache sync (warm start)" subsection under the
  Remote shared cache section — behavior, `cache.sync.*` keys, `fleet.engine_cache_sync_image`,
  the best-effort/dirty-snapshot semantics, termination-grace and temp-disk
  caveats, and the per-version tag naming. Add rows to the config-key table.
- `config/config.app.yaml.sample`: add `cache.sync` block + `fleet.engine_cache_sync_image`
  with comments mirroring `loader.go` defaults.
- `deploy/helm/dagger-kubernetes/values.yaml` + `README.md` (chart): document
  `supervisor.config.cache.sync.*` and `fleet.cacheSyncImage`.
- `AGENTS.local.md` §3/§7: update the deployed-values reference if the live
  release's values change (per the mandate).
- No `DAGGER.md` change (no `dagger/`, CI-script, or workflow change).
- No new ADR required (this is additive and documented in README); note the
  design in the feature PR description.

---

## 10. Out of scope / follow-ups

- Cross-version snapshot restore (worker dirs are version-specific; the remote
  `:cache` tag already covers cross-version sharing).
- Secret-ref (not literal env) injection of backend registry credentials into
  the sync containers.
- Compression tuning (gzip level, skipping already-compressed blobs) and
  skip-if-unchanged optimization (compare remote digest before a full re-upload).
- Prometheus metrics for sync (last push size/duration/success).
- Excluding the snapshot repo from the admin "Purge cache" button (currently
  snapshots would also be purged — acceptable and arguably desirable; revisit if
  operators ask to preserve them).
