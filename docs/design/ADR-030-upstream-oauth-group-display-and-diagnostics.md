# ADR-030: Upstream OAuth group display + group-mapping diagnostics

- **Status:** accepted
- **Date:** 2026-09-09
- **Deciders:** dagger-kubernetes maintainers
- **Related:** ADR-022 (allowlists + regex mapping), ADR-027 (membership revalidation), ADR-031 (admin_groups role promotion)

## Context

ADR-022 introduced regex group mapping (`group_mappings`) but nothing tells the
operator what actually happened at login time: which upstream groups arrived,
what they mapped to, and which supervisor groups the user joined (or were
skipped because they do not exist). Meanwhile, once a user is a member, there is
no way to see *which upstream IdP groups* they came in with — support questions
like "why is this Dex user not in the `admin` group?" require digging through
IdP-side group claims.

## Decision

### 1. Persist upstream groups (D1)

`domain.User` gains an informational field:

```go
OAuthGroups []string `json:"oauth_groups,omitempty"`
```

- Content: the **raw upstream** provider groups captured at the last successful
  OAuth login or revalidation (OIDC `groups`-claim values; GitHub org logins +
  `"org/team"` team slugs).
- Normalized via `normalizeGroupList` (dedupe, drop empties, sort) for
  deterministic storage/display; empty input → `nil` (omitted in JSON).
- Informational only: authorization and membership reconciliation continue to
  use `OAuthGroupIDs` / supervisor-side memberships. Zero trust in IdP-claimed
  display data.

### 2. Persistence path (D2)

The field rides the existing Raft `cmdUser` payload (`oauth_groups` +
`oauth_admin` JSON keys, both `omitempty`). No new command kind, no schema
migration, no `fsm_snapshot.go` change (the snapshot serializes `cmdUser`).
`omitempty` keeps the payload forward/backward compatible in mixed-version
clusters: older nodes ignore the unknown keys; newer nodes see nil/zero for
pre-upgrade users until their next login.

### 3. Diagnostics (D3)

- Every successful login/revalidation logs
  `oauth: group mapping applied` (INFO) with `user_id`, `oauth_provider`,
  `upstream_groups`, `mapped_groups`, `oauth_group_ids` — the full
  arrived → mapped → joined chain in one line. A mapped target that did not
  exist is auto-created (ADR-035) and logged separately as
  `oauth: auto-created mapped group` (INFO) with the group name and quota; the
  per-group Warn (`oauth: group not found, skipping`) now only signals a missing
  `default_group` fallback.
- Startup (cmd/api) warns for each `group_mappings` rule whose replacement has
  no `$` capture reference **and** is not a valid supervisor group name (via
  the exported `service.ValidateGroupName`): such a replacement can never
  match an existing group.

### 4. Exposure (D4)

- `GET /api/v1/users` (admin): `oauth_groups` added to the user rows.
- `GET /api/v1/auth/me` (+ login/OAuth-complete responses): `oauth_groups` on
  the current user.
- UI: admin Users page gains an "OAuth groups" column (muted badges, next to
  the existing supervisor-Groups column); Settings shows the current user's
  upstream groups.
- The internal `OAuthAdmin` bookkeeping flag (ADR-031) is intentionally **not**
  exposed.

## Consequences

- Operators can answer "who mapped into what" from one INFO log line and see
  IdP-side groups in the UI without IdP access.
- Additive, `omitempty` fields only: no Raft/log/JSON breaking change.
- Pre-upgrade users have no `oauth_groups` until their next successful
  login/revalidation (acceptable: informational, refreshed opportunisticly).
- After a revoked/unavailable revalidation the last-known `oauth_groups` may be
  stale (documented; informational only).

## Alternatives considered

- Store a historical log of mapping events: rejected — needs retention/GC, far
  beyond the diagnostic need.
- Recompute upstream groups on-demand in the API: rejected — requires a live
  IdP round-trip per read and credentials may be revoked.
