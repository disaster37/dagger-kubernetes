# ADR-010: SQLite-backed multi-user RBAC

- **Status:** Accepted
- **Date:** 2026-08-10
- **Supersedes:** none
- **Related:** ADR-009 (clean architecture layering)

## Context

The platform had no users: authentication was a flat bearer-token file
(`service.TokenValidator`), all state was in-memory (leases) or flat files,
`GET /api/v1/traces` was a hardcoded stub, and the UI auth store was a stub
(`role: 'viewer'` hardcoded, `Login.vue` never called the backend,
`Callback.vue` treated the OAuth `code` as a token).

This was acceptable for a single-tenant CI cache but blocked multi-team
adoption: there was no way to attribute pipelines to teams, enforce per-team
engine quotas, or scope trace visibility.

## Decision

Introduce a persistent multi-user store backed by SQLite
(`modernc.org/sqlite`, pure-Go driver) with role-based access control:

1. **Users** (roles `admin` / `user`) with bcrypt-hashed passwords, optional
   GitHub OAuth linkage, and one API token per user (`dct_<32 random bytes
   hex>`, stored as SHA-256).
2. **Groups** (many-to-many with users) carrying `max_runner_sessions`,
   `agent_available`, and an `auto_assign_pattern` regex.
3. **Projects** (CI pipelines identified by repo slug) assigned to groups
   manually, pre-created, or auto-matched by regex.
4. **AuthN**: username/password → JWT (HS256, access 15m / refresh 7d, rotated
   on use), GitHub OAuth → JWT, per-user API tokens for CI
   (`DAGGER_CLOUD_TOKEN` compatible).
5. **RBAC middleware**: identity resolution (JWT → API token → legacy
   fallback), `requireAdmin` gate, `authorizeTrace` visibility gate
   (owner/member/admin; unknown meta → admin-only, fail-closed).
6. **Quota**: a group's active sessions = active leases of all its members
   (multi-group users count against EACH group). Admission to `POST
   /v1/engines` requires ≥1 group with `agent_available=true` and remaining
   capacity; admins bypass.
7. **Trace attribution**: two-phase — `POST /v1/engines` records
   `trace_id → user_id`; `POST /v1/traces` OTLP ingest parses root-span attrs
   best-effort, upserts the project, resolves the group (explicit assignment
   wins; otherwise regex auto-assign by group id order), and enriches
   `trace_meta`. Group is set once (first resolution wins). The pipeline
   identity (`project_name`/`ci_repo`) is the CI repo slug
   (`dagger.io/ci.repo`) when present, otherwise the git remote
   (`dagger.io/git.remote`) reported by the Dagger CLI for local runs.
8. **Zero-breakage migration**: legacy flat-file tokens still authenticate
   (running as a synthetic `legacy` admin identity) until
   `auth.internal.tokens_file` is removed. `supervisor migrate-tokens` imports
   each token line as a real user + API token.

### Why SQLite?

- Single-file, zero-ops, embedded — fits the supervisor's single-binary
  deployment model.
- `modernc.org/sqlite` is pure-Go (no CGO), so the existing cross-compile and
  static-binary story is preserved.
- WAL mode + a small connection pool handles the modest write load (users,
  tokens, trace metadata) without contention.
- The data set is small (users/groups/projects/tokens + trace metadata) and
  fits comfortably in a 1 GiB PVC.

### Key decisions (resolved with stakeholder)

- **D1**: One API token per user (CI runs as the user).
- **D3**: Quota counts a multi-group user's session against EACH of their
  groups (permissive admission, conservative counting).
- **D4**: Trace visibility — admin = all; user = own groups + own unassigned;
  unknown meta → admin-only (fail-closed).
- **D6**: JWT HS256, stateless, groups re-fetched from DB at resolve time
  (claims can be stale; access TTL kept short at 15m).
- **D7**: Bootstrap admin created on first boot when the users table is empty
  (random password logged once at WARN if unset).
- **D8**: Legacy compat fallback + `migrate-tokens` command for zero-breakage
  cutover.
- **D12**: `handler.NewServer` refactored to `(cfg, Deps)` with a `Deps`
  struct (the old 11-param signature + ~6 new ones was unmaintainable).

### Security controls

The following hardening is part of this design (security audit 2026-08-10):

- **Brute-force protection (CWE-307):** password verification endpoints
  (login, change-password) enforce a per username+client-IP lockout after 5
  consecutive failures: 30s initial lock, doubling per further failure,
  capped at 15min. In-memory (single-node supervisor; resets on restart).
- **JWT key strength (CWE-326):** HS256 secrets must be ≥ 32 bytes;
  shorter configured secrets are rejected at startup; auto-generated secrets
  are 32 random bytes (`crypto/rand`) persisted in the `meta` table.
- **JWT validation (CWE-287):** `jwt.ParseWithClaims` with
  `WithValidMethods(["HS256"])` (rejects `alg=none` and cross-algorithm
  attacks); `typ` claim separates access/refresh/oauth-state tokens; role
  and group membership are re-read from the DB on every resolve (claims can
  be stale, never trusted for authorization).
- **Timing equalization (CWE-208):** login performs a bcrypt comparison
  against a dummy hash for unknown users so response timing does not reveal
  username existence.
- **DB file permissions (CWE-732):** the SQLite file (password hashes,
  token hashes, JWT secret) is `chmod 0600` at open.
- **Input bounds (CWE-770/CWE-20):** trace IDs (≤128, restricted charset),
  project names / OTLP-derived fields (≤256), passwords (8–72, bcrypt
  limit), usernames/group names (regex + length).
- **Response headers (CWE-1021/CWE-200):** `X-Frame-Options: DENY` +
  `Content-Security-Policy: frame-ancestors 'none'` (clickjacking),
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` (keeps
  the SSE `?token=` param out of Referer headers).
- **Credential forwarding (CWE-522):** all reverse proxies (collector,
  VictoriaMetrics, internal cache registry) strip the client `Authorization`
  header before forwarding.
- **Open-redirect guard (CWE-601):** OAuth `redirect` param and post-login
  redirects accept internal absolute paths only (validated server-side and
  in the SPA).

## Consequences

- **Positive**: Multi-team isolation, per-team quotas, scoped trace visibility,
  self-service API tokens, OAuth login, and a real admin UI — all without a
  new external dependency.
- **Negative**: SQLite is single-node; horizontal supervisor scaling requires
  either pinning the DB to one replica or migrating to Postgres (future ADR).
  The `readOnlyRootFilesystem` security context requires a writable volume
  mount at `/var/lib/dagger-kubernetes`.
- **Risk**: Multi-group quota double-counting may surprise operators; mitigated
  by the admin Groups view showing live `active_sessions / max` and README
  documentation.
- **Migration**: Legacy tokens file support is retained but marked
  DEPRECATED; removal is a future release decision.

## Testing

- Repository: each repo tested against temp SQLite DBs (CRUD, case-insensitive
  uniqueness, FK cascades, `SetMembers` replace, `UpsertIngest` group set-once,
  `List` filter matrix, `Migrate` idempotency, WAL pragma).
- Services: validation matrices, bcrypt verify, JWT issue/parse/tamper/expiry/
  alg-none, auth resolution order, quota capacity matrix, attribution
  auto-assign + set-once, OTLP extraction, OAuth via httptest, legacy import.
- Handlers: full endpoint matrix via `ut.PerformRequest` (RBAC 403/401, CRUD
  409/400/404, tokens self-service, trace visibility fail-closed, quota
  403/429, OAuth callback redirect).
- Integration: black-box `TestRBACQuotaAndVisibility` against a booted server
  with temp SQLite (quota 429, visibility 404, admin sees all, regenerate
  invalidates old token, legacy compat).
