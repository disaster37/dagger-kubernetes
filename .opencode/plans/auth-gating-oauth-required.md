# Plan: Auth always enforced + multi-provider OAuth (GitHub + generic OIDC)

## Goal

1. **Config rule**: `auth.internal.enabled: false` is only allowed when `auth.oauth.enabled: true` AND OAuth is actually configured for the chosen provider (`provider: github` → `client_id` + `client_secret` + `redirect_url`; `provider: oidc` → `issuer_url` + `client_id` + `client_secret` + `redirect_url`). Any other combination that would leave the platform with no usable auth provider MUST fail config loading with a clear error.
2. **Remove the no-auth / anonymous-admin dev mode entirely.** Authentication is ALWAYS enforced, including in dev. The only permitted way to "disable internal auth" is to enable OAuth as the sole auth provider. When internal auth is disabled, username/password login (`POST /api/v1/auth/login`) is also disabled (returns 404), so OAuth is truly the sole login path.
3. **Multi-provider OAuth**: support `provider: github` (unchanged behavior) and `provider: oidc` (generic OIDC via `issuer_url` discovery, covering Dex, Keycloak, Google, Auth0, etc.). A single active provider per deployment (selected by `auth.oauth.provider`); simultaneous multi-provider is out of scope.

## Decisions (resolved)

### Original auth-gating decisions (preserved)

- **Validation location**: a new `validateAuthConfig(*domain.Config) error` called from `config.Load` after `v.Unmarshal`, before returning. Fail-fast at startup, consistent with `validateRaftConfig` / `validateCacheConfig` / `validateFleetEnv`.
- **`AuthServiceConfig.Disabled`**: removed entirely (struct + field + constructor param). Auth is always enforced; the anonymous-admin resolution branch is deleted.
- **`TokenValidator.Enabled`**: removed (field + constructor param). The validator always validates against the file and fails closed. The validator is only constructed when `cfg.Auth.Internal.Enabled && cfg.Auth.Internal.TokensFile != ""`.
- **`Deps.AuthDisabled` / `Server.authDisabled`**: replaced by `Deps.InternalAuthEnabled` / `Server.internalAuthEnabled` (positive sense). It drives only `handleProviders.Internal` and the new `handleLogin` gate. No bypass.
- **`domain.AuthNone`**: removed (constant + all references). Nothing produces it after the disabled branch is gone.
- **`handleLogin` gating**: `handleLogin` returns 404 `"internal auth disabled"` when `!s.internalAuthEnabled`. `handleRefresh` stays enabled (OAuth users need rotation). `handleChangePassword` stays enabled (self-service, requires an already-authenticated identity with a password).
- **Bootstrap admin**: unchanged — still created on first boot regardless of `internal.enabled` (harmless; useful if internal is re-enabled later).

### New multi-provider OAuth decisions

- **OAuth "configured" means** (generalized): `enabled: true` AND `provider` is one of `{"github","oidc"}` AND the per-provider required fields are all non-empty:
  - `provider: github` → `client_id`, `client_secret`, `redirect_url`.
  - `provider: oidc` → `issuer_url`, `client_id`, `client_secret`, `redirect_url`.
- **`auth.oauth.provider` validation timing**: validate the provider value ONLY when `oauth.enabled: true`. When `oauth.enabled: false`, an unknown `provider` value is tolerated (so configs that set `provider` but leave oauth disabled keep loading). Rationale: avoids breaking existing configs that may set `provider: gitlab` speculatively while oauth is off; the provider only matters once oauth is turned on.
- **Provider abstraction interface location**: define `service.OAuthProvider` in `internal/service` (NOT `internal/domain`). Rationale: the interface's method signatures reference `*domain.User` and `context.Context`, and the concrete implementations depend on `*UserService`, `domain.GroupRepository`, `*JWTService`, `*logrus.Logger` — all `service`/`observ` collaborators. Per the project dependency rule (`handler → service → domain ← repository`), `handler` already imports `service`, so an interface defined in `service` and consumed by `handler` is consistent. Putting it in `domain` would force `domain` to know about `context.Context`-returning service methods and would not buy decoupling (only two implementations exist, both in `service`). The interface is a service-layer seam, not a domain port.
  ```go
  // internal/service/oauth.go (new file)
  package service
  import (
      "context"
      "github.com/disaster/dagger-kubernetes/internal/domain"
  )
  // OAuthProvider is the single active OAuth provider. Implementations:
  // GitHubOAuthService (provider: github) and OIDCOAuthService (provider: oidc).
  type OAuthProvider interface {
      LoginURL(state string) string
      Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error)
  }
  ```
  `GitHubOAuthService` already satisfies this signature (no change to its methods). `OIDCOAuthService` will satisfy it.
- **Config schema shape**: keep OIDC fields FLAT on `OAuthConfig` with a `provider` discriminator (no `oauth.oidc` sub-block). Rationale: matches the current single-block shape, keeps viper defaults simple, and avoids a nested mapstructure decode. Risk: field overlap (e.g. a future `scopes` key for github) — acceptable for now since github ignores OIDC-only fields and vice versa; document the discriminator in the struct comment.
- **Single active provider**: `Deps.OAuth` is a single `service.OAuthProvider` field (not a map). `OAuthConfig` carries one `provider` value. Simultaneous multi-provider (GitHub AND Dex at once) is OUT OF SCOPE — see "Out of scope".
- **Routing strategy**: register BOTH sets of routes unconditionally:
  - `GET /api/v1/auth/oauth/github/login` → `handleOAuthLogin` (existing)
  - `GET /api/v1/auth/oauth/github/callback` → `handleOAuthCallback` (existing)
  - `GET /api/v1/auth/oauth/oidc/login` → `handleOAuthOIDCLogin` (new, thin wrapper)
  - `GET /api/v1/auth/oauth/oidc/callback` → `handleOAuthOIDCCallback` (new, thin wrapper)
  Each handler 404s when `s.oauth == nil` OR when the configured provider does not match the route's provider. The provider-match check uses a single `s.oauthProvider` string field on `Server` (set from `cfg.Auth.OAuth.Provider` when oauth enabled, `""` when disabled). Exact 404 semantics:
  - `handleOAuthLogin` (github route): `if s.oauth == nil || s.oauthProvider != "github" { 404 "oauth not enabled" }`
  - `handleOAuthOIDCLogin` (oidc route): `if s.oauth == nil || s.oauthProvider != "oidc" { 404 "oauth not enabled" }`
  - Same for the two callback handlers.
  Rationale: 404 (not 401/409) mirrors the existing `oauth not enabled` convention and signals "this provider isn't configured here" to the SPA, which reads `/api/v1/auth/providers` to decide which button to show.
- **`providersResponse`**: add `OAuthOIDC bool` (JSON `oauth_oidc`). `handleProviders` sets `OAuthGitHub: s.oauth != nil && s.oauthProvider == "github"` and `OAuthOIDC: s.oauth != nil && s.oauthProvider == "oidc"`.
- **OIDC `user_id`**: use the ID token `sub` claim as the stable OAuth ID → `EnsureOAuthUser(ctx, "oidc", sub, username)`. `sub` is mandatory in OIDC and stable per issuer.
- **OIDC username claim**: default `username_claim: "preferred_username"`. Fallback logic (exact): if the configured claim is absent OR its value is empty string in both the ID token and userinfo, fall back to `"email"`; if `email` is also absent/empty, return an error `fmt.Errorf("oidc: no usable username claim (tried %q and %q", usernameClaim, "email")` from `Complete` (surfaced as a generic oauth error → `redirectOAuthError`).
- **OIDC groups claim**: default `groups_claim: "groups"`. Normalization: the claim may be a `[]string`/`[]any` (each stringified) OR a single string (treated as a one-element list). Absent claim → empty list. `allowed_orgs` matching: if `allowed_orgs` is non-empty, the user's normalized groups MUST intersect `allowed_orgs` (reuse `orgsIntersect`); else allow all (mirrors github `allowed_orgs` semantics exactly). `default_group` auto-join is unchanged (group name lookup, not claim-based).
- **OIDC scopes**: default `["openid","profile","email"]`. Operators may override (e.g. add `groups` for Dex). The `openid` scope is always implicitly required by go-oidc; if the configured list omits `openid`, the constructor appends it.
- **OIDC nonce**: NOT used. The existing `state` param is already a signed HS256 JWT issued by `JWTService.IssueOAuthState` (10m TTL, validated in the callback via `ParseOAuthState`). This binds the callback to the login request (CSRF/login-CSRF defense). go-oidc's `Verifier` does not require a nonce when `oidc.VerifyAudience` is used without `WithNonce`. Adding a nonce would require carrying it through the state JWT and the SPA redirect; the state JWT already provides the replay binding. Document this reasoning in `oauth_oidc.go`.
- **OIDC audience verification**: `provider.Verifier(oidc.VerifyAudience(clientID))`. The ID token `aud` MUST equal our `client_id`.
- **OIDC issuer validation / HTTPS**: go-oidc requires HTTPS issuers except for loopback (`http://127.0.0.1` / `http://localhost`). Dex in dev is typically served over http on localhost — supported. Non-loopback http issuers are rejected by go-oidc at discovery time (surfaced as a discovery error). Document this caveat in the ADR and config sample.
- **Issuer URL trailing-slash handling**: normalize by trimming trailing `/` before passing to `oidc.NewProvider` and before comparing. go-oidc is sensitive to trailing slashes in the discovery document URL.
- **`EnsureOAuthUser` provider identifiers**: exact strings `"github"` and `"oidc"`. The FSM uniqueness key is `(provider, oauthID)` (see `oauthKey` in `user_service.go` / repository), so a github user and an oidc user with the same `sub`/id CANNOT collide. Username collisions across providers are handled by the existing suffix logic in `EnsureOAuthUser` (suffix `-2`, `-3`, …): a github user "alice" and an oidc user "alice" become two distinct users (`alice` and `alice-2`), each with their own `(provider, oauthID)` key. No edge-case change needed; existing `TestOAuthCompleteUsernameCollision` covers the suffix path. Flag: this means a single human logging in via both github and oidc gets TWO user records — documented as expected behavior in the ADR.
- **OIDC testability seam**: the `OIDCOAuthService` struct holds a `providerFactory func(ctx context.Context, issuerURL string) (oidcProvider, error)` field (an unexported interface wrapping `*oidc.Provider`'s `Endpoint()` / `Verifier()` methods). Production constructor sets it to a default that calls `oidc.NewProvider`. Tests inject a fake factory that returns a fake `oidcProvider` backed by an `httptest.Server` serving `/.well-known/openid-configuration`, JWKS, token, and userinfo endpoints. This avoids needing a real HTTPS issuer in tests and lets us drive discovery/exchange/verify error paths deterministically. The `oauth2.Config` is built inside `Complete` from the provider's `Endpoint()` so the fake issuer's token endpoint is used.

## Validation rules (exact, generalized)

Implemented in `config.validateAuthConfig(cfg *domain.Config) error`, called from `Load`. Returns `nil` on success. On failure, `Load` returns `fmt.Errorf("validate auth config: %w", err)`.

| `internal.enabled` | `oauth.enabled` | `provider` | per-provider fields | Result |
|---|---|---|---|---|
| true  | false | (any)            | (any)                          | OK (default dev setup) |
| true  | true  | "github"         | client_id+secret+redirect set  | OK (both providers) |
| true  | true  | "github"         | missing                        | ERROR: `auth.oauth.enabled: true requires client_id, client_secret, and redirect_url` |
| true  | true  | "oidc"           | issuer+client_id+secret+redirect set | OK (both providers) |
| true  | true  | "oidc"           | missing issuer_url            | ERROR: `auth.oauth.enabled: true with provider "oidc" requires issuer_url, client_id, client_secret, and redirect_url` |
| true  | true  | "oidc"           | missing client_id             | ERROR (same message as above) |
| true  | true  | "gitlab"         | (any)                          | ERROR: `auth.oauth.provider: only "github" and "oidc" are supported` |
| true  | false | "gitlab"         | (any)                          | OK (provider not validated when oauth disabled) |
| false | false | (any)            | (any)                          | ERROR: `auth.internal.enabled: false requires auth.oauth.enabled: true with a fully configured provider` |
| false | true  | "github"         | client_id+secret+redirect set | OK (OAuth sole provider) |
| false | true  | "github"         | missing                        | ERROR (internal-disabled message takes precedence, then oauth-fields check) |
| false | true  | "oidc"           | issuer+client_id+secret+redirect set | OK (OAuth sole provider) |
| false | true  | "oidc"           | missing issuer_url             | ERROR (internal-disabled message takes precedence) |
| false | true  | "gitlab"         | (any)                          | ERROR: `auth.oauth.provider: only "github" and "oidc" are supported` |

Order of checks in `validateAuthConfig`:
1. If `cfg.Auth.OAuth.Enabled`:
   - switch `cfg.Auth.OAuth.Provider`:
     - `"github"`: if `ClientID == "" || ClientSecret == "" || RedirectURL == ""` → error `auth.oauth.enabled: true requires client_id, client_secret, and redirect_url`
     - `"oidc"`: if `IssuerURL == "" || ClientID == "" || ClientSecret == "" || RedirectURL == ""` → error `auth.oauth.enabled: true with provider "oidc" requires issuer_url, client_id, client_secret, and redirect_url`
     - default: error `auth.oauth.provider: only "github" and "oidc" are supported` (use `fmt.Sprintf` since it lists two literals — actually a plain literal is fine; pick the literal form)
   - (Normalize `IssuerURL` trailing slash here? No — normalization happens in the OIDC service constructor; validation only checks non-empty.)
2. If `!cfg.Auth.Internal.Enabled`:
   - if `!cfg.Auth.OAuth.Enabled` OR oauth not fully configured (per the per-provider check above) → error `auth.internal.enabled: false requires auth.oauth.enabled: true with a fully configured provider`
   - (Re-run the per-provider field check here so the message is precise; OR rely on step 1 having already produced the more specific oauth-fields error. Prefer: step 1 runs first and returns the specific oauth-fields error; step 2 only needs to check `!oauth.Enabled` and the same field-emptiness, but if step 1 already returned we won't reach step 2. So step 2's message is only hit when `oauth.enabled: false`. Simplify step 2 to: if `!cfg.Auth.OAuth.Enabled` → error `auth.internal.enabled: false requires auth.oauth.enabled: true with a fully configured provider`. The per-provider field errors are already caught by step 1 when `oauth.enabled: true`.)

(Step 1 runs first so a misconfigured OAuth is reported even when internal is also disabled; step 2 then guards the "no provider at all" case. Use `fmt.Errorf` for each message; no `%w` needed since these are leaf errors. Strings built with `fmt.Sprintf` only if interpolation is needed — these are string literals, so plain constants are fine.)

## Files to modify / create / delete

### Config layer

**`internal/domain/config.go`** (modify)
- Extend `OAuthConfig` with OIDC fields (flat, discriminator = `Provider`):
  ```go
  type OAuthConfig struct {
      Enabled      bool     `mapstructure:"enabled"`
      Provider     string   `mapstructure:"provider"`      // "github" | "oidc"
      ClientID     string   `mapstructure:"client_id"`
      ClientSecret string   `mapstructure:"client_secret"`
      RedirectURL  string   `mapstructure:"redirect_url"`
      AllowedOrgs  []string `mapstructure:"allowed_orgs"`  // github: org membership; oidc: groups claim intersection
      DefaultGroup string   `mapstructure:"default_group"` // auto-membership for new OAuth users; empty = none

      // OIDC-only fields (ignored when provider: github).
      IssuerURL    string   `mapstructure:"issuer_url"`    // required for provider: oidc
      Scopes       []string `mapstructure:"scopes"`         // default ["openid","profile","email"]
      UsernameClaim string `mapstructure:"username_claim"` // default "preferred_username"; fallback "email"
      GroupsClaim   string  `mapstructure:"groups_claim"`   // default "groups"
  }
  ```
- Add a doc comment on the struct explaining the `provider` discriminator and that github ignores the OIDC-only fields.

**`config/loader.go`** (modify)
- Add viper defaults next to the existing oauth defaults:
  ```go
  v.SetDefault("auth.oauth.issuer_url", "")
  v.SetDefault("auth.oauth.scopes", []string{"openid", "profile", "email"})
  v.SetDefault("auth.oauth.username_claim", "preferred_username")
  v.SetDefault("auth.oauth.groups_claim", "groups")
  ```
- After `v.Unmarshal(&cfg)` succeeds and before `return &cfg, nil`, call:
  ```go
  if err := validateAuthConfig(&cfg); err != nil {
      return nil, fmt.Errorf("validate auth config: %w", err)
  }
  ```
- Add new function in the same file:
  ```go
  // validateAuthConfig enforces the auth-provider gating rules:
  //   - auth is always required (no fully-disabled mode);
  //   - auth.internal.enabled: false is only allowed when OAuth is enabled and
  //     fully configured for the chosen provider (github: client_id+secret+redirect_url;
  //     oidc: issuer_url+client_id+secret+redirect_url);
  //   - when auth.oauth.enabled: true, the provider must be "github" or "oidc"
  //     and the per-provider required fields must be non-empty.
  func validateAuthConfig(cfg *domain.Config) error { ... }
  ```
- No change to existing defaults (`auth.internal.enabled` stays `true`, `auth.oauth.enabled` stays `false`, `auth.oauth.provider` stays `"github"`).

### Domain layer

**`internal/domain/identity.go`** (modify)
- Remove the `AuthNone AuthMethod = "none" // auth disabled` constant from the `const` block. Keep `AuthJWT`, `AuthAPIToken`, `AuthLegacyTok`.

### Service layer

**`internal/service/oauth.go`** (CREATE)
- Define the `OAuthProvider` interface (see Decisions). No other content.

**`internal/service/oauth_github.go`** (modify — minimal)
- No behavior change. The struct and methods already satisfy `OAuthProvider`. Optionally add a compile-time assertion `var _ OAuthProvider = (*GitHubOAuthService)(nil)` at file end.

**`internal/service/oauth_oidc.go`** (CREATE)
- Struct:
  ```go
  type OIDCOAuthService struct {
      clientID      string
      clientSecret  string
      redirectURL   string
      issuerURL     string // trailing slash trimmed
      scopes        []string
      usernameClaim string
      groupsClaim   string
      allowedOrgs   []string
      defaultGroup  string
      users         *UserService
      groups        domain.GroupRepository
      jwt           *JWTService
      logger        *logrus.Logger
      // providerFactory is the testability seam. Production: defaultFactory
      // calling oidc.NewProvider. Tests: inject a fake returning a fake
      // oidcProvider backed by an httptest.Server.
      providerFactory func(ctx context.Context, issuerURL string) (oidcProvider, error)
  }
  ```
- Unexported interface to wrap `*oidc.Provider`:
  ```go
  type oidcProvider interface {
      Endpoint() oauth2.Endpoint
      Verifier(opts *oidc.Config) *oidc.IDTokenVerifier
  }
  ```
- Constructor:
  ```go
  func NewOIDCOAuthService(cfg *domain.OAuthConfig, users *UserService, groups domain.GroupRepository, jwtSvc *JWTService, logger *logrus.Logger) *OIDCOAuthService
  ```
  - Trim trailing `/` from `cfg.IssuerURL`.
  - Copy `cfg.Scopes`; if empty or missing `openid`, append `openid`.
  - Default `usernameClaim` to `"preferred_username"` if empty; default `groupsClaim` to `"groups"` if empty.
  - Set `providerFactory` to a default that calls `oidc.NewProvider(ctx, issuerURL)` and returns it (the real `*oidc.Provider` satisfies `oidcProvider`).
- `LoginURL(state string) string`:
  - Lazily discover the provider endpoint once (cache the `oauth2.Endpoint` on first call; the factory is invoked on first `LoginURL`/`Complete`). Use a `sync.Once` + stored `endpoint` + stored error.
  - Build `oauth2.Config{ClientID, ClientSecret, Endpoint, RedirectURL, Scopes}` and return `AuthCodeURL(state)` (no PKCE — state JWT is the binding).
- `Complete(ctx, code) (access, refresh string, u *domain.User, err error)`:
  1. Discover provider via factory (same `sync.Once` as LoginURL).
  2. `oauth2.Config.Exchange(ctx, code)` → access token + (optional) ID token. If no ID token returned, fetch userinfo via `provider.UserInfo(ctx, tokenSource)`; go-oidc requires the ID token for `Verifier.Verify`, so if absent, error `fmt.Errorf("oidc: no id_token returned")`.
  3. `verifier := provider.Verifier(&oidc.Config{ClientID: clientID})` (this enables audience verification against clientID).
  4. `idToken, err := verifier.Verify(ctx, rawIDToken)` — validates signature (JWKS), issuer, audience, expiry.
  5. Extract claims: `sub` (mandatory; error if empty), username claim (with fallback to `email` per Decisions), groups claim (normalized per Decisions).
  6. `allowed_orgs` enforcement via `orgsIntersect(s.allowedOrgs, groups)` (reuse the existing helper from `oauth_github.go` — move it to a shared `oauth.go` or keep it where it is and reference it; since both files are package `service`, direct reference is fine).
  7. `u, created, err := s.users.EnsureOAuthUser(ctx, "oidc", sub, username)`.
  8. If `created && s.defaultGroup != ""` → `s.joinDefaultGroup(ctx, u.ID)` (mirror github helper; extract a shared `joinDefaultGroup` or duplicate — prefer extracting to `oauth.go`).
  9. `gids, _ := s.groups.GroupsForUser(ctx, u.ID)`; `s.jwt.IssuePair(u, groupIDs(gids))`.
  10. Return access, refresh, u, nil.
- All errors wrapped with `fmt.Errorf("...: %w", err)` per AGENTS.md.
- Doc comment on the struct explaining: nonce omitted (state JWT binds), audience = clientID, HTTPS issuer required (loopback exception), trailing-slash normalization.

**`internal/service/oauth.go`** (CREATE — extended)
- Besides the `OAuthProvider` interface, move `orgsIntersect` and `joinDefaultGroup` (as a method on a small shared helper or as free functions taking the collaborators) here so both github and oidc services reuse them. Concretely:
  - Move `orgsIntersect(allowed, have []string) bool` from `oauth_github.go` to `oauth.go` (delete from github file).
  - Add `func joinDefaultGroup(ctx context.Context, groups domain.GroupRepository, defaultGroup string, userID string, logger *logrus.Logger)` in `oauth.go`; update `GitHubOAuthService.joinDefaultGroup` to call it (or keep its method and have `OIDCOAuthService` call the free function). Prefer the free function for symmetry.
  - `groupIDs` helper already exists in the service package (used by github); reuse it.

**`internal/service/auth_service.go`** (modify — original plan)
- Delete the `AuthServiceConfig` struct entirely.
- Remove the `cfg AuthServiceConfig` parameter from `NewAuthService` and the `cfg` field from `AuthService`. New signature:
  ```go
  func NewAuthService(
      users *UserService,
      groups domain.GroupRepository,
      tokens *TokenService,
      jwtSvc *JWTService,
      legacy domain.TokenValidator,
      logger *logrus.Logger,
  ) *AuthService
  ```
- In `Resolve`, delete the `if a.cfg.Disabled { return &domain.Identity{... AuthNone ...}, nil }` branch. Resolution now always starts at `if bearer == "" { return nil, domain.ErrUnauthenticated }`. Update the doc comment to remove "1. auth disabled -> anonymous admin".

**`internal/service/auth.go`** (modify — original plan)
- Remove the `Enabled bool` field from `TokenValidator`.
- Change `NewTokenValidator(tokensFile string, enabled bool, logger *logrus.Logger)` → `NewTokenValidator(tokensFile string, logger *logrus.Logger)`.
- In `ValidateToken`, delete the `if !v.Enabled { ... return token, nil }` branch. The function now always validates. Update the doc comment.

### Handler layer

**`internal/handler/server.go`** (modify)
- In `Deps`:
  - Replace `AuthDisabled bool` with `InternalAuthEnabled bool`.
  - Replace `OAuth *service.GitHubOAuthService` with `OAuth service.OAuthProvider` (interface; nil when disabled).
  - Add `OAuthProvider string` (the configured provider string, `""` when oauth disabled; used for route 404 matching and `providersResponse`).
- In `Server`:
  - Replace `authDisabled bool` with `internalAuthEnabled bool`.
  - Replace `oauth *service.GitHubOAuthService` with `oauth service.OAuthProvider`.
  - Add `oauthProvider string`.
- In `NewServer`: update field assignments (`internalAuthEnabled: deps.InternalAuthEnabled`, `oauth: deps.OAuth`, `oauthProvider: deps.OAuthProvider`).
- In `configure()` route registration: add the two new routes after the existing github routes:
  ```go
  h.GET("/api/v1/auth/oauth/github/login", s.handleOAuthLogin)
  h.GET("/api/v1/auth/oauth/github/callback", s.handleOAuthCallback)
  h.GET("/api/v1/auth/oauth/oidc/login", s.handleOAuthOIDCLogin)
  h.GET("/api/v1/auth/oauth/oidc/callback", s.handleOAuthOIDCCallback)
  ```

**`internal/handler/auth_endpoints.go`** (modify)
- `providersResponse`: add `OAuthOIDC bool `json:"oauth_oidc"``.
- `handleProviders`:
  ```go
  c.JSON(consts.StatusOK, providersResponse{
      Internal:    s.internalAuthEnabled,
      OAuthGitHub: s.oauth != nil && s.oauthProvider == "github",
      OAuthOIDC:   s.oauth != nil && s.oauthProvider == "oidc",
  })
  ```
- `handleLogin`: delete the `if s.authDisabled { ... anonymous ... return }` block. Add at the very top (after the method receiver, before `decodeBody`):
  ```go
  if !s.internalAuthEnabled {
      writeError(c, consts.StatusNotFound, "internal auth disabled")
      return
  }
  ```
- `handleRefresh`: delete the `if s.authDisabled { ... return }` block. Refresh always calls `s.auth.Refresh`.
- `handleMe`: change `if id.Method == domain.AuthNone || id.Method == domain.AuthLegacyTok` → `if id.Method == domain.AuthLegacyTok`.
- `syntheticUserResponse`: simplify — remove the `if method == domain.AuthNone { name = "anonymous" }` branch. The function now only handles the legacy case. Update its doc comment. Keep `name := "legacy"`.
- `handleChangePassword`: change `if id.Method == domain.AuthNone || id.Method == domain.AuthLegacyTok` → `if id.Method == domain.AuthLegacyTok`.
- `handleOAuthLogin` (github route): change the gate to `if s.oauth == nil || s.oauthProvider != "github" { writeError(c, consts.StatusNotFound, "oauth not enabled"); return }`.
- `handleOAuthCallback` (github route): same gate.
- Add `handleOAuthOIDCLogin` and `handleOAuthOIDCCallback`:
  ```go
  func (s *Server) handleOAuthOIDCLogin(_ context.Context, c *app.RequestContext) {
      if s.oauth == nil || s.oauthProvider != "oidc" {
          writeError(c, consts.StatusNotFound, "oauth not enabled")
          return
      }
      // identical body to handleOAuthLogin from here (issue state, redirect)
      redirect := safeRedirectPath(c.Query("redirect"))
      state, err := s.jwt.IssueOAuthState(redirect)
      if err != nil {
          writeError(c, consts.StatusInternalServerError, "oauth state error")
          return
      }
      c.Redirect(consts.StatusFound, []byte(s.oauth.LoginURL(state)))
  }

  func (s *Server) handleOAuthOIDCCallback(_ context.Context, c *app.RequestContext) {
      if s.oauth == nil || s.oauthProvider != "oidc" {
          writeError(c, consts.StatusNotFound, "oauth not enabled")
          return
      }
      // identical body to handleOAuthCallback from here (parse state, exchange, redirect fragment)
      ... 
  }
  ```
  To avoid duplicating the ~20-line callback body, PREFER extracting a shared `s.completeOAuthCallback(c)` helper that both `handleOAuthCallback` and `handleOAuthOIDCCallback` call after their provider-match gate. Same for `handleOAuthLogin` → `s.startOAuthLogin(c)`. This keeps the provider-match gate per-route while sharing the flow.

**`internal/handler/middleware.go`** (modify — original plan)
- `attributionUserID`: change `if id == nil || id.Method == domain.AuthNone || id.Method == domain.AuthLegacyTok` → `if id == nil || id.Method == domain.AuthLegacyTok`. Update the doc comment to drop "anonymous".

**`internal/handler/tokens.go`** (modify — original plan)
- `issueMyToken`: change `if id.Method == domain.AuthNone || id.Method == domain.AuthLegacyTok` → `if id.Method == domain.AuthLegacyTok`. Update the doc comment to drop "auth-disabled anonymous".

### Wiring

**`cmd/api/main.go`** (modify)
- Legacy validator construction (around line 183-187): gate on internal enabled:
  ```go
  var legacyValidator domain.TokenValidator
  if cfg.Auth.Internal.Enabled && cfg.Auth.Internal.TokensFile != "" {
      legacyValidator = service.NewTokenValidator(cfg.Auth.Internal.TokensFile, logger)
  }
  ```
- `authSvc` construction (around line 189-191): drop the `AuthServiceConfig`:
  ```go
  authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, legacyValidator, logger)
  ```
- OAuth service construction (around line 201-204): replace the github-only block with provider selection:
  ```go
  var oauthSvc service.OAuthProvider
  var oauthProvider string
  if cfg.Auth.OAuth.Enabled {
      switch cfg.Auth.OAuth.Provider {
      case "github":
          oauthSvc = service.NewGitHubOAuthService(&cfg.Auth.OAuth, usersSvc, groupRepo, jwtSvc, logger)
          oauthProvider = "github"
      case "oidc":
          oauthSvc = service.NewOIDCOAuthService(&cfg.Auth.OAuth, usersSvc, groupRepo, jwtSvc, logger)
          oauthProvider = "oidc"
      default:
          // validateAuthConfig already rejected this, but fail closed.
          return fmt.Errorf("unsupported oauth provider: %s", cfg.Auth.OAuth.Provider)
      }
  }
  ```
- `handler.Deps` (around line 267): replace `AuthDisabled: !cfg.Auth.Internal.Enabled` with `InternalAuthEnabled: cfg.Auth.Internal.Enabled`; replace `OAuth: oauthSvc` with `OAuth: oauthSvc, OAuthProvider: oauthProvider`.

### Dependencies

**`go.mod` / `go.sum`** (modify via `go get`)
- Add `github.com/coreos/go-oidc/v3` (latest v3.x). It transitively pulls `github.com/go-jose/go-jose/v4` and depends on `golang.org/x/oauth2` and `golang.org/x/crypto` (both already present).
- Promote `golang.org/x/oauth2` from indirect to direct (it becomes a direct import of `oauth_oidc.go`).
- Exact commands:
  ```bash
  go get github.com/coreos/go-oidc/v3@latest
  go get golang.org/x/oauth2@v0.30.0
  go mod tidy
  ```
- Verify with `go mod tidy` that `go-jose/v4` is added to `go.sum` and `oauth2` moves to the `require` block (no `// indirect`).

### Tests — delete / rewrite

**`config/loader_test.go`** (modify)
- Keep `TestLoadDefaults` (still asserts `auth.internal.enabled` default `true`, `auth.oauth.provider` default `"github"`, and the new `auth.oauth.scopes` default `["openid","profile","email"]`, `username_claim` default `"preferred_username"`, `groups_claim` default `"groups"`).
- Replace the original `TestValidateAuthConfig` matrix with the generalized matrix above. Cases:
  1. internal=true,  oauth=false, provider="gitlab"            → OK (provider not validated when disabled)
  2. internal=true,  oauth=true,  provider github, fields set   → OK
  3. internal=true,  oauth=true,  provider github, fields missing → error contains `auth.oauth.enabled: true requires client_id`
  4. internal=true,  oauth=true,  provider oidc, issuer+fields set → OK
  5. internal=true,  oauth=true,  provider oidc, missing issuer_url → error contains `provider "oidc" requires issuer_url`
  6. internal=true,  oauth=true,  provider oidc, missing client_id → error contains `provider "oidc" requires issuer_url` (same message)
  7. internal=true,  oauth=true,  provider gitlab              → error contains `only "github" and "oidc" are supported`
  8. internal=false, oauth=false                              → error contains `auth.internal.enabled: false requires auth.oauth.enabled`
  9. internal=false, oauth=true,  provider github, fields missing → error contains `auth.oauth.enabled: true requires client_id` (step 1 fires first)
  10. internal=false, oauth=true, provider github, fields set  → OK
  11. internal=false, oauth=true, provider oidc, issuer+fields set → OK
  12. internal=false, oauth=true, provider oidc, missing issuer_url → error contains `provider "oidc" requires issuer_url`
  13. internal=false, oauth=true, provider gitlab              → error contains `only "github" and "oidc" are supported`
- Add `TestLoadRejectsInternalDisabledWithoutOAuth` writing a YAML file with `auth.internal.enabled: false` and asserting `Load` returns an error wrapping the validation message.

**`internal/service/oauth_github_test.go`** (modify — minimal)
- No behavior change to github service. Tests still pass. If `orgsIntersect` / `joinDefaultGroup` are moved to `oauth.go`, the github tests still compile (same package). Add a compile-time assertion test `var _ OAuthProvider = (*GitHubOAuthService)(nil)` if not added in the source file.

**`internal/service/oauth_oidc_test.go`** (CREATE)
- Build a fake OIDC issuer `httptest.Server` serving:
  - `GET /.well-known/openid-configuration` → JSON with `issuer`, `authorization_endpoint`, `token_endpoint`, `jwks_uri`, `userinfo_endpoint` all pointing at the test server.
  - `GET /jwks` → a JWKS document with a generated RSA key (use `go-jose/v4` to generate a signing key and publish its JWK).
  - `POST /token` → returns `{"access_token":"...","id_token":"<signed JWT>"}`. The JWT is signed with the test key; claims include `sub`, `aud` (== clientID), `iss` (== issuer), `exp`, and the configured `username_claim` / `groups_claim`.
  - `GET /userinfo` (optional) → returns claims for the no-id_token path test.
- Inject the fake via `OIDCOAuthService.providerFactory` (set the field directly in the test, or add an unexported `WithProviderFactory` option). The factory returns a fake `oidcProvider` whose `Endpoint()` returns the test server's endpoints and whose `Verifier()` returns a real `*oidc.IDTokenVerifier` configured with the test key's JWKS (use `oidc.NewVerifier` against a custom key set, or point the verifier at the test server's JWKS by constructing the provider via `oidc.NewProvider` against the httptest issuer URL — go-oidc supports http loopback issuers).
- Test cases:
  1. `TestOIDCCompleteSuccess`: sub="alice-sub", preferred_username="alice", groups=["devs"], allowed_orgs=["devs"] → user created with provider "oidc", OAuthID "alice-sub", username "alice".
  2. `TestOIDCCompleteIdempotent`: second call returns same user.
  3. `TestOIDCCompleteGroupsNotAllowed`: groups=["other"], allowed_orgs=["devs"] → `domain.ErrForbidden`.
  4. `TestOIDCCompleteNoAllowedOrgsRestriction`: allowed_orgs=nil → success regardless of groups.
  5. `TestOIDCCompleteUsernameClaimFallback`: preferred_username absent, email="alice@example.com" → username "alice@example.com".
  6. `TestOIDCCompleteUsernameClaimMissing`: both preferred_username and email absent → error (surfaced; assert error message contains `no usable username claim`).
  7. `TestOIDCCompleteGroupsClaimString`: groups claim is a single string "devs" (not array) → normalized to ["devs"], allowed_orgs=["devs"] → success.
  8. `TestOIDCCompleteDefaultGroupAutoJoin`: pre-create group, default_group set → user auto-joins.
  9. `TestOIDCCompleteDiscoveryFailure`: factory returns error → `Complete` returns error wrapping discovery failure.
  10. `TestOIDCCompleteTokenExchangeFailure`: token endpoint returns 500 → `Exchange` error wrapped.
  11. `TestOIDCCompleteIDTokenVerificationFailure`: id_token signed by a different key → `Verify` error wrapped.
  12. `TestOIDCCompleteIDTokenExpired`: id_token `exp` in the past → `Verify` error wrapped.
  13. `TestOIDCCompleteIDTokenWrongAudience`: id_token `aud` != clientID → `Verify` error wrapped.
  14. `TestOIDCCompleteMissingSub`: id_token has no `sub` → error.
  15. `TestOIDCLoginURL`: `LoginURL` returns the test server's authorization endpoint with `client_id`, `redirect_uri`, `scope`, `state` params; `openid` is present in scope.
  16. `TestOIDCLoginURLAppendsOpenIDScope`: cfg scopes=["profile","email"] → resulting URL scope contains "openid".
  17. `TestOIDCIssuerTrailingSlashTrimmed`: cfg issuer_url="http://localhost:xxxxx/" → constructor trims; discovery uses no trailing slash.

**`internal/service/auth_service_test.go`** (modify — original plan)
- Delete `TestAuthResolveDisabled`.
- Update `newAuthForTest` signature: drop the `cfg AuthServiceConfig` param. All callers updated.

**`internal/service/auth_test.go`** (modify — original plan)
- Delete `TestValidateTokenDisabledAcceptsAny` and `TestValidateTokenDisabledAcceptsEmpty`.
- Update `newValidator(t, tokensFile string, enabled bool)` → `newValidator(t, tokensFile string)`. Update all callers.

**`internal/handler/test_helper_test.go`** (modify — original plan + oauth)
- `newTestEnv(t, authDisabled bool)` → `newTestEnv(t *testing.T)` (drop the param; auth always enabled). Update the doc comment.
- `service.NewAuthService(service.AuthServiceConfig{Disabled: authDisabled}, ...)` → `service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, legacy, logger)`.
- `Deps`: replace `AuthDisabled: authDisabled` with `InternalAuthEnabled: true`. Add `OAuthProvider: ""` (no oauth in the default test env).
- Update all `newTestEnv(t, ...)` call sites across `internal/handler/` (grep `newTestEnv(`).

**`internal/handler/auth_test.go`** (modify — original plan)
- Delete `TestRequireAuthDisabled`.
- Keep `TestRequireAuthEnabledRejects`. Update `newTestEnv(t, false)` → `newTestEnv(t)`.
- `TestRequireAuthWithQueryFallback`: `newTestEnv(t, false)` → `newTestEnv(t)`.

**`internal/handler/auth_endpoints_test.go`** (modify — original plan + oauth)
- Delete `TestHandleLoginAuthDisabled`, `TestHandleMeAuthDisabled`, `TestMyTokenCreateAuthDisabled`, `TestHandleRefreshAuthDisabled`, `TestHandleChangePasswordAuthDisabled`.
- Update all remaining `newTestEnv(t, false)` → `newTestEnv(t)`.
- Add `TestHandleLoginInternalDisabled`: build a Server with `InternalAuthEnabled: false` and assert `POST /api/v1/auth/login` returns 404.
- Add `TestHandleProvidersInternalDisabled`: assert `providers.internal == false` when `InternalAuthEnabled: false`.
- Update existing `TestHandleProviders` (the one asserting `oauth_github == false` at line 135-136): keep it, and add assertions for `oauth_oidc == false` in the default (no-oauth) env.
- Add `TestHandleProvidersOIDC`: construct a Server with `OAuth: fakeOIDCProvider{...}` (a tiny in-test stub implementing `service.OAuthProvider` returning dummy values) and `OAuthProvider: "oidc"`, assert `oauth_oidc == true` and `oauth_github == false`.
- Add `TestHandleProvidersGitHub`: with `OAuthProvider: "github"` and a stub, assert `oauth_github == true` and `oauth_oidc == false`.
- Add `TestHandleOAuthOIDCLoginNotEnabled`: `GET /api/v1/auth/oauth/oidc/login` with `s.oauth == nil` → 404.
- Add `TestHandleOAuthOIDCLoginWrongProvider`: `s.oauth != nil` but `s.oauthProvider == "github"` → 404.
- Add `TestHandleOAuthGitHubLoginWrongProvider`: `s.oauth != nil` but `s.oauthProvider == "oidc"` → 404 (the github route 404s when oidc is the active provider).

**`internal/handler/connect_test.go`** (modify — original plan)
- Delete `TestConnectEnvAuthDisabled`.
- Update `newTestEnv(t, ...)` calls → `newTestEnv(t)`.
- Optionally add `TestConnectEnvRejectsUnauthenticated` asserting `GET /api/v1/connect/env` without a token returns 401.

**`internal/handler/server_test.go`** (modify — original plan)
- `TestHandleFleetInfoError` (lines ~416-477): rewrite to authenticate (see original plan). Drop `AuthServiceConfig{Disabled: true}` and `AuthDisabled: true`.

**`tests/integration/`** (modify — original plan; oauth references)
- Grep confirmed NO oauth references in `tests/integration/` (no `oauth_github`, `OAuthGitHub`, `GitHubOAuthService` matches there). So no oauth-specific integration test changes are required. Apply only the original-plan changes:
  - `tests/integration/api_test.go` `TestHealthEndpoint`: `service.NewAuthService(service.AuthServiceConfig{Disabled: true}, ...)` → `service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)`; `Deps`: `AuthDisabled: true` → `InternalAuthEnabled: true`.
  - `tests/integration/api_test.go` `TestProvisionEngineWithAPIToken`: drop the `AuthServiceConfig{}` arg.
  - `tests/integration/rbac_test.go` line 240: `service.NewTokenValidator(tokensPath, true, logger)` → `service.NewTokenValidator(tokensPath, logger)`.

### Documentation (same changeset as code)

**`config/config.app.yaml.sample`** (modify)
- Line 35 comment: replace `# false = auth fully disabled (dev mode).` with:
  `# false = disable username/password + legacy-token auth; ONLY allowed when auth.oauth.enabled: true with a fully configured provider. Auth is always enforced (no fully-disabled mode).`
- Replace the `oauth:` block (lines 56-64) with an expanded version:
  ```yaml
  # Optional OAuth for human/UI login. Single active provider per deployment
  # (selected by `provider`); simultaneous multi-provider is not supported.
  oauth:
    enabled: false
    provider: "github"                 # "github" | "oidc" (generic OIDC: Dex, Keycloak, Google, Auth0, ...).
    client_id: "${OAUTH_CLIENT_ID}"    # set via env, do not commit secrets.
    client_secret: "${OAUTH_CLIENT_SECRET}"
    redirect_url: "https://supv.example.com/api/v1/auth/oauth/github/callback"  # backend endpoint (302s to SPA fragment). Use /oidc/callback for provider: oidc.
    allowed_orgs: ["acme"]             # github: org membership; oidc: groups-claim intersection; empty = allow all.
    default_group: ""                  # new OAuth users auto-join this group (must exist); empty = none.
    # OIDC-only fields (provider: oidc). Ignored when provider: github.
    issuer_url: ""                     # e.g. "https://dex.example.com" or "http://localhost:5556" (loopback http allowed for dev).
    scopes: ["openid", "profile", "email"]  # "openid" is always included.
    username_claim: "preferred_username"   # fallback: "email"; error if both absent.
    groups_claim: "groups"              # claim holding group names; array or single string; absent = no groups.
  ```
- Add a one-line note near the `internal:` block: `# When auth.internal.enabled: false, OAuth must be fully configured for the chosen provider or config load fails.`

**`config/config.app.yaml`** (modify, local copy)
- Update the `oauth:` block to mirror the sample (add the four OIDC fields with their defaults, expand the `provider` comment). Keep `enabled: false` and `provider: "github"` so the file stays valid (internal enabled + oauth off = OK).

**`docs/README.md`** (modify)
- Config table (line ~342): update the `auth.internal` → `enabled` row Notes from `Static bearer-token auth.` to `Username/password + legacy-token auth. false = OAuth-only (requires auth.oauth fully configured); auth is always enforced.`
- Config table (line ~344-346): update the `auth.oauth` rows:
  - `enabled` Notes: `OAuth for UI login. Single active provider.`
  - `provider` Default: `github`; Notes: `"github" | "oidc" (generic OIDC).`
  - Add rows for `issuer_url` (default `""`, Notes `OIDC issuer; required for provider: oidc`), `scopes` (default `["openid","profile","email"]`), `username_claim` (default `preferred_username`), `groups_claim` (default `groups`).
- Auth mechanisms section (~line 639): update the GitHub OAuth bullet and add an OIDC bullet:
  - "GitHub OAuth (`auth.oauth.enabled: true` with `provider: github`) → JWT. The callback is `/api/v1/auth/oauth/github/callback`."
  - "Generic OIDC (`auth.oauth.enabled: true` with `provider: oidc`) → JWT. Discovery via `issuer_url` `/.well-known/openid-configuration`; the callback is `/api/v1/auth/oauth/oidc/callback`. `allowed_orgs` is matched against the `groups_claim` (default `groups`). Covers Dex, Keycloak, Google, Auth0, etc."
  - Add the "auth always enforced" paragraph (original plan): "Auth is always enforced — there is no fully-disabled mode. Setting `auth.internal.enabled: false` disables username/password login and legacy flat-file tokens; it requires `auth.oauth.enabled: true` with a fully configured provider, or the supervisor refuses to start. When internal auth is disabled, `POST /api/v1/auth/login` returns 404 and OAuth is the sole login path."
- Add a Dex example config block (new subsection under OAuth):
  ```yaml
  auth:
    internal:
      enabled: false
    oauth:
      enabled: true
      provider: "oidc"
      issuer_url: "https://dex.example.com"
      client_id: "dagger-cache"
      client_secret: "${OAUTH_CLIENT_SECRET}"
      redirect_url: "https://supv.example.com/api/v1/auth/oauth/oidc/callback"
      allowed_orgs: ["devs"]   # matches the `groups` claim from Dex
      default_group: ""
      scopes: ["openid", "profile", "email", "groups"]
      username_claim: "preferred_username"
      groups_claim: "groups"
  ```
- Migration/cutover section (~line 773): ensure wording does not imply a fully-disabled mode exists.

**`docs/design/ADR-013-connect-env-menu.md`** (modify — original plan)
- Section 7 "Flag (dev mode)" (lines 114-119): rewrite to state the unauthenticated dev-mode posture has been removed; auth is always enforced; `auth.internal.enabled: false` now means OAuth-only.

**`docs/design/ADR-017-auth-always-enforced-and-multi-provider-oauth.md`** (CREATE)
- New ADR documenting:
  - The decision to remove the anonymous-admin dev mode (original plan).
  - The config validation rule (generalized matrix).
  - The `handleLogin` gate.
  - The multi-provider OAuth design: `OAuthProvider` interface in `service`, single active provider, `provider: github` | `oidc`, flat config fields, OIDC via `coreos/go-oidc/v3`.
  - The "single active provider" decision and rationale (config shape, route simplicity, no simultaneous-multi-provider use case).
  - OIDC specifics: discovery, audience = clientID, nonce omitted (state JWT binds), HTTPS issuer + loopback exception, trailing-slash normalization, username claim fallback, groups claim normalization, `sub` as stable ID.
  - `EnsureOAuthUser` provider identifiers `"github"` / `"oidc"`; cross-provider same-human = two user records (expected).
  - Reference ADR-010 (multi-user RBAC), ADR-013 (connect-env dev mode).
  - Include the validation matrix.

**`docs/design/index.md`** (modify)
- Add row: `| 017  | [Auth always enforced + multi-provider OAuth](ADR-017-auth-always-enforced-and-multi-provider-oauth.md) |`.

## Error handling & logging philosophy (per AGENTS.md)

- All new errors use `fmt.Errorf` with `%w` when wrapping (`Load` wraps `validateAuthConfig`'s error as `validate auth config: %w`; OIDC service wraps discovery/exchange/verify errors). Leaf validation errors are plain `fmt.Errorf("...")` string literals (no wrap needed).
- No string concatenation with `+`; use `fmt.Sprintf` if interpolation is needed (the validation messages are literals, so none required).
- No new logging in `validateAuthConfig` (fail fast via returned error; `run` in `cmd/api/main.go` already logs fatal on `config.Load` error). Handler gates use `writeError` (existing pattern). OIDC service logs discovery/verify failures at `Warn`/`Error` via the injected `*logrus.Logger` with `WithError(err)`.

## Ordered implementation sequence

1. **Dependencies**: `go get github.com/coreos/go-oidc/v3@latest && go get golang.org/x/oauth2@v0.30.0 && go mod tidy`. Verify `go build ./...` still passes (no code change yet, just deps).
2. **Domain**: remove `AuthNone` from `internal/domain/identity.go`; extend `OAuthConfig` in `internal/domain/config.go` with the four OIDC fields. (Compiler flags `AuthNone` references.)
3. **Service**: 
   - Create `internal/service/oauth.go` with the `OAuthProvider` interface, move `orgsIntersect` and `joinDefaultGroup` (free function) there.
   - Update `internal/service/oauth_github.go` (drop the moved helpers, keep its method form for `joinDefaultGroup` delegating to the free function; add compile-time `var _ OAuthProvider = (*GitHubOAuthService)(nil)`).
   - Create `internal/service/oauth_oidc.go` (`OIDCOAuthService` + `oidcProvider` interface + default factory).
   - Update `internal/service/auth_service.go` (drop `AuthServiceConfig` + disabled branch) and `internal/service/auth.go` (drop `Enabled` field + accept-any branch).
4. **Handler**: update `server.go` (`Deps.InternalAuthEnabled`, `Deps.OAuth` interface, `Deps.OAuthProvider`, `Server` fields, new routes), `auth_endpoints.go` (remove bypasses, add `handleLogin` gate, generalize `handleProviders`, add `handleOAuthOIDCLogin`/`handleOAuthOIDCCallback` with shared helpers, fix `handleMe`/`handleChangePassword`/`syntheticUserResponse`), `middleware.go` (`attributionUserID`), `tokens.go` (`issueMyToken`).
5. **Wiring**: update `cmd/api/main.go` (legacy validator gate, `NewAuthService` call, OAuth provider switch, `Deps.InternalAuthEnabled` / `Deps.OAuth` / `Deps.OAuthProvider`).
6. **Config validation**: add `validateAuthConfig` to `config/loader.go` and call it from `Load`; add the new viper defaults for the OIDC fields.
7. **Tests**: update/delete/rewrite tests per the list above. Update `newTestEnv` and all call sites. Update `newAuthForTest` and `newValidator` helpers. Rewrite `TestHandleFleetInfoError`. Create `internal/service/oauth_oidc_test.go`. Update `providers` handler tests. Update integration tests.
8. **Docs**: update `config.app.yaml.sample`, `config.app.yaml`, `docs/README.md` (config table + auth mechanisms + Dex example), `ADR-013`, create `ADR-017`, update `docs/design/index.md`.
9. **Format & verify**: `gofmt -w .` and `goimports -w -local github.com/disaster/dagger-kubernetes .` on all changed `.go` files.

## Verification commands

```bash
go build ./...
go vet ./...
go test ./...
gofmt -l .                  # must print nothing
goimports -l -local github.com/disaster/dagger-kubernetes .  # must print nothing
```

Specifically:
- `go test ./config/...` — must pass including the generalized `TestValidateAuthConfig` matrix and `TestLoadRejectsInternalDisabledWithoutOAuth`.
- `go test ./internal/service/...` — must pass with disabled tests removed and the new `oauth_oidc_test.go` (17 cases) green.
- `go test ./internal/handler/...` — must pass with `TestHandleFleetInfoError` rewritten, `TestHandleLoginInternalDisabled`/`TestHandleProvidersInternalDisabled`/`TestHandleProvidersOIDC`/`TestHandleProvidersGitHub`/`TestHandleOAuthOIDCLoginNotEnabled`/`TestHandleOAuthOIDCLoginWrongProvider`/`TestHandleOAuthGitHubLoginWrongProvider` added.
- `go test ./tests/integration/...` — must pass with `TestHealthEndpoint` updated.
- `go mod tidy` — must leave `coreos/go-oidc/v3` and `go-jose/v4` in `go.sum`, and `golang.org/x/oauth2` in the direct `require` block.

## Edge cases & risks

### Original-plan edge cases (preserved)
- **Backward compatibility — default dev setup**: the local `config/config.app.yaml` has no `auth.internal.enabled` key (defaults `true`) and `auth.oauth.enabled: false` → valid. The docker-compose dev setup uses only `DAGGER_CACHE_*` env vars with no config file → defaults apply → valid. **No breakage to existing dev flow.**
- **Breaking change for `internal.enabled: false` users**: anyone currently relying on the no-auth dev bypass will get a startup error unless they configure OAuth. This is intended. Document prominently in `docs/README.md` and `ADR-017`.
- **`handleLogin` gate returns 404, not 401/409**: chosen to mirror `handleOAuthLogin`'s `oauth not enabled` → 404 convention. The UI reads `providers.internal` to decide whether to show the password form.
- **Legacy flat-file tokens when internal disabled**: the legacy validator is no longer constructed when `!cfg.Auth.Internal.Enabled`, so legacy tokens cannot authenticate. This closes the backdoor.
- **`TestHandleFleetInfoError` rewrite risk**: the test must authenticate to reach the 500 path. Use `newTestEnv(t)` + swap `env.server.fleetManager` for a `faultyProvider`.
- **`AuthNone` removal sweep**: grep confirmed only the listed references are affected. After removal, `go build ./...` surfaces any missed reference.
- **`NewTokenValidator` signature change**: 2 call sites + test helper. All addressed.
- **`NewAuthService` signature change**: call sites in `cmd/api/main.go`, `internal/handler/test_helper_test.go`, `tests/integration/api_test.go` (×2), `internal/service/auth_service_test.go`. All addressed.
- **Post-implementation cluster verification** (per `AGENTS.local.md` §6): after the code change passes local tests, rebuild + redeploy on the home cluster (`dagger-cache-test` release, image `docker.io/disaster/dagger-kubernetes:dev`) and verify the supervisor starts (config validates) and authenticated endpoints behave. Separate execution step, not part of this code plan.

### New OIDC edge cases & risks
- **HTTPS issuer requirement**: go-oidc requires HTTPS issuers except loopback. Dex in dev over `http://localhost:5556` works; Dex over `http://10.0.0.5` does NOT (go-oidc rejects at discovery). Document in config sample + ADR. Operators must use HTTPS (or a loopback http dev issuer).
- **Issuer URL trailing-slash**: `https://dex.example.com/` vs `https://dex.example.com` — go-oidc is sensitive. Constructor trims trailing `/`. Tests cover this (`TestOIDCIssuerTrailingSlashTrimmed`).
- **`sub` stability**: OIDC `sub` is stable per issuer and unique per issuer. We pair it with `provider: "oidc"` so two different OIDC issuers configured in different deployments don't collide (but a single deployment has one issuer, so this is moot within a deployment). Document.
- **Username claim missing → fallback → error**: defined exactly (preferred_username → email → error). Tests cover all three branches.
- **Groups claim shape**: array of strings, array of any (stringified), or single string. Normalized to `[]string`. Absent → empty list (then `allowed_orgs` non-empty → forbidden). Tests cover.
- **State/nonce replay (CSRF/login-CSRF)**: the existing JWT state (HS256, 10m TTL, validated in callback) binds the callback to the login request. No nonce added; the state JWT is the replay defense. Document in `oauth_oidc.go` and ADR.
- **OAuth callback code replay**: the authorization code is single-use at the provider; go-oidc/oauth2 `Exchange` will fail on replay. No additional defense needed.
- **Cross-provider same-human = two users**: a person logging in via github ("alice", sub 42) and via oidc ("alice", sub "alice-sub") gets two distinct user records (different `(provider, oauthID)` keys, username suffix on collision). Documented as expected in ADR.
- **`provider` validation when oauth disabled**: we deliberately do NOT reject unknown `provider` values when `oauth.enabled: false` (so speculative configs keep loading). Test case 1 covers this. Risk: an operator sets `provider: gitlab` with `oauth.enabled: false` then flips `enabled: true` and gets a startup error — acceptable (fail fast at the flip).
- **Field overlap risk on flat `OAuthConfig`**: github ignores `issuer_url`/`scopes`/`username_claim`/`groups_claim`; oidc ignores nothing extra. A future github `scopes` field would collide — document the discriminator and revisit if/when github grows scopes config.
- **go-oidc / go-jose transitive deps**: `go mod tidy` will add `github.com/go-jose/go-jose/v4` and possibly bump `golang.org/x/crypto`. Verify no version conflict with the existing `golang.org/x/crypto v0.51.0`. If conflict, pin to a compatible version and note in the ADR.
- **Fake OIDC issuer test complexity**: the test harness must serve a real JWKS and sign real JWTs. Use `go-jose/v4` to generate an RSA key, publish its JWK at `/jwks`, sign id_tokens with `jose.Signer`. The `providerFactory` seam lets us inject a provider whose `Verifier` uses this key. This is the trickiest test; allocate care there.

## Out of scope

- **Other OAuth providers as separate implementations** (GitLab, Google-as-separate-provider, Bitbucket, etc.): NOT added. Generic `provider: oidc` covers any OIDC-compliant issuer (including Google, Auth0, Keycloak, Dex). GitHub stays a dedicated `provider: github` because its non-OIDC API (orgs membership) is already implemented and tested.
- **Simultaneous multi-provider** (GitHub AND Dex active at once): OUT OF SCOPE. The config carries a single `provider`; `Deps.OAuth` is a single interface; routes are provider-scoped. Rationale: no concrete use case yet; the config shape, route set, and `providersResponse` would all need to become lists; the SPA would need a provider picker. A future ADR can revisit if needed.
- **Changing the bootstrap-admin behavior** beyond keeping it as-is.
- **Removing `AuthLegacyTok` / the legacy flat-file validator entirely** (legacy tokens remain supported when `internal.enabled: true` and `tokens_file` is set; deprecation is documented elsewhere).
- **Helm chart values changes** — the chart already sets `auth.internal.enabled: true` in production; no chart change required. (If the chart exposes `internal.enabled: false` anywhere, that's a separate chart PR; not touched here.)
- **PKCE for the OIDC flow**: not added (state JWT binds the request). Can be revisited if a provider requires it.
- **OIDC logout / single-logout (RP-initiated logout)**: not added; OAuth login only.
- **Group claim → group membership sync** (mapping OIDC groups to dagger-cache groups beyond `allowed_orgs` enforcement and `default_group` auto-join): not added. `allowed_orgs` is an allow-list filter, not a group-sync mechanism.
