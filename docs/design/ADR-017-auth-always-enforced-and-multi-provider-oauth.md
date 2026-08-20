# ADR-017: Auth always enforced + multi-provider OAuth (GitHub + generic OIDC)

- **Status:** accepted
- **Date:** 2026-08-17
- **Deciders:** dagger-kubernetes maintainers

## Context

Previously the supervisor had a dev-mode escape hatch: setting
`auth.internal.enabled: false` resolved every request to an anonymous admin
identity (`AuthNone`), leaving username/password login, refresh, `/me`, API
tokens, and the Connect-env endpoint behaving specially for the anonymous
principal. That posture was dangerous because a misconfiguration (flipping the
flag in a production-like environment) silently disabled authentication.

At the same time, human/UI login only supported GitHub OAuth. Operators using
Dex, Keycloak, Google, or Auth0 had to bridge through GitHub, which couples
login to GitHub's non-OIDC orgs API.

## Decision

### 1. Auth is always enforced (remove anonymous-admin dev mode)

The anonymous-admin resolution branch is removed entirely, together with its
supporting config (`AuthServiceConfig.Disabled`, `TokenValidator.Enabled`,
`domain.AuthNone`, `Deps.AuthDisabled`/`Server.authDisabled`). `AuthService`
always starts resolution with the empty-bearer check and fails closed; the
legacy flat-file validator always validates against the file.

The only permitted way to disable username/password login is to enable OAuth as
the sole auth provider: `auth.internal.enabled: false` requires
`auth.oauth.enabled: true` with a fully configured provider. When internal auth
is disabled, `POST /api/v1/auth/login` returns 404 `"internal auth disabled"`
(the `handleLogin` gate), so OAuth is truly the sole login path. `handleRefresh`
and `handleChangePassword` stay enabled (OAuth users need refresh-token
rotation; change-password requires an already-authenticated identity).

Bootstrap admin creation is unchanged — it still runs on first boot regardless
of `internal.enabled` (harmless, and useful if internal auth is re-enabled).

### 2. Config validation rule

A new `config.validateAuthConfig(*domain.Config) error` runs in `config.Load`
after `v.Unmarshal` and fails fast at startup, consistent with
`validateRaftConfig` / `validateCacheConfig` / `validateFleetEnv`. The rules:

| `internal.enabled` | `oauth.enabled` | `provider` | per-provider fields | Result |
|---|---|---|---|---|
| true  | false | (any)     | (any)                  | OK (default dev setup) |
| true  | true  | "github"  | client_id+secret+redirect set | OK (both providers) |
| true  | true  | "github"  | missing                | ERROR: `auth.oauth.enabled: true requires client_id, client_secret, and redirect_url` |
| true  | true  | "oidc"    | issuer+client_id+secret+redirect set | OK (both providers) |
| true  | true  | "oidc"    | missing issuer_url     | ERROR: `auth.oauth.enabled: true with provider "oidc" requires issuer_url, client_id, client_secret, and redirect_url` |
| true  | true  | "gitlab"  | (any)                  | ERROR: `auth.oauth.provider: only "github" and "oidc" are supported` |
| true  | false | "gitlab"  | (any)                  | OK (provider not validated when oauth disabled) |
| false | false | (any)     | (any)                  | ERROR: `auth.internal.enabled: false requires auth.oauth.enabled: true with a fully configured provider` |
| false | true  | "github"  | client_id+secret+redirect set | OK (OAuth sole provider) |
| false | true  | "oidc"    | issuer+client_id+secret+redirect set | OK (OAuth sole provider) |
| false | true  | "gitlab"  | (any)                  | ERROR: `auth.oauth.provider: only "github" and "oidc" are supported` |

Order: the OAuth-provider checks run first (so a misconfigured OAuth is
reported even when internal auth is also disabled), then the "no provider at
all" check. The `provider` value is only validated when `oauth.enabled: true`,
so speculative `provider` values in disabled configs keep loading.

### 3. Multi-provider OAuth

A single active OAuth provider per deployment, selected by `auth.oauth.provider`
(`"github"` | `"oidc"`). Simultaneous multi-provider (GitHub AND Dex at once) is
out of scope: there is no concrete use case, and the config shape, route set,
and `providersResponse` would all have to become lists.

- **Provider seam**: `service.OAuthProvider` is defined in `internal/service`
  (not `internal/domain`) because its method signatures reference
  `*domain.User`, `context.Context`, and the implementations depend on
  `*UserService`, `domain.GroupRepository`, `*JWTService`, and
  `*logrus.Logger`. `Deps.OAuth` is a single `service.OAuthProvider` field.
- **`GitHubOAuthService`** satisfies the interface unchanged (provider
  `github`).
- **`OIDCOAuthService`** (provider `oidc`) uses `github.com/coreos/go-oidc/v3`
  + `golang.org/x/oauth2` for discovery, token exchange, and ID-token
  verification.
- **Config shape**: OIDC fields are flat on `OAuthConfig` with a `provider`
  discriminator (`issuer_url`, `scopes`, `username_claim`, `groups_claim`);
  github ignores the OIDC-only fields and vice versa.
- **Routes** registered unconditionally; each handler 404s when `s.oauth == nil`
  or the configured provider does not match the route:
  - `GET /api/v1/auth/oauth/github/login|callback`
  - `GET /api/v1/auth/oauth/oidc/login|callback`
  `providersResponse` gains `oauth_oidc`; `handleProviders` reports
  `Internal`, `OAuthGitHub`, and `OAuthOIDC` so the SPA shows the right button.

### 3.1 Login-CSRF nonce cookie

The login flow (`startOAuthLogin`) binds the OAuth `state` token to a random
nonce in a `oauth_state` cookie (path `/api/v1/auth/oauth`, `HttpOnly`,
`SameSite=Lax`, 10m max-age), so the callback can only be completed by the
browser that initiated the login (login-CSRF, CWE-352). The cookie's `Secure`
flag is set when the request arrives over TLS, or unconditionally when the
operator sets `auth.oauth.cookie_secure: true` — required when TLS terminates
at an ingress/proxy in front of the supervisor (the backend sees plain HTTP).
The flag is never inferred from `X-Forwarded-Proto` (spoofable).

### 4. OIDC specifics

- **Discovery** via `issuer_url` `/.well-known/openid-configuration`, cached
  once with `sync.Once` (shared by `LoginURL` and `Complete`). The issuer URL
  is trailing-slash-normalized (go-oidc is sensitive to trailing slashes).
- **Audience** = our `client_id` (`oidc.Config{ClientID: clientID}`).
- **Nonce omitted**: the `state` parameter is a signed HS256 JWT
  (`JWTService.IssueOAuthState`, 10m TTL, validated in the callback via
  `ParseOAuthState`), which binds the callback to the login request
  (CSRF/login-CSRF defense). go-oidc's `Verifier` does not require a nonce
  when audience verification is used without `WithNonce`.
- **HTTPS**: go-oidc requires HTTPS issuers except loopback
  (`http://127.0.0.1` / `http://localhost`). Non-loopback http issuers are
  rejected at discovery.
- **`sub`** is the stable OAuth ID → `EnsureOAuthUser(ctx, "oidc", sub, username)`.
- **Username claim**: default `preferred_username`; fallback to `email` when
  absent/empty; error `oidc: no usable username claim` when both are missing.
- **Groups claim**: default `groups`; normalized from `[]string`/`[]any`
  (stringified) or a single string (one-element list); absent → empty list.
  `allowed_orgs` (non-empty) must intersect the normalized groups
  (`orgsIntersect`), mirroring github semantics; `default_group` auto-join is
  unchanged (group-name lookup, not claim-based).
- **Scopes**: default `["openid","profile","email"]`; `openid` is always
  appended when missing.

### 5. User identity across providers

`EnsureOAuthUser` provider identifiers are the exact strings `"github"` and
`"oidc"`; the FSM uniqueness key is `(provider, oauthID)`. A github user and an
oidc user with the same `sub`/id cannot collide, and username collisions across
providers get the existing `-2`, `-3`, … suffix. Consequence: a single human
logging in via both github and oidc gets TWO user records — documented,
expected behavior.

## Alternatives rejected

- **Keep the anonymous-admin dev mode** — too easy to misconfigure into
  production; silently disables auth.
- **A `provider` map / simultaneous multi-provider** — no use case; complicates
  config, routes, and the SPA.
- **Nested `oauth.oidc` config block** — adds mapstructure decode complexity
  for no benefit with a single active provider.
- **Put `OAuthProvider` in `domain`** — would force the domain layer to know
  about service collaborators; a service-layer seam is the right granularity.
- **PKCE for OIDC** — the state JWT already binds the request; can be revisited
  if a provider requires it.
- **Group-claim → group-membership sync** — `allowed_orgs` is an allow-list
  filter, not a sync mechanism.

## Consequences

- Existing dev setups keep working: `config.app.yaml` has no
  `auth.internal.enabled` key (defaults `true`) and `auth.oauth.enabled: false`,
  which is valid.
- Operators currently relying on `internal.enabled: false` for no-auth access
  will get a startup error until they configure OAuth — intended, and
  documented in `docs/README.md`.
- Legacy flat-file tokens cannot authenticate when internal auth is disabled
  (the validator is no longer constructed), closing the backdoor.
- Two new direct dependencies: `github.com/coreos/go-oidc/v3` and
  `golang.org/x/oauth2` (with `github.com/go-jose/go-jose/v4` transitively).

## References

- ADR-010 (multi-user RBAC), ADR-013 (Connect-env dev mode, updated here).
