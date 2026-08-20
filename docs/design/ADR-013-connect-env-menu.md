# ADR-013: Connect-env UI menu — ready-to-copy Dagger CLI environment

- **Status:** accepted
- **Date:** 2026-08-17
- **Deciders:** dagger-kubernetes maintainers

## Context

Pointing the Dagger CLI at this Supervisor requires assembling several
environment variables by hand (`DAGGER_CLOUD_URL`, `DAGGER_CLOUD_TOKEN`,
`_EXPERIMENTAL_DAGGER_RUNNER_HOST`, optionally `_EXPERIMENTAL_DAGGER_TAG` and
`_EXPERIMENTAL_DAGGER_CACHE_CONFIG`). Users previously had to read
`docs/README.md` and hand-assemble these, including the per-user API token,
whose plaintext is only shown once at creation/regeneration.

We wanted a single UI page that shows every required env var with its
fully-resolved value (including the token plaintext on demand) and offers
one-click copy of ready-to-paste snippets for bash/zsh, `.bashrc` sourcing,
GitHub Actions, GitLab CI, and generic `export` lines.

## Decision

### 1. Store API-token plaintext reversibly encrypted (AES-256-GCM)

Serving a fully-formed snippet with the token on every visit is impossible
when only the SHA-256 hash is persisted. We add a `token_ciphertext` column to
`api_tokens` and store the plaintext encrypted with AES-256-GCM
(`nonce||ciphertext||tag`, base64). The encryption key is managed exactly like
the JWT secret:

- Configurable via `auth.token.encryption_key`
  (env `DAGGER_KUBERNETES_AUTH_TOKEN_ENCRYPTION_KEY`).
- Auto-generated (32 random bytes, hex-encoded) and persisted in the SQLite
  `meta` table under key `token_encryption_key` when not configured, with a
  startup WARN.
- A configured key shorter than 32 bytes is rejected at startup.
- Because AES-256 requires a key of exactly 32 bytes while operators may
  configure secrets of any length ≥ 32 bytes (and the auto-generated meta value
  is a 64-char hex string), the raw secret material is SHA-256-derived into a
  fixed 32-byte AES key before use (`deriveAESKey` in `cmd/api/main.go`).

Fresh installs include the column in the v1 schema; a v2 migration adds it to
existing databases (guarded by `schema_migrations` version 2 and a
`PRAGMA table_info` check, since SQLite lacks `ADD COLUMN IF NOT EXISTS`).

### 2. Auth-gated `GET /api/v1/connect/env` with `?reveal=true`

The new `ConnectService` assembles a `ConnectEnvSnapshot`: server URL, data
hostname, cache backend, version floor, allowed versions, the masked token
meta, and the env-var list. The endpoint is gated by `requireAuth`; the token
is revealed only to the authenticated owner (scoped by `identityOf(c).UserID`).

- `?reveal=true` populates the `DAGGER_CLOUD_TOKEN` value with the plaintext
  when the token is recoverable (ciphertext present + key available).
- The default view (`reveal` unset) returns an empty token value and the
  masked prefix only.
- `Cache-Control: no-store, no-cache, must-revalidate` + `Pragma: no-cache`
  on every response; the token value is never logged.

### 3. Masked default view; reveal is an explicit user action

Revealing the plaintext is an explicit UI action (checkbox → `?reveal=true`).
The handler logs nothing on the success path, and error logging uses
sentinel-wrapped errors that never contain the token.

### 4. CI snippets use secret references by default

Interactive-shell snippets (bash exports, `.bashrc` heredoc, generic exports)
include the plaintext token directly — they land in the user's local
shell/dotfiles, which are not committed. CI snippets (GitHub Actions `env:`,
GitLab CI `variables:`) use a secret reference
(`${{ secrets.DAGGER_CLOUD_TOKEN }}` / `$DAGGER_CLOUD_TOKEN`) by default, with
a "Copy token value" button and a one-line instruction to paste it into the CI
secret store once. A "Include plaintext in CI snippet" toggle (off by default,
red warning when on) allows embedding the literal value for users who accept
the risk.

### 5. Single source of truth for the cache-config value

`ConnectService` reuses `service.Cache.BuildCacheConfig(v, "max")` and
`domain.VersionResolver` (`ResolveMinimal`, `IsAllowed`, `AllReleases`) so the
`_EXPERIMENTAL_DAGGER_CACHE_CONFIG` value and the version allowlist are
computed by the same code the CI wrapper and CLI script already use.

`_EXPERIMENTAL_DAGGER_CACHE_CONFIG` is **always** emitted (for both the
registry and s3 backends) whenever `BuildCacheConfig` returns a non-empty
string, regardless of whether an engine version is pinned. The cache ref's
version tag targets the client's *effective* engine version: the explicitly
pinned version when provided, otherwise the latest allowed release (the last
element of `AllReleases()`, which is sorted ascending), falling back to
`VersionResolver.Floor()` when no releases are known.
`_EXPERIMENTAL_DAGGER_TAG` remains opt-in and is emitted only when the user
pins a version.

Trade-off: when no version is pinned, the cache ref targets the platform's
default version (latest allowed release or floor), which is the best-known
proxy for the client's real engine version rather than a value read from the
client itself.

### 6. Threat-model trade-off

Hashing meant DB compromise alone did not yield usable tokens. Reversible
encryption means DB compromise **plus** encryption-key compromise yields token
plaintexts. Because the key can be auto-generated into the same DB `meta`
table, DB compromise alone suffices in that (dev-mode) case. Mitigation:
configure `auth.token.encryption_key` via env/K8s Secret in production
(separate from the DB), exactly as `auth.jwt.secret` is recommended. The DB is
already a crown jewel (JWT signing key, bcrypt password hashes, token hashes),
so this does not open a new attack class beyond "DB compromise = full
compromise." Pre-existing tokens (created before this change) have no
ciphertext and are **not recoverable** — the UI shows a "Regenerate to enable"
link (a one-time migration cost).

### 7. Flag (auth gating)

The unauthenticated dev-mode posture described by earlier drafts has been
removed: auth is always enforced. `auth.internal.enabled: false` no longer
means "anonymous access" — it disables username/password + legacy-token auth
and is only permitted when OAuth is enabled and fully configured, making
OAuth the sole login path. The Connect-env endpoint always requires an
authenticated identity (401 otherwise) and never serves the anonymous user.

## Alternatives rejected

- **Regenerate on every reveal** — invalidates the existing CI token each time
  the page is opened; the user's CI breaks on every visit.
- **Hybrid "Generate + copy full snippet" one-shot** — regenerates the token
  and returns the plaintext bundled with the snippet in a single response,
  preserving the one-time invariant but invalidating the token on each use. It
  remains available as the existing Settings "Generate"/"Regenerate" flow for
  users who prefer one-time plaintext.

## Consequences

- The Connect page delivers a full, ready-to-copy environment on every visit
  without invalidating CI.
- Token plaintexts become recoverable by an attacker who compromises both the
  DB and the encryption key (or just the DB in dev-mode auto-key); production
  operators must set `auth.token.encryption_key` via env/Secret.
- Pre-existing tokens are not recoverable until regenerated.
- No new third-party dependencies: AES-256-GCM uses the Go stdlib
  (`crypto/aes`, `crypto/cipher`), and the UI uses the existing Vue 3 + axios
  stack.
