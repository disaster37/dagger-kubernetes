# Plan: On-the-fly Dagger CLI provisioning addon (Jenkins + Drone)

**Status:** Ready for implementation
**Module:** `github.com/disaster/dagger-kubernetes`
**Scope:** Supervisor CLI binary cache + version-aware "latest" resolution + Jenkins & Drone addon glue. GitHub Actions is **out of scope** (has its own dedicated plugin).

---

## 1. Requirements (restated precisely)

1. **Supervisor caches the Dagger CLI binary** so CI runners can provision it "on the fly" without relying on a public Dagger image (none exists for this purpose).
2. **"latest" resolution is allowlist-aware:** when `version.allowlist` is non-empty, "latest" = the highest released `vX.Y.Z` whose `major.minor` is in the allowlist **and** `>= version.floor`. When the allowlist is empty, "latest" = the highest released `vX.Y.Z >= version.floor`.
3. **CI integration for Jenkins and Drone only.** GitHub Actions keeps its dedicated plugin — no GHA changes.
4. The supervisor exposes version-discovery + verified-download endpoints; the CI wrapper / Jenkins shared library / Drone plugin download, verify (server-side), extract, and inject the CLI into `PATH`.

**Out of scope (explicit):**
- GitHub Actions integration.
- Populating the existing `service.Resolver.releases` map (connect-env menu) from upstream — separate enhancement.
- Cache GC/eviction policy beyond a best-effort `cli.cache_dir` on the existing PVC (see §7 edge cases).
- Windows/Darwin CI runners in Jenkins/Drone glue (the supervisor supports any `os`/`arch` it can fetch; the Jenkins/Drone scripts target `linux/amd64` and are parameterized).

---

## 2. Design overview

### 2.1 Where the binary cache lives — **dedicated filesystem cache, NOT the magic cache**

The "magic cache" (`cache.backend=registry`, ADR-006/012/014) stores BuildKit **OCI layer blobs** under version-tagged refs, proxied by the supervisor. It is the wrong tool for a single ~20 MB CLI tarball:

- A CLI binary is not an OCI layer and shares nothing across versions; pushing it into the registry would require implementing OCI manifest/blob upload client-side for zero benefit.
- The upstream integrity artifact is a plain `sha256` checksum of the tarball, which maps 1:1 to a content-verified file on disk.

**Decision:** cache verified tarballs on the supervisor's existing PVC at `cli.cache_dir` (default `<database.dir>/cli-cache`, i.e. `/var/lib/dagger-kubernetes/cli-cache`). Key = `<version>_<os>_<arch>.tar.gz`. The supervisor verifies `sha256` against upstream `checksums.txt` **before** atomically renaming into place, so clients receive a verified tarball and only need to extract.

Consequences to document:
- The supervisor pod already mounts `database.dir` on a PVC with `fsGroup: 10001` and runs as uid `10001`; the new subdirectory is writable without chart changes.
- Per-pod caches on multi-node Raft (each pod has its own PVC): re-download on a different pod is idempotent and cheap; acceptable, no shared RWX needed.

### 2.2 Version discovery ("latest")

- Upstream source: **GitHub Releases API** `https://api.github.com/repos/dagger/dagger/releases` (paginated, `?per_page=100`). Confirmed on 2026-08-24: latest tag `v0.21.8`, assets named `dagger_v0.21.8_<os>_<arch>.tar.gz` / `.zip`, plus a `checksums.txt` asset (sha256 lines) and a per-asset `digest` field.
- Discovery returns the list of release tag names; `CLIService.ResolveLatest` filters with the existing `domain.VersionResolver.IsAllowed` + `Floor()` and picks the max by `Compare`.
- The release list is cached in-memory with TTL (`cli.release_list_ttl`, default `1h`) to absorb GitHub API rate limits (60 req/hr unauthenticated; `cli.upstream.github_token` raises this).
- Only the list endpoint hits the API; tarball + checksums come from `cli.upstream.download_base` (github release download host), fetched only on cache miss.

### 2.3 Layering (dependency rule `handler → service → domain ← repository`)

- `internal/domain/cli.go` — entities + interfaces, **stdlib only**.
- `internal/repository/cli_upstream.go` — GitHub releases client (`net/http`, per ADR-007 outbound clients use stdlib).
- `internal/repository/cli_cache.go` — filesystem cache.
- `internal/service/cli_service.go` — orchestration: resolve-latest, ensure-cached, open-for-stream, in-flight dedup, metrics.
- `internal/handler/cli.go` — two Hertz endpoints.
- `cmd/api/main.go` — wiring.
- `cmd/ci/main.go` — wrapper provision helper.
- `ci-integrations/jenkins/daggerKubernetes.groovy` + `ci-integrations/drone/config-extension.sh` — addon glue.

### 2.4 End-to-end flow

```
CI runner (Jenkins/Drone/wrapper)
  │  1. GET  /api/v1/cli/versions/latest?os=linux&arch=amd64   (Bearer token)
  │     └─ supervisor: list upstream → filter allowlist+floor → max → ensure cached
  │        └─ (miss) fetch checksums.txt + tarball, verify sha256, atomic-rename into cache
  │     → {version, os, arch, filename, url, sha256, size}
  │  2. GET  /api/v1/cli/<version>?os=linux&arch=amd64
  │     └─ stream verified tarball (application/gzip)
  │  3. extract tar.gz → chmod +x dagger → prepend dir to PATH
  │  4. run `dagger ...` (existing DAGGER_CLOUD_* env unchanged)
```

---

## 3. File-by-file change list

### New files

| Path | Purpose |
|---|---|
| `internal/domain/cli.go` | `CLIArtifact`, `CLIReleaseIndex`, `CLIUpstream`, `CLICache` interfaces + sentinel errors |
| `internal/domain/cli_test.go` | domain-level unit tests (parse/normalize helpers if any) |
| `internal/repository/cli_upstream.go` | GitHub releases client (`List`, `FetchChecksums`, `FetchTarball`) |
| `internal/repository/cli_upstream_test.go` | httptest-server-driven tests + filename/URL construction |
| `internal/repository/cli_cache.go` | Filesystem cache (`Get`, `Put`, `Dir`) |
| `internal/repository/cli_cache_test.go` | cache hit/miss, checksum mismatch, partial write cleanup |
| `internal/service/cli_service.go` | resolve-latest + ensure-cached + streaming + dedup |
| `internal/service/cli_service_test.go` | table-driven with stub upstream/cache |
| `internal/handler/cli.go` | `handleCLILatest`, `handleCLIDownload` |
| `internal/handler/cli_test.go` | route tests via `ut.PerformRequest` (mirrors existing handler tests) |
| `docs/design/ADR-023-cli-provisioning.md` | ADR |

### Modified files

| Path | Change |
|---|---|
| `internal/domain/config.go` | add `CLIConfig`, `CLIUpstreamConfig` + `Config.CLI` field |
| `config/loader.go` | add `cli.*` defaults + `validateCLIConfig` call |
| `config/config.app.yaml.sample` | add `cli:` section with comments |
| `internal/observ/metrics.go` | add `CLICacheTotal`, `CLIUpstreamFetchTotal` collectors + register |
| `internal/handler/server.go` | add `CLI *service.CLIService` to `Deps` + `cli` field; register 2 routes in `configure()` |
| `cmd/api/main.go` | build `CLIConfig` default cache dir, construct upstream+cache+service, inject into `handler.Deps` |
| `cmd/ci/main.go` | add flags + `provisionCLI` helper; prepend bin dir to `PATH` |
| `cmd/ci/main_test.go` | unit tests for `provisionCLI` URL building / extraction (httptest) |
| `ci-integrations/jenkins/daggerKubernetes.groovy` | add `provisionCli` step |
| `ci-integrations/drone/config-extension.sh` | add provision step |
| `deploy/helm/dagger-kubernetes/values.yaml` | add `supervisor.config.cli.*` |
| `deploy/helm/dagger-kubernetes/templates/configmap.yaml` | render `cli:` section |
| `deploy/helm/dagger-kubernetes/README.md` | add chart param table rows |
| `docs/README.md` | new "CLI provisioning" section + config table + CI examples |
| `docs/design/index.md` | add ADR-023 row |
| `tests/integration/cli_test.go` (new) | end-to-end handler+service against stub upstream |

---

## 4. Data structures, interfaces, signatures

### 4.1 `internal/domain/cli.go` (stdlib only)

```go
package domain

import (
	"context"
	"errors"
	"io"
)

// Sentinel errors surfaced by the CLI addon. Live in domain so the handler can
// map them to HTTP statuses without importing repository/service.
var (
	ErrCLINotFound             = errors.New("dagger cli version not found")
	ErrCLIVersionNotAllowed    = errors.New("dagger cli version not allowed")
	ErrCLIChecksumMismatch     = errors.New("dagger cli checksum mismatch")
	ErrCLIUpstreamUnavailable  = errors.New("dagger cli upstream unavailable")
)

// CLIArtifact describes one Dagger CLI tarball (cached or resolvable).
type CLIArtifact struct {
	Version  string `json:"version"`  // "v0.21.8"
	OS       string `json:"os"`       // "linux" | "darwin"
	Arch     string `json:"arch"`     // "amd64" | "arm64" | "armv7"
	Filename string `json:"filename"` // "dagger_v0.21.8_linux_amd64.tar.gz"
	URL      string `json:"url"`      // supervisor download URL (absolute)
	SHA256   string `json:"sha256"`   // hex digest of the tarball ("" = unverified)
	Size     int64  `json:"size"`     // bytes; -1 unknown
}

// CLIReleaseIndex lists upstream release version strings (e.g. ["v0.21.8", ...]).
type CLIReleaseIndex interface {
	List(ctx context.Context) ([]string, error)
}

// CLIUpstream fetches upstream release artifacts.
type CLIUpstream interface {
	CLIReleaseIndex
	// FetchChecksums returns filename -> sha256 hex from <version>/checksums.txt.
	FetchChecksums(ctx context.Context, version string) (map[string]string, error)
	// FetchTarball returns a stream of the tarball and its byte length.
	FetchTarball(ctx context.Context, version, osName, arch string) (io.ReadCloser, int64, error)
}

// CLICache is a sha256-verified, atomic filesystem cache for tarballs.
type CLICache interface {
	// Get returns the cached tarball path, re-verifying its sha256 sidecar.
	Get(version, osName, arch string) (path string, ok bool)
	// Put streams r to a temp file, verifies sha256Hex, atomically renames.
	Put(version, osName, arch string, r io.Reader, sha256Hex string) (path string, err error)
	// Dir returns the cache root directory.
	Dir() string
}
```

Helper (domain, used by repository + tests): `func AssetFilename(version, osName, arch string) string` returning `fmt.Sprintf("dagger_%s_%s_%s.tar.gz", version, osName, arch)`.

### 4.2 `internal/domain/config.go` additions

```go
type Config struct {
	// ...existing fields...
	CLI CLIConfig `mapstructure:"cli"`
}

// CLIConfig configures the on-the-fly Dagger CLI provisioning addon.
type CLIConfig struct {
	Enabled         bool             `mapstructure:"enabled"`
	CacheDir        string           `mapstructure:"cache_dir"`         // "" = <database.dir>/cli-cache
	ReleaseListTTL  time.Duration    `mapstructure:"release_list_ttl"`  // default "1h"
	DownloadTimeout time.Duration    `mapstructure:"download_timeout"`  // default "5m"
	Upstream        CLIUpstreamConfig `mapstructure:"upstream"`
}

// CLIUpstreamConfig points at the Dagger release source (mirror-able for
// self-hosted/offline deployments).
type CLIUpstreamConfig struct {
	ReleasesURL  string `mapstructure:"releases_url"`  // https://api.github.com/repos/dagger/dagger/releases
	DownloadBase string `mapstructure:"download_base"` // https://github.com/dagger/dagger/releases/download
	GitHubToken  string `mapstructure:"github_token"`  // optional, raises API rate limit; set via env only
}
```

### 4.3 `internal/repository/cli_upstream.go`

```go
type GitHubCLIUpstream struct {
	client       *http.Client
	releasesURL  string
	downloadBase string
	token        string
}

// GitHubCLIUpstreamConfig is the repository-side constructor config (mapped from
// domain.CLIUpstreamConfig in cmd/api).
type GitHubCLIUpstreamConfig struct {
	ReleasesURL  string
	DownloadBase string
	GitHubToken  string
	Timeout      time.Duration
}

func NewGitHubCLIUpstream(cfg GitHubCLIUpstreamConfig) *GitHubCLIUpstream

func (u *GitHubCLIUpstream) List(ctx context.Context) ([]string, error)
func (u *GitHubCLIUpstream) FetchChecksums(ctx context.Context, version string) (map[string]string, error)
func (u *GitHubCLIUpstream) FetchTarball(ctx context.Context, version, osName, arch string) (io.ReadCloser, int64, error)
```

- `List` GETs `releasesURL?per_page=100&page=N` (follow `Link` rel="next" until exhausted), decodes `[{ "tag_name": "v0.21.8" }]`.
- `FetchChecksums` GETs `fmt.Sprintf("%s/%s/checksums.txt", u.downloadBase, version)` and parses `sha256  dagger_v0.21.8_linux_amd64.tar.gz` lines.
- `FetchTarball` GETs `fmt.Sprintf("%s/%s/%s", u.downloadBase, version, domain.AssetFilename(version, osName, arch))`, returns the body; non-2xx → wrapped error (404 → `domain.ErrCLINotFound`).
- Sends `Authorization: Bearer <token>` (and `Accept: application/vnd.github+json`) when a token is set.

### 4.4 `internal/repository/cli_cache.go`

```go
type FileCLICache struct {
	dir string
}

func NewFileCLICache(dir string) (*FileCLICache, error) // MkdirAll(0755)
func (c *FileCLICache) Dir() string
func (c *FileCLICache) Get(version, osName, arch string) (string, bool)
func (c *FileCLICache) Put(version, osName, arch string, r io.Reader, sha256Hex string) (string, error)
func (c *FileCLICache) cleanupTemps() // remove leftover tmp-* files (called at startup)
```

- `Put` writes to `<dir>/tmp-<random>` while streaming through a `sha256` hash; on mismatch deletes the temp and returns `fmt.Errorf("%w: %s", domain.ErrCLIChecksumMismatch, expected)`; on success writes `<key>.sha256` sidecar and `os.Rename` to `<dir>/<key>` (0644).
- `Get` re-reads the sidecar and re-hashes the file; mismatch or missing sidecar → delete + `ok=false`.

### 4.5 `internal/service/cli_service.go`

```go
type CLIService struct {
	resolver  domain.VersionResolver
	upstream  domain.CLIUpstream
	cache     domain.CLICache
	publicURL string
	logger    *logrus.Logger
	metrics   *observ.Metrics

	mu          sync.Mutex
	releases    []string
	releasesAt  time.Time
	releaseTTL  time.Duration
	inflight    map[string]*cliInflight // key = version|os|arch
}

type cliInflight struct {
	done chan struct{}
	path string
	err  error
}

func NewCLIService(
	resolver domain.VersionResolver,
	upstream domain.CLIUpstream,
	cache domain.CLICache,
	publicURL string,
	releaseTTL time.Duration,
	logger *logrus.Logger,
	metrics *observ.Metrics,
) *CLIService

func (s *CLIService) ResolveLatest(ctx context.Context, osName, arch string) (*domain.CLIArtifact, error)
func (s *CLIService) EnsureCached(ctx context.Context, version, osName, arch string) (*domain.CLIArtifact, error)
func (s *CLIService) Open(ctx context.Context, version, osName, arch string) (io.ReadSeekCloser, int64, error)
```

- `ResolveLatest`: `listReleases()` (TTL-cached) → parse each with `domain.Parse`, keep `s.resolver.IsAllowed(v)` → max by `Compare`; none → `fmt.Errorf("%w: no released version satisfies floor %s and allowlist", domain.ErrCLINotFound, s.resolver.Floor())`; then `EnsureCached`.
- `EnsureCached`: strict full `vX.Y.Z` parse (`domain.Parse`), reject patch==0/partial → `ErrCLIVersionNotAllowed`; `!IsAllowed` → `ErrCLIVersionNotAllowed`; cache hit → artifact (sha from sidecar); miss → inflight dedup; `upstream.FetchChecksums` to get expected sha for `AssetFilename`; `upstream.FetchTarball`; `cache.Put`; build artifact with `URL = fmt.Sprintf("%s/api/v1/cli/%s?os=%s&arch=%s", s.publicURL, version, osName, arch)`.
- Metrics: `CLICacheTotal{result=hit|miss|error}` and `CLIUpstreamFetchTotal{status=success|error}`.
- In-flight dedup is stdlib-only (map + `sync.Mutex` + a per-key channel), avoiding a new dependency.

### 4.6 `internal/handler/cli.go`

```go
func (s *Server) handleCLILatest(ctx context.Context, c *app.RequestContext)
func (s *Server) handleCLIDownload(ctx context.Context, c *app.RequestContext)
```

`Server` gains `cli *service.CLIService`; `Deps` gains `CLI *service.CLIService`.

### 4.7 `cmd/ci/main.go` additions

```go
// provisionCLI resolves (or pins) the Dagger CLI version, downloads the verified
// tarball from the supervisor, extracts `dagger`, and returns the directory to
// prepend to PATH plus a cleanup func.
func provisionCLI(ctx context.Context, serverURL, token, version, osName, arch string) (binDir string, cleanup func(), err error)
```

- New flags: `--cli` (bool, default `false`; enable on-the-fly provision), `--cli-version` (string, default `""` = latest allowed), `--cli-os` (default `linux`), `--cli-arch` (default `amd64`).
- In `run`, when `--cli`: call `provisionCLI`; set `cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s:%s", binDir, os.Getenv("PATH")))`.

---

## 5. Config keys

Env prefix `DAGGER_KUBERNETES_` (dots → underscores). Defaults added in `config/loader.go`:

| Key | Type | Default | Notes |
|---|---|---|---|
| `cli.enabled` | bool | `true` | master switch; `false` disables endpoints (404) and wiring |
| `cli.cache_dir` | string | `""` | derived to `<database.dir>/cli-cache` at startup (in `cmd/api/main.go`), NOT in loader |
| `cli.release_list_ttl` | duration | `"1h"` | TTL of the in-memory upstream release list |
| `cli.download_timeout` | duration | `"5m"` | outbound HTTP timeout for upstream fetches |
| `cli.upstream.releases_url` | string | `https://api.github.com/repos/dagger/dagger/releases` | mirror-able |
| `cli.upstream.download_base` | string | `https://github.com/dagger/dagger/releases/download` | mirror-able |
| `cli.upstream.github_token` | string | `""` | set via env `DAGGER_KUBERNETES_CLI_UPSTREAM_GITHUB_TOKEN`; never in the file |

**Validation** (`validateCLIConfig`, called from `Load` after `validateFleetConfig`):
- if `cfg.CLI.Enabled`:
  - `releases_url` and `download_base` must parse as absolute `http(s)` URLs (`url.ParseRequestURI`), else error naming the key.
  - `release_list_ttl` and `download_timeout` must be `> 0` (reject non-positive to avoid a 0-TTL hot loop).
- `github_token` is never validated for length (optional).

**Sample config** (`config/config.app.yaml.sample`) — add under the `ci:` section:

```yaml
# --- On-the-fly Dagger CLI provisioning ------------------------------------
cli:
  enabled: true                       # serve verified Dagger CLI tarballs + latest-version resolution.
  cache_dir: ""                       # "" = <database.dir>/cli-cache (persists on the supervisor PVC).
  release_list_ttl: "1h"              # how long to cache the upstream release list in memory.
  download_timeout: "5m"              # outbound fetch timeout for the tarball/checksums.
  upstream:
    releases_url: "https://api.github.com/repos/dagger/dagger/releases"   # release discovery (mirror-able).
    download_base: "https://github.com/dagger/dagger/releases/download"   # tarball + checksums.txt base (mirror-able).
    github_token: ""                  # optional; set via DAGGER_KUBERNETES_CLI_UPSTREAM_GITHUB_TOKEN (raises rate limit).
```

---

## 6. HTTP API endpoints (supervisor, Hertz)

Auth: **both** endpoints require a valid token (`s.requireAuth(c)`), consistent with `/api/v1/cache`, `/api/v1/connect/env`, etc. The CI runner's `DAGGER_CLOUD_TOKEN` (per-user API token or legacy token) is sent as `Authorization: Bearer <token>`.

### 6.1 `GET /api/v1/cli/versions/latest?os=linux&arch=amd64`

- Query params `os` (default `linux`), `arch` (default `amd64`); both validated against a small allowlist (`os ∈ {linux,darwin}`, `arch ∈ {amd64,arm64,armv7}`) → 400 on invalid.
- Resolves latest allowed version, ensures cached, returns:

```json
{
  "version": "v0.21.8",
  "os": "linux",
  "arch": "amd64",
  "filename": "dagger_v0.21.8_linux_amd64.tar.gz",
  "url": "https://supv.example.com/api/v1/cli/v0.21.8?os=linux&arch=amd64",
  "sha256": "53e226c7...",
  "size": 21927634
}
```

- Status codes: `200` OK; `400` bad os/arch; `404` no allowed version; `502` upstream/checksum failure; `401` unauthenticated.

### 6.2 `GET /api/v1/cli/:version?os=linux&arch=amd64`

- `:version` must be a full `vX.Y.Z` (partial like `0.21` → 400) and allowed → else `400` `"version vX.Y.Z not allowed (floor v0.19.0)"` (mirrors `handleEngines` wording).
- Streams the verified tarball:
  - `Content-Type: application/gzip`
  - `Content-Disposition: attachment; filename="dagger_v0.21.8_linux_amd64.tar.gz"`
  - `Content-Length` set when known.
- Status codes: `200`; `400` invalid/not-allowed version; `404` version not in upstream; `502` upstream/checksum failure; `401` unauthenticated.
- Implementation note: use `c.FileAttachment(cachePath, filename)` (Hertz) then override `Content-Type`; if `FileAttachment` is unavailable in hertz v0.10.5, open the file and stream via `c.Response` with explicit headers. Verify at implementation time; the contract (headers + body) is fixed.

Route registration in `configure()` (near the other `/api/v1/...` GET routes):

```go
h.GET("/api/v1/cli/versions/latest", s.handleCLILatest)
h.GET("/api/v1/cli/:version", s.handleCLIDownload)
```

When `cli.enabled=false` or `s.cli == nil`, handlers return `404` `"cli provisioning disabled"`.

---

## 7. Edge cases & error handling

| Case | Behavior |
|---|---|
| Unknown version (download) | `FetchTarball` upstream 404 → `ErrCLINotFound` → HTTP `404` |
| Version not allowed | `IsAllowed` false → `ErrCLIVersionNotAllowed` → HTTP `400` (same wording as engine gating) |
| Partial/malformed version (`0.21`, `v0.21`) | `domain.Parse` accepts `MAJOR.MINOR` but download requires full `vX.Y.Z` (patch != 0) → `400` |
| Network failure upstream | `ErrCLIUpstreamUnavailable` → `502`; log `WithError` + fields `{version, os, arch}` |
| Checksum mismatch | `cache.Put` deletes temp, returns `ErrCLIChecksumMismatch` → `502`; **never** serves the corrupt tarball |
| Cache hit | serve from disk; re-verify sha256 sidecar on `Get` |
| Cache miss | fetch checksums → fetch tarball → verify → atomic rename |
| Concurrent same-version requests | in-flight dedup: one fetch, waiters share the result channel |
| Allowlist empty | admit all `>= floor`; latest = highest upstream |
| Allowlist excludes all upstream | `ResolveLatest` → `404` with explicit "no released version satisfies floor + allowlist" message |
| Corrupt cached binary | `Get` re-hashes; mismatch → delete + miss → re-fetch |
| Partial downloads / crashes | `Put` writes to `tmp-*`; atomic rename only on success; `cleanupTemps()` at startup removes orphans |
| Disk pressure / ENOSPC | `Put` returns error → `502` for that request; cache is best-effort (see open question Q3 re: GC) |
| Release list staleness | `release_list_ttl` (1h); "latest" may lag a just-published release by ≤ TTL |
| Upstream rate limit (GitHub API 60/hr) | mitigated by TTL cache + optional `github_token`; tarball host is not the API host |
| Self-hosted / offline | `releases_url`/`download_base` point at a mirror; pre-seed `cli.cache_dir` to serve without upstream; otherwise `502` |
| Multi-node Raft | per-pod cache (idempotent re-download); no shared volume required |
| `cli.enabled=false` | endpoints 404; service not wired |

---

## 8. Testing plan

Conventions: stdlib `testing` only, table-driven, `logrus` logger via `observ.NewTestLogger()` (output `io.Discard`), stub external deps. Target 100% coverage per package.

### Unit tests

- `internal/repository/cli_upstream_test.go` — use `httptest.Server`: `List` (single + paginated via `Link` header), `FetchChecksums` parsing, `FetchTarball` success/404/non-200, filename & URL construction (assert exact `https://.../download/v0.21.8/dagger_v0.21.8_linux_amd64.tar.gz`).
- `internal/repository/cli_cache_test.go` — `t.TempDir()`; `Put`+`Get` round-trip; checksum mismatch returns `ErrCLIChecksumMismatch` and leaves no final file; `Get` re-verification deletes tampered file; `cleanupTemps` removes `tmp-*`.
- `internal/service/cli_service_test.go` — stub `CLIUpstream` + in-memory `CLICache` (or `FileCLICache` on `t.TempDir()`): latest resolution with/without allowlist, allowlist-excludes-all → error, cache hit avoids upstream fetch (assert call counts), checksum mismatch surfaces `ErrCLIChecksumMismatch`, concurrent `EnsureCached` for same version fetches once, not-allowed version rejected.
- `internal/handler/cli_test.go` — `ut.PerformRequest` with the existing `test_helper_test.go` fixture (mirrors `cache_test.go`): `latest` JSON shape, `download` headers/status, 401 without token, 400 invalid os/arch/version, 404 disabled.
- `cmd/ci/main_test.go` — `provisionCLI` against `httptest.Server` serving a small real `tar.gz` (built with `archive/tar`+`compress/gzip` in the test): extracted `dagger` file is executable and path returned; error path on non-200.
- `config/loader_test.go` — add cases for `cli.*` defaults + `validateCLIConfig` rejections (bad URL, non-positive TTL).

### Integration tests (`tests/integration/cli_test.go`)

Prove the feature against the real handler/service wiring with a stub upstream `httptest.Server` (the only fake — the external download HTTP client is never hit):

- Build a full `handler.Server` (as in `api_test.go`), inject a `CLIService` backed by `repository.FileCLICache` + a `GitHubCLIUpstream` pointed at the httptest server, `service.NewResolver("v0.19.0", []string{"0.21"}, nil)`.
- `GET /api/v1/cli/versions/latest` with admin token → 200, `version == "v0.21.8"`, `url` points back at the supervisor.
- `GET /api/v1/cli/v0.21.8` → 200, `Content-Type: application/gzip`, body sha256 matches the upstream `checksums.txt` fixture, and `gzip.NewReader` + `tar.NewReader` yield a valid `dagger` entry.
- `GET /api/v1/cli/v0.22.0` (not in allowlist) → 400; unknown `v9.9.9` → 404.

---

## 9. Documentation updates (AGENTS.md mandate)

- `config/config.app.yaml.sample` — add `cli:` section (§5).
- `docs/README.md`:
  - New "CLI provisioning" subsection under "CI integrations": document the two endpoints, the `latest` allowlist semantics, the mirror/offline story, and per-CI usage.
  - Update Jenkins example: show the provision step and `version: 'v0.21.4'` pinning.
  - Update Drone example: note the plugin now provisions the CLI (needs `curl`+`tar`).
  - Add the `cli.*` keys to the config reference table (§Full reference).
- `docs/design/ADR-023-cli-provisioning.md` — new ADR: decision (dedicated filesystem cache vs magic cache; GitHub releases discovery; allowlist-aware latest; auth-gated endpoints), alternatives considered, consequences. Add row to `docs/design/index.md`.
- `deploy/helm/dagger-kubernetes/README.md` — chart param table for `supervisor.config.cli.*` (regenerate via `scripts/update-helm-docs.sh`).

---

## 10. Helm chart changes & local-test redeployment

- `deploy/helm/dagger-kubernetes/values.yaml`: add under `supervisor.config`:

```yaml
## @param supervisor.config.cli.enabled Enable on-the-fly Dagger CLI provisioning addon.
## @param supervisor.config.cli.cacheDir Cache dir for verified CLI tarballs (empty = <database.dir>/cli-cache).
## @param supervisor.config.cli.releaseListTtl Upstream release-list cache TTL.
## @param supervisor.config.cli.downloadTimeout Outbound upstream fetch timeout.
## @param supervisor.config.cli.upstream.releasesUrl Release discovery URL (mirror-able).
## @param supervisor.config.cli.upstream.downloadBase Tarball + checksums.txt base URL (mirror-able).
## @param supervisor.config.cli.upstream.githubToken Optional GitHub token for the releases API.
    cli:
      enabled: true
      cacheDir: ""
      releaseListTtl: "1h"
      downloadTimeout: "5m"
      upstream:
        releasesUrl: "https://api.github.com/repos/dagger/dagger/releases"
        downloadBase: "https://github.com/dagger/dagger/releases/download"
        githubToken: ""
```

- `deploy/helm/dagger-kubernetes/templates/configmap.yaml`: render a `cli:` block (after `version:`):

```yaml
    cli:
      enabled: {{ .Values.supervisor.config.cli.enabled }}
      cache_dir: {{ .Values.supervisor.config.cli.cacheDir | quote }}
      release_list_ttl: {{ .Values.supervisor.config.cli.releaseListTtl | quote }}
      download_timeout: {{ .Values.supervisor.config.cli.downloadTimeout | quote }}
      upstream:
        releases_url: {{ .Values.supervisor.config.cli.upstream.releasesUrl | quote }}
        download_base: {{ .Values.supervisor.config.cli.upstream.downloadBase | quote }}
        github_token: ""
```

(`github_token` is rendered empty; set via `DAGGER_KUBERNETES_CLI_UPSTREAM_GITHUB_TOKEN` or `supervisor.extraEnv`.)

- **Local redeployment** (per `AGENTS.local.md` §4/§5/§6, context `home`):

```bash
docker build -t docker.io/disaster/dagger-kubernetes:dev .
docker push docker.io/disaster/dagger-kubernetes:dev

helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml

helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes --namespace dagger-kubernetes-test \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes

kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

- **Verification** (agent §5.1 + human §5.2):
  1. Pods Ready; `curl -sk https://localhost:8080/healthz` / `readyz`.
  2. `curl -sk https://localhost:8080/api/v1/cli/versions/latest -H "Authorization: Bearer $TOKEN"` → 200 with `version` in the live allowlist (`0.19/0.20/0.21`, e.g. `v0.21.8`).
  3. Download the tarball and confirm `tar tzf` lists `dagger`; `sha256sum` matches the returned `sha256`.
  4. Human confirms the UI still renders (feature is API-only; no UI change required).

---

## 11. Step-by-step implementation order (each milestone builds & tests independently)

1. **Domain** — add `internal/domain/cli.go` + `internal/domain/config.go` (structs/fields) + `config/loader.go` defaults + `validateCLIConfig` + `config/config.app.yaml.sample`. Unit-test `config` + `domain`. (`go build ./... && go test ./config/... ./internal/domain/...`)
2. **Repository** — `internal/repository/cli_upstream.go` + `internal/repository/cli_cache.go` + tests. (`go test ./internal/repository/...`)
3. **Service + metrics** — `internal/service/cli_service.go` + tests; `internal/observ/metrics.go` collectors. (`go test ./internal/service/... ./internal/observ/...`)
4. **Handler + wiring** — `internal/handler/cli.go`, `server.go` (Deps + routes), `cmd/api/main.go` wiring; handler tests. (`go test ./internal/handler/... ./cmd/api/...`)
5. **Integration test** — `tests/integration/cli_test.go` proving the real contract with a stub upstream. (`go test ./tests/integration/...`)
6. **CI wrapper** — `cmd/ci/main.go` `provisionCLI` + flags + tests. (`go test ./cmd/ci/...`)
7. **Jenkins + Drone glue** — `ci-integrations/jenkins/daggerKubernetes.groovy` + `ci-integrations/drone/config-extension.sh` provision steps.
8. **Helm** — `values.yaml` + `configmap.yaml` + `README.md` (helm-docs).
9. **Docs + ADR** — `docs/README.md`, `docs/design/ADR-023-cli-provisioning.md`, `docs/design/index.md`.
10. **Redeploy + verify** on the `home` cluster per §10 (mandatory, §6 of `AGENTS.local.md`).

---

## Open questions / risks

- **Q1 (upstream stability):** GitHub Releases asset naming and `checksums.txt` presence were confirmed for `v0.21.8`; if Dagger ever changes the asset suffix (`.zip` vs `.tar.gz`) or drops `checksums.txt`, `FetchChecksums`/`FetchTarball` must degrade. Mitigation: keep both `checksums.txt` and the per-asset `digest` from the releases API as fallback verification sources (recommend using `digest` first, `checksums.txt` second).
- **Q2 (auth scope):** Downloading a public Dagger binary through a token-gated endpoint is a consistency choice (open-proxy/bandwidth protection). If product wants the CLI download to be unauthenticated (it is a public artifact), the endpoint can drop `requireAuth` — flag for PO.
- **Q3 (cache growth):** No eviction in v1; `cli.cache_dir` grows one tarball per version (~20 MB each). Acceptable short-term; a follow-up TTL/LRU sweep (or reusing the cache GC pattern from ADR-012) is recommended.
- **Q4 (rate limit):** Unauthenticated GitHub API is 60 req/hr; the 1h TTL + optional token covers normal use, but a busy multi-pod fleet re-downloading the list per pod can approach the limit. Consider a shared Raft-stored release list in a follow-up.
- **Q5 (multi-arch coverage):** Jenkins/Drone glue targets `linux/amd64`; the supervisor supports `arm64`/`armv7`/`darwin` fetch paths but they are untested against real upstream artifacts here.
