# ADR-035: OAuth group-mapping auto-creates missing groups with a default engine limit

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** dagger-kubernetes maintainers
- **Related:** ADR-017 (multi-provider OAuth), ADR-022 (allowlists + regex mapping), ADR-027 (membership revalidation), ADR-030 (upstream group display), ADR-031 (admin_groups promotion)

## Context

`auth.oauth.group_mappings` maps upstream IdP group names to supervisor group
names. The mapped names were looked up by name only: a rule whose target did not
already exist was logged (`oauth: group not found … skipping`) and the user was
left without that membership. Because only the `default` group is bootstrapped
at first boot, every operator had to pre-create each mapped group by hand and
keep its `max_runner_sessions` in sync manually. A new IdP group (e.g. a fresh
Dex team) silently produced unassigned users until an admin noticed — the exact
toil SSO is meant to remove (issue #7).

Group-name uniqueness is case-insensitive at the storage (`fsm`) layer, but the
FSM does **not** validate the name charset; only `service.ValidateGroupName`
(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`) does. Mapping replacements are arbitrary
strings (capture expansion can yield spaces, empty, or >64 chars).

## Decision

### 1. Auto-create mapped groups (D2, D4)

`group_mappings` targets are auto-created when missing, and only those. The
`default_group` fallback keeps its current behavior (must pre-exist; missing →
Warn + skip). An auto-created group gets:

- `AgentAvailable: true` — hardcoded, otherwise the user could not provision
  engines and the feature would be pointless.
- `MaxRunnerSessions` = `auth.oauth.mapped_group_max_runner_sessions`.
- `AutoAssignPattern: ""`, `Description: ""`.

The value is applied **only at creation time**. A group that already exists
(admin-created or previously auto-created) is returned as-is; its quota and
`agent_available` are **never** overwritten. This is enforced by
`ensureGroupByName` returning the resolved group unchanged.

### 2. Config and Helm keys (D1)

- Config `auth.oauth.mapped_group_max_runner_sessions` (`int`, default `0` =
  unlimited; env `DAGGER_KUBERNETES_AUTH_OAUTH_MAPPED_GROUP_MAX_RUNNER_SESSIONS`).
- Helm `auth.oauth.mappedGroupMaxRunnerSessions` (rendered to the ConfigMap).
- `config.Load` rejects a negative value
  (`validateOAuthGroupMappingDefaultLimit`); `0` is valid and means unlimited.

### 3. Fail-open membership sync (D3)

The `allowed_groups`/`allowed_orgs`/`allowed_teams` allowlist stays
**fail-closed** and is evaluated on the raw upstream groups **before** mapping
(unchanged). Once the allowlist passes, group auto-creation and membership sync
remain **best-effort (fail-open)**: if `ensureGroupByName` cannot create or
resolve a group (invalid name, store error, `ErrNotLeader` on a follower), the
group is skipped with a Warn and the login/refresh still succeeds.

Rationale: authentication has already succeeded, so denying the user here is
surprising; and revalidation runs on **every** pod via `AuthService.Resolve`,
while only the Raft leader can write — a follower's `Create`/`SetMembers`
returns `ErrNotLeader`, so failing the request would break login on all
non-leader pods. This matches the pre-existing handling of
`reconcileMemberships` errors (logged as Warn, non-fatal).

### 4. Race safety (D5)

`ensureGroupByName` is `GetByName` → (on `ErrNotFound`) `Create` → (on
`ErrConflict`) `GetByName`. The Raft FSM serializes creates on the leader, so a
simultaneous first login yields one `Create` winner and the loser re-fetches by
name. The re-fetched group's quota (set by the winner) is respected and never
overwritten.

### 5. Name validation before create (D6)

`ensureGroupByName` runs `service.ValidateGroupName(name)` before creating.
Invalid names are skipped with a Warn and never stored, because the FSM does not
validate names and mapping replacements can expand to arbitrary strings. Empty
results are already dropped by `GroupMapper.Map`.

### 6. Both entry points

The helper is used by `ensureGroupByName` inside `reconcileMemberships`, which
is shared by login (`completeOAuthLogin`, both providers) and IdP revalidation
(`OAuthRevalidator.refresh`), so both paths auto-create identically.

## Consequences

- A GitHub/OIDC login whose mapped target is absent auto-creates it with the
  configured limit and joins the user; existing groups are untouched.
- No HTTP status changes: the auth path never fails because of a group create
  (fail-open).
- All new logic lives in `service` (business) and `config` (validation) plus one
  plain int field in `domain`; `handler` and `repository` are unchanged and
  `domain` stays stdlib-only.
- Testability: the tri-state `ensureGroupByName` contract and the conflict
  re-fetch are unit-tested, and the end-to-end flow is covered by a black-box
  integration test.

## Alternatives considered

- **Pre-seed every mapped target at startup**: rejected — cannot enumerate
  arbitrary capture-expanded names, and would resurrect deleted groups.
- **Fail the login when a mapped group cannot be created**: rejected — breaks
  login on non-leader pods and is hostile to already-authenticated users.
- **Apply the default quota to existing groups too**: rejected — silently resets
  an operator's deliberate limit; creation-time only is the least surprising.
