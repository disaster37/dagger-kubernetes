# Plan: Store CLI provisioning cache on the Dagger registry

**Status:** Ready for implementation
**Module:** `github.com/disaster/dagger-kubernetes`
**Scope:** Replace local-filesystem CLI tarball cache (`FileCLICache`) with OCI-artifact storage on the shared Dagger registry so every supervisor pod (multi-node Raft) can serve cached CLI binaries without re-downloading from GitHub on each pod.

---

## 1. Problem summary

`FileCLICache` stores verified CLI tarballs at `<database.dir>/cli-cache` on the supervisor's PVC. In a multi-pod Raft cluster each pod has its own PVC, so each pod independently downloads the tarball from GitHub on first request. The Dagger registry already acts as a shared cache accessible to all pods — storing the CLI tarball there makes the cache cluster-wide.

## 2. Solution design

Store each CLI tarball as a **full OCI artifact** in the shared registry:

- **Blob**: the raw `dagger_vX.Y.Z_os_arch.tar.gz` file, content-addressed by sha256 digest.
- **Manifest**: `application/vnd.oci.image.manifest.v1+json` with a single layer referencing the blob, annotated with the tarball's sha256 checksum.
- **Tag**: `vX.Y.Z-os-arch` (e.g. `v0.21.8-linux-amd64`).
- **Repository**: configurable, default `dagger-kubernetes/cli-cache`.

Retrieval: HEAD manifest → extract blob digest → GET blob → write to temp file → stream.

Push: compute sha256 → POST/PUT monolithic blob upload → PUT manifest with annotations.

### 2.1 Cache backend

Only the OCI registry backend is supported. The filesystem backend (`FileCLICache`) and the `cli.cache_backend` config key are removed entirely. `RegistryCLICache` is always used when CLI provisioning is enabled.

### 2.2 Interface changes

`domain.CLICache` gains context on all methods and a lightweight `Has` for existence checks (avoiding a full download on cache-hit in `EnsureCached`):

```go
type CLICache interface {
    Has(ctx context.Context, version, osName, arch string) (bool, error)
    Get(ctx context.Context, version, osName, arch string) (path string, ok bool)
    Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (path string, err error)
    Dir() string
}
```

## 3. Files to create, modify, or delete

### 3.1 New files

| File | Purpose |
|---|---|
| `internal/repository/cli_cache_registry.go` | `RegistryCLICache` — implements `domain.CLICache` on the OCI registry |
| `internal/repository/cli_cache_registry_test.go` | Tests for `RegistryCLICache` |

### 3.2 Modified files

| File | Change |
|---|---|
| `internal/domain/cli.go` | Add context to `CLICache` interface; add `Has` method; add `CLIRegistryClient` interface |
| `internal/repository/registry_client.go` | Add blob-upload and manifest-push methods |
| `internal/service/cli_service.go` | Use `Has` for lightweight existence check; modify `artifact()` to accept sha256/size directly; modify `Open` for temp-file downloads |
| `internal/service/cli_service_test.go` | Update to work with registry-backed cache; add tests for `Has` |
| `internal/handler/cli.go` | No logic changes (interface stays compatible) |
| `internal/handler/cli_test.go` | Update to create registry-backed cache |
| `internal/domain/config.go` | Add `CacheRepo` field to `CLIConfig` (remove `CacheBackend` and `CacheDir`) |
| `config/loader.go` | Add default + validation for `cli.cache_repo` config key |
| `config/config.app.yaml.sample` | Document new `cli.cache_repo` key; remove `cli.cache_backend` and `cli.cache_dir` |
| `cmd/api/main.go` | Wire `RegistryCLICache` directly (no backend switch) |
| `tests/integration/cli_test.go` | Update to use registry-backed cache |
| `docs/design/ADR-023-cli-provisioning.md` | Update "alternatives considered" and cache design section |
| `docs/README.md` | Update CLI provisioning config docs |

### 3.3 Deleted files

| File | Reason |
|---|---|
| `internal/repository/cli_cache.go` | `FileCLICache` is removed; no filesystem backend support |
| `internal/repository/cli_cache_test.go` | Tests for removed `FileCLICache` |

## 4. Data structures and function signatures

### 4.1 Domain interfaces (`internal/domain/cli.go`)

```go
type CLICache interface {
    Has(ctx context.Context, version, osName, arch string) (bool, error)
    Get(ctx context.Context, version, osName, arch string) (path string, ok bool)
    Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (path string, err error)
    Dir() string
}

// CLIRegistryClient is the subset of the OCI registry client the CLI cache
// needs (blob/manifest push + pull). Implemented by repository.RegistryStatsClient.
type CLIRegistryClient interface {
    ManifestExists(ctx context.Context, repo, tag string) (bool, error)
    GetManifest(ctx context.Context, repo, tag string) (*CLIManifest, error)
    GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error)
    UploadBlob(ctx context.Context, repo string, body io.Reader) (digest string, size int64, err error)
    PutManifest(ctx context.Context, repo, tag string, manifest *CLIManifest) error
}

// CLIManifest is a minimal OCI manifest for a single-layer CLI tarball artifact.
type CLIManifest struct {
    SchemaVersion int                `json:"schemaVersion"`
    MediaType     string             `json:"mediaType"`
    Config        CLIManifestConfig  `json:"config"`
    Layers        []CLIManifestLayer `json:"layers"`
    Annotations   map[string]string  `json:"annotations,omitempty"`
}

type CLIManifestConfig struct {
    MediaType string `json:"mediaType"`
    Digest    string `json:"digest"`
    Size      int64  `json:"size"`
}

type CLIManifestLayer struct {
    MediaType string `json:"mediaType"`
    Digest    string `json:"digest"`
    Size      int64  `json:"size"`
}
```

Annotation keys:
- `com.dagger-kubernetes.cli.sha256`: hex sha256 of the tarball
- `com.dagger-kubernetes.cli.version`: e.g. `v0.21.8`
- `com.dagger-kubernetes.cli.filename`: e.g. `dagger_v0.21.8_linux_amd64.tar.gz`

### 4.2 Config changes (`internal/domain/config.go`)

```go
type CLIConfig struct {
    Enabled         bool              `mapstructure:"enabled"`
    CacheRepo       string            `mapstructure:"cache_repo"`     // OCI repo, default "dagger-kubernetes/cli-cache"
    ReleaseListTTL  time.Duration     `mapstructure:"release_list_ttl"`
    DownloadTimeout time.Duration     `mapstructure:"download_timeout"`
    Upstream        CLIUpstreamConfig `mapstructure:"upstream"`
}
```

### 4.3 Registry client additions (`internal/repository/registry_client.go`)

```go
// UploadBlob performs a monolithic blob upload to repo, returning the digest
// and byte count. Uses the OCI Distribution v2 blob upload flow:
// POST /v2/<repo>/blobs/uploads/ → PUT <location>?digest=sha256:<hex>.
func (c *RegistryStatsClient) UploadBlob(ctx context.Context, repo string, body io.Reader) (digest string, size int64, err error)

// PutManifest pushes an OCI manifest to repo:tag.
func (c *RegistryStatsClient) PutManifest(ctx context.Context, repo, tag string, manifest *domain.CLIManifest) error

// GetManifest fetches and decodes a CLI manifest for repo:tag.
// Returns ErrManifestNotFound on 404.
func (c *RegistryStatsClient) GetManifest(ctx context.Context, repo, tag string) (*domain.CLIManifest, error)

// GetBlob streams a blob from repo:digest, returning the body and Content-Length.
func (c *RegistryStatsClient) GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error)

// ManifestExists is a HEAD-based check for a manifest (no body download).
func (c *RegistryStatsClient) ManifestExists(ctx context.Context, repo, tag string) (bool, error)
```

`ManifestExists` already exists as `ProbeManifest` with a slightly different signature. We'll add the convenience method or alias.

### 4.4 RegistryCLICache (`internal/repository/cli_cache_registry.go`)

```go
type RegistryCLICache struct {
    client   domain.CLIRegistryClient
    repo     string   // e.g. "dagger-kubernetes/cli-cache"
    tmpDir   string   // for temp downloads
    logger   *logrus.Logger
}

func NewRegistryCLICache(client domain.CLIRegistryClient, repo, tmpDir string, logger *logrus.Logger) *RegistryCLICache

func (c *RegistryCLICache) Has(ctx context.Context, version, osName, arch string) (bool, error)
func (c *RegistryCLICache) Get(ctx context.Context, version, osName, arch string) (string, bool)
func (c *RegistryCLICache) Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (string, error)
func (c *RegistryCLICache) Dir() string  // returns "" for registry-backed cache
```

**Tag format**: `tagFor(version, osName, arch)` returns `vX.Y.Z-os-arch` (e.g. `v0.21.8-linux-amd64`). The "v" prefix and dashes replace dots so the tag is OCI-compliant (no dots in tag segments are OK per OCI spec, but dashes are safer).

**Has**: HEAD manifest → return `(true, nil)` on 200, `(false, nil)` on 404, `(false, err)` on transport errors.

**Get**: `Has` check → if missing, `(false, "")`. If exists, GET manifest → extract blob digest from `Layers[0].Digest` → GET blob → write to temp file → return path. Temp file is `os.CreateTemp(c.tmpDir, "cli-cache-*")`.

**Put**: Read full body into a buffer while computing sha256 → verify against `sha256Hex` → `UploadBlob` → build manifest with layer descriptor + annotations → `PutManifest`. On success, returns "" (no local path). On checksum mismatch, returns `ErrCLIChecksumMismatch` without uploading.

### 4.5 CLIService changes (`internal/service/cli_service.go`)

**EnsureCached** — use `Has` instead of `Get` for lightweight existence:
```go
func (s *CLIService) EnsureCached(ctx context.Context, version, osName, arch string) (*domain.CLIArtifact, error) {
    // ... validation as before ...

    if ok, err := s.cache.Has(ctx, version, osName, arch); err != nil {
        s.incCache("error")
        return nil, fmt.Errorf("check cache: %w", err)
    } else if ok {
        s.incCache("hit")
        // Build artifact from sha256/size stored in CLIService metadata or
        // from a separate lightweight call. We'll store this on the service
        // side: after Put, we record the sha256 in an in-memory map.
        return s.artifactFromMeta(version, osName, arch), nil
    }
    // ... inflight dedup + fetchAndCache as before ...
}
```

**artifact** split into two:
```go
// artifact builds CLIArtifact from a local file path (used by Open).
func (s *CLIService) artifact(version, osName, arch, path string) *domain.CLIArtifact

// artifactFromMeta builds CLIArtifact from in-memory metadata (used by EnsureCached hit).
func (s *CLIService) artifactFromMeta(version, osName, arch string) *domain.CLIArtifact
```

The metadata (sha256, size) is stored in `CLIService` in a `sync.Map` or simple `map[string]cliMeta` guarded by the existing mutex:
```go
type cliMeta struct {
    sha256 string
    size   int64
}
```

**Open**:
```go
func (s *CLIService) Open(ctx context.Context, version, osName, arch string) (io.ReadSeekCloser, int64, error) {
    if _, err := s.EnsureCached(ctx, version, osName, arch); err != nil {
        return nil, 0, err
    }
    path, ok := s.cache.Get(ctx, version, osName, arch)
    if !ok {
        return nil, 0, fmt.Errorf("%w: cached tarball disappeared", domain.ErrCLINotFound)
    }
    // ... open + stat + return as before ...
}
```

**fetchAndCache** — `Put` returns "" for registry cache; metadata is recorded in-memory:
```go
func (s *CLIService) fetchAndCache(ctx context.Context, version, osName, arch string) (string, error) {
    // ... fetch checksums + tarball as before ...
    path, err := s.cache.Put(ctx, version, osName, arch, rc, expected)
    if err != nil {
        return "", s.failUpstream(err, version, osName, arch)
    }
    // Record metadata for artifactFromMeta.
    s.recordMeta(version, osName, arch, expected, sizeFromPut)
    s.incUpstream("success")
    return path, nil
}
```

## 5. Edge cases and error handling

### 5.1 Registry unreachable

If the registry is down when `Has`/`Get`/`Put` is called, the error propagates as a transport error wrapped in `ErrRegistryUnreachable`. The service layer maps this to a 502 (`ErrCLIUpstreamUnavailable`). The handler already maps 502 correctly via `writeCLIError`.

### 5.2 Multi-pod race (two pods cache same version)

The in-flight dedup in `CLIService.EnsureCached` serializes concurrent requests within a single pod. Across pods, two pods may independently push the same artifact. The OCI registry handles this naturally: the second push creates a manifest with identical content but a different digest (if the blob was uploaded again) or overwrites the tag. Either way, the artifact is valid and available.

The blob upload is content-addressed (`sha256:<hex>` digest). If the same tarball is uploaded twice, the second blob upload may be deduplicated by the registry (most registries return 200/201 for already-existing blobs). The manifest PUT always overwrites the tag.

### 5.3 Partial upload (crash mid-push)

If the supervisor crashes between blob upload and manifest push, the uploaded blob is orphaned but harmless (no manifest references it). The next request for the same version will re-upload and complete the manifest push. Registry GC (if enabled) will clean up orphaned blobs.

### 5.4 Checksum mismatch during Put

`Put` reads the entire body into memory, computes sha256, and compares against `sha256Hex` **before** uploading. On mismatch, it returns `ErrCLIChecksumMismatch` without touching the registry. No cleanup needed.

### 5.5 Temp file lifecycle

`Get` creates a temp file. The caller (`Open`) returns the file handle to the handler, which passes it to `SetBodyStream`. Hertz closes the stream after reading. The temp file is not explicitly deleted — it's in `os.TempDir()`.

For the `registry` backend, `Dir()` returns `""`, and `RegistryCLICache` uses `os.TempDir()` for downloads. These temp files accumulate until the OS cleans them up. To prevent unbounded growth, `Get` could register cleanup on `context.Done()` or use a bounded LRU of temp files. **Deferred**: temp file cleanup is a follow-up; the current volume (~20 MB per version) is acceptable for v1.

### 5.6 Empty registry repo

If the repo doesn't exist in the registry, `ManifestExists` returns `(false, nil)`. The first `Put` creates the repo implicitly (OCI registries auto-create repos on push). No explicit repo creation needed.

### 5.7 Config validation

- `cache.backend` must be `"registry"` (can't push to S3).
- At least one registry backend must be configured.
- `cli.cache_repo` must be non-empty and a valid OCI repo name (alphanumeric + `/` + `-_.`).

## 6. Config changes

### 6.1 Defaults (`config/loader.go`)

```go
v.SetDefault("cli.cache_repo", "dagger-kubernetes/cli-cache")
```

### 6.2 Validation (`config/loader.go` — `validateCLIConfig`)

Add:
```go
if cfg.Cache.Backend != "registry" {
    return fmt.Errorf("cli cache requires cache.backend=registry")
}
if cfg.CLI.CacheRepo == "" {
    return fmt.Errorf("cli.cache_repo must not be empty")
}
if !validOCIRepoName(cfg.CLI.CacheRepo) {
    return fmt.Errorf("cli.cache_repo %q is not a valid OCI repository name", cfg.CLI.CacheRepo)
}
```

`validOCIRepoName` checks: non-empty, matches `[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*`.

### 6.3 Sample config (`config/config.app.yaml.sample`)

```yaml
cli:
  enabled: true
  cache_repo: "dagger-kubernetes/cli-cache"  # OCI repo for CLI tarballs
  release_list_ttl: "1h"
  download_timeout: "5m"
  upstream:
    releases_url: "https://api.github.com/repos/dagger/dagger/releases"
    download_base: "https://github.com/dagger/dagger/releases/download"
    github_token: ""
```

## 7. Wiring changes (`cmd/api/main.go`)

Replace the fixed `NewFileCLICache` construction with direct `RegistryCLICache`:

```go
var cliSvc *service.CLIService
if cfg.CLI.Enabled {
    // Use the first backend's client for CLI cache operations.
    var cliRegClient domain.CLIRegistryClient
    if len(cacheBackends) > 0 {
        b := cacheBackends[0]
        cliRegClient = repository.NewRegistryStatsClientWithAuth(b.InternalAddr, b.Username, b.Password)
    } else {
        return fmt.Errorf("CLI cache requires at least one registry cache backend")
    }
    cliCache := repository.NewRegistryCLICache(cliRegClient, cfg.CLI.CacheRepo, os.TempDir(), logger)
    cliUpstream := repository.NewGitHubCLIUpstream(repository.GitHubCLIUpstreamConfig{
        ReleasesURL:  cfg.CLI.Upstream.ReleasesURL,
        DownloadBase: cfg.CLI.Upstream.DownloadBase,
        GitHubToken:  cfg.CLI.Upstream.GitHubToken,
        Timeout:      cfg.CLI.DownloadTimeout,
    })
    cliSvc = service.NewCLIService(versionResolver, cliUpstream, cliCache, cfg.Server.PublicURL, cfg.CLI.ReleaseListTTL, logger, metrics)
}
```

Note: `cacheBackends` is available at this point (populated by `validateCacheConfig` earlier).

## 8. Test strategy

### 8.1 Unit: `RegistryCLICache` (`internal/repository/cli_cache_registry_test.go`)

Use a stub `CLIRegistryClient` (in-memory map):

| Test | Description |
|---|---|
| `TestRegistryCLICachePutGetRoundTrip` | Put → Has true → Get returns valid temp file with correct content |
| `TestRegistryCLICachePutChecksumMismatch` | Put with wrong sha256 → `ErrCLIChecksumMismatch`, no manifest created |
| `TestRegistryCLICacheHasMissing` | Has on non-existent tag → `(false, nil)` |
| `TestRegistryCLICacheGetMissing` | Get on non-existent tag → `("", false)` |
| `TestRegistryCLICacheHasError` | Has with registry error → `(false, err)` |
| `TestRegistryCLICacheGetError` | Get with registry error → `("", false)` |
| `TestRegistryCLICachePutUploadError` | Put with upload failure → error propagated |
| `TestRegistryCLICachePutManifestError` | Put with manifest push failure → error propagated |
| `TestRegistryCLICacheDir` | Dir() returns "" for registry backend |
| `TestRegistryCLICacheTagFormat` | tagFor("v0.21.8", "linux", "amd64") = "v0.21.8-linux-amd64" |
| `TestRegistryCLICacheGetReturnsCorrectContent` | Verify temp file contains exact uploaded bytes |

### 8.2 Unit: `CLIService` (`internal/service/cli_service_test.go`)

Update existing tests to use a stub `CLICache` that implements the new interface (with `Has`).

Add new tests:

| Test | Description |
|---|---|
| `TestEnsureCachedUsesHasNotGet` | Verify `Has` is called, not `Get`, on cache hit |
| `TestEnsureCachedRegistryUnavailable` | `Has` returns error → error propagated |
| `TestOpenDownloadsFromRegistry` | `Get` returns temp file path → `Open` returns valid stream |
| `TestArtifactFromMeta` | `artifactFromMeta` returns correct artifact from in-memory metadata |
| `TestInflightDedupWithRegistry` | Concurrent `EnsureCached` for same version fetches once |

### 8.3 Integration: CLI endpoints (`tests/integration/cli_test.go`)

Update `startCLIServer` to use registry backend (stub client). Add tests for:
- Latest endpoint with registry cache
- Download endpoint with registry cache
- Cache hit (second request returns cached artifact from registry)

### 8.4 Integration: Registry client (`internal/repository/`)

Add integration tests for new `RegistryStatsClient` methods against a real registry (requires a running registry, tagged as integration tests). These can be added to `registry_client_test.go` or a new file.

### 8.5 CI gate

`dagger call -m ./dagger --src . ci export --path out` must pass. All new code must be covered by `go test -race -covermode=atomic ./...`.

## 9. Implementation order

1. **Domain interfaces** (`internal/domain/cli.go`):
   - Add `Has` to `CLICache`, add `context.Context` to `Get`/`Put`.
   - Add `CLIRegistryClient` interface and `CLIManifest` types.
   - Run `go build ./...` to find all compilation errors.

2. **Delete FileCLICache** (`internal/repository/cli_cache.go`, `internal/repository/cli_cache_test.go`):
   - Delete both files entirely. No backward-compatible filesystem backend.

3. **Registry client upload methods** (`internal/repository/registry_client.go`):
   - Add `UploadBlob`, `PutManifest`, `GetManifest`, `GetBlob`, `ManifestExists`.
   - Add unit tests with `httptest.NewServer` mocking the registry.

4. **RegistryCLICache** (`internal/repository/cli_cache_registry.go`):
   - Implement `NewRegistryCLICache`, `Has`, `Get`, `Put`, `Dir`, `tagFor`.
   - Add unit tests with stub `CLIRegistryClient`.

5. **CLIService refactor** (`internal/service/cli_service.go`):
   - Add `cliMeta` struct and `recordMeta`/`metaFor` helpers.
   - Modify `EnsureCached` to use `Has` on hit path.
   - Add `artifactFromMeta`.
   - Modify `fetchAndCache` to record metadata after `Put`.
   - Update tests.

6. **Config** (`config/loader.go`, `internal/domain/config.go`, `config/config.app.yaml.sample`):
   - Add `CacheRepo` to `CLIConfig`.
   - Add default and validation for `cli.cache_repo`.
   - Remove `cache_backend` and `cache_dir` keys from sample config.
   - Update sample config.

7. **Wiring** (`cmd/api/main.go`):
   - Construct `RegistryCLICache` directly (no backend switch).
   - Pass the first cache backend's credentials to `RegistryCLICache`.

8. **Handler/integration tests** (`internal/handler/cli_test.go`, `tests/integration/cli_test.go`):
   - Update to use new interface (context on Get/Put).
   - Add registry-backed cache tests.

9. **Docs**:
   - Update `docs/design/ADR-023-cli-provisioning.md` (alternatives, cache design).
   - Update `docs/README.md` (config reference).
   - Update `deploy/helm/dagger-kubernetes/values.yaml` and `README.md` (Helm docs auto-generated).

10. **CI gate**:
    - Run `dagger call -m ./dagger --src . ci export --path out`.
    - Fix any lint/test failures.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Registry push adds latency to first cache | Acceptable; happens once per version per cluster. ~20 MB upload on internal network is fast. |
| Temp file accumulation on registry backend | `os.TempDir()` is OS-managed; follow-up can add explicit cleanup. |
| Registry credentials change | CLI cache uses first backend's credentials; if backends rotate, restart supervisor. |
| Large tarball exceeding memory during Put | `Put` reads body into memory to compute sha256 before upload. Capped by existing `maxCLITarballBytes` (1 GiB). Real tarballs are ~20 MB. |
