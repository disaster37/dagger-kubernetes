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

### References

- ADR-017: Auth always enforced + multi-provider OAuth
- ADR-022: OAuth group allowlists + regex group mapping
