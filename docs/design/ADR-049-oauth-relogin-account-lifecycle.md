# ADR-049 — OAuth re-login recovery, visible failures, and protected account lifecycle

Status: Accepted (owner-approved scope expansion)

Date: 2026-10-15

## Context

Two independent defects combined into a same-identity OAuth re-login loop with
no visible error:

1. The per-user revalidation cache (ADR-027) keyed a `stateUnavailable` denial
   by local user ID and only re-checked on a changed credential ciphertext for
   `stateExpired`. A transport failure followed by a successful same-account
   login therefore kept denying `me` until the negative TTL expired. A different
   account had no such entry and worked, and logout-then-login appeared to help
   only because time or leader routing changed.
2. The UI Axios interceptor handled terminal 401s globally and navigated away
   before the OAuth callback/login error screen rendered, dropping the error
   hint and target. A failed `logout()` went back through the same interceptor,
   allowing recursion.

The owner approved a broader lifecycle scope than a minimal UI patch:

- persistent administrator Disable/Enable distinct from IdP deactivation;
- unified restorable soft-delete tombstones for all local accounts;
- revision-checked atomic Raft mutations so stale authentication work cannot
  overwrite newer credentials, bans, deletions, memberships or token versions;
- a coordinated all-voter maintenance rollout for the new command format.

## Decision

### Login generation and cache invalidation

`domain.User` gains `OAuthLoginVersion`, incremented on every accepted OAuth
login even when credential encryption is disabled (empty ciphertext). The
revalidator snapshots both `OAuthLoginVersion` and the credential ciphertext in
its cache entry and re-checks the IdP whenever either changes, for **every**
cache state (including a cached success or a `stateUnavailable` denial). Store
failures never fabricate a `stateOK` entry or a synthetic last-good result.

### Account lifecycle

`domain.User` gains `DisabledAt`, `DeletedAt` and a `Revision`
optimistic-concurrency counter, plus `AccessBlocked()` (disabled, deleted or
IdP-deactivated). A successful IdP login clears **only** `DeactivatedAt`; it can
never clear an administrator disable or a deletion. Delete is a soft delete for
all local accounts: it reserves the ID, username and `(provider, subject)`
identity, removes credentials/memberships/API tokens and increments the token
version. Restore clears only the deletion, leaving the account disabled and
credentialless. Enable rejects still-deleted rows.

The FSM enforces, atomically, the explicit-management guards: no self-disable,
no self-delete and no removal of the last active administrator (disable, delete
or role demotion). IdP policy can still revoke the final OAuth administrator's
authorization by design; the guard covers administrator management only.

### Atomic revision-checked commands

New Raft command kinds (`kindUserState`, `kindSoftDeleteUser`,
`kindOAuthUserMutation`) are appended after every existing numeric ID so replay
of older logs is unchanged. `OAuthUserMutation` is applied under the FSM write
mutex: the revision precondition is evaluated before any account, membership or
token effect, so a stale mutation has zero effect. Each kind has a strict
writable-field whitelist (login, membership, credential, revoke); missing
groups roll the whole mutation back. Membership mutations (`setMembers`, group
deletion) advance each affected user's revision so an in-flight OAuth mutation
conflicts with an administrator edit.

The repository's `Update` now uses `u.Revision` as a CAS precondition and
advances it on success. General updates cannot change immutable identity,
administrator timestamps or `OAuthLoginVersion`; a general update cannot
resurrect a tombstone.

### Commit-order concurrency and preserved policy

Competing already-validated flows use record revisions: one commit wins, the
other ends with a visible retry/conflict error. Commit order wins, not
initiation time. Non-OAuth-managed memberships are never touched; a managed ID
that is already manually present keeps its current ownership. The single
signed-state/nonce cookie is preserved; a callback that does not match the
current nonce does not erase another flow's cookie. Exchanged authorization
codes are never replayed.

### Snapshot and replay compatibility

Snapshots retain the new fields and tombstones and restore active administrator
state for legacy rows lacking them (`Revision`/`OAuthLoginVersion` zero).
`ExpectedRevision` is command-only and never serialized in snapshots. The legacy
hard-delete and upsert replay semantics are untouched; accounts hard-deleted
before the upgrade cannot be reconstructed.

### Rollout

Old binaries ignore the new fields and commands and may clear bans or diverge,
so the expanded design requires a coordinated all-voter maintenance upgrade
(stop all supervisor processes, back up the quiesced Raft PVCs, start every
voter on the same new binary). Mixed-version rollout and rollback to the old
binary after new commands/snapshots are unsupported.

## Consequences

- Same-identity re-login recovers immediately without an account switch or
  waiting for the negative TTL.
- Authentication failures terminate on a stable, visible error; the UI no
  longer navigates or recurses on auth-endpoint 401s.
- Administrator bans survive successful IdP authentication; old authentication
  work cannot clobber newer state.
- Operational cost: a maintenance window is mandatory, and external IdP calls
  (leadership-change token rotation, already-authorized engine leases) remain
  outside the transactional boundary. A follower observes a small replication
  delay before a committed ban; no linearizable-read protocol is introduced.
- Explicitly out of scope: no config migration, no issuer-change identity
  migration, no retroactive tombstone recovery, no synthetic-legacy ban model,
  no initiation-order winner guarantee, no automatic IdP retry and no forced
  credential prompt.
