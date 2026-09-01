# OAuth Group-Membership Revalidation & Token Invalidation

**Module:** `github.com/disaster/dagger-kubernetes`
**Status:** implementation-ready plan (not yet implemented)
**Scope:** close the de-provisioning gap — a user removed from an allowed OAuth group (or deleted from the IdP) must lose API access promptly, not only on the next UI login.

---

## 1. Current-state analysis

All findings below were verified against the code in `/projects/dagger-cache`.

### 1.1 Auth entry points and token issuance

| Concern | Location | Function(s) |
|---|---|---|
| Password login | `internal/handler/auth_endpoints.go` | `(*Server).handleLogin` → `AuthService.Login` |
| Refresh | `internal/handler/auth_endpoints.go` | `(*Server).handleRefresh` → `AuthService.Refresh` |
| OAuth login (both providers) | `internal/handler/auth_endpoints.go` | `handleOAuthLogin` / `handleOAuthOIDCLogin` → `startOAuthLogin` |
| OAuth callback | `internal/handler/auth_endpoints.go` | `handleOAuthCallback` / `handleOAuthOIDCCallback` → `completeOAuthCallback` → `OAuthProvider.Complete` |
| Token validation (all routes) | `internal/handler/middleware.go` | `resolveIdentity` / `requireAuth` / `requireAuthWithQueryFallback` / `requireAdmin` → `AuthService.Resolve` |
| Bearer extraction | `internal/handler/auth.go` | `extractToken` |
| Bearer resolution | `internal/service/auth_service.go` | `(*AuthService).Resolve`, `identityForUser` |
| JWT issuance/parsing | `internal/service/jwt_service.go` | `(*JWTService).IssuePair`, `ParseAccess`, `ParseRefresh`, `ParseOAuthState` |
| API-token validation | `internal/service/token_service.go` | `(*TokenService).Validate` |
| OAuth provider seam | `internal/service/oauth.go` | `OAuthProvider` interface (`LoginURL`, `Complete`) |
| GitHub OAuth | `internal/service/oauth_github.go` | `(*GitHubOAuthService).Complete`, `fetchOrgs`, `fetchTeams`, `fetchUser`, `exchangeCode` |
| OIDC OAuth | `internal/service/oauth_oidc.go` | `(*OIDCOAuthService).Complete`, `discover`, `mergeUserInfo`, `resolveGroups`, `resolveUsername` |
| Shared OAuth tail | `internal/service/oauth.go` | `completeOAuthUser`, `joinGroupByName`, `joinMappedGroups`, `orgsIntersect` |
| Group mapping | `internal/service/group_mapper.go` | `(*GroupMapper).Map`, `mapIfActive`, `Active` |
| User persistence | `internal/service/user_service.go` | `EnsureOAuthUser`, `Authenticate`, `Create`, `UpdateRole`, `Delete` |
| Group persistence | `internal/service/group_service.go` | `GroupsForUser`, `SetMembers`, `SetUserGroups`, `EnsureMember`, `addGroupMember`/`removeGroupMember` |
| User/group repo (Raft) | `internal/repository/user_repo.go`, `internal/repository/group_repo.go` | FSM-backed |
| FSM command encoding | `internal/repository/fsm.go` | `cmdUser`, `cmdToken`, `toDomain`/`cmdUserFrom`/`cmdTokenFrom` |
| Config | `internal/domain/config.go` (`AuthConfig`/`OAuthConfig`/`JWTConfig`), `config/loader.go` (`SetDefault`, `validateAuthConfig`) | — |
| Wiring | `cmd/api/main.go` | `run` (constructs `jwtSvc`, `authSvc`, `oauthSvc`, `tokensSvc`) |

### 1.2 How groups/roles are extracted from claims

- **OIDC** (`oauth_oidc.go`): the `groups_claim` (default `"groups"`) is normalized from `[]string` / `[]any` / single string (`resolveGroups`). `allowed_groups ∪ allowed_orgs` is the allowlist; non-empty allowlist with no intersection → `domain.ErrForbidden` (`Complete`). Username comes from `username_claim` (fallback `email`). Role is **always `domain.RoleUser`** for OAuth users — role is never derived from claims; it is stored on `domain.User`.
- **GitHub** (`oauth_github.go`): provider groups = orgs (`/user/orgs` → `login`) ∪ teams (`/user/teams` → `"org/team"`). `allowed_orgs` and `allowed_teams` are AND-ed. Role is `domain.RoleUser`.
- **Mapping**: `GroupMapper.mapIfActive` translates provider groups → supervisor group names; `completeOAuthUser` joins the user to mapped groups via `joinMappedGroups`, and falls back to `default_group` when the user has zero memberships.

### 1.3 Token model (three kinds)

1. **Session JWT (HS256)** — access TTL `auth.jwt.access_ttl` (default `15m`), refresh TTL `auth.jwt.refresh_ttl` (default `7d`). `Claims` (`jwt_service.go`) carries `uid`, `username`, `role`, `groups`, `typ`. Refresh is **rotated on use** (`AuthService.Refresh` issues a fresh pair with a fresh `7d` refresh token).
2. **Per-user API token** (`dct_<32 hex>`) — opaque, hashed (SHA-256), **no expiry**, one per user (`domain.APIToken`).
3. **Legacy flat-file token** — synthetic `legacy` admin identity (migration fallback only).

### 1.4 Where tokens are validated

`AuthService.Resolve` (`auth_service.go`) is the single choke point, called by `resolveIdentity`/`requireAuth*` in `middleware.go` for every authenticated route (control API, engine provisioning `/v1/engines`, OTel ingest, cache proxy is exempt — it uses `cache.auth_token`). Resolution order: empty → `dct_` API token → JWT access → legacy file → `ErrUnauthenticated`.

### 1.5 The exact gap (root cause)

- `identityForUser` re-fetches group membership **from the local Raft DB** (`groups.GroupsForUser`) — *not* from JWT claims (comment: "claims can be stale"). This is correct in spirit, **but** local membership is updated only by `completeOAuthUser`, which is **additive-only** and only runs on *login* (ADR-022 D4: "memberships are never removed").
- No upstream OAuth credential (GitHub access token / OIDC access+refresh token) is persisted. `Complete` discards the `oauth2.Token` / GitHub access token after issuing the session JWT.
- `AuthService.Refresh` re-loads the user from the DB but never re-checks the IdP.

Consequences, concretely, when Alice is removed from `"devs"` on the IdP:

| Token | Behavior today | Exposure window |
|---|---|---|
| UI login | fails (`Complete` re-runs allowlist) ✅ | n/a |
| Session access JWT | valid until TTL (15m) | 15m |
| Session refresh JWT | valid; **rotation on use** means Alice can mint fresh pairs forever | **indefinite** (7d rolling) |
| API token `dct_…` | valid forever (no expiry) | **indefinite** |

Additional related gap: local DB membership is never reconciled, so even after Alice's IdP groups change, her local `"devs"` membership (and its quota/project visibility) persists.

### 1.6 Session store note

`domain.SessionStore` (`internal/domain/session.go`) is about **engine lease sessions** (client-cert fingerprint → `Lease`), not auth sessions. It is out of scope for this change; do not touch it.

---

## 2. Proposed design

### 2.1 Recommended approach (combination)

The best practice for this gap, adapted to a platform whose authorization source is an external IdP, is a **layered** design:

1. **Persist the upstream OAuth credential (encrypted at rest)** so the supervisor *can* re-query the IdP later. OIDC: store access token + refresh token (request `offline_access`). GitHub: store the (long-lived) access token.
2. **Re-validate group membership against the IdP** at the two token choke points, behind a **TTL-bounded, single-flight, jittered per-user cache**:
   - **On every refresh** (synchronous) — this is the primary revocation detector for sessions.
   - **On `Resolve` when the cached check is stale** (synchronous, but cheap because cached) — this catches long-lived API tokens and JWT access tokens used past a cache TTL.
3. **On revocation**, persist the decision so it is cluster-wide and immediate: mark the user **deactivated** (all JWTs fail everywhere) and **revoke the API token**. Reconcile OAuth-managed local memberships (remove stale groups) on successful re-validation.
4. **Failure-mode handling** is configurable: fail-closed by default, with a bounded "offline grace" window that keeps serving from last-known-good when the IdP is briefly unreachable.
5. **Hard backstop**: an optional `session_max_age` forces full re-login (which re-runs the complete allowlist check) after a bound, closing the window even when a credential is missing (e.g. pre-upgrade users) or the IdP cannot be reached.

### 2.2 Why this over the alternatives

- **Short-lived access tokens alone** (already 15m) do not help: refresh rotation + non-expiring API tokens keep the door open indefinitely.
- **Re-validate on every request, uncached** would hammer the IdP (DoS against our own dependency) and add latency to every `/v1/engines`, OTel, and UI call. The bounded cache (interval `5m` default) bounds the maximum revocation-detection delay to the interval while making steady-state cost ~zero.
- **Revocation lists / versioned claims** require an out-of-band "membership changed" signal from the IdP (webhook/SCIM), which neither provider flow currently has. A per-user `DeactivatedAt` flag in the replicated store is the closest equivalent and is sufficient because every auth decision already reads the local user record.
- **Jitter** (per-entry ±10% TTL) prevents all pods from refreshing the same user's entry simultaneously after an upgrade or mass session establishment (thundering herd).
- **Fail-open vs fail-closed** must be explicit and configurable; default is **fail-closed with an offline grace**, because the security guarantee (revoked users lose access) must survive a flaky IdP, but a brief IdP outage must not instantly lock out the whole fleet.

### 2.3 Concrete behavior per scenario

Definitions: `interval` = `auth.oauth.revalidate_interval` (default `5m`), `grace` = `auth.oauth.revalidate_grace` (default `1h`), `fail_open` = `auth.oauth.revalidate_fail_open` (default `false`), `max_age` = `auth.oauth.session_max_age` (default `0` = disabled).

| # | Scenario | Expected behavior |
|---|---|---|
| S1 | Alice in `"devs"`, tokens fresh | `Resolve` serves from cache; no IdP call until cache stale. |
| S2 | Alice removed from `"devs"` (only allowed group) | Next cache-expiry `Resolve`/`Refresh` re-checks IdP → allowlist fails → **deactivate Alice + revoke her API token** → `401`. Replicated via Raft; all pods reject her JWTs within `interval` (plus Raft replication latency). |
| S3 | Alice still in *some* allowed group but groups changed | Re-validation succeeds with new group set → reconcile local memberships (remove stale OAuth-managed groups, add new ones) → identity carries the new `GroupIDs`; no deactivation. |
| S4 | Alice deleted from the IdP entirely | GitHub `/user` returns 401/404; OIDC userinfo returns 401 or refresh fails `invalid_grant` → treated as **revoked** (`ErrSessionRevoked`) → deactivate + token revoke → `401`. |
| S5 | IdP unreachable, last-good check within `grace` | Serve last-known-good from cache (allow); log `Warn`. No IdP retry until grace elapses or cache expiry. |
| S6 | IdP unreachable, last-good check older than `grace`, `fail_open=false` | **Deny** (`401`), log `Error`. (Fail-closed.) |
| S7 | IdP unreachable, older than `grace`, `fail_open=true` | **Allow** with last-known-good, log `Error`. (Fail-open, explicitly opted-in.) |
| S8 | User has **no stored credential** (pre-upgrade OAuth user) | Cannot re-check → **allow**, log `Debug` once. Bounded by `session_max_age` if set (recommended during rollout). |
| S9 | Stored OIDC access token expired | `Revalidate` refreshes it via the stored refresh token, updates the stored credential, proceeds. If refresh fails `invalid_grant`/`invalid_token` → S4. |
| S10 | `max_age` set; refresh token older than `max_age` | `Refresh` rejects with `ErrSessionRevoked` (force re-login) regardless of cache, even if IdP check is stale/unreachable. |
| S11 | Two requests race for the same user's stale entry | Single-flight: one goroutine does the IdP call; others wait on its result (channel), then serve the same outcome. |
| S12 | Revocation write fails on a follower pod (`ErrNotLeader`) | Deny the *current* request anyway (401). The local cache records "revoked" so further requests on this pod deny without IdP calls; the leader independently revalidates and persists deactivation. |
| S13 | Non-OAuth user (internal/bootstrap/legacy) | Revalidation is **skipped entirely**; behavior unchanged. |
| S14 | GitHub OAuth token revoked by the user/app | `/user` returns 401 → `ErrSessionRevoked` → deactivate + token revoke. |

---

## 3. Detailed implementation spec

### 3.1 New domain types & sentinel

**File: `internal/domain/user.go`** — extend `User` (stdlib types only; no import changes):

```go
type User struct {
    ID            string    `json:"id"`
    Username      string    `json:"username"`
    Role          Role      `json:"role"`
    PasswordHash  string    `json:"-"`
    OAuthProvider string    `json:"oauth_provider,omitempty"`
    OAuthID       string    `json:"oauth_id,omitempty"`

    // OAuthTokenCiphertext is AES-256-GCM(nonce||ciphertext||tag), base64, of
    // the JSON-encoded service.oauthCredential (access + optional refresh
    // token) captured at OAuth login. Empty for pre-revalidation users and for
    // non-OAuth users.
    OAuthTokenCiphertext string `json:"-"`

    // OAuthGroupIDs are the supervisor group IDs currently auto-managed by
    // OAuth group mapping (see ADR-027). Reconciliation only adds/removes
    // memberships within this set; admin-managed memberships are never touched.
    OAuthGroupIDs []string `json:"oauth_group_ids,omitempty"`

    // DeactivatedAt is set when IdP revalidation revokes access; identity
    // resolution and refresh reject deactivated users cluster-wide.
    DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`

    CreatedAt     time.Time `json:"created_at"`
    UpdatedAt     time.Time `json:"updated_at"`
}

// Deactivated reports whether the user's access has been revoked by IdP
// revalidation.
func (u *User) Deactivated() bool { return u != nil && u.DeactivatedAt != nil }
```

**File: `internal/domain/identity.go`** — add sentinel (alongside the existing ones):

```go
// ErrSessionRevoked indicates IdP revalidation determined the principal is no
// longer authorized (removed from allowed groups, or deleted from the IdP).
// Handlers map it to 401 with a distinct message so the SPA can force re-login.
ErrSessionRevoked = errors.New("authorization revoked by identity provider")
```

### 3.2 Config

**File: `internal/domain/config.go`** — extend `OAuthConfig`:

```go
// OIDC-only + revalidation fields. Revalidation settings apply to both
// providers (github and oidc).
IssuerURL     string   `mapstructure:"issuer_url"`
Scopes        []string `mapstructure:"scopes"`
UsernameClaim string   `mapstructure:"username_claim"`
GroupsClaim   string   `mapstructure:"groups_claim"`
CACertPath    string   `mapstructure:"ca_cert_path"`

// Revalidation of IdP group membership (ADR-027).
RevalidateInterval time.Duration `mapstructure:"revalidate_interval"` // default 5m; per-user cache TTL
RevalidateGrace    time.Duration `mapstructure:"revalidate_grace"`    // default 1h; offline grace window
RevalidateFailOpen bool          `mapstructure:"revalidate_fail_open"`// default false (fail closed)
SessionMaxAge      time.Duration `mapstructure:"session_max_age"`     // default 0 (disabled); hard refresh-token bound for OAuth users
```

**File: `config/loader.go`** — add `SetDefault` (after the existing oauth block, ~line 51):

```go
v.SetDefault("auth.oauth.revalidate_interval", 5*time.Minute)
v.SetDefault("auth.oauth.revalidate_grace", time.Hour)
v.SetDefault("auth.oauth.revalidate_fail_open", false)
v.SetDefault("auth.oauth.session_max_age", 0)
```

Add validation inside `validateAuthConfig` (or a new `validateOAuthRevalidation` invoked right after it in `Load`, following the existing pattern). Rules:

```go
// Only validated when auth.oauth.enabled is true.
if cfg.Auth.OAuth.Enabled {
    if cfg.Auth.OAuth.RevalidateInterval <= 0 {
        return fmt.Errorf("auth.oauth.revalidate_interval must be > 0 when auth.oauth.enabled is true")
    }
    if cfg.Auth.OAuth.RevalidateGrace < 0 {
        return fmt.Errorf("auth.oauth.revalidate_grace must be >= 0")
    }
    if cfg.Auth.OAuth.SessionMaxAge < 0 {
        return fmt.Errorf("auth.oauth.session_max_age must be >= 0 (0 disables)")
    }
}
```

**File: `config/config.app.yaml.sample`** — add under the `oauth:` block (after `groups_claim`):

```yaml
    # --- IdP group-membership revalidation (ADR-027) -----------------------
    # Detects when a user is removed from the allowed OAuth group (or deleted
    # from the IdP) while already holding tokens, and revokes their access.
    revalidate_interval: "5m"   # per-user cache TTL for successful IdP re-checks; bounds the detection delay.
    revalidate_grace: "1h"      # serve last-known-good membership for this long when the IdP is unreachable.
    revalidate_fail_open: false # after grace, false = deny (fail-closed), true = allow (fail-open).
    session_max_age: "0"        # optional hard bound on an OAuth session (refresh-token age); 0 = disabled.
                                #   Set e.g. "24h" during rollout to bound sessions issued before this change.
```

### 3.3 FSM persistence

**File: `internal/repository/fsm.go`** — extend `cmdUser` and both converters:

```go
cmdUser struct {
    // ... existing fields ...
    OAuthTokenCiphertext string     `json:"oauth_token_ciphertext"`
    OAuthGroupIDs        []string   `json:"oauth_group_ids,omitempty"`
    DeactivatedAt        *time.Time `json:"deactivated_at,omitempty"`
    Create               bool       `json:"create,omitempty"`
}
```

`toDomain` and `cmdUserFrom` must carry the three new fields (mirror the existing `PasswordHash` handling). No new command kinds are needed — the existing `kindUpsertUser` (insert-only vs update-only via `Create`) already persists arbitrary user fields.

> Note: FSM command payloads are JSON and versioned by the FSM's schema. Adding fields is additive and backward-compatible with existing persisted state (missing fields decode to zero values). No snapshot migration is required.

### 3.4 Credential codec

**File: `internal/service/oauth_credential.go`** (new):

```go
// oauthCredential is the upstream OAuth credential captured at login and
// persisted encrypted on domain.User.OAuthTokenCiphertext.
type oauthCredential struct {
    Provider     string    `json:"provider"`                 // "github" | "oidc"
    AccessToken  string    `json:"access_token"`
    RefreshToken string    `json:"refresh_token,omitempty"`  // OIDC only
    ExpiresAt    time.Time `json:"expires_at,omitempty"`     // zero = unknown/does not expire
}

// encryptOAuthCredential JSON-encodes c and AES-256-GCM-encrypts it with
// encKey (reuses encryptToken). A nil/empty key returns ("", nil): credential
// persistence is disabled (pre-config dev mode).
func encryptOAuthCredential(encKey []byte, c *oauthCredential) (string, error) {
    if c == nil || len(encKey) == 0 {
        return "", nil
    }
    b, err := json.Marshal(c)
    if err != nil {
        return "", fmt.Errorf("marshal oauth credential: %w", err)
    }
    return encryptToken(encKey, string(b))
}

// decryptOAuthCredential reverses encryptOAuthCredential. Returns (nil, nil)
// when ct is empty (no stored credential).
func decryptOAuthCredential(encKey []byte, ct string) (*oauthCredential, error) {
    if ct == "" {
        return nil, nil
    }
    if len(encKey) == 0 {
        return nil, fmt.Errorf("oauth credential stored but no encryption key configured")
    }
    pt, err := decryptToken(encKey, ct)
    if err != nil {
        return nil, fmt.Errorf("decrypt oauth credential: %w", err)
    }
    var c oauthCredential
    if err := json.Unmarshal([]byte(pt), &c); err != nil {
        return nil, fmt.Errorf("decode oauth credential: %w", err)
    }
    return &c, nil
}
```

Reuses the existing package-private `encryptToken`/`decryptToken` in `internal/service/token_service.go` (both already live in `service`).

### 3.5 OAuth provider interface + shared tail refactor

**File: `internal/service/oauth.go`**:

1. Extend the interface:
```go
type OAuthProvider interface {
    LoginURL(state string) string
    Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error)
    // Revalidate re-checks the user's current IdP group membership using the
    // stored credential and returns the current provider group names. Returns
    // domain.ErrSessionRevoked when the credential is invalid/expired beyond
    // refresh (user must re-login) and domain.ErrForbidden when membership no
    // longer satisfies the allowlist.
    Revalidate(ctx context.Context, u *domain.User) ([]string, error)
}
```

2. Replace `completeOAuthUser` with `completeOAuthLogin` (same callers, richer behavior). Signature:

```go
func completeOAuthLogin(
    ctx context.Context,
    users *UserService,
    groups domain.GroupRepository,
    jwt *JWTService,
    logger *logrus.Logger,
    encKey []byte,
    provider, oauthID, username, defaultGroup string,
    mappedGroups []string,
    credential *oauthCredential,
) (access, refresh string, u *domain.User, err error)
```

Behavior of `completeOAuthLogin` (order matters):
1. `EnsureOAuthUser` (unchanged).
2. Clear deactivation from a previous revocation: if `u.DeactivatedAt != nil`, set `u.DeactivatedAt = nil` (a successful IdP login re-authorizes).
3. **Reconcile OAuth-managed memberships** (see §3.8) using `mappedGroups` → resolve to supervisor group IDs → diff against `u.OAuthGroupIDs` → `addGroupMember`/`removeGroupMember` → update `u.OAuthGroupIDs`. Replaces the old `joinMappedGroups` add-only behavior.
4. `default_group` fallback unchanged (auto-join when zero memberships remain).
5. Persist credential: `ct, err := encryptOAuthCredential(encKey, credential); u.OAuthTokenCiphertext = ct`.
6. `users.Update(ctx, u)` to persist credential + reconciled groups + deactivation clear (wrap: `persist oauth user: %w`).
7. `jwt.IssuePair(u, groupIDs(gids))` (unchanged tail).

Keep `orgsIntersect` and `joinGroupByName` (still used by the default-group fallback).

### 3.6 GitHub OAuth service

**File: `internal/service/oauth_github.go`**:

1. Add `encKey []byte` field; extend `NewGitHubOAuthService(cfg, mapper, users, groups, jwtSvc, logger, encKey []byte)`.
2. In `Complete`, after computing `mappedGroups`, build `cred := &oauthCredential{Provider: "github", AccessToken: accessToken}` and call `completeOAuthLogin(...)` (pass `encKey`, `cred`).
3. Implement `Revalidate`:
```go
func (s *GitHubOAuthService) Revalidate(ctx context.Context, u *domain.User) ([]string, error) {
    cred, err := decryptOAuthCredential(s.encKey, u.OAuthTokenCiphertext)
    if err != nil || cred == nil {
        return nil, domain.ErrSessionRevoked // no credential: cannot re-check, force re-login
    }
    if _, err := s.fetchUser(ctx, cred.AccessToken); err != nil {
        return nil, domain.ErrSessionRevoked // 401/404 => user/token gone
    }
    orgs, err := s.fetchOrgs(ctx, cred.AccessToken)
    if err != nil {
        return nil, fmt.Errorf("fetch github orgs: %w", err)
    }
    var teams []string
    if len(s.allowedTeams) > 0 || s.mapper.Active() {
        teams, err = s.fetchTeams(ctx, cred.AccessToken)
        if err != nil {
            if len(s.allowedTeams) > 0 {
                return nil, fmt.Errorf("fetch github teams: %w", err)
            }
            s.logger.WithError(err).Warn("oauth: github teams fetch failed during revalidation")
        }
    }
    if len(s.allowedOrgs) > 0 && !orgsIntersect(s.allowedOrgs, orgs) {
        return nil, domain.ErrForbidden
    }
    if len(s.allowedTeams) > 0 && !orgsIntersect(s.allowedTeams, teams) {
        return nil, domain.ErrForbidden
    }
    out := make([]string, 0, len(orgs)+len(teams))
    out = append(out, orgs...)
    out = append(out, teams...)
    return out, nil
}
```

### 3.7 OIDC OAuth service

**File: `internal/service/oauth_oidc.go`**:

1. Add `encKey []byte` field; extend `NewOIDCOAuthService(cfg, mapper, users, groups, jwtSvc, logger, httpClient, encKey []byte)`.
2. In `NewOIDCOAuthService`, auto-append `"offline_access"` to scopes when missing (so a refresh token is returned for later revalidation). Update the existing `openid` append loop to also ensure `offline_access`.
3. In `Complete`, build the credential from the exchanged `*oauth2.Token`:
```go
cred := &oauthCredential{
    Provider:     "oidc",
    AccessToken:  tok.AccessToken,
    RefreshToken: tok.RefreshToken,
    ExpiresAt:    tok.Expiry,
}
```
and call `completeOAuthLogin(...)`.
4. Implement `Revalidate`. Reuse the existing `discover`, `oauth2Config`, `mergeUserInfo`, `resolveUsername`, `resolveGroups`, and `effectiveAllowedGroups` helpers:
```go
func (s *OIDCOAuthService) Revalidate(ctx context.Context, u *domain.User) ([]string, error) {
    cred, err := decryptOAuthCredential(s.encKey, u.OAuthTokenCiphertext)
    if err != nil || cred == nil {
        return nil, domain.ErrSessionRevoked
    }
    if s.httpClient != nil {
        ctx = oidc.ClientContext(ctx, s.httpClient)
    }
    p, err := s.discover(ctx)
    if err != nil {
        return nil, fmt.Errorf("oidc: discover provider: %w", err)
    }

    ts, err := s.tokenSource(ctx, p, cred) // refresh when expired, persist-on-refresh via users.Update
    if err != nil {
        return nil, domain.ErrSessionRevoked // invalid_grant => user gone/revoked
    }

    ui, err := p.UserInfo(ctx, ts)
    if err != nil {
        return nil, domain.ErrSessionRevoked // userinfo 401 => token revoked
    }
    var claims map[string]any
    if err := ui.Claims(&claims); err != nil {
        return nil, fmt.Errorf("oidc: userinfo decode: %w", err)
    }
    groups := s.resolveGroups(claims)
    if allowlist := s.effectiveAllowedGroups(); len(allowlist) > 0 && !orgsIntersect(allowlist, groups) {
        return nil, domain.ErrForbidden
    }
    return groups, nil
}

// tokenSource returns a TokenSource for cred, refreshing (and persisting) the
// stored credential when the access token is expired or userinfo returns 401.
func (s *OIDCOAuthService) tokenSource(ctx context.Context, p oidcProvider, cred *oauthCredential) (oauth2.TokenSource, error)
```

`tokenSource` details: use `oauth2.ReuseTokenSource(nil, source)` where `source.Token()` first calls userinfo with the cached access token; on a `401` (or `cred.ExpiresAt` past), call `s.oauth2Config(p.Endpoint()).TokenSource(ctx, &oauth2.Token{AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken, Expiry: cred.ExpiresAt}).Token()`. On refresh success, update `cred` and best-effort re-encrypt+persist on the user via `users.Update` (log `Warn` on persist failure). On `invalid_grant`/`invalid_token`/non-retryable error → return `domain.ErrSessionRevoked`.

> **Interface note:** `(*OIDCOAuthService)` currently uses the field `users *UserService`. `Revalidate` may persist a refreshed credential, so it needs `users`. That field already exists.

### 3.8 OAuth membership reconciliation

**File: `internal/service/oauth_revalidator.go`** (new) — reconciliation helper:

```go
// reconcileMemberships applies the desired OAuth-managed supervisor group
// memberships for u: add memberships for names that newly resolve, remove
// memberships for previously OAuth-managed names that no longer resolve.
// Admin-managed memberships (groups not in u.OAuthGroupIDs) are never touched.
// Returns the resulting supervisor group IDs.
func (r *OAuthRevalidator) reconcileMemberships(
    ctx context.Context, u *domain.User, mappedNames []string,
) ([]string, error) {
    // resolve names -> IDs (skip missing groups with Warn, like joinGroupByName)
    wanted := make(map[string]bool, len(mappedNames))
    for _, name := range mappedNames {
        g, err := r.groups.GetByName(ctx, name)
        if err != nil {
            r.logger.WithError(err).WithField("group", name).Warn("oauth: group not found during revalidation, skipping")
            continue
        }
        wanted[g.ID] = true
    }

    previous := make(map[string]bool, len(u.OAuthGroupIDs))
    for _, gid := range u.OAuthGroupIDs {
        previous[gid] = true
    }

    // adds
    for gid := range wanted {
        if !previous[gid] {
            if err := addGroupMember(ctx, r.groups, gid, u.ID); err != nil {
                return nil, fmt.Errorf("add oauth group %s: %w", gid, err)
            }
        }
    }
    // removes (only within the previously OAuth-managed set)
    for gid := range previous {
        if !wanted[gid] {
            if err := removeGroupMember(ctx, r.groups, gid, u.ID); err != nil {
                return nil, fmt.Errorf("remove oauth group %s: %w", gid, err)
            }
        }
    }

    u.OAuthGroupIDs = make([]string, 0, len(wanted))
    for gid := range wanted {
        u.OAuthGroupIDs = append(u.OAuthGroupIDs, gid)
    }
    return keysOf(wanted), nil
}
```

> This deliberately **supersedes ADR-022 D4** ("additive only, never remove") for the OAuth-managed set only. Document in ADR-027.

### 3.9 The revalidator service

**File: `internal/service/oauth_revalidator.go`** (new) — full spec:

```go
type OAuthRevalidatorConfig struct {
    Interval time.Duration // successful-check cache TTL
    Grace    time.Duration // offline grace window before fail-open/fail-closed applies
    FailOpen bool          // after grace, true = allow, false = deny
}

type revalidationState int

const (
    stateOK        revalidationState = iota
    stateRevoked                     // IdP says no / credential invalid
    stateUnavailable                  // IdP unreachable
)

type revalidationEntry struct {
    state     revalidationState
    groupIDs  []string
    checkedAt time.Time
    inflight  chan struct{} // nil when not refreshing; closed when done
}

type OAuthRevalidator struct {
    provider OAuthProvider
    mapper   *GroupMapper
    users    *UserService
    groups   domain.GroupRepository
    tokens   *TokenService
    logger   *logrus.Logger
    cfg      OAuthRevalidatorConfig
    clock    func() time.Time

    mu    sync.Mutex
    cache map[string]*revalidationEntry
}

func NewOAuthRevalidator(
    provider OAuthProvider,
    mapper *GroupMapper,
    users *UserService,
    groups domain.GroupRepository,
    tokens *TokenService,
    logger *logrus.Logger,
    cfg OAuthRevalidatorConfig,
) *OAuthRevalidator {
    return &OAuthRevalidator{
        provider: provider, mapper: mapper, users: users, groups: groups,
        tokens: tokens, logger: logger, cfg: cfg,
        clock: func() time.Time { return time.Now().UTC() },
        cache: make(map[string]*revalidationEntry),
    }
}

// Check returns the user's current effective supervisor group IDs, enforcing
// IdP revalidation behind a bounded cache. It returns a non-nil error only
// when access must be DENIED (revoked, or unavailable past grace with
// fail-closed). On a definitive revocation it applies side effects
// (deactivate user + revoke API token) best-effort.
func (r *OAuthRevalidator) Check(ctx context.Context, u *domain.User) ([]string, error)
```

`Check` logic (single-flight + cache + jitter):

1. `r.mu.Lock()`; read `entry`. If absent, create with `inflight` channel and lock is released; the creator runs `checkFresh`.
2. If `entry` present and fresh (`now.Sub(checkedAt) < jitteredTTL(entry.state)`) and `inflight == nil` → return cached result (state `ok` → groupIDs, `revoked` → `ErrSessionRevoked`, `unavailable` → apply grace/fail-open policy).
3. If `entry.inflight != nil` (another goroutine refreshing) → wait on `entry.inflight` (with `ctx`), then re-read and return per step 2. (This is the single-flight wait.)
4. Creator path `checkFresh`:
   - If `u.DeactivatedAt != nil` → state `revoked`, return `ErrSessionRevoked` (no IdP call).
   - `groups, err := r.provider.Revalidate(ctx, u)`:
     - success → `mapped := r.mapper.mapIfActive(groups)`; `gids, err := r.reconcileMemberships(ctx, u, mapped)`; persist `u` via `r.users.Update`; state `ok`; cache groupIDs.
     - `errors.Is(err, domain.ErrForbidden)` or `ErrSessionRevoked` → `r.revoke(ctx, u)`; state `revoked`; return `ErrSessionRevoked`.
     - other error (transport) → record `unavailable` with `checkedAt = now`; policy: if `grace` elapsed since last good → apply fail-open/fail-closed; else serve last-known-good. Cache accordingly.
5. `jitteredTTL(state)`: `interval` × `[0.9, 1.1]` for `ok`/`revoked`; for `unavailable`, the retry window is `min(interval, grace)` (re-check sooner while degraded).

`revoke`:
```go
func (r *OAuthRevalidator) revoke(ctx context.Context, u *domain.User) {
    now := r.clock()
    u.DeactivatedAt = &now
    if err := r.users.Update(ctx, u); err != nil {
        r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: deactivate user failed (not leader?)")
    }
    if err := r.tokens.Revoke(ctx, u.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
        r.logger.WithError(err).WithField("user_id", u.ID).Warn("oauth: revoke api token failed")
    }
    r.logger.WithFields(logrus.Fields{
        "user_id":        u.ID,
        "username":       u.Username,
        "oauth_provider": u.OAuthProvider,
    }).Warn("oauth: membership revalidation revoked access")
}
```

> `tokens.Revoke` returns `nil` when no token exists (see `TokenService.Revoke` → `tokens.Delete`); guard is defensive only.

`Check` must return errors wrapped per convention (`fmt.Errorf(...: %w)`), and the "no credential" case must **not** deny: when `Revalidate` returns `ErrSessionRevoked` due to missing credential, `Check` distinguishes it (it *is* definitive in that we cannot verify, but failing closed would lock out pre-upgrade users). Per S8: treat missing-credential `ErrSessionRevoked` as **allow** (log `Debug`) — the `session_max_age` backstop bounds it. Implementation: have `Revalidate` return a dedicated wrapped error for "missing credential" (`errOAuthNoCredential`) vs "credential invalid". Simplest: define in `oauth_revalidator.go`:

```go
var errOAuthNoCredential = errors.New("no stored oauth credential")
```
and have both providers return `errOAuthNoCredential` for the empty-credential case; `Check` maps that to `stateOK`-with-DB-groups (allow) instead of revoke.

### 3.10 AuthService changes

**File: `internal/service/auth_service.go`**:

1. Add field `revalidator *OAuthRevalidator`.
2. Add setter (minimizes churn to the 14 existing `NewAuthService` call sites, which correctly leave revalidation disabled when OAuth is off):
```go
// SetOAuthRevalidator wires the OAuth membership revalidator. nil (default)
// disables revalidation; only wired when OAuth is the configured provider.
func (a *AuthService) SetOAuthRevalidator(r *OAuthRevalidator) { a.revalidator = r }
```
3. Add shared group loader used by both `identityForUser` and `Refresh`:
```go
// loadAuthorizedGroups returns the user's current effective supervisor group
// IDs, enforcing deactivation and (for OAuth users) IdP revalidation. A
// non-nil error means DENY (unauthenticated).
func (a *AuthService) loadAuthorizedGroups(ctx context.Context, u *domain.User) ([]string, error) {
    if u.Deactivated() {
        a.logger.WithField("user_id", u.ID).Debug("user deactivated by IdP revalidation")
        return nil, domain.ErrUnauthenticated
    }
    if u.OAuthProvider != "" && a.revalidator != nil {
        gids, err := a.revalidator.Check(ctx, u)
        if err != nil {
            a.logger.WithError(err).WithFields(logrus.Fields{
                "user_id": u.ID, "oauth_provider": u.OAuthProvider,
            }).Warn("oauth revalidation denied")
            return nil, domain.ErrUnauthenticated
        }
        return gids, nil
    }
    gids, _ := a.groups.GroupsForUser(ctx, u.ID)
    return groupIDs(gids), nil
}
```
4. `identityForUser`: replace the `groups.GroupsForUser` block with `gids, err := a.loadAuthorizedGroups(ctx, u)`; on err return `nil, err`; build `Identity.GroupIDs = gids`.
5. `Refresh`: after loading `u`, add the max-age backstop (if configured and `u.OAuthProvider != ""`):
```go
if a.maxOAuthSessionAge > 0 && u.OAuthProvider != "" {
    if age := time.Since(claims.IssuedAt.Time); age > a.maxOAuthSessionAge {
        a.logger.WithFields(logrus.Fields{
            "user_id": u.ID, "age": age, "max_age": a.maxOAuthSessionAge,
        }).Warn("oauth session exceeded max age; re-login required")
        return "", "", domain.ErrSessionRevoked
    }
}
```
then `gids, err := a.loadAuthorizedGroups(ctx, u)`; on err return `"", "", err`; pass `gids` into `issuePairForUser` (change its signature to accept `groupIDs []string` instead of re-fetching, or add an overload `issuePair(ctx, u, gids)`).
6. Add `maxOAuthSessionAge time.Duration` field + set via `NewAuthService`? To avoid touching the constructor, add it to the revalidator config instead and expose `r.SessionMaxAge()`. Simpler: fold `SessionMaxAge` into `OAuthRevalidatorConfig` and expose `func (r *OAuthRevalidator) SessionMaxAge() time.Duration`. `Refresh` reads it via `a.revalidator` when non-nil. This keeps `AuthService` constructor unchanged.

**File: `internal/service/user_service.go`** — add a passthrough for persisting OAuth-derived state:
```go
// Update persists a modified user (OAuth credential, OAuth-managed groups,
// deactivation flag). Mirrors UpdateRole's shape.
func (s *UserService) Update(ctx context.Context, u *domain.User) error {
    return s.users.Update(ctx, u)
}
```

### 3.11 Handler changes

**File: `internal/handler/middleware.go`** — extend `writeServiceError` (before the `default` case):

```go
case errors.Is(err, domain.ErrSessionRevoked):
    writeError(c, consts.StatusUnauthorized, "session revoked; please sign in again")
```

**File: `internal/handler/auth_endpoints_test.go`** — add `Revalidate` to the fake providers (`fakeOAuthProvider`, `forbiddenOAuthProvider`) so they keep satisfying the widened `OAuthProvider` interface:
```go
func (fakeOAuthProvider) Revalidate(context.Context, *domain.User) ([]string, error) { return nil, nil }
func (forbiddenOAuthProvider) Revalidate(context.Context, *domain.User) ([]string, error) { return nil, domain.ErrForbidden }
```

No other handler changes. The existing `requireAuth`/`resolveIdentity` flow already funnels through `AuthService.Resolve`, which now enforces revalidation.

### 3.12 Wiring

**File: `cmd/api/main.go`** — in `run`, inside the `if cfg.Auth.OAuth.Enabled` block:

1. Pass `tokenEncKey` into both provider constructors:
   - `service.NewGitHubOAuthService(&cfg.Auth.OAuth, mapper, usersSvc, groupRepo, jwtSvc, logger, tokenEncKey)`
   - `service.NewOIDCOAuthService(&cfg.Auth.OAuth, mapper, usersSvc, groupRepo, jwtSvc, logger, oauthHTTPClient, tokenEncKey)`
2. After `authSvc := service.NewAuthService(...)`:
```go
if cfg.Auth.OAuth.Enabled && oauthSvc != nil {
    revalidator := service.NewOAuthRevalidator(oauthSvc, mapper, usersSvc, groupRepo, tokensSvc, logger, service.OAuthRevalidatorConfig{
        Interval: cfg.Auth.OAuth.RevalidateInterval,
        Grace:    cfg.Auth.OAuth.RevalidateGrace,
        FailOpen: cfg.Auth.OAuth.RevalidateFailOpen,
        SessionMaxAge: cfg.Auth.OAuth.SessionMaxAge,
    })
    authSvc.SetOAuthRevalidator(revalidator)
}
```
3. Add `SessionMaxAge` to `OAuthRevalidatorConfig` (see §3.9) and a `func (r *OAuthRevalidator) SessionMaxAge() time.Duration` accessor.

> `mapper` is already in scope in that block. `tokenEncKey` is returned from `initRaftStore` earlier in `run`.

### 3.13 Logging & error conventions (summary)

- Wrap with `fmt.Errorf(...: %w)` everywhere a lower error is propagated (`completeOAuthLogin` persistence, `Revalidate` discovery/userinfo, `reconcileMemberships`, credential codec).
- `logrus.Fields` for structured context: `user_id`, `username`, `oauth_provider`, `group`, `age`, `max_age`.
- `Warn` for revocation events, best-effort persist failures, and IdP-unreachable-served-from-cache; `Error` for fail-closed denial after grace; `Debug` for cache hits / skipped revalidation / no-credential allow.
- Never log credential material (access/refresh tokens) or `OAuthTokenCiphertext`.

---

## 4. Edge-case table

(Consolidated; see §2.3 for the S1–S14 matrix. Repeats the key security-critical rows for the implementer.)

| Scenario | Input | Expected |
|---|---|---|
| Revoked from only allowed group | IdP userinfo/orgs no longer intersect allowlist | deactivate + token revoke + `401` on resolve/refresh |
| Deleted from IdP | GitHub `/user` 401/404; OIDC userinfo 401 or refresh `invalid_grant` | same as revoked |
| Still allowed, groups changed | new groups intersect allowlist | reconcile memberships, keep session, new `GroupIDs` |
| IdP down < grace | transport error, last-good within grace | allow from cache, `Warn` |
| IdP down > grace, fail-open=false | transport error, stale last-good | deny `401`, `Error` |
| IdP down > grace, fail-open=true | transport error, stale last-good | allow from cache, `Error` |
| No stored credential | `OAuthTokenCiphertext == ""` | allow, `Debug`; bounded by `session_max_age` |
| Refresh token > `session_max_age` | OAuth user, max age configured | reject refresh with `ErrSessionRevoked` |
| Concurrent requests, stale cache | single-flight | one IdP call; others wait then reuse result |
| Revoke write on follower | `ErrNotLeader` | deny current request; cache "revoked"; leader persists eventually |
| Non-OAuth user | `OAuthProvider == ""` | revalidation skipped |
| GitHub token invalidated | `/user` 401 | revoke + `401` |
| OIDC access token expired | `expires_at` passed | refresh via stored refresh token, persist, continue |

---

## 5. Test plan

### 5.1 Unit tests (standard `testing`, table-driven, `testLogger()`)

**New `internal/service/oauth_credential_test.go`:**
- round-trip `encryptOAuthCredential`/`decryptOAuthCredential` with a 32-byte key.
- nil credential / empty key → `("", nil)`; decrypt of `""` → `(nil, nil)`; decrypt with empty key + non-empty ct → error.

**New `internal/service/oauth_revalidator_test.go`** (use `newServiceDB(t)`, `stub*` repos, a fake `OAuthProvider`):
- `TestRevalidatorCacheHit`: second `Check` within TTL returns cached groups without calling the fake provider again (count calls).
- `TestRevalidatorRevokes`: fake returns `ErrForbidden` → `Check` error; user `DeactivatedAt != nil`; API token deleted.
- `TestRevalidatorReconcileAddRemove`: fake returns new group set → OAuth-managed memberships reconciled (add new, remove stale), admin-assigned group untouched.
- `TestRevalidatorFailClosed`: fake returns transport error, `FailOpen=false`, grace elapsed, no prior good → deny.
- `TestRevalidatorFailOpen`: same, `FailOpen=true` → allow with last-known-good.
- `TestRevalidatorGraceWindow`: transport error but last-good within grace → allow.
- `TestRevalidatorNoCredential`: `Revalidate` returns `errOAuthNoCredential` → allow, no deactivation.
- `TestRevalidatorSingleFlight`: N concurrent `Check` on stale entry → exactly one provider call.
- `TestRevalidatorDeactivatedSkipsIDP`: user already deactivated → no provider call, deny.

**Extend `internal/service/auth_service_test.go`:**
- `TestAuthResolveDeactivatedUser`: user with `DeactivatedAt` → `Resolve` returns `ErrUnauthenticated` (JWT and API token).
- `TestAuthRefreshDeactivated`: `Refresh` returns `ErrUnauthenticated`.
- `TestAuthRefreshSessionMaxAge`: refresh token with `iat` older than `SessionMaxAge` → `ErrSessionRevoked`.
- `TestAuthRefreshRevalidatesOAuth`: OAuth user + revalidator that returns `ErrForbidden` → `Refresh` denies.

**Extend `internal/service/oauth_github_test.go` / `oauth_oidc_test.go`:**
- `Complete` persists a non-empty `OAuthTokenCiphertext` (when encKey provided) and clears a prior `DeactivatedAt`.
- `Revalidate` returns current groups (success), `ErrForbidden` (allowlist fail), `ErrSessionRevoked` (401/404), `errOAuthNoCredential` (empty ciphertext).
- OIDC `NewOIDCOAuthService` auto-appends `offline_access`.

**Extend `internal/repository/fsm_test.go`:**
- `cmdUser` round-trip carries `OAuthTokenCiphertext`, `OAuthGroupIDs`, `DeactivatedAt` (upsert then read back).

**Extend `config/loader_test.go`:**
- defaults applied; `revalidate_interval <= 0` with oauth enabled → load error; `session_max_age < 0` → error.

### 5.2 Integration tests (`tests/integration/`, `freeAddr(t)`, `t.Cleanup` shutdown)

**Extend `tests/integration/oauth_oidc_test.go`** (extend the `oidcIssuer` fake to serve a `/userinfo` endpoint and return a refresh token; add a mutable `groups` field):

- `TestOIDCRevocationOnRefresh`: login succeeds (groups `["devs"]`, allowed). Flip issuer groups to `["ops"]`. Call `POST /api/v1/auth/refresh` with the refresh cookie → expect `401`. Assert the user's API token no longer resolves (`GET /api/v1/auth/me` with the `dct_` token → `401`).
- `TestOIDCRevocationOnResolve`: login; set `revalidate_interval` small (or force cache miss); flip issuer groups; call `GET /api/v1/auth/me` with the access JWT → `401`.
- Follow the existing harness exactly: `freeAddr`, `newIntegrationStore(t)`, `handler.NewServer`, `srv.Start` with `t.Cleanup` timed `Shutdown`, `noRedirect` client for the OAuth redirects.

> Port discipline (AGENTS.md): use `freeAddr(t)`; never hardcode ports; shut down via a 5s-timeout context in `t.Cleanup`.

---

## 6. Compliance checklist (AGENTS.md)

- [ ] **Dependency rule** `handler → service → domain ← repository`: new logic lives in `service` (`oauth_revalidator.go`, `oauth_credential.go`); `domain` gains only data fields + one sentinel (stdlib only, no new imports); `repository/fsm.go` persists fields; `handler` only adds one error mapping. `domain` still imports stdlib only.
- [ ] **Hertz** for HTTP: no new HTTP surface; only `writeServiceError` mapping changes.
- [ ] **logrus** logging with `logrus.Fields`; `fmt.Errorf` with `%w`; `fmt.Sprintf` for any string building (no `+` concatenation).
- [ ] **Viper**: `v.SetDefault` for all four new keys; env prefix auto-works via existing `AutomaticEnv` (`DAGGER_KUBERNETES_AUTH_OAUTH_REVALIDATE_INTERVAL`, etc.).
- [ ] **No dead symbols** (`golangci-lint unused`): every new helper/field is referenced — `errOAuthNoCredential`, `keysOf`, `revalidationState`, `oauthCredential` codec, `UserService.Update`, `SetOAuthRevalidator`, `SessionMaxAge()`. After implementing, grep each new symbol and delete orphans.
- [ ] **CI gate**: full `dagger call -m ./dagger --src . ci export --path out` when Docker is available; minimum otherwise `go build ./... && go vet ./... && go test ./...` plus `dagger call -m ./dagger --src . lint`.
- [ ] **Docs**: `config/config.app.yaml.sample`, `docs/README.md`, new ADR + `docs/design/index.md` entry — all in the same changeset.

---

## 7. Documentation updates

1. **`docs/design/ADR-027-oauth-membership-revalidation.md`** (new): context (the gap), decision (persist encrypted credential + bounded revalidation + reconciliation + revocation side effects + fail-open/grace + `session_max_age`), alternatives rejected, consequences (supersedes ADR-022 D4 additive-only rule for the OAuth-managed set; pre-upgrade users have no credential and rely on `session_max_age`), references ADR-017/ADR-022.
2. **`docs/design/index.md`**: add row for ADR-027.
3. **`docs/README.md`**:
   - "Auth mechanisms" section: describe revalidation behavior and the new config keys.
   - Config reference table: add `revalidate_interval`, `revalidate_grace`, `revalidate_fail_open`, `session_max_age` rows (near the existing `auth.oauth` rows).
   - Note the `offline_access` scope now auto-added for OIDC.

---

## 8. Implementation order (numbered)

1. **Domain**: add `User` fields + `Deactivated()`; add `ErrSessionRevoked` sentinel. (`internal/domain/user.go`, `internal/domain/identity.go`)
2. **FSM**: extend `cmdUser` + converters. (`internal/repository/fsm.go`) — run `go build ./...`.
3. **Config**: add `OAuthConfig` fields, `SetDefault`s, validation, sample. (`internal/domain/config.go`, `config/loader.go`, `config/config.app.yaml.sample`)
4. **Credential codec**: new `internal/service/oauth_credential.go`.
5. **Provider interface + tail refactor**: `oauth.go` (`completeOAuthLogin`, widen interface). This breaks compilation until step 6/7 and the test fakes are updated.
6. **GitHub provider**: encKey + `Complete` credential + `Revalidate`. (`oauth_github.go`)
7. **OIDC provider**: encKey + `offline_access` + `Complete` credential + `Revalidate` + `tokenSource`. (`oauth_oidc.go`)
8. **Revalidator + reconciliation**: new `internal/service/oauth_revalidator.go`.
9. **AuthService**: `SetOAuthRevalidator`, `loadAuthorizedGroups`, `Refresh` gate + max-age, `identityForUser` change, `issuePairForUser` group override. (`auth_service.go`)
10. **UserService.Update** passthrough. (`user_service.go`)
11. **Handler error mapping** + update fake providers in `auth_endpoints_test.go`. (`middleware.go`)
12. **Wiring** in `cmd/api/main.go`.
13. **Unit tests**: new + extended files listed in §5.1.
14. **Integration test**: extend `tests/integration/oauth_oidc_test.go`.
15. **Docs**: ADR-027, index, README, sample (already updated in step 3).
16. **Verify**:
    ```bash
    go build ./... && go vet ./... && go test ./...
    dagger call -m ./dagger --src . lint          # golangci-lint (latest)
    # full gate when Docker is available:
    dagger call -m ./dagger --src . ci export --path out
    ```
17. **Grep for dead symbols** on all touched helpers/fields (see §6).
18. **Deploy + validate** on the local cluster per `AGENTS.local.md` §6 (mandatory agent + human verification).

---

## 9. Rollout / migration notes

- **Pre-upgrade OAuth users** have `OAuthTokenCiphertext == ""`. They are allowed (S8) but, if the operator wants to bound their stale sessions immediately, set `auth.oauth.session_max_age: "24h"` (or lower) so their next refresh forces a full re-login, which then captures a credential and enables revalidation.
- **OIDC providers must issue a refresh token** for revalidation to survive access-token expiry. The service now requests `offline_access`; operators may need to enable it for the client on the IdP (Keycloak/Dex/Google). If the IdP omits `refresh_token`, revalidation still works until the access token expires, then forces re-login (S4/S9).
- **IdP outages**: with defaults (`fail_open=false`, `grace=1h`), a >1h IdP outage will deny OAuth users. Operators with high availability requirements should raise `grace` or set `fail_open=true` consciously.
- No snapshot/DB migration required (additive JSON fields on existing FSM command payloads).

## 10. Out of scope (explicit)

- SCIM/webhook-driven immediate revocation (would remove the `interval` detection delay; future enhancement).
- Extending the legacy flat-file validator or internal password users (they have no IdP; unchanged).
- SPA changes to auto-redirect on the new `401` body (the `401` status already triggers the existing re-login path; a distinct message is informational).
- Cache shared across pods (each pod revalidates independently; eventual consistency is sufficient and documented).
