# Remove worker-cache sync + post-prune sync, add local image mirror

**Status:** Ready for implementation
**Scope:** Go supervisor (`cmd/`, `internal/`, `config/`), Vue UI (`ui/`), Helm chart (`deploy/helm/`), docs (`docs/`, ADRs)
**Supersedes:** `.opencode/plans/engine-cache-sync.md`, `engine-cache-sync-v2.md`, `engine-cache-sync-v3.md`; amends `.opencode/plans/engine-cache-purge.md` (drops its sync step)
**New ADR:** `ADR-033-local-image-mirror.md`

---

## 1. Overview / problem statement

Three coupled changes, driven by an operator decision:

1. **Remove the worker-cache sync entirely.** The S3 snapshot of `/var/lib/dagger/worker` (`cache.sync.*`, `cache-restore` init container, `cache-sync` sidecar, `S3SnapshotStore`, `push-once`) is deleted. Restoring large snapshots on fresh pods is slow and restores data that may never be used; the retained engine PVC already gives warm starts. The PVC stays on scale-down and is deleted only when the engine StatefulSet is deleted.
2. **Keep the per-version local-cache purge** (in-process dagql `engine.localCache.prune`) but **drop the post-prune `push-once` sync step** (there is no snapshot anymore). The purge becomes purely local.
3. **Add a local image cache / registry mirror** (new, enable/disable via Helm): a CNCF Distribution pull-through proxy per upstream registry, backed by the shared MinIO/S3, wired into the engine's `engine.toml` registry mirrors, with presets for the major registries plus custom/private registries.

A cross-cutting discovery drives the config layout of (1): **`cache.sync.s3_endpoint/s3_region/s3_use_ssl/s3_access_key/s3_secret_key` are not sync-only** — they are the shared S3 client config consumed by the **CLI cache** (`cli.enabled`, default `true`) and the **S3 cache GC** (`S3CacheGC`, `cache/` prefix) in `cmd/api/main.go:304-313`. They must be **relocated to `cache.s3.*`**, not deleted, or the CLI cache and cache GC break.

---

## 2. Design decisions

### D1 — Remove worker-cache sync; relocate shared S3 config to `cache.s3.*`

- **Delete** the sync-specific config (`cache.sync.enabled/on_start/on_stop/interval/quiesce_wait`, `cache.sync.s3_bucket`) and `fleet.engine_cache_sync_image`.
- **Relocate** the shared S3 client settings from `cache.sync.s3_*` to `cache.s3.*`: `endpoint`, `region` (default `us-east-1`), `use_ssl`, `access_key`, `secret_key`. `cache.s3.bucket` already exists.
- **Delete** all snapshot code (see §5 file list). Shared S3 primitives (`s3ObjectAPI`, `minioS3API`, `NewS3Client`, `isNoSuchKey`) are preserved by moving them to a new `s3_client.go`.
- **Keep** the PVC retention policy exactly as-is (`WhenScaled: Retain`, `WhenDeleted: Delete`, `k8s_provider.go:326-329`). No new config key: the operator judged it not genuinely useful yet; if later needed it can be added, but this plan does not.
- `domain.S3CachePrefix` (`"cache"`) is **kept** (used by `S3CacheGC`) and moved into `cache.go`; every other symbol in `domain/cache_sync.go` is deleted.

### D2 — Keep purge, drop the sync step

- Remove `EnginePodPurgeResult.Synced`, `domain.SidecarSyncPusher`, `repository.ExecSyncPusher`, the `supervisor cache-sync push-once` subcommand, and the `pods/exec` dependency.
- `EngineCachePurgeService` becomes `NewEngineCachePurgeService(provider, pruner, logger)`; `prunePod` only calls the pruner.
- The `synced` field and `"synced %d of %d"` summary are removed from the API result shape, the UI, tests, and docs.
- **RBAC:** `pods/exec` was added solely for `ExecSyncPusher` (verified: the only `SubResource("exec")`/`remotecommand` use is `exec_sync_pusher.go`). Remove the `pods/exec` rule from `rbac.yaml`. `pods/log` is also unused by Go code (engine logs flow through Loki), but that is a pre-existing condition — flagged, not removed here (see §10).

### D3 — Image mirror mechanism: CNCF Distribution pull-through proxy, one Deployment per upstream, S3 (MinIO) backing

Evaluated alternatives:

| Option | Verdict |
|---|---|
| **(a) CNCF Distribution (`registry:2`/`registry:3`) pull-through proxy, one instance per upstream, S3 backing** | **Chosen.** Standard, single-remote proxy mode (`REGISTRY_PROXY_REMOTEURL`), one Deployment+Service per upstream; S3 driver stores blobs in the existing MinIO; no new DB/Redis; one mirror host maps 1:1 to `engine.toml mirrors = [...]`. |
| (b) Harbor proxy-cache projects | Rejected: full Harbor stack (DB/Redis/registry) for a single concern; heavy. |
| (c) Zot multi-upstream sync | Rejected: replication/sync model (explicit repo list), not on-demand pull-through; more moving parts. |
| (d) single registry + multiple proxy instances behind one Service | Rejected: distribution proxy is single-remote per process, so one instance per upstream is unavoidable; a shared Service cannot route by upstream host without a custom router. |

**Why S3 (MinIO) rather than per-mirror PVC:** the chart already ships MinIO with a bucket + `engine-s3-auth` Secret; S3 backing is the operator-stated preference ("backed by S3/MinIO"), shares capacity, and needs no new per-mirror storage class. A `pvc` filesystem option is provided as an escape hatch.

**Mechanism summary:**

- One `Deployment` + one `Service` (port `5000`) per enabled preset / custom registry.
- Mirror address = `<release>-<slug>-mirror.<namespace>.svc:5000` (`slug` = host with non-`[a-z0-9-]` → `-`; e.g. `docker.io` → `docker-io`).
- Distribution is configured **entirely via env vars** (no config file), e.g. `REGISTRY_PROXY_REMOTEURL`, `REGISTRY_PROXY_TTL`, `REGISTRY_STORAGE=s3`, `REGISTRY_STORAGE_S3_*`. `REGISTRY_STORAGE_S3_REGIONENDPOINT` points at `<release>-minio.<ns>.svc:9000` with `forcepathstyle=true`, `secure=false`, `v4auth=true`. Credentials come from the existing `engine-s3-auth` Secret.
- Private upstreams get `REGISTRY_PROXY_USERNAME`/`REGISTRY_PROXY_PASSWORD` from a per-registry Secret ref.
- **Auth for the engine:** the in-cluster mirror is unauthenticated plaintext HTTP, so the engine needs no credential for it; `engine-image-auth` (`.dockerconfigjson`) stays for the kubelet's engine-image pull and any non-mirrored registry.
- **TTL/invalidation:** `REGISTRY_PROXY_TTL` (default `168h`). Moving tags re-validate after TTL. Manual invalidation requires `REGISTRY_STORAGE_DELETE_ENABLED=true` (documented, default off).

**Registry presets (public, fixed `remoteurl`):**

| Helm preset key | `engine.toml` host | `REGISTRY_PROXY_REMOTEURL` |
|---|---|---|
| `docker.io` | `docker.io` | `https://registry-1.docker.io` |
| `ghcr.io` | `ghcr.io` | `https://ghcr.io` |
| `public.ecr.aws` | `public.ecr.aws` | `https://public.ecr.aws` |
| `quay.io` | `quay.io` | `https://quay.io` |
| `gcr.io` | `gcr.io` | `https://gcr.io` |
| `registry.dagger.io` | `registry.dagger.io` | `https://registry.dagger.io` (default **off**) |

Private AWS ECR, GAR, and any custom upstream are handled by the **`imageCache.registries`** list (explicit `host` + `remoteUrl` + credentials), which covers "ECR private / GAR / Harbor / internal".

### D4 — `engine.toml` mirror wiring needs `http = true` for in-cluster plaintext mirrors

The current `render()` (in `internal/repository/engine_toml.go`) only emits `[registry."<host>"]\n  mirrors = [...]`. Dagger's `engine.toml` uses **BuildKit's** registry config (`docs.dagger.io/reference/configuration/engine` → "legacy BuildKit-style `engine.toml`"). For a plaintext in-cluster mirror, BuildKit needs the mirror host itself declared with `http = true` (per `buildkitd.toml`):

```toml
[registry."docker.io"]
  mirrors = ["docker-io-mirror.dagger-kubernetes.svc:5000"]

# optionally mirror configuration can be done by defining it as a registry.
[registry."docker-io-mirror.dagger-kubernetes.svc:5000"]
  http = true
```

Decision: add a new supervisor config key **`fleet.engine_registry_mirrors_http`** (`[]string`, mirror host[:port] values that are plaintext HTTP). The chart computes it from the generated mirror addresses and merges the generated mirrors into `fleet.engine_registry_mirrors`. `engineTOML.render()` emits a `[registry."<mirror>"]\n  http = true` section per entry. This keeps the existing `mirrors` map rendering intact and makes HTTP marking explicit (no hostname-pattern magic).

### D5 — `cache/` GC/stats: keep for now (evaluate, flag)

`S3CacheGC` and the registry `CacheStatsService` target the **remote BuildKit `cache/` prefix / `dagger-cache` OCI repo**, which is a *separate* feature from the worker snapshots. Removing the worker sync does **not** require removing them. However `CacheStatsService` currently skips `domain.WorkerSnapshotsRepo`, which no longer exists — that skip (and the constructor param) is removed as part of (1). The broader question of removing `S3CacheGC`/cache stats/`RegistryRouter` (the whole remote-cache proxy) is **flagged as an optional follow-up**, out of scope here.

---

## 3. Data structures & signatures

### 3.1 Go — domain

`internal/domain/config.go`:

```go
type S3Config struct {
    Bucket    string `mapstructure:"bucket"`
    Region    string `mapstructure:"region"`
    Endpoint  string `mapstructure:"endpoint"`   // NEW (relocated from cache.sync.s3_endpoint)
    UseSSL    bool   `mapstructure:"use_ssl"`    // NEW
    AccessKey string `mapstructure:"access_key"` // NEW
    SecretKey string `mapstructure:"secret_key"` // NEW
}
```

`CacheConfig`: delete the `Sync CacheSyncConfig` field; delete the whole `CacheSyncConfig` type.

`FleetConfig`: delete `EngineCacheSyncImage`; add:

```go
EngineRegistryMirrorsHTTP []string `mapstructure:"engine_registry_mirrors_http"`
```

`internal/domain/cache.go`: add (moved from `cache_sync.go`):

```go
// S3CachePrefix is the S3 key prefix holding the BuildKit remote-cache refs.
const S3CachePrefix = "cache"
```

`internal/domain/cache_purge.go`: delete `Synced` from `EnginePodPurgeResult` and delete `SidecarSyncPusher`:

```go
type EnginePodPurgeResult struct {
    PodName string `json:"pod_name"`
    Ordinal int    `json:"ordinal"`
    Pruned  bool   `json:"pruned"` // engine.localCache.prune succeeded on this pod
    Error   string `json:"error,omitempty"`
}
// EnginePruner, EngineCachePurger, ErrEngineFleetNotFound, ErrPurgeInProgress unchanged.
```

`internal/domain/cache_sync.go`: **delete file** (all constants/functions/`CacheSnapshotClient` except `S3CachePrefix`, moved above).

### 3.2 Go — service

`internal/service/engine_cache_purge.go`:

```go
func NewEngineCachePurgeService(provider domain.FleetProvider, pruner domain.EnginePruner, logger *logrus.Logger) *EngineCachePurgeService
```

Remove `pusher` field, `pushOnceTimeout`, the `s.pusher == nil` check, and the push block in `prunePod`. `purgeSummary` drops the `synced` branch.

`internal/service/cache_stats.go`:

```go
func NewCacheStatsService(cache *Cache, router *RegistryRouter, metricsClient domain.CacheMetricsClient, gcCfg domain.GCConfig, logger *logrus.Logger, obs *observ.Metrics) *CacheStatsService
```

Remove the `snapshotRepo` field, its constructor param, and the `skipRepo` skip logic.

### 3.3 Go — repository

`internal/repository/engine_toml.go`:

```go
type engineTOML struct {
    Debug           bool
    LogFormat       string
    RegistryMirrors map[string][]string
    MirrorHTTP      []string // mirror host[:port] strings dialed over plaintext HTTP
}
```

`render()` emits, after the existing sections, one `[registry."<mirror>"]\n  http = true\n` per `MirrorHTTP` entry (sorted, deduped).

`internal/repository/k8s_provider.go`: delete `K8sCacheSyncConfig`, the `CacheSync` field, and the sync constants/methods; add to `K8sProviderConfig`:

```go
MirrorHTTP []string // engine.toml: [registry."<mirror>"] http = true
```

and pass it through `renderEngineTOML()`.

New `internal/repository/s3_client.go` (moved verbatim from `s3_snapshot.go`): `s3ObjectAPI`, `minioS3API`, `NewS3Client`, `isNoSuchKey`.

### 3.4 Go — cmd/api

- `newK8sClientset() (kubernetes.Interface, error)` (drop the `*rest.Config`; it was only consumed by `ExecSyncPusher`).
- `NewS3Client(cfg.Cache.S3.Endpoint, cfg.Cache.S3.Region, cfg.Cache.S3.AccessKey, cfg.Cache.S3.SecretKey, cfg.Cache.S3.UseSSL)`.
- `NewEngineCachePurgeService(provider, pruner, logger)` (no pusher).
- `NewCacheStatsService(cacheBackend, nil, metricsClient, cfg.Cache.GC, logger, metrics)` (no `domain.WorkerSnapshotsRepo`).
- `createProvider`: drop `CacheSync:`; add `MirrorHTTP: cfg.Fleet.EngineRegistryMirrorsHTTP`.

### 3.5 Helm values schema (`imageCache`)

```yaml
imageCache:
  enabled: false
  image:
    repository: "registry"          # CNCF Distribution
    tag: "2.8.3"                    # battle-tested pull-through proxy; registry:3 is an option
    pullPolicy: IfNotPresent
  ttl: "168h"                        # REGISTRY_PROXY_TTL
  logLevel: "warn"
  storage:
    backend: "s3"                    # "s3" (shared MinIO) | "pvc" (per-mirror PVC)
    s3:
      bucket: "image-cache"          # auto-created when minio.enabled
      region: "us-east-1"
      endpoint: ""                   # auto-wired <release>-minio.<ns>.svc:9000 when minio.enabled
      forcePathStyle: true
      secure: false
    pvc:
      storageClass: ""
      size: "20Gi"
  resources:
    requests: { cpu: "100m", memory: "128Mi" }
    limits:   { cpu: "500m", memory: "512Mi" }
  nodeSelector: {}
  tolerations: []
  presets:
    docker.io:          { enabled: true,  username: "", passwordSecretRef: { name: "", key: "" } }
    ghcr.io:            { enabled: true,  username: "", passwordSecretRef: { name: "", key: "" } }
    public.ecr.aws:     { enabled: true,  username: "", passwordSecretRef: { name: "", key: "" } }
    quay.io:            { enabled: true,  username: "", passwordSecretRef: { name: "", key: "" } }
    gcr.io:             { enabled: false, username: "", passwordSecretRef: { name: "", key: "" } }
    registry.dagger.io: { enabled: false, username: "", passwordSecretRef: { name: "", key: "" } }
  registries: []                     # extra/custom upstreams (private ECR/GAR/Harbor/…)
    # - name: "my-ecr"
    #   host: "123456789012.dkr.ecr.us-east-1.amazonaws.com"
    #   remoteUrl: "https://123456789012.dkr.ecr.us-east-1.amazonaws.com"
    #   username: "AWS"
    #   passwordSecretRef: { name: "ecr-creds", key: "password" }
```

Also: `supervisor.config.cache.s3` gains `endpoint`/`region`/`useSSL`/`accessKey`/`secretKey`; `supervisor.config.cache.sync.*` and `supervisor.config.fleet.engineCacheSyncImage` are deleted; `minio.buckets` gains `image-cache`.

### 3.6 Helm helpers / templates to add

- `_helpers.tpl`: `dagger-kubernetes.imageCacheRegistries` (normalized list of `{slug,host,remoteUrl,username,passwordSecretRef}` from presets+custom), `dagger-kubernetes.engineRegistryMirrors` (merged `engineRegistryMirrors` + image-cache mirrors), `dagger-kubernetes.imageCacheMirrorHosts` (list of generated mirror addresses), `dagger-kubernetes.imageCacheMirrorAddress`. Update `s3Endpoint`/`s3Bucket` helpers to read `cache.s3.*` instead of `cache.sync.*`.
- New `templates/image-cache.yaml`: for each registry in `imageCacheRegistries`, a `Deployment` + `Service` (and, when `storage.backend: pvc`, a PVC via `volumeClaimTemplates`).
- `configmap.yaml`: render `fleet.engine_registry_mirrors` (merged) and `fleet.engine_registry_mirrors_http` (generated mirror hosts); delete the `sync:` block and `engine_cache_sync_image`; render `cache.s3.endpoint/use_ssl/...`.
- `secret.yaml`: `engine-s3-auth` stays (now feeds the supervisor env `DAGGER_KUBERNETES_CACHE_S3_*` and the mirror S3 env); update the comment.
- `statefulset.yaml`: rename the two supervisor env vars `DAGGER_KUBERNETES_CACHE_SYNC_S3_*` → `DAGGER_KUBERNETES_CACHE_S3_*`; update the comment.

### 3.7 Rendered `engine.toml` shape (example)

```toml
[log]
  format = "json"

[registry."docker.io"]
  mirrors = ["docker-io-mirror.dagger-kubernetes.svc:5000"]

[registry."ghcr.io"]
  mirrors = ["ghcr-io-mirror.dagger-kubernetes.svc:5000"]

[registry."docker-io-mirror.dagger-kubernetes.svc:5000"]
  http = true

[registry."ghcr-io-mirror.dagger-kubernetes.svc:5000"]
  http = true
```

---

## 4. File-by-file change list

### Delete (entire files)

| Path | Why |
|---|---|
| `cmd/api/cache_sync.go` | `cache-sync` CLI (restore/serve/push-once), `loadCacheSyncEnv`, `S3SnapshotStore` wiring |
| `cmd/api/cache_sync_test.go` | tests the above |
| `internal/domain/cache_sync.go` | worker-snapshot constants/functions/`CacheSnapshotClient` (keep `S3CachePrefix`, moved) |
| `internal/repository/worker_snapshot.go` + `_test.go` | registry `WorkerSnapshotStore` (v1/v2) |
| `internal/repository/worker_sync.go` + `_test.go` | content-store walk/probe/upload/manifest helpers |
| `internal/repository/worker_archive.go` + `_test.go` | `TarGzipDir`/`TarGzipSubset`/`UntarGzipDir`/`writeTarFile` |
| `internal/repository/s3_snapshot.go` + `_test.go` | `S3SnapshotStore` (shared S3 primitives moved to `s3_client.go`) |
| `internal/repository/exec_sync_pusher.go` | `pods/exec` `ExecSyncPusher` |
| `tests/integration/worker_snapshot_test.go` | registry snapshot integration |
| `tests/integration/worker_snapshot_s3_test.go` | S3 snapshot integration |

### Create

| Path | Content |
|---|---|
| `internal/repository/s3_client.go` | `s3ObjectAPI`, `minioS3API`, `NewS3Client`, `isNoSuchKey` (moved) |
| `deploy/helm/dagger-kubernetes/templates/image-cache.yaml` | mirror Deployments + Services (+ PVC option) |
| `docs/design/ADR-033-local-image-mirror.md` | image mirror ADR |

### Modify

| Path | Change |
|---|---|
| `internal/domain/config.go` | remove `CacheSyncConfig`/`Sync`/`EngineCacheSyncImage`; extend `S3Config`; add `EngineRegistryMirrorsHTTP` |
| `internal/domain/cache.go` | add `S3CachePrefix` |
| `internal/domain/cache_purge.go` | remove `Synced` + `SidecarSyncPusher` |
| `internal/service/engine_cache_purge.go` | drop pusher/synced |
| `internal/service/engine_cache_purge_test.go` | drop fake pusher + synced assertions |
| `internal/service/cache_stats.go` | drop `snapshotRepo` param/field/skip |
| `internal/service/cache_stats_test.go` | drop snapshotRepo/skipRepo cases |
| `internal/repository/k8s_provider.go` | remove sync config/containers/volumes/env; add `MirrorHTTP` |
| `internal/repository/k8s_provider_test.go` | remove sync env/sidecar assertions |
| `internal/repository/engine_toml.go` + `_test.go` | add `MirrorHTTP` + `http = true` rendering |
| `config/loader.go` | remove `cache.sync.*` defaults + `validateCacheSyncConfig`; add `cache.s3.endpoint/use_ssl/access_key/secret_key` + `fleet.engine_registry_mirrors_http` defaults |
| `config/loader_test.go` | update/remove cache.sync validation cases |
| `cmd/api/main.go` | wiring per §3.4 |
| `internal/handler/fleet_purge.go` | comment only (drop "syncs each pruned pod") |
| `internal/handler/fleet_purge_test.go` | drop synced fields in fixtures |
| `tests/integration/engine_cache_purge_test.go` | drop fake pusher + synced assertions |
| `tests/integration/cache_status_test.go` | drop `domain.WorkerSnapshotsRepo` arg |
| `ui/src/api/types.ts` | remove `synced` from `EnginePodPurgeResult` |
| `ui/src/fleet/Runners.vue` | remove `synced` from `purgeSummary` |
| `deploy/helm/dagger-kubernetes/values.yaml` | remove sync values + `engineCacheSyncImage`; add `imageCache` + `cache.s3` fields + `image-cache` bucket |
| `deploy/helm/dagger-kubernetes/templates/configmap.yaml` | remove `sync:` block + `engine_cache_sync_image`; add `cache.s3.*` + mirrors-http |
| `deploy/helm/dagger-kubernetes/templates/_helpers.tpl` | rewire `s3Endpoint`/`s3Bucket`; add image-cache helpers |
| `deploy/helm/dagger-kubernetes/templates/secret.yaml` | comment update |
| `deploy/helm/dagger-kubernetes/templates/statefulset.yaml` | rename S3 env vars |
| `deploy/helm/dagger-kubernetes/templates/rbac.yaml` | remove `pods/exec` rule |
| `deploy/helm/dagger-kubernetes/README.md` | regenerate values table + notes |
| `config/config.app.yaml.sample` | remove `cache.sync.*` + `engine_cache_sync_image`; add `cache.s3.endpoint/use_ssl` + `engine_registry_mirrors_http` |
| `docs/README.md` | remove worker-sync sections; document `cache.s3.*`, image mirror, purge result shape |
| `docs/design/ADR-032-engine-cache-purge.md` | supersede note: remove sync step; keep pruner |
| `docs/design/ADR-006/012/013/028` | update the "Superseded by `cache.sync.*`" banners → "worker-cache sync removed; warm start is the retained PVC" |
| `docs/design/index.md` | add ADR-033 row |
| `DAGGER.md` | no-op unless pins change (image mirror touches Helm templates → update Helm-related troubleshooting if any) |

---

## 5. Implementation order

1. **Config + domain**: relocate `cache.s3.*` + delete `CacheSyncConfig`/`EngineCacheSyncImage` + add `EngineRegistryMirrorsHTTP` (`config/loader.go`, `internal/domain/config.go`, `internal/domain/cache.go`, `internal/domain/cache_sync.go` deleted).
2. **Repository snapshot deletion**: move shared S3 primitives to `s3_client.go`, delete `worker_snapshot.go`/`worker_sync.go`/`worker_archive.go`/`s3_snapshot.go`/`exec_sync_pusher.go` (+ their tests).
3. **Purge service** (`engine_cache_purge.go`, `cache_stats.go`, `cache_purge.go`): drop pusher/synced/snapshotRepo.
4. **K8s provider + engine.toml** (`k8s_provider.go`, `engine_toml.go`): remove sync; add `MirrorHTTP`.
5. **cmd/api** (`main.go`, delete `cache_sync.go`/`cache_sync_test.go`): rewire.
6. **Handler + tests**: `fleet_purge.go`, `fleet_purge_test.go`, unit tests, integration tests.
7. **UI**: `types.ts`, `Runners.vue`.
8. **Helm**: values/configmap/helpers/secret/statefulset/rbac + new `image-cache.yaml`.
9. **Docs + ADRs** (last, so the changeset matches the code).
10. **Lint + CI gate + redeploy** (§9).

---

## 6. Edge cases, error handling, validation

- **`cache.s3.endpoint` empty** (non-S3 deployments): keep the existing WARN + disable behavior in `cmd/api/main.go` (`"cache.s3.endpoint is empty; s3-backed CLI cache and cache GC disabled"`). No hard validation error — preserves non-S3 dev setups. The old hard error `cache.sync.s3_endpoint is required` disappears with `validateCacheSyncConfig`.
- **`cli.enabled` requires an S3 bucket**: unchanged (`validateCLIConfig` already errors when `cli.s3_bucket`/`cache.s3.bucket` empty).
- **`cache.s3.region` default**: `""` → `us-east-1` (matches the removed `cache.sync.s3_region` default; MinIO ignores it).
- **Empty rendered `engine.toml`**: unchanged — `render()` returns `""` → no ConfigMap volume; `MirrorHTTP` empty contributes nothing.
- **Purge with no running pods**: unchanged — `replicas:0`, `completed`, "no running engine pods; nothing to prune"; no snapshot prefix exists to worry about anymore.
- **Purge partial failure**: per-pod `pruned`/`error`; no sync to skip; aggregate `completed` with per-pod detail.
- **Image cache disabled (`imageCache.enabled: false`)**: no Deployments/Services; `engine_registry_mirrors` and `engine_registry_mirrors_http` are unchanged (user-provided values pass through). The mirror Deployment templates are gated on the flag.
- **Preset + custom host collision / duplicate slug**: the chart derives `slug` from `host`; a custom registry whose sanitized slug collides with a preset's is a template error (`fail`) — validate at `helm template` time.
- **Private upstream credential missing**: a preset/custom entry with a `passwordSecretRef` referencing a missing Secret must fail-closed at pod admission (non-optional `secretKeyRef`), matching the existing `secretEnvVar` convention.
- **Mirror upstream unreachable**: distribution returns a registry error on pull; the engine retries/falls back per BuildKit resolver semantics. Documented as degraded (mirror down → engine fails the pull; no silent fallback to upstream by the engine).
- **`pods/exec` RBAC removed**: `ExecSyncPusher` is gone, so nothing calls it; no runtime path needs it.

---

## 7. Testing plan

### Unit (table-driven, `testing` only)

- `config/loader_test.go`: `cache.s3.endpoint/use_ssl/access_key/secret_key` defaults + decode; no `cache.sync.*` keys remain; `engine_registry_mirrors_http` decodes (dotted values preserved); `validateCacheSyncConfig` gone.
- `internal/repository/engine_toml_test.go`: `MirrorHTTP` renders `[registry."<mirror>"]\n  http = true` sorted/deduped; combined with mirrors; empty case.
- `internal/repository/s3_client_test.go` (or kept in `s3_cli_cache_test.go`): `NewS3Client`/`isNoSuchKey` still covered.
- `internal/repository/k8s_provider_test.go`: no `cache-restore`/`cache-sync` containers, no `cache-sync-tmp` volume, no `CACHE_SYNC_*` env; `MirrorHTTP` passes into the ConfigMap.
- `internal/service/engine_cache_purge_test.go`: drop fake pusher; `TestPurgeHappyPathNRunning` asserts N pruner calls + `pruned:true` only; partial-failure case asserts no sync side-effects.
- `internal/service/cache_stats_test.go`: `NewCacheStatsService` without `snapshotRepo`; remove `skipRepo` assertions.

### Integration (`tests/integration/`)

- `engine_cache_purge_test.go`: fake pruner only; assert `pruned:true`, no `synced`, no pusher; GET status shape.
- `cache_status_test.go`: drop the `WorkerSnapshotsRepo` argument.
- Delete `worker_snapshot_test.go` + `worker_snapshot_s3_test.go`.
- New: Helm `helm template` render assertions (no `pods/exec`; mirror Deployments/Services present when `imageCache.enabled: true`; `engine_registry_mirrors_http` populated).

### Manual live validation (AGENTS.local.md §6)

After redeploy:
1. Confirm engine pods no longer show `cache-restore` init or `cache-sync` sidecar; PVCs intact across a scale-down/up (warm cache retained).
2. Trigger a purge in the UI; confirm "Pruned N/N pods" with no "synced" text; engines stay up.
3. `mc ls` the bucket: `worker-snapshots/` no longer grows (and the `cache/` prefix is untouched by this change).
4. Enable `imageCache` with `docker.io` preset; `dagger -M call ... container from --address=hello-world`; confirm the mirror's access log shows the pull; confirm `engine.toml` in the ConfigMap has the mirror + `http = true`.
5. Private-upstream test (e.g. an ECR private `registries` entry) with a credential Secret.

---

## 8. Documentation changes

- `docs/README.md`: remove the "Worker-cache sync" sections; rewrite the cache section to document `cache.s3.*` (endpoint/region/use_ssl/credentials) as the shared S3 client for CLI cache + cache GC; document the image mirror feature (presets, custom registries, auth, TTL, engine.toml wiring); update the purge API result shape (no `synced`).
- `config/config.app.yaml.sample`: drop `cache.sync.*` + `fleet.engine_cache_sync_image`; add `cache.s3.endpoint/use_ssl/access_key/secret_key` + `fleet.engine_registry_mirrors_http`.
- `deploy/helm/dagger-kubernetes/README.md`: regenerated values table (remove sync; add `imageCache.*` + `cache.s3.*`).
- ADRs:
  - **New** `ADR-033-local-image-mirror.md`: decision D3/D4 (distribution per-upstream proxy, S3 backing, presets, `http = true` mirror marking, `engine_registry_mirrors_http`).
  - `ADR-032-engine-cache-purge.md`: add a superseded banner "post-prune `push-once` removed with the worker-cache sync; purge is now purely local" and trim the sync sections.
  - `ADR-006/012/013/028`: update the "Superseded by `cache.sync.*`" banners to "worker-cache sync removed (2026-09); warm start is now the retained engine PVC; remote BuildKit cache remains removed".

---

## 9. CI gate + redeploy (AGENTS.local.md)

- **Gate** (mandatory): `dagger call -m ./dagger --src . ci export --path out`. Minimum without a daemon: `go build ./... && go vet ./... && go test ./...` plus `dagger call -m ./dagger --src . lint`.
- **Dead-symbol sweep** (the `unused` linter): after deleting files, `grep` the removed symbols (`WorkerSnapshot*`, `MetaTarballName`, `BlobsPrefix`, `CacheSyncConfig`, `K8sCacheSyncConfig`, `SidecarSyncPusher`, `ExecSyncPusher`, `TarGzipDir`, `walkContentStore`, `probeAndUploadMissing`, `buildMultiLayerManifest`, `S3SnapshotStore`, `cacheSync*`, `syncEnv`, `pushWorkerSnapshotS3`, etc.) and confirm zero remaining references; delete any orphaned helper. `emptyJSONDigest`/`emptyJSONSize` stay (used by `cli_cache_registry.go`).
- **Redeploy** (read `AGENTS.local.md` first): build → push `docker.io/disaster/dagger-kubernetes:dev` → `helm upgrade dagger-kubernetes-test ./deploy/helm/dagger-kubernetes` → wait rollout. Verify per §7 manual steps; run the agent + human verification steps from `AGENTS.local.md`.

---

## 10. Migration / rollout

- **Existing S3 `worker-snapshots/` objects**: after upgrade nothing reads/writes them. They are left in place (the removal code no longer exists to delete them; a rollback would still read them). Recommend operators add a MinIO/S3 lifecycle rule to expire `worker-snapshots/` after a grace period; document a one-time `mc rm --recursive <bucket>/worker-snapshots` cleanup.
- **Existing engine PVCs**: unaffected. The StatefulSet update (dropping `cache-restore` + `cache-sync` and their volume) triggers a rolling restart of engine pods; the PVCs are retained (already `WhenScaled: Retain`), so local caches survive. Rolling restart during business hours is the only visible impact.
- **Existing `cache/` prefix + cache GC/stats**: unchanged by this plan (kept; see D5). Removing them is a **flagged follow-up**.
- **Rollback**: revert the image/chart; the previous `cache.sync.*` behavior returns (snapshots still exist if not yet expired).
- **Out-of-scope flags**: (a) removing `pods/log` RBAC (dead but pre-existing); (b) removing `S3CacheGC`/cache stats/`RegistryRouter`; (c) routing the *kubelet's* engine-image pull through the mirror (`fleet.engine_image_registry` → mirror) — `engine.toml` mirrors only cover pipeline image pulls, not the engine image the kubelet pulls.
