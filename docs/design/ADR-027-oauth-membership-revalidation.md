# ADR-027: OAuth Group-Membership Revalidation & Token Invalidation

## Context

A user removed from an allowed OAuth group (or deleted from the IdP) must lose
API access promptly, not only on the next UI login. The existing gap:

- `AuthService.identityForUser` re-fetches group membership from the local Raft
  DB (`groups.GroupsForUser`) — correct in spirit, but local membership is
  updated only by `completeOAuthUser`, which is **additive-only** and only runs
  on *login* (ADR-022 D4: "memberships are never removed").
- No upstream OAuth credential (GitHub access token / OIDC access+refresh token)
  is persisted. `Complete` discards the `oauth2.Token` after issuing the session
  JWT.
- `AuthService.Refresh` re-loads the user from the DB but never re-checks the
  IdP.

Consequences when Alice is removed from `"devs"` on the IdP:

| Token | Behavior today | Exposure window |
|---|---|---|
| UI login | fails (`Complete` re-runs allowlist) ✅ | n/a |
| Session access JWT | valid until TTL (15m) | 15m |
| Session refresh JWT | valid; **rotation on use** means Alice can mint fresh pairs forever | **indefinite** (7d rolling) |
| API token `dct_…` | valid forever (no expiry) | **indefinite** |

## Decision

Implement a **layered** design:

1. **Persist the upstream OAuth credential encrypted at rest** so the supervisor
   can re-query the IdP later. OIDC: store access + refresh token (request
   `offline_access`). GitHub: store the long-lived access token.
2. **Re-validate group membership against the IdP** at the two token choke points,
   behind a **TTL-bounded, single-flight, jittered per-user cache**:
   - On every refresh (synchronous) — primary revocation detector for sessions.
   - On `Resolve` when the cached check is stale (synchronous, cheap because cached)
     — catches long-lived API tokens and JWT access tokens used past a cache TTL.
3. **On revocation**, persist the decision cluster-wide: mark the user
   **deactivated** (all JWTs fail everywhere) and **revoke the API token**.
   Reconcile OAuth-managed local memberships (remove stale groups) on successful
   re-validation.
4. **Failure-mode handling** is configurable: fail-closed by default, with a
   bounded "offline grace" window that keeps serving from last-known-good when
   the IdP is briefly unreachable.
5. **Hard backstop**: an optional `session_max_age` forces full re-login (which
   re-runs the complete allowlist check) after a bound, closing the window even
   when a credential is missing (e.g. pre-upgrade users).

### Why this over alternatives

- **Short-lived access tokens alone** (already 15m) do not help: refresh rotation
  + non-expiring API tokens keep the door open indefinitely.
- **Re-validate on every request, uncached** would hammer the IdP (DoS against our
  own dependency) and add latency to every `/v1/engines`, OTel, and UI call. The
  bounded cache (interval `5m` default) bounds the maximum revocation-detection
  delay to the interval while making steady-state cost ~zero.
- **Revocation lists / versioned claims** require an out-of-band "membership
  changed" signal from the IdP (webhook/SCIM), which neither provider flow
  currently has. A per-user `DeactivatedAt` flag in the replicated store is the
  closest equivalent and is sufficient because every auth decision already reads
  the local user record.
- **Jitter** (per-entry ±10% TTL) prevents all pods from refreshing the same
  user's entry simultaneously after an upgrade or mass session establishment.

## Consequences

### New domain fields

- `User.OAuthTokenCiphertext` — AES-256-GCM(nonce||ciphertext||tag), base64, of
  the JSON-encoded upstream OAuth credential. Empty for pre-revalidation users.
- `User.OAuthGroupIDs` — supervisor group IDs auto-managed by OAuth group mapping.
  Reconciliation only adds/removes within this set; admin-managed memberships are
  untouched.
- `User.DeactivatedAt` — set when IdP revalidation revokes access.
- `domain.ErrSessionRevoked` — sentinel error for IdP-determined revocation.

### Config keys

| Key | Default | Description |
|---|---|---|
| `auth.oauth.revalidate_interval` | `5m` | Per-user cache TTL for successful IdP re-checks. |
| `auth.oauth.revalidate_grace` | `1h` | Serve last-known-good when IdP is unreachable. |
| `auth.oauth.revalidate_fail_open` | `false` | After grace: false = deny, true = allow. |
| `auth.oauth.session_max_age` | `0` | Hard bound on OAuth session age; 0 = disabled. |

### Supersedes ADR-022 D4

ADR-022 D4 stated "memberships are never removed." This ADR supersedes that rule
for the **OAuth-managed set only**: reconciliation removes memberships for groups
that no longer resolve during re-validation. Admin-managed memberships are never
touched.

### Pre-upgrade users

Pre-upgrade OAuth users have `OAuthTokenCiphertext == ""`. They are allowed
without deactivation (bounded by `session_max_age` if configured). During rollout,
operators should set `session_max_age: "24h"` to force re-login for these users,
which then captures a credential and enables revalidation.

## Revision (issue #30): `invalid_grant` is not definitive revocation

Originally, any OIDC credential failure during revalidation — a refresh
rejected with `invalid_grant`/`invalid_token` or a userinfo 401 — was returned
as `domain.ErrSessionRevoked` and mapped to the destructive revoke path
(deactivate the user **and** permanently delete their `dct_` API token). That
classification is wrong: per RFC 6749 §5.2 `invalid_grant` also means the
refresh token **expired** or was **rotated** (Dex
`oauth2.refreshTokens.absoluteLifetime`/`validIfNotUsedFor`, a stateless Dex
restart invalidating all refresh tokens, or the per-pod single-flight refresh
race with `replicaCount: 3`) — the supervisor cannot distinguish these from
genuine revocation. Valid OIDC users were deactivated "after some time" and
their tokens died until a UI re-auth cleared `DeactivatedAt` (issue #30).

The fix reclassifies ambiguous credential failures as "cannot verify":

- `OIDCOAuthService.Revalidate` returns the package-private sentinel
  `errOAuthCredentialExpired` (instead of `domain.ErrSessionRevoked`) for the
  credential-unusable cases: decrypt failure, refresh
  `invalid_grant`/`invalid_token`, and a userinfo 401 refresh cannot recover.
- `OAuthRevalidator` gains a new `stateExpired` state. It is served exactly
  like `stateUnavailable` — cached groups within `revalidate_grace`, then
  `revalidate_fail_open`/fail-closed — but it **never** calls `revoke()`:
  the user is never deactivated and the API token is never deleted. The cache
  entry snapshots the credential it was built from, so a UI re-login (fresh
  ciphertext) forces an immediate re-check instead of waiting for
  `revalidate_interval`; `stateExpired` retries on the full
  `revalidate_interval` so a dead credential is not hammered against the IdP.
- **Positive revocation stays destructive**: `domain.ErrForbidden` (userinfo
  succeeds but `allowed_groups`/`admin_groups` no longer match) still
  deactivates + revokes, as does GitHub's `domain.ErrSessionRevoked` (401/404
  on an access token that never expires).

Behaviour matrix after the revision:

| Revalidation outcome | State | Side effects |
|---|---|---|
| userinfo OK | `stateOK` | reconcile memberships/role |
| userinfo OK, groups fail allowlist | `stateRevoked` | **deactivate + revoke API token** (unchanged) |
| refresh `invalid_grant`/`invalid_token`/401 (expired/rotated/revoked) | `stateExpired` | serve cache within grace, then fail policy; **never deactivate/delete** |
| IdP unreachable (transport) | `stateUnavailable` | existing grace/fail policy (unchanged) |

No config keys changed; the existing `revalidate_grace` /
`revalidate_fail_open` knobs express the expiry policy. Operators who want
"never re-auth" should configure Dex with persistent storage, no
`absoluteLifetime`, a generous `validIfNotUsedFor`, `reuseInterval >
revalidate_interval`, and use `session_max_age` as the hard backstop (see
`docs/README.md`).

### References

- ADR-017: Auth always enforced + multi-provider OAuth
- ADR-022: OAuth group allowlists + regex group mapping
