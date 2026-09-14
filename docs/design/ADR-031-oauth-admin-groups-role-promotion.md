# ADR-031: OAuth `admin_groups` → `RoleAdmin` promotion

- **Status:** accepted
- **Date:** 2026-09-09
- **Deciders:** dagger-kubernetes maintainers
- **Related:** ADR-017 (multi-provider OAuth), ADR-022 (allowlists + regex mapping), ADR-027 (membership revalidation), ADR-030 (upstream group display)

## Context

OAuth users are always created with `RoleUser`; the only paths to `RoleAdmin`
are the bootstrap admin and a manual promotion through the API (which itself
requires an existing admin). ADR-022 deliberately scoped group mapping to
**memberships**: a group named `admin` is an ordinary group; the user's `Role`
stays `user`. Operators running Dex/Keycloak (or GitHub) therefore had to
hand-promote every IdP-side administrator — exactly the work SSO is supposed to
remove.

## Decision

### 1. Config (D1)

`auth.oauth.admin_groups` (`AdminGroups []string` in `OAuthConfig`; Helm value
`auth.oauth.adminGroups`) — a list of **raw upstream** provider group names:

- Matched **pre-mapping**, against the same group list the allowlists see
  (OIDC `groups`-claim values; GitHub org logins + `"org/team"` slugs).
- **Exact, case-sensitive** match (`orgsIntersect`); no regex, no supervisor
  group-name validation (these are IdP names, not supervisor groups).
- Empty = feature disabled (no OAuth user is ever auto-promoted).
- Loader validation: entries must be non-empty (whitespace-only rejected) and
  unique (exact equality, matching the match semantics). Stored values are not
  mutated/trimmed — matching stays exact.

### 2. Role-assignment semantics (D2 — the critical part)

Promotion/demotion must compose with manual promotions, so ownership of the
role has to be distinguishable. `domain.User` gains:

```go
OAuthAdmin bool `json:"oauth_admin,omitempty"`
```

A single pure helper drives both entry points:

```go
func applyOAuthAdminRole(u *domain.User, adminGroups, upstreamGroups []string) bool
```

- match + not admin → promote to `RoleAdmin`, `OAuthAdmin = true` (changed).
- no match + `OAuthAdmin` → demote to `RoleUser`, `OAuthAdmin = false`
  (changed).
- match + already admin → unchanged (`OAuthAdmin` stays `false`: a
  manually-promoted admin who also matches is **not** taken over, so they
  survive later group removal).
- no match + not `OAuthAdmin` → unchanged (manual admin or plain user).
- empty `admin_groups` / nil upstream groups → no-op.

Consequences (documented, intentional):

- **Manual promotions are never auto-demoted** (`OAuthAdmin=false` untouched).
- An **OAuth-granted admin** removed from the IdP group is demoted on the next
  login or revalidation.
- Making a manual demotion of an OAuth-granted admin stick requires removing
  them from the IdP group (the same "IdP is the source of truth" trade-off
  already documented in ADR-022 for memberships).
- Privilege escalation requires a literally named, IdP-controlled upstream
  group: matched pre-mapping, case-sensitive, so a mapped supervisor group name
  can never satisfy `admin_groups`.

### 3. Where it applies (D3)

Exactly two call sites, both after the allowlist and membership reconciliation:

1. **Login** — `completeOAuthLogin` (shared by both providers) applies the rule
   before `users.Update`. `EnsureOAuthUser` still creates `RoleUser`; the
   promotion happens in the same login, and since `Identity` re-loads `Role`
   from the users table at resolve time (not from JWT claims), the promotion is
   effective immediately.
2. **Revalidation** — `OAuthRevalidator.refresh` success path (before
   `users.Update`). Revoked / no-credential / IdP-unavailable paths leave
   `Role` and `OAuthAdmin` untouched.

Providers (`OIDCOAuthService`, `GitHubOAuthService`) and the revalidator carry
`adminGroups` from `cfg.Auth.OAuth.AdminGroups` at construction; no per-request
config reads.

### 4. Upsert of `cmdUser` (D4)

`repository.cmdUser` carries both new fields (`oauth_groups`, `oauth_admin`,
`omitempty`) through `toDomain`/`cmdUserFrom` so the promotion survives the
Raft-replicated update path and snapshots (see ADR-030 for the persistence
rationale).

## Consequences

- Dex/Keycloak/GitHub group administrators become supervisors' admins with
  zero manual work, consistent with the fail-closed IdP-driven model of
  ADR-022/ADR-027.
- Mixed-version clusters: additive `omitempty` fields only (older nodes ignore
  them; pre-upgrade users resolve as non-OAuth-admin until their next
  login/revalidation).
- Testability: the decision logic is one pure, fully table-tested function;
  call sites are integration-tested via the fake IdP issuers.

## Alternatives considered

- **Role per group** (mapping `admin` role via a magic group name): rejected —
  implicit, collides with a genuinely-likely group literally named `admin`, and
  cannot distinguish manual vs granted roles.
- **JWT-embedded role claims**: rejected — roles must survive the 15m access
  window; identity resolution deliberately re-reads the users table (ADR-017).
- **Dedicated `role_source` enum**: considered; a boolean suffices for exactly
  two ownership states and keeps the FSM payload minimal.
