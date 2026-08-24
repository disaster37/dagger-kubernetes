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

## Decision

### 1. Dedicated filesystem cache (not the magic cache)

The magic cache (`cache.backend=registry`, ADR-006/012/014) stores BuildKit OCI
layer blobs under version-tagged refs. It is the wrong tool for a single ~20 MB
CLI tarball: a CLI binary is not an OCI layer, and the upstream integrity
artifact is a plain `sha256` checksum of the tarball that maps 1:1 to a
content-verified file on disk.

Instead, verified tarballs are cached on the supervisor's existing PVC at
`cli.cache_dir` (default `<database.dir>/cli-cache`, i.e.
`/var/lib/dagger-kubernetes/cli-cache`), keyed `<version>_<os>_<arch>.tar.gz`.
The supervisor verifies the `sha256` against upstream `checksums.txt` **before**
atomically renaming into place (`internal/repository/cli_cache.go`), so clients
receive a verified tarball and only need to extract it.

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
  `CLICache`, sentinel errors (`ErrCLINotFound`, `ErrCLIVersionNotAllowed`,
  `ErrCLIChecksumMismatch`, `ErrCLIUpstreamUnavailable`), stdlib only.
- `internal/repository/cli_upstream.go` — GitHub releases client (stdlib
  `net/http`, per ADR-007).
- `internal/repository/cli_cache.go` — sha256-verified atomic filesystem cache.
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

- **Push the CLI into the OCI registry cache**: would require implementing OCI
  manifest/blob upload client-side for zero benefit (no layer sharing across
  versions); rejected.
- **Unauthenticated download**: the CLI is a public artifact, but token-gating
  is kept for consistency with the rest of the API (open-proxy/bandwidth
  protection). Revisit if product wants public downloads (see open questions).
- **A separate shared RWX volume for a fleet-wide cache**: per-pod caches on
  multi-node Raft re-download idempotently and cheaply; a shared volume adds
  operational complexity for no correctness gain.
- **Upstream per-asset `digest` as the primary verification source**: kept as a
  future fallback; `checksums.txt` is the primary source today (both were
  confirmed present for `v0.21.8`).

## Consequences

- The supervisor pod already mounts `database.dir` on a PVC (fsGroup/uid 10001);
  the `cli-cache` subdirectory is writable without chart changes.
- Cache grows ~20 MB per version with no eviction in v1 (accepted short-term; a
  follow-up TTL/LRU sweep is recommended).
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
