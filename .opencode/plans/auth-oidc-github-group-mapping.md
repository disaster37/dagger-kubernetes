# Plan: OAuth allowed-groups + regex group mapping (GitHub teams + OIDC groups)

## Goal

1. **Helm chart** (`deploy/helm/dagger-kubernetes/`): expose complete, documented
   sample configuration for both auth providers — **GitHub OAuth** (clientID,
   clientSecret, redirectURL, optional orgs **and teams** restriction) and
   **generic OIDC** (issuer URL, clientID, clientSecret, scopes, groups claim,
   username claim, redirectURL). Today the chart renders only
   `enabled/provider/client_id/client_secret/redirect_url/allowed_orgs/default_group`
   — the OIDC fields (`issuer_url`, `scopes`, `username_claim`, `groups_claim`,
   `cookie_secure`) are **not rendered at all**, so OIDC is unusable via Helm.
   This plan closes that gap.
2. **Supervisor auth — OIDC `allowed_groups`**: add a dedicated allowed-group
   list for OIDC (a user must belong to ≥1 allowed provider group to log in),
   mirroring the existing GitHub `allowed_orgs` pattern. A similar mechanism
   already exists (`allowed_orgs`, reused for the OIDC groups claim); this plan
   extends it with a clearly-named `allowed_groups` key and adds the missing
   GitHub `allowed_teams` key.
3. **Supervisor auth — group mapping**: add regex-based mapping of upstream
   provider groups (GitHub orgs/teams, OIDC groups claim) → supervisor group
   names. Mapped groups become the user's DB memberships (the source of
   authorization decisions). Rules support capture-group substitution (`$1`).

All decisions below are final; where the repo was ambiguous I state the choice
and rationale in **Decisions**.

## Current state (verified in code)

- `internal/domain/config.go` — `OAuthConfig` already carries `AllowedOrgs`,
  `DefaultGroup`, `IssuerURL`, `Scopes`, `UsernameClaim`, `GroupsClaim`.
- `internal/service/oauth.go` — `OAuthProvider` interface (`LoginURL`,
  `Complete`), `orgsIntersect`, `joinDefaultGroup`, `completeOAuthUser`.
- `internal/service/oauth_github.go` — fetches `/user/orgs`; enforces
  `allowed_orgs` via `orgsIntersect`; no teams support.
- `internal/service/oauth_oidc.go` — `resolveGroups` normalizes the groups
  claim; enforces `allowed_orgs` against the groups claim via `orgsIntersect`.
- `internal/service/auth_service.go` — `Resolve`/`identityForUser` **re-fetches**
  `groups.GroupsForUser(userID)` on every request, so authorization (`Identity.GroupIDs`)
  always reads **DB memberships**, never JWT claims. JWT `groups` claim is
  informational only.
- `config/loader.go` — `validateAuthConfig` gates provider field requirements;
  no group-mapping validation exists.
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — renders only the
  subset of OAuth keys listed above; `statefulset.yaml` injects
  `DAGGER_KUBERNETES_AUTH_OAUTH_CLIENT_SECRET` (provider-agnostic, no change needed).

## Decisions

### D1 — Provider group collection
- **GitHub provider groups** = orgs (`/user/orgs` → `login`) **∪** teams
  (`/user/teams` → `fmt.Sprintf("%s/%s", org.Login, team.Slug)`).
- **OIDC provider groups** = the normalized `groups_claim` list (existing
  `resolveGroups`).
- Teams are fetched only when `len(allowedTeams) > 0 || mapper.Active()` (no
  extra API call for orgs-only setups). `read:org` scope is already requested.

### D2 — Allowlists (who may log in)
- `allowed_orgs` (existing, unchanged): GitHub → org-login allowlist. For OIDC
  it remains honored as a **deprecated alias** of `allowed_groups` (backward
  compatibility: existing OIDC deployments that set `allowed_orgs` keep working).
- `allowed_teams` (new, **GitHub only**): allowlist of `"org/team"` slugs.
  GitHub semantics: when both `allowed_orgs` and `allowed_teams` are non-empty,
  the user must satisfy **BOTH** (≥1 allowed org AND ≥1 allowed team). Each
  independently empty → no constraint from that dimension. Rationale: strictest,
  most predictable interpretation of "orgs and/or teams"; a team check implies
  org membership anyway.
- `allowed_groups` (new, **OIDC only**): allowlist of provider group names
  (matched against the raw groups claim, **before** mapping). Effective OIDC
  allowlist = `allowed_groups ∪ allowed_orgs` (deduplicated). If the effective
  list is non-empty, the user's groups claim must intersect it, else
  `domain.ErrForbidden`.
- **Empty allowlist = allow all** (matches existing `allowed_orgs` behavior).
- **Provider returns zero groups + non-empty allowlist → deny (403).**

### D3 — Group mapping rules
- Config key `auth.oauth.group_mappings` (list of `{pattern, replacement}`),
  applied to the **raw provider groups** to produce supervisor group **names**.
- **First-match-wins per provider group**: iterate rules in order; the first
  rule whose `pattern` matches the incoming group produces the replacement.
- **No rule matches → drop the group** (fail-closed; pass-through requires an
  explicit `.*` rule). Rationale: provider group names must not be implicitly
  trusted/kept as supervisor group names.
- **Replacement substitution**: Go `regexp.Expand` semantics — `$1`, `${1}`,
  `${name}` refer to capture groups; `$$` produces a literal `$`.
- **Case sensitivity**: case-sensitive by default (Go `regexp`); operators opt
  into case-insensitivity with the inline `(?i)` flag. No implicit lowercasing.
- **Regex engine**: Go `regexp` is RE2 (linear-time), so catastrophic
  backtracking / ReDoS is not a concern; document this.
- **Empty list = identity mapping** at the `GroupMapper.Map` level (returns
  input unchanged, de-duplicated). At the service level, however, group sync is
  gated on `mapper.Active()` (non-empty rules), so an **empty list produces no
  membership changes** — exactly today's behavior (only `default_group`
  auto-join). This preserves backward compatibility.
- **Duplicate/overlapping rules**: first match wins; duplicates are harmless.

### D4 — Membership application (what authorization uses)
- Mapped supervisor group names are looked up via `GroupRepository.GetByName`.
- **Missing group → log `Warn` + skip** (never fatal, never auto-create), the
  exact pattern used by `joinDefaultGroup` today.
- **Additive**: `addGroupMember` appends the user; existing memberships are
  **never removed** by this feature. Membership removal/reconciliation is out of
  scope (admins manage memberships via the UI).
- **Applied on every login** (not just first login), so upstream group changes
  propagate and existing OAuth users gain mapped groups on their next login.
  Footgun (documented): an admin removing a user from an auto-mapped group will
  see it restored on that user's next OAuth login.
- `default_group` auto-join is unchanged and composes with mapped groups.
- Because `AuthService.Resolve` re-fetches `GroupsForUser` per request, the
  persisted memberships flow automatically into `Identity.GroupIDs` → quota,
  project visibility, and engine-provisioning checks. **No JWT-claim changes.**

### D5 — Validation (fail-fast at startup)
- New `config.validateGroupMappings(cfg *domain.Config) error`, called from
  `config.Load` after `validateAuthConfig`. Rules:
  - every `group_mappings[i].pattern` must be non-empty and compile with
    `regexp.Compile` — else error naming `auth.oauth.group_mappings[i].pattern`
    and the offending regex;
  - every `group_mappings[i].replacement` must be non-empty — else error naming
    `auth.oauth.group_mappings[i].replacement`;
  - every `allowed_teams` entry must be non-empty and contain exactly one `/`
    (light shape check: `"org/team"`).
- The authoritative compile is `service.NewGroupMapper` (called once in
  `cmd/api/main.go`); `validateGroupMappings` is the fail-fast duplicate that
  keeps `config` free of a `service` import.

### D6 — Error handling & status codes
- Startup: `config.Load` returns `validate auth config: %w` /
  `validate group mappings: %w`; `cmd/api/main.go` `run` returns
  `load config: %w`; the CLI exits non-zero with the config path + regex in the
  message.
- Allowlist denial: `Complete` returns `domain.ErrForbidden` (unchanged sentinel).
  The OAuth callback is a **redirect** flow, so it maps to the SPA route
  `/auth/login?error=forbidden` (new distinct code) instead of an HTTP 403. All
  other OAuth failures keep `/auth/login?error=oauth`.
- Mapped-group lookup failures and best-effort team fetches: `logrus` `Warn`
  with `WithError(err)` + `WithField("group", name)`; never fatal.
- Per-request service errors already wrap with `%w`; `writeServiceError` already
  maps `ErrForbidden` → 403 for non-redirect endpoints (no change).

## Data structures (exact)

### `internal/domain/config.go` (modify)

Add to `OAuthConfig` (after `AllowedOrgs`, before `DefaultGroup`):

```go
AllowedOrgs   []string `mapstructure:"allowed_orgs"`  // github: org membership allowlist; oidc: deprecated alias for allowed_groups
AllowedTeams  []string `mapstructure:"allowed_teams"` // github only: "org/team" slug allowlist
AllowedGroups []string `mapstructure:"allowed_groups"` // oidc only: groups-claim allowlist (canonical)
GroupMappings []GroupMappingRule `mapstructure:"group_mappings"` // provider group -> supervisor group regex mapping
```

New type in the same file:

```go
// GroupMappingRule maps an upstream provider group name to a supervisor group
// name. Pattern is a Go regexp matched against the incoming group name;
// Replacement is the target supervisor group name and may reference capture
// groups via $1 / ${name} (Go regexp.Expand semantics; $$ = literal $).
type GroupMappingRule struct {
    Pattern     string `mapstructure:"pattern"`
    Replacement string `mapstructure:"replacement"`
}
```

Update the `OAuthConfig` doc comment: github ignores `AllowedGroups`/OIDC-only
fields; oidc ignores `AllowedTeams`.

### `internal/service/group_mapper.go` (create)

```go
package service

import (
    "regexp"

    "github.com/disaster/dagger-kubernetes/internal/domain"
)

// GroupMapper maps upstream provider group names to supervisor group names
// using an ordered list of regex rules. First-match-wins per group; a group
// matching no rule is dropped. Go regexp is RE2, so matching is linear-time
// and not susceptible to catastrophic backtracking (ReDoS).
type GroupMapper struct {
    rules []compiledRule
}

type compiledRule struct {
    pattern     *regexp.Regexp
    replacement string
}

// NewGroupMapper compiles the configured rules. A nil or empty list yields the
// identity mapper (Map returns its input unchanged, de-duplicated). It returns
// an error naming the offending rule when a pattern fails to compile
// (defense-in-depth; config.Load already validated them).
func NewGroupMapper(rules []domain.GroupMappingRule) (*GroupMapper, error)

// Active reports whether any mapping rules are configured. When false, the
// OAuth services skip the mapping/sync step entirely (backward compatible).
func (m *GroupMapper) Active() bool

// Map applies the ordered rules to each provider group. First-match-wins per
// group; the first rule whose pattern matches produces the replacement (with
// capture substitution). A group matching no rule is dropped. Output order is
// deterministic (input order, first match); duplicates are removed preserving
// first occurrence. With no rules it returns the input unchanged (identity).
func (m *GroupMapper) Map(providerGroups []string) []string
```

## Function signatures (exact)

### `config/loader.go` (modify)

```go
// validateGroupMappings fails fast on invalid group-mapping rules: each rule
// needs a non-empty pattern that compiles as a Go regexp, and a non-empty
// replacement. allowed_teams entries must be non-empty "org/team" strings.
// Errors name the offending config path and regex.
func validateGroupMappings(cfg *domain.Config) error
```

New `v.SetDefault` lines (next to the existing oauth defaults):

```go
v.SetDefault("auth.oauth.allowed_teams", []string{})
v.SetDefault("auth.oauth.allowed_groups", []string{})
v.SetDefault("auth.oauth.group_mappings", []domain.GroupMappingRule{})
```

Call site in `Load` (after `validateAuthConfig`):

```go
if err := validateGroupMappings(&cfg); err != nil {
    return nil, fmt.Errorf("validate group mappings: %w", err)
}
```

### `internal/service/oauth.go` (modify)

```go
// completeOAuthUser is the shared post-verification tail for both OAuth
// providers: ensure the local user exists, auto-join the default group on first
// login, add mapped supervisor groups, and issue a JWT pair.
func completeOAuthUser(ctx context.Context, users *UserService, groups domain.GroupRepository, jwt *JWTService, logger *logrus.Logger, provider, oauthID, username, defaultGroup string, mappedGroups []string) (access, refresh string, u *domain.User, err error)

// joinGroupByName best-effort adds userID to the named group. Missing groups
// and membership errors are logged (never fatal).
func joinGroupByName(ctx context.Context, groups domain.GroupRepository, name, userID string, logger *logrus.Logger)

// joinMappedGroups adds userID to each supervisor group named in mappedGroups
// that exists (never fatal, never auto-creates groups).
func joinMappedGroups(ctx context.Context, groups domain.GroupRepository, mappedGroups []string, userID string, logger *logrus.Logger)
```

`joinDefaultGroup` becomes a thin wrapper over `joinGroupByName` (kept for the
existing call path and tests).

### `internal/service/oauth_github.go` (modify)

```go
type GitHubOAuthService struct {
    // ... existing fields ...
    allowedTeams []string
    mapper       *GroupMapper
}

func NewGitHubOAuthService(cfg *domain.OAuthConfig, mapper *GroupMapper, users *UserService, groups domain.GroupRepository, jwtSvc *JWTService, logger *logrus.Logger) *GitHubOAuthService

// fetchTeams fetches the user's teams across orgs and returns "org/team" slugs.
func (s *GitHubOAuthService) fetchTeams(ctx context.Context, accessToken string) ([]string, error)
```

`Complete` new flow (after `fetchOrgs`, before `completeOAuthUser`):

```go
var teams []string
if len(s.allowedTeams) > 0 || s.mapper.Active() {
    teams, err = s.fetchTeams(ctx, accessToken)
    if err != nil {
        if len(s.allowedTeams) > 0 {
            return "", "", nil, fmt.Errorf("fetch github teams: %w", err)
        }
        s.logger.WithError(err).Warn("oauth: github teams fetch failed; mapping will use orgs only")
        teams = nil
    }
}

if len(s.allowedOrgs) > 0 && !orgsIntersect(s.allowedOrgs, orgs) {
    return "", "", nil, domain.ErrForbidden
}
if len(s.allowedTeams) > 0 && !orgsIntersect(s.allowedTeams, teams) {
    return "", "", nil, domain.ErrForbidden
}

providerGroups := make([]string, 0, len(orgs)+len(teams))
providerGroups = append(providerGroups, orgs...)
providerGroups = append(providerGroups, teams...)

var mappedGroups []string
if s.mapper.Active() {
    mappedGroups = s.mapper.Map(providerGroups)
}

access, refresh, u, err = completeOAuthUser(ctx, s.users, s.groups, s.jwt, s.logger, "github", strconv.Itoa(ghUser.ID), ghUser.Login, s.defaultGroup, mappedGroups)
```

### `internal/service/oauth_oidc.go` (modify)

```go
type OIDCOAuthService struct {
    // ... existing fields ...
    allowedGroups []string
    mapper        *GroupMapper
}

func NewOIDCOAuthService(cfg *domain.OAuthConfig, mapper *GroupMapper, users *UserService, groups domain.GroupRepository, jwtSvc *JWTService, logger *logrus.Logger) *OIDCOAuthService

// effectiveAllowedGroups returns allowed_groups ∪ allowed_orgs (allowed_orgs is
// the deprecated OIDC alias).
func (s *OIDCOAuthService) effectiveAllowedGroups() []string
```

`Complete` new flow (replaces the current `allowed_orgs` check):

```go
groups := s.resolveGroups(claims)

if allowlist := s.effectiveAllowedGroups(); len(allowlist) > 0 && !orgsIntersect(allowlist, groups) {
    return "", "", nil, domain.ErrForbidden
}

var mappedGroups []string
if s.mapper.Active() {
    mappedGroups = s.mapper.Map(groups)
}

access, refresh, u, err = completeOAuthUser(ctx, s.users, s.groups, s.jwt, s.logger, "oidc", sub, username, s.defaultGroup, mappedGroups)
```

### `internal/handler/auth_endpoints.go` (modify)

Replace `redirectOAuthError(c)` with a parameterized helper and route allowlist
denials to a distinct SPA error code:

```go
const oauthErrorRedirectBase = "/auth/login"

// redirectOAuthErrorCode sends the browser to the SPA login screen with the
// given error hint.
func redirectOAuthErrorCode(c *app.RequestContext, code string) {
    c.Redirect(consts.StatusFound, []byte(fmt.Sprintf("%s?error=%s", oauthErrorRedirectBase, code)))
}
```

In `completeOAuthCallback`:

```go
access, refresh, _, err := s.oauth.Complete(context.Background(), code)
if err != nil {
    if errors.Is(err, domain.ErrForbidden) {
        redirectOAuthErrorCode(c, "forbidden")
        return
    }
    redirectOAuthErrorCode(c, "oauth")
    return
}
```

Update the remaining `redirectOAuthError(c)` call sites to
`redirectOAuthErrorCode(c, "oauth")` and delete the old `oauthErrorRedirect`
const (or repurpose it).

### `cmd/api/main.go` (modify)

In `run`, after `groupsSvc`/`usersSvc` construction and before the OAuth switch:

```go
mapper, err := service.NewGroupMapper(cfg.Auth.OAuth.GroupMappings)
if err != nil {
    return fmt.Errorf("compile group mappings: %w", err)
}
```

Update the switch:

```go
case "github":
    oauthSvc = service.NewGitHubOAuthService(&cfg.Auth.OAuth, mapper, usersSvc, groupRepo, jwtSvc, logger)
    oauthProvider = "github"
case "oidc":
    oauthSvc = service.NewOIDCOAuthService(&cfg.Auth.OAuth, mapper, usersSvc, groupRepo, jwtSvc, logger)
    oauthProvider = "oidc"
```

## Helm values structure (complete dex-like examples)

### `deploy/helm/dagger-kubernetes/values.yaml` (modify `auth.oauth` block)

Add `@param` doc comments and values. Replace the existing `oauth:` block with:

```yaml
## @param auth.oauth.enabled Enable OAuth2 authentication.
## @param auth.oauth.provider OAuth2 provider: "github" or "oidc" (generic OIDC: Dex, Keycloak, Google, Auth0, ...).
## @param auth.oauth.clientId OAuth2 client ID.
## @param auth.oauth.clientSecret OAuth2 client secret (rendered into the <release>-oauth Secret; injected as DAGGER_KUBERNETES_AUTH_OAUTH_CLIENT_SECRET). Ignored when auth.oauth.clientSecretRef.name is set.
## @param auth.oauth.clientSecretRef.name [optional] K8s Secret name holding the OAuth2 client secret (takes precedence over auth.oauth.clientSecret; the chart-managed <release>-oauth Secret is not rendered).
## @param auth.oauth.clientSecretRef.key Key inside the Secret holding the client secret (empty = "client_secret").
## @param auth.oauth.redirectUrl OAuth2 redirect URL (empty = computed `<publicUrl>/api/v1/auth/oauth/<provider>/callback`).
## @param auth.oauth.allowedOrgs [array] Allowed OAuth organizations (github: org membership; oidc: deprecated alias for allowedGroups).
## @param auth.oauth.allowedTeams [array] (github) Allowed "org/team" slugs; when set with allowedOrgs, both must be satisfied.
## @param auth.oauth.allowedGroups [array] (oidc) Allowed provider group names (groups-claim allowlist; union with allowedOrgs).
## @param auth.oauth.groupMappings [array] Regex group mapping: list of {pattern, replacement} mapping provider groups to supervisor group names (first-match-wins; no match drops the group).
## @param auth.oauth.defaultGroup Default group for OAuth users.
## @param auth.oauth.cookieSecure Force the Secure flag on the oauth_state cookie (set true when TLS terminates in front of the supervisor).
## @param auth.oauth.issuerUrl (oidc) OIDC issuer URL (e.g. https://dex.example.com).
## @param auth.oauth.scopes [array] (oidc) OIDC scopes; "openid" is always appended.
## @param auth.oauth.usernameClaim (oidc) OIDC username claim (fallback: email).
## @param auth.oauth.groupsClaim (oidc) OIDC groups claim (array or single string).
## @param auth.cookie.accessName Access-JWT session cookie name (httpOnly).
## @param auth.cookie.refreshName Refresh-JWT session cookie name (httpOnly).
## @param auth.cookie.secure Force the Secure flag on session cookies.
## @param auth.cors.allowedOrigins [array] Exact-match Origin allowlist.
auth:
  bootstrapAdmin:
    username: "admin"
    password: ""
    secretRef:
      name: ""
      key: "password"
  jwt:
    secret: ""
    secretRef:
      name: ""
      key: "secret"
    accessTtl: "15m"
    refreshTtl: "168h"
  oauth:
    enabled: false
    provider: "github"
    clientId: ""
    clientSecret: ""
    clientSecretRef:
      name: ""
      key: "client_secret"
    redirectUrl: ""
    allowedOrgs: []
    # allowedOrgs example (GitHub org membership):
    # allowedOrgs: ["acme"]
    allowedTeams: []
    # allowedTeams example (GitHub team restriction, "org/team"):
    # allowedTeams: ["acme/eng"]
    allowedGroups: []
    # allowedGroups example (OIDC groups-claim allowlist):
    # allowedGroups: ["devs", "platform"]
    groupMappings: []
    # groupMappings example (regex → supervisor group name):
    # groupMappings:
    #   - pattern: '^github\.com/acme-(.*)$'
    #     replacement: 'acme-$1'
    #   - pattern: '^ldap/(.+)$'
    #     replacement: '$1'
    defaultGroup: ""
    cookieSecure: false
    issuerUrl: ""
    scopes: ["openid", "profile", "email"]
    usernameClaim: "preferred_username"
    groupsClaim: "groups"
  cookie:
    accessName: "dagger_kubernetes_access"
    refreshName: "dagger_kubernetes_refresh"
    secure: false
  cors:
    allowedOrigins: []
```

Add two complete commented provider examples (dex-like) immediately below the
`auth:` block as a comment block:

```yaml
# ---- GitHub OAuth example ---------------------------------------------
# auth:
#   oauth:
#     enabled: true
#     provider: "github"
#     clientId: "Ov23li...github-app-client-id"
#     clientSecretRef: { name: "github-oauth", key: "client_secret" }
#     redirectUrl: "https://supv.example.com/api/v1/auth/oauth/github/callback"
#     allowedOrgs: ["acme"]          # optional org restriction
#     allowedTeams: ["acme/eng"]     # optional team restriction (AND-ed with orgs)
#     groupMappings:
#       - pattern: '^acme$'
#         replacement: 'acme-all'
#
# ---- Generic OIDC example (Dex) ---------------------------------------
# auth:
#   internal:
#     enabled: false                  # OIDC-only login
#   oauth:
#     enabled: true
#     provider: "oidc"
#     issuerUrl: "https://dex.example.com"
#     clientId: "dagger-kubernetes"
#     clientSecretRef: { name: "dex-oauth", key: "client_secret" }
#     redirectUrl: "https://supv.example.com/api/v1/auth/oauth/oidc/callback"
#     scopes: ["openid", "profile", "email", "groups"]
#     usernameClaim: "preferred_username"
#     groupsClaim: "groups"
#     allowedGroups: ["devs"]         # optional allowlist
#     groupMappings:
#       - pattern: '^dex:dev-(.*)$'
#         replacement: 'dev-$1'
```

### `deploy/helm/dagger-kubernetes/templates/configmap.yaml` (modify)

Replace the `oauth:` block (lines 33-41) with:

```yaml
      oauth:
        enabled: {{ .Values.auth.oauth.enabled }}
        provider: {{ .Values.auth.oauth.provider | quote }}
        client_id: {{ .Values.auth.oauth.clientId | quote }}
        client_secret: ""
        redirect_url: {{ include "dagger-kubernetes.oauthRedirectUrl" . | quote }}
        allowed_orgs:
{{ toYaml .Values.auth.oauth.allowedOrgs | indent 10 }}
        allowed_teams:
{{ toYaml .Values.auth.oauth.allowedTeams | indent 10 }}
        allowed_groups:
{{ toYaml .Values.auth.oauth.allowedGroups | indent 10 }}
        group_mappings:
{{ toYaml .Values.auth.oauth.groupMappings | indent 10 }}
        default_group: {{ .Values.auth.oauth.defaultGroup | quote }}
        cookie_secure: {{ .Values.auth.oauth.cookieSecure }}
        issuer_url: {{ .Values.auth.oauth.issuerUrl | quote }}
        scopes:
{{ toYaml .Values.auth.oauth.scopes | indent 10 }}
        username_claim: {{ .Values.auth.oauth.usernameClaim | quote }}
        groups_claim: {{ .Values.auth.oauth.groupsClaim | quote }}
```

No `secret.yaml` or `statefulset.yaml` changes: `clientSecret` already covers
both providers; the new keys are not secrets.

### `deploy/helm/dagger-kubernetes/README.md` (modify)

Update the `auth.oauth.*` parameter table (lines ~632-640) with rows for
`allowedTeams`, `allowedGroups`, `groupMappings`, `cookieSecure`, `issuerUrl`,
`scopes`, `usernameClaim`, `groupsClaim` (mirroring the `@param` text above).
The `scripts/update-helm-docs.sh` script only touches version markers — edit
the parameter table by hand.

## Config file sync

### `config/config.app.yaml.sample` (modify)

Extend the `oauth:` block with the new keys + comments:

```yaml
  oauth:
    enabled: false
    provider: "github"                 # "github" | "oidc" (generic OIDC: Dex, Keycloak, Google, Auth0, ...).
    client_id: "${OAUTH_CLIENT_ID}"    # set via env, do not commit secrets.
    client_secret: ""                   # set via env (DAGGER_KUBERNETES_AUTH_OAUTH_CLIENT_SECRET) or the Helm <release>-oauth Secret — never this file.
    redirect_url: "https://supv.example.com/api/v1/auth/oauth/github/callback"  # backend endpoint (302s to SPA with session cookies). Use /oidc/callback for provider: oidc.
    allowed_orgs: []                   # github: org membership allowlist; oidc: deprecated alias for allowed_groups; empty = allow all.
    allowed_teams: []                  # github only: "org/team" slug allowlist; empty = allow all. AND-ed with allowed_orgs when both set.
    allowed_groups: []                 # oidc only: groups-claim allowlist (canonical); union with allowed_orgs; empty = allow all.
    group_mappings: []                 # regex provider-group -> supervisor-group mapping (first-match-wins; no match drops; empty = no mapping).
      # - pattern: '^github\.com/acme-(.*)$'   # Go regexp vs incoming provider group name (case-sensitive; (?i) for insensitive).
      #   replacement: 'acme-$1'               # target supervisor group name; $1/${name} capture substitution, $$ = literal $.
    default_group: ""                  # new OAuth users auto-join this group (must exist); empty = none.
    cookie_secure: false               # force the Secure flag on the oauth_state cookie; set true when TLS terminates at an ingress/proxy in front of the supervisor.
    # OIDC-only fields (provider: oidc). Ignored when provider: github.
    issuer_url: ""                     # e.g. "https://dex.example.com" or "http://localhost:5556" (loopback http allowed for dev).
    scopes: ["openid", "profile", "email"]  # "openid" is always included.
    username_claim: "preferred_username"   # fallback: "email"; error if both absent.
    groups_claim: "groups"              # claim holding group names; array or single string; absent = no groups.
```

### `config/config.app.yaml` (modify)

Mirror the sample's new keys (add `allowed_teams`, `allowed_groups`,
`group_mappings`, plus the existing OIDC fields with comments). Keep
`enabled: false` / `provider: "github"` so the file stays valid.

## Edge cases (explicit behavior)

| Case | Behavior |
|---|---|
| Bad regex in `group_mappings[i].pattern` | `config.Load` fails fast: `validate group mappings: auth.oauth.group_mappings[i].pattern ...: <regexp err>` |
| Empty `group_mappings[i].replacement` | `config.Load` fails fast: `auth.oauth.group_mappings[i].replacement must not be empty` |
| Empty `group_mappings` list | No mapping/no sync (identity at `Map` level; services skip sync via `Active()`). Backward compatible. |
| No rule matches a provider group | Group dropped (no supervisor membership from it). |
| Provider returns zero groups, allowlist non-empty | `domain.ErrForbidden` → `/auth/login?error=forbidden`. |
| Provider returns zero groups, allowlist empty | Allowed; no memberships added. |
| Missing OIDC groups claim | `resolveGroups` → nil; treated as zero groups (see above). |
| Group name case differences | Case-sensitive regex; use `(?i)` to opt into insensitivity. |
| Duplicate / overlapping rules | First match wins; later matches for the same group ignored. |
| Mapped supervisor group does not exist | Log `Warn` + skip (never create, never fatal). |
| Manual removal from an auto-mapped group | Restored on the user's next OAuth login (documented footgun). |
| `allowed_orgs` + `allowed_groups` both set (OIDC) | Union; user must be in ≥1 of either. |
| `allowed_orgs` + `allowed_teams` both set (GitHub) | AND; user must satisfy both. |
| Old `allowed_orgs`-only configs | Keep working for both providers (OIDC alias preserved). |
| Teams fetch fails, `allowed_teams` non-empty | Login fails (`fetch github teams: %w`). |
| Teams fetch fails, only mapping needs teams | Log `Warn`, continue with orgs only. |

## Error handling & logging

- `config.Load` wraps validation errors: `validate auth config: %w` /
  `validate group mappings: %w`; `run` wraps again: `load config: %w`. Messages
  name the full config path + offending regex.
- OAuth `Complete` errors wrap with `%w` (`fetch github teams`, `exchange code`,
  `verify id token`, etc.).
- `logrus` structured fields for skips/failures:
  `logger.WithError(err).WithField("group", name).Warn("oauth: mapped group not found, skipping")`,
  `logger.WithError(err).Warn("oauth: github teams fetch failed; mapping will use orgs only")`.
- Allowlist denial is a 302 redirect (not an HTTP status); all other endpoints
  map `ErrForbidden` → 403 via the existing `writeServiceError`.

## Testing strategy

### `internal/service/group_mapper_test.go` (create, table-driven, stdlib `testing`)

`NewGroupMapper` + `Map` cases:
- empty/nil rules → identity (input returned de-duplicated, order preserved);
- first-match-wins (two overlapping rules → only first applies);
- no match → dropped;
- capture substitution `$1`, `${name}`;
- literal `$` via `$$`;
- case sensitivity: `^Acme$` does not match `acme`; `(?i)^acme$` does;
- anchored vs unanchored: `^org$` vs `org` (substring match);
- de-duplication preserving first occurrence;
- deterministic ordering (input order);
- `NewGroupMapper` returns error for `[` (bad regex) and for empty pattern;
- linear-time sanity: a large input (e.g. 100k-char group name) against an
  anchored pattern completes (documents RE2/no-ReDoS).

### `config/loader_test.go` (modify)

- Extend `TestLoadDefaults` for the new defaults (`allowed_teams`/`allowed_groups`/
  `group_mappings` empty, `scopes` default, `username_claim`/`groups_claim` defaults).
- Add `TestValidateGroupMappings` table: valid rules pass; bad pattern, empty
  pattern, empty replacement, malformed `allowed_teams` entry each fail with the
  expected message substring.

### `internal/service/oauth_github_test.go` (modify)

- Add `/user/teams` handler to `newGitHubServer` (accept a `teams []string`
  param of `"org/team"` slugs).
- Update `newOAuthService` to build a `GroupMapper` from `cfg.GroupMappings`.
- New tests: `TestOAuthCompleteAllowedTeams` (pass/deny), `TestOAuthCompleteOrgsAndTeamsBoth`,
  `TestOAuthCompleteGroupMapping` (org/team → mapped supervisor group
  membership asserted via `GroupsForUser`), `TestOAuthCompleteGroupMappingMissingGroup`
  (mapped group absent → no membership, still logged in), `TestOAuthCompleteNoGroupMappingsNoSync`
  (no rules → no auto-membership), `TestOAuthCompleteTeamsFetchFailureBestEffort`.

### `internal/service/oauth_oidc_test.go` (modify)

- Update `newOIDCService` to build a `GroupMapper`.
- New tests: `TestOIDCCompleteAllowedGroups` (pass/deny via `AllowedGroups`),
  `TestOIDCCompleteAllowedGroupsUnionOrgs` (union semantics),
  `TestOIDCCompleteGroupMapping` (groups claim → mapped membership),
  `TestOIDCCompleteNoGroupMappingsNoSync`.
- Existing `AllowedOrgs`-based tests must still pass (backward compat).

### `internal/handler/auth_endpoints_test.go` (modify)

- Add `TestOAuthCallbackForbidden`: stub `service.OAuthProvider` whose `Complete`
  returns `domain.ErrForbidden`; assert the response is a 302 to
  `/auth/login?error=forbidden`. Keep existing oauth-error redirect tests.

### Integration (`tests/integration/oauth_oidc_test.go`, optional but recommended)

- Spin a loopback `httptest` OIDC issuer (discovery + JWKS + token + userinfo),
  start a supervisor pointed at it via a temp config file, drive
  `GET /api/v1/auth/oauth/oidc/login` (assert authorize redirect), then assert
  `allowed_groups` denial by completing a login for a user outside the allowlist
  and expecting a redirect to `/auth/login?error=forbidden`. No real Dagger
  client contract is involved (auth feature); this is a black-box OIDC flow test.

## Sequencing (dependency order)

1. **Domain** — `internal/domain/config.go`: add `AllowedTeams`, `AllowedGroups`,
   `GroupMappings`, `GroupMappingRule`.
2. **Config** — `config/loader.go`: defaults + `validateGroupMappings` + call;
   `config/loader_test.go` updates.
3. **Service** — create `internal/service/group_mapper.go` (+ test); modify
   `oauth.go`, `oauth_github.go`, `oauth_oidc.go` (+ their tests).
4. **Handler** — `internal/handler/auth_endpoints.go` (+ test) for the forbidden
   redirect code.
5. **Wiring** — `cmd/api/main.go`: build `GroupMapper`, pass to both OAuth
   constructors.
6. **Helm** — `values.yaml` (keys + `@param` + examples), `configmap.yaml`,
   `deploy/helm/dagger-kubernetes/README.md` parameter table.
7. **Docs** — `config/config.app.yaml.sample`, `config/config.app.yaml`,
   `docs/README.md` (config table + auth mechanisms + provider examples),
   `docs/design/ADR-022-oauth-group-allowlists-and-regex-mapping.md` (create),
   `docs/design/index.md` (add ADR-022 row).
8. **Format & verify** — `gofmt -w .`,
   `goimports -w -local github.com/disaster/dagger-kubernetes .` on changed
   `.go` files; `go build ./...`, `go vet ./...`, `go test ./...`,
   `gofmt -l .`, `goimports -l ...` (all empty).

## Files to create / modify (full paths)

Create:
- `internal/service/group_mapper.go`
- `internal/service/group_mapper_test.go`
- `docs/design/ADR-022-oauth-group-allowlists-and-regex-mapping.md`

Modify:
- `internal/domain/config.go`
- `config/loader.go`
- `config/loader_test.go`
- `internal/service/oauth.go`
- `internal/service/oauth_github.go`
- `internal/service/oauth_oidc.go`
- `internal/service/oauth_github_test.go`
- `internal/service/oauth_oidc_test.go`
- `internal/handler/auth_endpoints.go`
- `internal/handler/auth_endpoints_test.go`
- `cmd/api/main.go`
- `config/config.app.yaml.sample`
- `config/config.app.yaml`
- `docs/README.md`
- `docs/design/index.md`
- `deploy/helm/dagger-kubernetes/values.yaml`
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml`
- `deploy/helm/dagger-kubernetes/README.md`

Optional:
- `tests/integration/oauth_oidc_test.go`

## ADR-022 content (outline)

Status accepted; documents: D1 provider-group model (GitHub orgs+teams, OIDC
groups claim); D2 allowlists (`allowed_teams` AND semantics, `allowed_groups`
canonical + `allowed_orgs` alias); D3 mapping semantics (first-match-wins,
no-match-drop, `$1` substitution, case-sensitivity, RE2/no-ReDoS, empty=identity);
D4 additive membership on every login + "mapped groups are DB memberships,
authorization re-fetches from DB" (no JWT change); D5 startup validation; D6
error mapping. Reference ADR-017 (multi-provider OAuth).

## Verification commands

```bash
go build ./...
go vet ./...
go test ./...
gofmt -l .
goimports -l -local github.com/disaster/dagger-kubernetes .
helm template deploy/helm/dagger-kubernetes -f <(echo 'auth:{oauth:{enabled:true,provider:oidc,issuerUrl:"https://dex.example.com",allowedGroups:["devs"],groupMappings:[{pattern:"^dex:(.*)$",replacement:"$1"}]}}')
```

Specifically verify:
- `go test ./config/...` — new defaults + `TestValidateGroupMappings` green.
- `go test ./internal/service/...` — `group_mapper_test.go` + updated oauth
  tests green (100% coverage target for `group_mapper.go`).
- `go test ./internal/handler/...` — forbidden-redirect test green.
- `helm template` renders `allowed_teams`, `allowed_groups`, `group_mappings`,
  `issuer_url`, `scopes`, `username_claim`, `groups_claim`, `cookie_secure`
  into the configmap without templating errors.

## Post-implementation (per AGENTS.local.md mandate)

Redeploy to the "home" cluster (§4 build → push → capture values → helm upgrade
→ rollout restart) and run §5.1 agent checks; request §5.2 human verification.
Exercise OAuth login with the new allowlists/mapping if a provider is available
on that cluster; otherwise verify the configmap renders the new keys and the
supervisor still boots with defaults (backward compatible).
