# ADR-022: OAuth group allowlists + regex group mapping

- **Status:** accepted
- **Date:** 2026-08-21
- **Deciders:** dagger-kubernetes maintainers

## Context

OAuth login already supported two providers (ADR-017): GitHub (orgs via
`/user/orgs`) and generic OIDC (groups claim). Two gaps remained:

1. **Allowlists were org-only.** GitHub had `allowed_orgs`; OIDC reused the
   same key against its groups claim. There was no way to restrict GitHub
   login to specific teams, and the OIDC allowlist had no clearly-named key.
2. **No group mapping.** Upstream provider groups (GitHub orgs/teams, OIDC
   groups claim) could not be translated into supervisor group memberships.
   Operators had to manually create supervisor groups and assign users, and
   group names had to match upstream names exactly.

This ADR documents the allowlist + mapping design that closes both gaps.

## Decision

### 1. Provider group model (D1)

- **GitHub provider groups** = orgs (`/user/orgs` → `login`) ∪ teams
  (`/user/teams` → `"org/team"` slugs).
- **OIDC provider groups** = the normalized `groups_claim` list.
- Teams are fetched only when `len(allowed_teams) > 0 || mapper.Active()`.

### 2. Allowlists (D2)

- `allowed_orgs` (unchanged): GitHub → org-login allowlist. OIDC → **deprecated
  alias** of `allowed_groups` (backward compatible).
- `allowed_teams` (new, GitHub only): `"org/team"` slug allowlist. When both
  `allowed_orgs` and `allowed_teams` are non-empty, the user must satisfy **both**
  (AND). Each independently empty → no constraint from that dimension.
- `allowed_groups` (new, OIDC only): groups-claim allowlist, matched **before**
  mapping. Effective allowlist = `allowed_groups ∪ allowed_orgs` (deduplicated).
- Empty allowlist = allow all; provider returns zero groups + non-empty
  allowlist → `domain.ErrForbidden`.

### 3. Group mapping rules (D3)

Config key `auth.oauth.group_mappings`: an ordered list of
`{pattern, replacement}` applied to the raw provider groups.

- **First-match-wins** per provider group; the first matching rule produces the
  replacement.
- **No rule matches → drop the group** (fail-closed; pass-through requires an
  explicit `.*` rule).
- **Substitution**: Go `regexp.Expand` semantics — `$1`, `${1}`, `${name}`,
  `$$` for a literal `$`.
- **Case-sensitive** by default (Go `regexp`); opt into case-insensitivity with
  `(?i)`.
- **RE2**: Go `regexp` is linear-time, so catastrophic backtracking / ReDoS is
  not a concern.
- **Empty list = identity** at `GroupMapper.Map`, but the service gates sync on
  `mapper.Active()` (non-empty rules), so an empty list produces no membership
  changes — exactly the pre-existing behavior (only `default_group` auto-join).

### 4. Membership application (D4)

- Mapped supervisor group names are looked up via `GroupRepository.GetByName`.
  Missing groups are logged (`Warn`) and skipped — never fatal, never
  auto-created (mirrors `joinDefaultGroup`).
- **Additive**: the user is appended to existing groups; memberships are never
  removed. Reconciliation is out of scope (admins manage memberships via UI).
- **Applied on every login** (not just first login). Footgun (documented): an
  admin removing a user from an auto-mapped group will see it restored on that
  user's next OAuth login.
- `default_group` auto-join is unchanged and composes with mapped groups.
- `AuthService.Resolve` re-fetches `GroupsForUser` per request, so persisted
  memberships flow into `Identity.GroupIDs` automatically — **no JWT-claim
  changes**.

### 5. Validation (D5)

`config.validateGroupMappings` (called from `config.Load` after
`validateAuthConfig`) fails fast on:

- non-empty `group_mappings[i].pattern` that compiles as a Go regexp;
- non-empty `group_mappings[i].replacement`;
- `allowed_teams` entries that are non-empty `"org/team"` strings.

The authoritative compile is `service.NewGroupMapper` (called once in
`cmd/api/main.go`); `validateGroupMappings` is the fail-fast duplicate that
keeps `config` free of a `service` import.

### 6. Error handling (D6)

- Startup: `config.Load` returns `validate group mappings: %w`; `run` wraps
  `load config: %w`.
- Allowlist denial: `Complete` returns `domain.ErrForbidden`; the OAuth callback
  is a redirect flow, so it maps to `/auth/login?error=group_required` (distinct
  code) rather than an HTTP 403. All other OAuth failures keep
  `/auth/login?error=oauth`.
- Mapped-group lookup failures and best-effort team fetches: `logrus` `Warn`
  with `WithError` + `WithField("group", name)`; never fatal.

## Alternatives rejected

- **Implicit pass-through of unmapped groups** — provider group names must not
  be implicitly trusted/kept as supervisor group names (fail-closed).
- **Auto-create missing supervisor groups** — group creation is an admin
  decision; silently creating groups from upstream names is surprising and
  bypasses group quotas/policy.
- **Membership removal/reconciliation** — additive-only keeps the feature
  simple and predictable; removal is a future follow-up (admins manage
  memberships via the UI).

## Consequences

- Existing configs keep working: `allowed_orgs` still behaves as before for
  both providers (OIDC alias preserved), and an empty `group_mappings` list
  changes nothing.
- New GitHub teams calls are only made when teams/mapping are configured.
- Group names produced by mapping must satisfy the existing supervisor group
  name validation (`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`) to be usable; names
  that violate it will simply not match any existing group and be skipped.

## References

- ADR-017 (multi-provider OAuth), ADR-010 (multi-user RBAC).
