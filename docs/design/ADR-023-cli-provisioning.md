# ADR-023: On-the-fly Dagger CLI provisioning addon

- **Status:** accepted
- **Date:** 2026-08-24
- **Deciders:** dagger-kubernetes maintainers

## Context

CI runners need a Dagger CLI to talk to the self-hosted supervisor. There is no
public "Dagger CLI" container image suitable for on-the-fly provisioning, and
requiring every Jenkins/Drone runner to pre-install and pin the exact CLI
version is fragile and drift-prone. The supervisor already knows which engine
versions are allowed (`version.allowlist` / `version.floor`), so it is the
natural place to also serve the matching CLI binary.

Requirements:

1. The supervisor caches the Dagger CLI binary and serves it to CI runners on
   demand.
2. `latest` resolution must be **allowlist-aware**: when `version.allowlist` is
   non-empty, `latest` = the highest released `vX.Y.Z` whose `major.minor` is in
   the allowlist and `>= version.floor`; when empty, `latest` = the highest
   released `vX.Y.Z >= version.floor`.
3. CI integration for Jenkins and Drone only (GitHub Actions keeps its
   dedicated plugin).
4. Downloads are sha256-verified server-side before anything is served.
5. The cache must be shared across all supervisor pods in a multi-node Raft
   cluster so that each pod does not independently download the tarball from
   GitHub.

## Decision

### 1. OCI registry-backed cache (not local filesystem)

The shared OCI registry (`cache.backend=registry`, ADR-006/012/014) acts as a
cluster-wide cache accessible to all supervisor pods. Each CLI tarball is stored
as a full OCI artifact:

- **Blob**: the raw `dagger_vX.Y.Z_os_arch.tar.gz` file, content-addressed by
  sha256 digest.
- **Manifest**: `application/vnd.oci.image.manifest.v1+json` with a single layer
  referencing the blob, annotated with the tarball's sha256 checksum, version,
  and filename.
- **Tag**: `vX.Y.Z-os-arch` (e.g. `v0.21.8-linux-amd64`).
- **Repository**: configurable via `cli.cache_repo` (default `dagger-kubernetes/cli-cache`).

Retrieval: HEAD manifest → extract blob digest → GET blob → write to temp file
→ stream. Push: compute sha256 → POST/PUT monolithic blob upload → PUT manifest
with annotations.

This replaces the previous per-pod filesystem cache at `<database.dir>/cli-cache`
which required each pod to independently download the tarball from GitHub on
first request.

### 2. GitHub Releases discovery

Version discovery uses the GitHub Releases API
(`https://api.github.com/repos/dagger/dagger/releases`, paginated) and returns
the list of release tag names. `service.CLIService.ResolveLatest` filters with
the existing `domain.VersionResolver.IsAllowed` + `Floor()` and picks the max by
`Compare`. The release list is cached in-memory with TTL
(`cli.release_list_ttl`, default `1h`) to absorb GitHub API rate limits
(60 req/hr unauthenticated; `cli.upstream.github_token` raises this). Only the
list endpoint hits the API; the tarball + checksums come from
`cli.upstream.download_base` (github release download host), fetched only on a
cache miss. Both URLs are mirror-able for self-hosted/offline deployments.

### 3. Auth-gated endpoints

Two supervisor endpoints (both `requireAuth`, consistent with `/api/v1/cache`,
`/api/v1/connect/env`; the CI runner's `DAGGER_CLOUD_TOKEN` is sent as
`Authorization: Bearer`):

- `GET /api/v1/cli/versions/latest?os=linux&arch=amd64` → resolves the latest
  allowed version, ensures it is cached, returns `{version, os, arch, filename,
  url, sha256, size}`.
- `GET /api/v1/cli/<version>?os=linux&arch=amd64` → streams the verified tarball
  with `Content-Type: application/gzip`, `Content-Disposition: attachment`,
  and `Content-Length`.

`cli.enabled=false` (or a nil service) makes both endpoints return 404
"cli provisioning disabled".

### 4. Layering

Follows the dependency rule (`handler → service → domain ← repository`):

- `internal/domain/cli.go` — `CLIArtifact`, `CLIReleaseIndex`, `CLIUpstream`,
  `CLICache`, `CLIRegistryClient`, sentinel errors (`ErrCLINotFound`,
  `ErrCLIVersionNotAllowed`, `ErrCLIChecksumMismatch`,
  `ErrCLIUpstreamUnavailable`), stdlib only.
- `internal/repository/cli_upstream.go` — GitHub releases client (stdlib
  `net/http`, per ADR-007).
- `internal/repository/cli_cache_registry.go` — OCI registry-backed cache.
- `internal/service/cli_service.go` — resolve-latest, ensure-cached,
  open-for-stream, in-flight dedup (stdlib `sync.Mutex` + per-key channel; no
  new dependency).
- `internal/handler/cli.go` — the two endpoints.

### 5. CI glue

- `cmd/ci` gains `--cli`, `--cli-version`, `--cli-os`, `--cli-arch` flags and a
  `provisionCLI` helper that resolves (or pins) the version, downloads the
  verified tarball, extracts `dagger`, and prepends its directory to `PATH`.
- `ci-integrations/jenkins/daggerKubernetes.groovy` gains a `provisionCli` step
  (and `provisionCli: true` on the main entry).
- `ci-integrations/drone/config-extension.sh` provisions the CLI by default
  (needs `curl` + `tar`); disable with `PLUGIN_CLI=false`.

## Alternatives considered

- **Local filesystem cache on PVC**: per-pod caches on multi-node Raft re-download
  idempotently but waste bandwidth and increase first-request latency. Rejected
  in favor of the shared OCI registry (see above).
- **Push the CLI into the magic cache**: would require implementing OCI
  manifest/blob upload client-side for zero benefit (no layer sharing across
  versions); rejected.
- **Unauthenticated download**: the CLI is a public artifact, but token-gating
  is kept for consistency with the rest of the API (open-proxy/bandwidth
  protection). Revisit if product wants public downloads (see open questions).
- **A separate shared RWX volume for a fleet-wide cache**: adds operational
  complexity for no correctness gain over the OCI registry approach.
- **Upstream per-asset `digest` as the primary verification source**: kept as a
  future fallback; `checksums.txt` is the primary source today (both were
  confirmed present for `v0.21.8`).

## Consequences

- The OCI registry stores CLI artifacts alongside engine layers; the repo is
  configurable (`cli.cache_repo`, default `dagger-kubernetes/cli-cache`).
- Cache GC applies uniformly to all artifacts in the repo (engine layers + CLI
  tarballs). No separate eviction policy needed.
- Temp files created during `Get` (blob download → temp file → stream) accumulate
  in `os.TempDir()` until OS cleanup. Follow-up can add explicit cleanup or an
  LRU bound.
- Unauthenticated GitHub API rate limits (60 req/hr) are mitigated by the 1h TTL
  + optional token; a busy multi-pod fleet re-downloading the list per pod could
  still approach the limit (consider a Raft-stored shared release list later).
- The Jenkins/Drone glue targets `linux/amd64` and is parameterized; the
  supervisor supports `arm64`/`armv7`/`darwin` fetch paths but they are untested
  against real upstream artifacts here.

## Open questions

- Should the CLI download be unauthenticated (it is a public artifact)? Flagged
  for the PO (Q2 in the plan).
- Cache GC/eviction policy (Q3) and a shared release list (Q4) are follow-ups.
- Upstream asset-naming / `checksums.txt` stability: if Dagger ever changes the
  suffix (`.zip` vs `.tar.gz`) or drops `checksums.txt`, `FetchChecksums`/
  `FetchTarball` must degrade (fallback to the per-asset `digest` field).
