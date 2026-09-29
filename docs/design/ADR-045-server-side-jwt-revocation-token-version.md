# ADR-045: Server-side JWT revocation via `token_version`

**Status:** Accepted
**Date:** 2026-09-29

## Context

Session JWTs (HS256, access 15m / refresh 7d rotated on use) are stateless:
once minted, a token is valid until it expires (or until ADR-027's IdP
revalidation deactivates the user for OAuth identities). Security audit finding
SEC-006 (CWE-613) identified the gaps that leaves open:

- **Logout is cosmetic.** `POST /api/v1/auth/logout` only cleared the cookies;
  the access token still carried in them (or copied elsewhere) kept resolving
  for its full TTL, and the refresh token kept rotating a fresh pair for 7
  days.
- **Password changes do not end sessions.** After a self service password
  change — or an admin password reset of a compromised account — every
  previously issued token stayed valid until natural expiry.

Server-side state for every token (a denylist, a session table) was rejected:
it would add a lookup on the hot auth path and duplicate state the Raft-backed
user record already carries.

## Decision

Add a per-user monotonic counter, `domain.User.TokenVersion`
(`json:"token_version"`), and stamp it into every issued JWT
(`service.Claims.TokenVersion`, `json:"token_version"`):

1. **Issue** — `JWTService.issue()` copies `u.TokenVersion` into the claims of
   both the access and the refresh token.
2. **Verify** — after a successful signature parse, `AuthService.Resolve`
   compares the claim against the freshly loaded user; `AuthService.Refresh`
   does the same on the refresh path. A mismatch means the token was minted
   before the last bump and yields `domain.ErrSessionRevoked`
   (401 `session revoked; please sign in again`). Because the user record is
   re-read from the Raft store on every resolution, the revocation takes
   effect on **all pods immediately** — no broadcast needed.
3. **Bump** — the counter increments on:
   - **logout** (`handleLogout`): *best-effort* — the cookies are cleared and
     204 is returned even when the bump fails (a WARN is logged); logout must
     never leave the user unable to sign out.
   - **self password change** and **admin password reset**: *fatal* — the bump
     is persisted in the **same** user update as the new password hash, so a
     failed apply fails the whole change (500) and a new password is never
     stored while old tokens would stay valid.

API tokens (`dct_…`) are unaffected: they are not JWTs, carry their own
revocation (row delete on revoke), and pass no version to the check.

**Backward compatibility.** Pre-upgrade Raft snapshots/user records lack the
`token_version` key and decode to `0`; pre-upgrade JWTs lack the claim and
parse to `0`. `0 == 0`, so existing sessions keep working across the upgrade
with no migration, in both directions (an old binary ignores the new field
entirely).

## Alternatives considered

- **Token denylist / session table** — rejected: a per-request lookup on the
  hottest auth path, plus expiry bookkeeping, when the user record is already
  loaded during resolution.
- **Shorten TTLs instead** — rejected: reduces (not removes) the window, and
  short refresh TTLs force frequent re-login without fixing logout.
- **Bump on every login (per-session IDs)** — rejected: a shared counter gives
  "revoke all sessions" semantics with one integer; per-session tracking would
  need a set of IDs per user and pruning for expired ones.
- **Separate `IncrementTokenVersion` write after the password update** —
  rejected: two Raft applies allow the intermediate state (new password, old
  tokens valid); the combined single update is atomic.

## Consequences

- `token_version` flows through `domain.User`, `service.Claims`,
  `service.UserService` (`IncrementTokenVersion` + the password mutators) and
  the Raft FSM payload (`cmdUser`, snapshot/restore) — the FSM field must be
  kept in the `toDomain`/`cmdUserFrom` conversions or the bump is silently
  lost (covered by `TestFSMUserTokenVersionRoundTrip`).
- Logout revokes **all** sessions of the user, not only the calling one (one
  integer per user, no per-session state). Users signing out of one browser
  are signed out everywhere — accepted for this platform's UI.
- The check runs only for `method == AuthJWT`; the identity resolution order
  (bearer → API token → JWT → legacy token) is unchanged.
- Upgrade is a no-op for existing records and tokens; downgrade (older binary
  ignoring the claim) re-opens the pre-ADR-045 window but breaks nothing.
