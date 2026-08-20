# Plan: Security Follow-ups (post dagger-cache→dagger-kubernetes rename)

Self-contained implementation plan for the 9 code-review/security-audit findings.
The implementer executes this verbatim with zero prior context. Research citations
(`file:line`) are from the working tree at planning time.

## Global context (read first)

- Module: `github.com/disaster/dagger-kubernetes`. Go + Hertz + Viper + logrus + Vue3 SPA.
- Config loader (`config/loader.go:15-185`): `viper.New()`, `SetEnvPrefix("DAGGER_KUBERNETES")`,
  `SetEnvKeyReplacer(strings.NewReplacer(".", "_"))`, `AutomaticEnv()`. Viper uppercases the
  composed env key, so `auth.jwt.secret` → `DAGGER_KUBERNETES_AUTH_JWT_SECRET`. **AutomaticEnv
  does NOT bind slice/array indices** — `cache.registries[0].password` cannot be set via env.
  Missing config file is skipped (`ConfigFileNotFoundError`, lines 164-169).
- Dependency rule (AGENTS.md): `handler → service → domain ← repository`; `domain` stdlib-only;
  `observ` cross-cutting. 100% test coverage, stdlib `testing` only, table-driven.
- B-whitelist (DO NOT RENAME, from `.opencode/plans/dagger-cache-to-dagger-kubernetes-rename.md:535-547`):
  the OCI repo path segment `dagger-cache` in cache refs, `_EXPERIMENTAL_DAGGER_CACHE_CONFIG`,
  `Cache`/`CacheConfig`/`CacheStats`/`RegistryBackend` types, `cache.*` config keys,
  `CACHE_REGISTRY` shell var, `DAGGER_CLOUD_*`/`DAGGER_TAG` env vars. These are intentionally
  KEPT — do not "fix" them.
- Live cluster redeploy mandate: AGENTS.local.md §6. Release name `dagger-kubernetes-test`,
  namespace `dagger-kubernetes-test`, image `docker.io/disaster/dagger-kubernetes:dev`,
  kubeconfig `/home/user/.kube/home` (context `home`). See "Cluster redeploy" at the end.
- Bearer-token auth for CI (`Authorization: Bearer dct_...` / legacy tokens / JWTs) MUST keep
  working. Cookie auth is ADDITIVE.

---

## Finding 1 [HIGH] — Plaintext secrets in the rendered ConfigMap

### Current behavior
`deploy/helm/dagger-kubernetes/templates/configmap.yaml` renders these sensitive values into a
plain ConfigMap:
- `auth.jwt.secret` (line 24): `password: {{ .Values.supervisor.config.auth.jwt.secret | quote }}` — wait, line 24 is `secret: {{ .Values.supervisor.config.auth.jwt.secret | quote }}`.
- `auth.oauth.client_secret` (line 32): `client_secret: {{ .clientSecret | quote }}`.
- `cache.auth_token` (line 84): `auth_token: {{ .Values.supervisor.config.cache.authToken | quote }}`.
- `cache.registries[].password` (line 92): `password: {{ .password | quote }}`.
- `auth.bootstrap_admin.password` (line 22): `password: {{ .Values.supervisor.config.auth.bootstrapAdmin.password | quote }}`.

Parallel chart-managed Secrets already exist (`templates/secret.yaml`): `<release>-tokens`,
`<release>-admin-password`, `<release>-jwt`, `engine-registry-auth`, `<release>-minting-ca`,
`<release>-tls`. The statefulset (`templates/statefulset.yaml:71-77`) injects ONLY
`DAGGER_KUBERNETES_AUTH_BOOTSTRAP_ADMIN_PASSWORD` from `<release>-admin-password`. The
`<release>-jwt` and `engine-registry-auth` Secrets are created but **not wired to the supervisor
via env** — `auth.jwtSecret` and `auth.engineToken` values are currently dead (created, unused).
`auth.oauth.clientSecret` has no Secret at all; `values.yaml:189` sets it to the literal
placeholder string `"${OAUTH_CLIENT_SECRET}"`.

The supervisor already reads `cache.auth_token` from the `engine-registry-auth` Secret at startup
when `cache.auth_token` is empty (`cmd/api/main.go:252-258`, `loadCacheTokenFromSecret` lines
1155-1174) — this is the precedent for K8s-Secret resolution.

### Target design
1. **Scalars → env vars from existing/new Secrets** (Viper `AutomaticEnv` binds them; env wins
   over the ConfigMap file):
   - `auth.jwt.secret` → env `DAGGER_KUBERNETES_AUTH_JWT_SECRET` from `<release>-jwt` key `secret`.
   - `auth.oauth.client_secret` → env `DAGGER_KUBERNETES_AUTH_OAUTH_CLIENT_SECRET` from a NEW
     Secret `<release>-oauth` key `client_secret`.
   - `auth.bootstrap_admin.password` → already wired (`DAGGER_KUBERNETES_AUTH_BOOTSTRAP_ADMIN_PASSWORD`).
   - `cache.auth_token` → leave empty in ConfigMap; supervisor already falls back to
     `engine-registry-auth` Secret. No env var needed (the fallback is K8s-Secret-based).
2. **`cache.registries[].password` (slice — env cannot bind)** → add an optional
   `password_secret` reference field to `RegistryBackend`; the supervisor resolves the password
   from the named K8s Secret at startup (mirrors `loadCacheTokenFromSecret`). The ConfigMap
   renders `password: ""` + `password_secret: {name, key}` (a reference is not a secret).
3. **Remove all five sensitive lines from `configmap.yaml`** (replace with empty/placeholder or
   omit). Keep non-sensitive fields (`client_id`, `bootstrap_admin.username`, `registries[].id/
   internal_addr/username`).
4. **Backwards compatibility**: keep the `supervisor.config.auth.jwt.secret` /
   `supervisor.config.cache.authToken` / `supervisor.config.auth.oauth.clientSecret` /
   `supervisor.config.auth.bootstrapAdmin.password` values keys (operators may still set them for
   non-Helm deploys), but the chart no longer renders them into the ConfigMap. The `auth.*`
   Secret-populating values (`auth.jwtSecret`, `auth.adminPassword`, `auth.engineToken`,
   `auth.oauthClientSecret`) are the Helm-recommended path. No `existingSecret` toggle is added
   in this change — chart-managed Secrets are reused in place across `helm upgrade` (Helm updates
   them), which satisfies "existing-secret reuse across helm upgrades". (Open decision D1.)

### Exact file changes

**`internal/domain/config.go`** — add `PasswordSecret` to `RegistryBackend` (lines 147-152):
```go
type RegistryBackend struct {
    ID            string          `mapstructure:"id"`
    InternalAddr  string          `mapstructure:"internal_addr"`
    Username      string          `mapstructure:"username"`
    Password      string          `mapstructure:"password"`
    PasswordSecret *SecretRef     `mapstructure:"password_secret"` // NEW: K8s Secret ref; resolves Password when empty
}
// SecretRef names one key of a K8s Secret in the fleet namespace.
type SecretRef struct {
    Name string `mapstructure:"name"`
    Key  string `mapstructure:"key"`
}
```

**`config/loader.go`** — no new default needed for `password_secret` (zero value = disabled).
Existing `cache.registries` default `[]domain.RegistryBackend{}` already covers it.

**`cmd/api/main.go`** — in `validateCacheConfig` (lines 1102-1120), after building `backends`,
resolve `password_secret` refs into `Password` via the K8s clientset. Add a helper:
```go
// resolveRegistryBackendSecrets fills Password from each backend's password_secret ref
// (mirrors loadCacheTokenFromSecret). A missing clientset/secret leaves Password empty
// (non-K8s deployments must set Password directly in config).
func resolveRegistryBackendSecrets(ctx context.Context, clientset kubernetes.Interface, namespace string, backends []domain.RegistryBackend, logger *logrus.Logger) error
```
Call it after `validateCacheConfig` returns (around line 109), before building `cacheBackend`/
`router`. Errors: log WARN (best-effort, like `loadCacheTokenFromSecret`), do not fail startup
(a missing secret yields empty password — the backend will 401, which is observable). Wrap
errors with `%w`. Log fields: `backend_id`, `secret_name`.

**`deploy/helm/dagger-kubernetes/templates/configmap.yaml`** — remove/blank the sensitive lines:
- Line 22: `password: {{ .Values.supervisor.config.auth.bootstrapAdmin.password | quote }}` →
  delete the line (env var supplies it). Keep `username:` line 21.
- Line 24: `secret: {{ .Values.supervisor.config.auth.jwt.secret | quote }}` → `secret: ""`
  (env var supplies it; keep the key so the YAML shape is stable).
- Line 32: `client_secret: {{ .clientSecret | quote }}` → `client_secret: ""` (env var supplies it).
- Line 84: `auth_token: {{ .Values.supervisor.config.cache.authToken | quote }}` → `auth_token: ""`
  (supervisor reads `engine-registry-auth` Secret).
- Lines 86-94 (registries block): change `password: {{ .password | quote }}` → render
  `password: ""` plus `password_secret:` from a new `passwordSecret` field:
  ```yaml
  {{- range .Values.supervisor.config.cache.registries }}
        - id: {{ .id | quote }}
          internal_addr: {{ .internalAddr | quote }}
          username: {{ .username | quote }}
          password: ""
          {{- with .passwordSecret }}
          password_secret:
            name: {{ .name | quote }}
            key: {{ .key | quote }}
          {{- end }}
  {{- end }}
  ```

**`deploy/helm/dagger-kubernetes/templates/secret.yaml`** — add an OAuth Secret block (after the
jwt block, lines 29-41):
```yaml
{{- if .Values.auth.oauthClientSecret }}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "dagger-kubernetes.fullname" . }}-oauth
  namespace: {{ include "dagger-kubernetes.namespace" . }}
  labels:
    {{- include "dagger-kubernetes.labels" . | nindent 4 }}
type: Opaque
stringData:
  client_secret: {{ .Values.auth.oauthClientSecret | quote }}
{{- end }}
```

**`deploy/helm/dagger-kubernetes/templates/statefulset.yaml`** — add env-var injections from the
Secrets (in the `env:` block, after the existing `DAGGER_KUBERNETES_AUTH_BOOTSTRAP_ADMIN_PASSWORD`
block at lines 71-77):
```yaml
{{- if .Values.auth.jwtSecret }}
- name: DAGGER_KUBERNETES_AUTH_JWT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "dagger-kubernetes.fullname" . }}-jwt
      key: secret
{{- end }}
{{- if .Values.auth.oauthClientSecret }}
- name: DAGGER_KUBERNETES_AUTH_OAUTH_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "dagger-kubernetes.fullname" . }}-oauth
      key: client_secret
{{- end }}
```
Note: `cache.auth_token` needs no env (K8s-Secret fallback). `auth.bootstrap_admin.password`
already wired. Add a pod annotation `checksum/secret-jwt` and `checksum/secret-oauth` so Secret
changes roll the pod (the existing `checksum/config` only covers the ConfigMap). Use:
`checksum/secret-jwt: {{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}`
— a single secret checksum is sufficient (any Secret change rolls the pod).

**`deploy/helm/dagger-kubernetes/values.yaml`** — add `auth.oauthClientSecret` (lines 323-328):
```yaml
auth:
  tokens: []
  adminPassword: ""
  engineToken: ""
  engineDockerConfig: ""
  jwtSecret: ""
  oauthClientSecret: ""   # NEW: OAuth2 client secret (rendered into <release>-oauth Secret)
```
And change `supervisor.config.auth.oauth.clientSecret` (line 189) from `"${OAUTH_CLIENT_SECRET}"`
to `""` (the placeholder string was never functional; the Secret path is now the documented one).
Add `passwordSecret` support to `supervisor.config.cache.registries` schema doc (line 207): add a
`## @param supervisor.config.cache.registries[].passwordSecret.name` and `.key` doc line.

**`deploy/k8s/namespace-rbac.yaml`** — the standalone ConfigMap (lines 81-124) renders
`cache.auth_token: "change-me"` (line 101). Replace with `auth_token: ""` and add a comment that
the token comes from the `engine-registry-auth` Secret. (No `auth.jwt.secret`/`oauth` rendered
here, so no other change for #1 in this file.)

**`config/config.app.yaml.sample`** — ensure `auth.jwt.secret`, `auth.oauth.client_secret`,
`cache.auth_token`, `cache.registries[].password` are documented as "set via env/Secret in
production, not this file" with empty defaults. Add `password_secret` example under
`cache.registries`. Keep in sync with `config/loader.go` (AGENTS.md doc rule).

**`docs/README.md`** — update the "Environment variables" section (lines 310-333) to list the new
secret env vars (`DAGGER_KUBERNETES_AUTH_JWT_SECRET`, `DAGGER_KUBERNETES_AUTH_OAUTH_CLIENT_SECRET`,
`DAGGER_KUBERNETES_AUTH_BOOTSTRAP_ADMIN_PASSWORD`) and the `cache.registries[].password_secret`
mechanism. Update the Security notes (lines 912-935) to state secrets are mounted via K8s
Secrets + env, never the ConfigMap.

### Edge cases
- **Viper env precedence**: `AutomaticEnv` wins over the config file, so an empty ConfigMap
  value + a set env var → env value is used. Verified by the existing
  `DAGGER_KUBERNETES_AUTH_BOOTSTRAP_ADMIN_PASSWORD` wiring.
- **Slice password via env**: impossible (Viper limitation) → `password_secret` ref is the
  documented mechanism for multi-backend. Single-backend mode uses `engine-registry-auth`
  (already a Secret).
- **Non-K8s deploys (docker-compose)**: no clientset → `resolveRegistryBackendSecrets` logs WARN
  and leaves `Password` empty; operator must set `cache.registries[].password` directly in the
  config file (documented). This preserves the docker-compose path.
- **Secret rotation**: chart-managed Secrets update in place on `helm upgrade`, but the pod needs
  a restart to re-read env vars. The new `checksum/secret-*` annotation forces that. Document.

---

## Finding 2 [WARNING] — Remove tracked stale binary `api`

### Current behavior
`/projects/dagger-cache/api` is a ~66-69 MB tracked binary (old `dagger-cache` build, not
referenced anywhere). `.gitignore` ignores `/supervisor`, `/dagger-kubernetes-ci`, `bin/`, `out/`
but NOT `/api`.

### Target / exact changes
- `git rm --cached api` (untrack) then `rm api` (delete the file).
- `.gitignore` — add after line 50 (`/dagger-kubernetes-ci`):
  ```
  /api
  ```
- Verify no source references `./api`: `grep -rn '"api"\|/api\b' --include=*.go --include=*.sh --include=*.yaml .` (the API path `/api/v1/...` is unrelated; the binary `api` is not). The `cmd/api/` Go package is the source, unaffected.

### Validation
`git status` shows `api` deleted from index; `git ls-files | grep '^api$'` empty; build unaffected.

---

## Finding 3 [MEDIUM] — Supervisor RBAC scope

### Current behavior
- **Helm chart** (`templates/rbac.yaml`): ALREADY namespaced. A `clusterScope` toggle
  (values.yaml line 89, default `false`) selects Role/RoleBinding vs ClusterRole/ClusterRoleBinding.
  Secrets verbs: `get,list,watch,create,update,patch` (no `delete` — already hardened, lines 46-52).
  The default is the secure namespaced Role. **No chart change needed for #3.**
- **Standalone** (`deploy/k8s/namespace-rbac.yaml`): ClusterRole/ClusterRoleBinding (lines 12-38)
  with full CRUD including `delete` on secrets (line 25). This is the over-broad one.

### Code research
`internal/repository/k8s_provider.go` operates ONLY on `p.cfg.Namespace` (single namespace) for
StatefulSets, Services, Pods, PVCs, ConfigMaps, Secrets (engine client certs). No cross-namespace,
no node, no Ingress operations. The fleet namespace = the release namespace (configmap.yaml line
108). **Single-namespace Role is sufficient.**

### Target / exact changes
**`deploy/k8s/namespace-rbac.yaml`** — replace the ClusterRole/ClusterRoleBinding (lines 12-38)
with a namespaced Role/RoleBinding scoped to `dagger-kubernetes`, and drop `delete` from secrets
(to match the Helm chart hardening):
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: supervisor-role
  namespace: dagger-kubernetes
rules:
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["services", "pods", "pods/log"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["get", "create"]
  - apiGroups: [""]
    resources: ["persistentvolumeclaims", "configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: supervisor-binding
  namespace: dagger-kubernetes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: supervisor-role
subjects:
  - kind: ServiceAccount
    name: supervisor
    namespace: dagger-kubernetes
```
(Keep the Namespace, ServiceAccount, and the Secret/ConfigMap objects below line 40 unchanged.)

**Helm chart**: no change (already correct). Add a one-line note to
`deploy/helm/dagger-kubernetes/README.md` RBAC section (if present) that the default is
namespaced and `clusterScope: true` is only for multi-namespace fleets.

### Edge case
If an operator runs engine fleets in a different namespace from the supervisor, they need
`clusterScope: true` (Helm) — already supported. The standalone manifest assumes single-namespace
(the documented `dagger-kubernetes` namespace).

---

## Finding 4 [MEDIUM] — Standalone `supervisor.yaml` missing tokens volume mount

### Current behavior
`deploy/k8s/supervisor.yaml` mounts the `config` ConfigMap at `/etc/dagger-kubernetes` (line 40-42,
a directory mount) but has NO `tokens` volume, while the ConfigMap
(`deploy/k8s/namespace-rbac.yaml:90`) sets `tokens_file: /etc/dagger-kubernetes/tokens`. The
supervisor runs with no token source (CWE-306). Also the directory mount of the ConfigMap at
`/etc/dagger-kubernetes` would shadow any other mount under that path (the nested-mount conflict
the Helm chart fixed via subPath).

### Target / exact changes
**`deploy/k8s/supervisor.yaml`** — mirror the Helm chart's volume/mount layout:
1. Change the `config` volumeMount (lines 39-42) to a subPath file mount:
   ```yaml
   volumeMounts:
     - name: config
       mountPath: /etc/dagger-kubernetes/config.app.yaml
       subPath: config.app.yaml
       readOnly: true
     - name: tokens
       mountPath: /etc/dagger-kubernetes/tokens
       subPath: tokens
       readOnly: true
     - name: tls
       mountPath: /etc/dagger-kubernetes/tls
       readOnly: true
     - name: db
       mountPath: /var/lib/dagger-kubernetes
   ```
2. Add the `tokens` volume in `volumes:` (after `config`, lines 67-70):
   ```yaml
   volumes:
     - name: config
       configMap:
         name: supervisor-config
     - name: tokens
       secret:
         secretName: supervisor-tokens
         optional: true
     - name: tls
       secret:
         secretName: supervisor-tls
     - name: db
       emptyDir: {}
   ```
3. The `args: ["--config=/etc/dagger-kubernetes/config.app.yaml"]` (line 31) already matches the
   subPath file mount — keep it.
4. **`deploy/k8s/namespace-rbac.yaml`** — add a `supervisor-tokens` Secret object (after the
   `engine-registry-auth` Secret, ~line 73) so the mount has a source:
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: supervisor-tokens
     namespace: dagger-kubernetes
   type: Opaque
   stringData:
     tokens: ""   # operator: paste one bearer token per line
   ```
   (Matches the Helm `<release>-tokens` Secret shape from `secret.yaml:1-15`.)

### Edge case
`optional: true` on the tokens volume lets the pod start with an empty token file (dev), matching
the Helm chart's `optional: true` (statefulset.yaml line 134).

---

## Finding 5 [MEDIUM] — UI stores tokens in localStorage (CWE-922)

### Current behavior (end-to-end auth flow)
- **UI store** (`ui/src/stores/auth.ts`): `token`/`refreshToken` in `localStorage` keys
  `dagger_kubernetes_token` / `dagger_kubernetes_refresh_token` (lines 7-8, 20-21, 63-64).
- **API client** (`ui/src/api/client.ts`): axios instance (lines 27-30) with a request interceptor
  (32-38) adding `Authorization: Bearer <token>`; response interceptor (40-63) retries once on 401
  via `refreshRequest(refreshToken)` then redirects to `/auth/login`.
- **Login** (`ui/src/auth/Login.vue:82-97`): calls `auth.login()` → `loginRequest()` →
  `POST /api/v1/auth/login` → body `{access_token, refresh_token, user}` → `setTokens()` stores
  in localStorage.
- **OAuth callback** (`ui/src/auth/Callback.vue`): reads tokens from URL fragment
  `#access_token=...&refresh_token=...`, stores via `setTokens()`.
- **Server login** (`internal/handler/auth_endpoints.go:74-113`): returns
  `loginResponse{AccessToken, RefreshToken, User}` in JSON body. No cookies set.
- **Server refresh** (lines 121-136): body `{refresh_token}` → `refreshResponse{AccessToken, RefreshToken}`.
- **OAuth callback** (lines 297-330): redirects to `/auth/callback#access_token=...&refresh_token=...`.
- **Auth middleware** (`internal/handler/auth.go:15-36`): `extractToken` reads `Authorization`
  (Bearer/Basic) only. `middleware.go:18-53`: `resolveIdentity`/`requireAuthWithQueryFallback`
  (header → `?token=` query for SSE).
- **OAuth state cookie** (`auth_endpoints.go:262,334`): `oauth_state` cookie, httpOnly,
  SameSite=Lax, Path=`/api/v1/auth/oauth`, Secure=`oauthCookieSecure || requestIsTLS`. Precedent
  for cookie attributes.
- **No logout endpoint** exists. **No CORS middleware** exists (SPA is same-origin).
- **CI auth**: `Authorization: Bearer dct_...` (API tokens) / legacy tokens / JWTs — MUST keep
  working. `~/.dagger-kubernetes.env` (Connect page, `ui/src/views/Connect.vue`) is for the Dagger
  CLI, not the browser — unaffected, keep.

### Target design (httpOnly cookies, additive to bearer)
- **Cookie names**: `dagger_kubernetes_access` (access JWT), `dagger_kubernetes_refresh`
  (refresh JWT). Attributes: `HttpOnly=true`, `Secure=(cookie.secure || requestIsTLS)`,
  `SameSite=Lax`, `Path=/`, `Max-Age=access_ttl`/`refresh_ttl` seconds.
- **SameSite=Lax** (not Strict): Strict would break the OAuth callback (cross-site top-level
  redirect from the IdP) and deep links. Lax blocks cross-site POST/PUT/DELETE (CSRF protection
  for all state-changing endpoints, which are non-GET) while allowing the OAuth callback GET
  redirect. **No CSRF token needed** — the API is RESTful (GET = read, mutations = POST/PUT/DELETE),
  so Lax is sufficient. This matches the existing `oauth_state` cookie choice.
- **Login** (`POST /api/v1/auth/login`): set both cookies, return ONLY `{user: ...}` (NO tokens
  in body — closes the XSS-reads-response-body hole). Breaking change for curl-based login;
  document (use API tokens for programmatic access).
- **Refresh** (`POST /api/v1/auth/refresh`): read refresh token from the cookie first, fall back
  to the JSON body (backwards compat). Rotate: set fresh cookies. Return `204 No Content` (no
  tokens in body).
- **OAuth callback**: set both cookies, redirect to `/auth/callback?redirect=<safe>` (NO fragment).
  The SPA `Callback.vue` no longer parses a fragment; it just calls `loadUser()` then navigates.
- **Logout** (NEW `POST /api/v1/auth/logout`): clear both cookies (Max-Age=-1). No body.
- **Auth middleware**: `extractToken` → try `Authorization` header (Bearer/Basic) FIRST, then the
  `dagger_kubernetes_access` cookie. Bearer stays primary (CI). `requireAuthWithQueryFallback`:
  header → cookie → `?token=` query (SSE fallback for non-cookie clients).
- **CORS**: add a configurable allowlist `auth.cors.allowed_origins` (empty = same-origin only,
  no `Access-Control-Allow-Origin` header). When the request `Origin` matches an entry, echo it
  with `Access-Control-Allow-Credentials: true` and `Vary: Origin`. Never `*` with credentials.
  The SPA is same-origin so this is opt-in for split UI deployments.
- **SSE `/live`**: EventSource sends cookies automatically (same-origin); the `?token=` query
  fallback stays for non-cookie clients. No change to `connectLiveTrace` beyond dropping the
  localStorage token (use `?token=` only when a bearer is somehow present — keep for CI).
- **Multi-instance**: cookies are JWTs (stateless validation), so any node validates. Refresh
  rotation is stateless JWT. No shared session store needed.
- **Token expiry/refresh race**: the axios 401 interceptor calls `/refresh` (cookie sent
  automatically), gets new cookies, retries once. `refreshInFlight` dedup stays.

### Exact file changes

**`internal/domain/config.go`** — add a `CookieConfig` to `AuthConfig` (lines 33-39):
```go
type AuthConfig struct {
    Internal       InternalAuthConfig   `mapstructure:"internal"`
    OAuth          OAuthConfig          `mapstructure:"oauth"`
    JWT            JWTConfig            `mapstructure:"jwt"`
    Token          TokenConfig          `mapstructure:"token"`
    BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
    Cookie         CookieConfig         `mapstructure:"cookie"`      // NEW
    CORS           CORSConfig           `mapstructure:"cors"`        // NEW
}
type CookieConfig struct {
    AccessName  string `mapstructure:"access_name"`   // default dagger_kubernetes_access
    RefreshName string `mapstructure:"refresh_name"`  // default dagger_kubernetes_refresh
    Secure      bool   `mapstructure:"secure"`        // force Secure; else auto-detect TLS
}
type CORSConfig struct {
    AllowedOrigins []string `mapstructure:"allowed_origins"` // empty = same-origin only
}
```

**`config/loader.go`** — add defaults (after line 52, the bootstrap_admin block):
```go
v.SetDefault("auth.cookie.access_name", "dagger_kubernetes_access")
v.SetDefault("auth.cookie.refresh_name", "dagger_kubernetes_refresh")
v.SetDefault("auth.cookie.secure", false)
v.SetDefault("auth.cors.allowed_origins", []string{})
```

**`internal/handler/server.go`** — add fields to `Deps` (line 200-232) and `Server` (250-295):
`CookieCfg domain.CookieConfig`, `CORSAllowedOrigins []string`. Wire in `NewServer`. Add a CORS
middleware `s.corsMiddleware()` registered in `configure()` (after `securityHeaders`, line 451):
```go
func (s *Server) corsMiddleware() app.HandlerFunc {
    return func(ctx context.Context, c *app.RequestContext) {
        origin := string(c.Request.Header.Peek("Origin"))
        if origin != "" && s.originAllowed(origin) {
            c.Response.Header.Set("Access-Control-Allow-Origin", origin)
            c.Response.Header.Set("Access-Control-Allow-Credentials", "true")
            c.Response.Header.Set("Vary", "Origin")
        }
        c.Next(ctx)
    }
}
func (s *Server) originAllowed(origin string) bool { /* exact match against s.corsAllowedOrigins */ }
```
Register `h.OPTIONS` preflight handling or handle in the middleware (respond 204 to OPTIONS for
allowed origins with `Access-Control-Allow-Methods/Headers`). Keep minimal: echo
`Allow-Methods: GET,POST,PUT,DELETE,OPTIONS` and `Allow-Headers: Authorization, Content-Type`.

**`internal/handler/auth_endpoints.go`** — add cookie helpers + rewrite login/refresh/callback + add logout:
```go
const (
    defaultAccessCookiePath  = "/"
    defaultRefreshCookiePath = "/"
)
// setAuthCookies sets httpOnly access+refresh cookies. Secure = cfg.secure || requestIsTLS.
func (s *Server) setAuthCookies(c *app.RequestContext, access, refresh string) {
    secure := s.cookieCfg.Secure || requestIsTLS(c)
    accessTTL := int(s.jwt.AccessTTL().Seconds())      // add AccessTTL()/RefreshTTL() getters to JWTService
    refreshTTL := int(s.jwt.RefreshTTL().Seconds())
    c.SetCookie(s.cookieCfg.AccessName, access, accessTTL, defaultAccessCookiePath, "", protocol.CookieSameSiteLaxMode, secure, true)
    c.SetCookie(s.cookieCfg.RefreshName, refresh, refreshTTL, defaultRefreshCookiePath, "", protocol.CookieSameSiteLaxMode, secure, true)
}
func (s *Server) clearAuthCookies(c *app.RequestContext) {
    secure := s.cookieCfg.Secure || requestIsTLS(c)
    c.SetCookie(s.cookieCfg.AccessName, "", -1, defaultAccessCookiePath, "", protocol.CookieSameSiteLaxMode, secure, true)
    c.SetCookie(s.cookieCfg.RefreshName, "", -1, defaultRefreshCookiePath, "", protocol.CookieSameSiteLaxMode, secure, true)
}
```
- `handleLogin` (74-113): after `s.auth.Login(...)`, call `s.setAuthCookies(c, access, refresh)`;
  change the response to `c.JSON(200, authMeResponse{...user...})` (drop `AccessToken`/`RefreshToken`).
- `handleRefresh` (121-136): read `refresh := c.Cookie(s.cookieCfg.RefreshName)`; if empty, fall
  back to `req.RefreshToken` (body). After `s.auth.Refresh(...)`, `s.setAuthCookies(c, access, refresh)`;
  respond `c.SetStatusCode(204)` (no body).
- `completeOAuthCallback` (297-330): after `s.oauth.Complete(...)`, `s.setAuthCookies(c, access, refresh)`;
  redirect to `fmt.Sprintf("/auth/callback?redirect=%s", url.QueryEscape(redirectPath))` (no fragment).
- NEW `handleLogout`:
  ```go
  func (s *Server) handleLogout(_ context.Context, c *app.RequestContext) {
      s.clearAuthCookies(c)
      c.SetStatusCode(consts.StatusNoContent)
  }
  ```
  Register `h.POST("/api/v1/auth/logout", s.handleLogout)` in `server.go` configure() (after line 483).

**`internal/handler/auth.go`** — extend `extractToken` to fall back to the access cookie. Add a
method on `*Server` (needs cookie name) — move cookie fallback into `resolveIdentity`:
```go
func (s *Server) resolveIdentity(c *app.RequestContext) (*domain.Identity, bool) {
    bearer, _ := extractToken(c)
    if bearer == "" && s.cookieCfg.AccessName != "" {
        bearer = string(c.Cookie(s.cookieCfg.AccessName))
    }
    id, err := s.auth.Resolve(context.Background(), bearer)
    ...
}
```
`requireAuthWithQueryFallback` (middleware.go:41-53): header → cookie → `?token=` query.
Note: `extractToken` stays header-only (used by cache proxy `extractCacheToken` which must NOT
read cookies — the cache vhost uses bearer only). Keep `extractToken` unchanged; add the cookie
fallback only in `resolveIdentity`/`requireAuthWithQueryFallback`.

**`internal/service/jwt_service.go`** — add `AccessTTL() time.Duration` and `RefreshTTL() time.Duration`
getters (store the TTLs passed to `NewJWTService`).

**`ui/src/api/client.ts`**:
- Line 27-30: add `withCredentials: true` to the axios instance.
- Lines 32-38: remove the request interceptor that sets `Authorization` from localStorage (the
  browser sends the cookie automatically). (Keep nothing — no Authorization header from the SPA.)
- Lines 40-63: response interceptor — on 401, call `auth.refreshSession()` (cookie-based), retry
  once; on failure `auth.logout()` + redirect. Drop the `Authorization` header from the retry config.
- `refreshRequest` (80-83): change to `POST /api/v1/auth/refresh` with NO body (cookie sent
  automatically); return type becomes `void`.
- `loginRequest` (76-79): return type changes to `AuthUser` (no tokens in body).
- `connectLiveTrace` (235-238): keep `?token=` only if a bearer is available (it won't be from the
  SPA); EventSource sends the cookie. Simplify to `new EventSource(\`/api/v1/traces/${id}/live\`)`
  (cookie auth). Keep `?token=` path for any non-cookie client by leaving a fallback is unnecessary
  for the SPA — drop it. (CI does not use the live SSE UI.)

**`ui/src/stores/auth.ts`** — drop localStorage entirely:
- Remove `token`/`refreshToken` refs and all `localStorage.getItem/setItem/removeItem`.
- `isAuthenticated` becomes a ref set by `loadUser()` success (call `/me` on app init).
- `login(username, password)`: `const user = await loginRequest(...)`; `user.value = user` (no
  setTokens; cookie set by server).
- `loadUser()`: `user.value = await fetchMe()`; on success `isAuthenticated = true`.
- `refreshSession()`: `await refreshRequest()` (no arg, no return tokens); on success return true.
- `logout()`: call `POST /api/v1/auth/logout` (server clears cookie), then clear `user`/`isAuthenticated`.
- Remove `setTokens` (no longer needed) — but `Callback.vue` calls it; update Callback.

**`ui/src/auth/Callback.vue`** — no fragment parsing. `onMounted`: call `auth.loadUser()`; if
  success, `router.push(safeRedirect(route.query.redirect))`; else redirect to `/auth/login?error=oauth`.

**`ui/src/auth/Login.vue`** — `handleLogin`: `await auth.login(...)` (no setTokens); `router.push(...)`.

**`internal/handler/auth_endpoints_test.go`** + `middleware_test.go` — update tests:
- Login/refresh tests: assert `Set-Cookie` present with `HttpOnly`, `SameSite=Lax`, `Path=/`,
  `Secure` (when TLS/cookieSecure), and that the body has NO `access_token`/`refresh_token`.
- Add table-driven tests for: cookie fallback in `resolveIdentity`; refresh-from-cookie vs
  refresh-from-body; logout clears cookies; CORS allow/deny by origin; SameSite on http vs https.
- SSE `?token=` fallback test still passes (header → cookie → query order).
- 100% coverage of new helpers (`setAuthCookies`, `clearAuthCookies`, `originAllowed`,
  `corsMiddleware`, `handleLogout`).

**`docs/README.md`** — Security notes (912-935): add a "Session cookies" subsection: access/refresh
JWTs in httpOnly cookies, SameSite=Lax, Secure when TLS, Path=/; bearer auth still accepted for
CI; logout endpoint; CORS config. Update the env-var table for `auth.cookie.*` and `auth.cors.*`.

**`deploy/helm/dagger-kubernetes/values.yaml`** + configmap — add `supervisor.config.auth.cookie.*`
and `supervisor.config.auth.cors.allowed_origins` with defaults matching `config/loader.go`.
Render `cookie:` and `cors:` blocks in `configmap.yaml` (non-secret).

### Edge cases
- **http vs https**: `Secure` auto-set via `requestIsTLS` (scheme `https`); `cookie.secure` forces
  it for TLS-terminating ingresses (mirrors `oauth.cookie_secure`). On plain http (dev), cookies
  are non-Secure so they still work.
- **Cookie vs SSE**: EventSource sends same-origin cookies; `?token=` query fallback retained for
  non-cookie clients (CI live trace is not a real use case; keep for safety).
- **Refresh race**: single-flight `refreshInFlight` stays; cookie rotation is atomic per response.
- **Existing logged-in sessions**: rolling out cookie auth invalidates localStorage sessions —
  users re-login once. Document as a one-time cutover. (Rollback = re-login again.)
- **OAuth state cookie**: unchanged (already httpOnly Lax). The new auth cookies coexist.

---

## Finding 6 [LOW] — Dockerfile CMD/config mismatch

### Current behavior
`Dockerfile:39` copies `config/config.app.yaml.sample` to
`/etc/dagger-kubernetes/config.app.yaml.sample`, but `CMD` (line 44) points to
`/etc/dagger-kubernetes/config.app.yaml` (no `.sample`). The loader skips the missing file
(`config/loader.go:164-169`), so the container runs on compiled defaults + env vars. Helm mounts
the ConfigMap at that path (subPath), so Helm deploys are fine; standalone `docker run` silently
falls back. `deploy/docker/Dockerfile` has the same CMD (line 25) and copies NO sample at all.

### Target (least-surprise)
Copy the sample AS `config.app.yaml` so the documented defaults load (matching operator
expectations); env vars still override.

### Exact changes
**`Dockerfile:39`**:
```dockerfile
COPY config/config.app.yaml.sample /etc/dagger-kubernetes/config.app.yaml
```
(renames the destination from `.sample` to the CMD path). Keep `EXPOSE`/`CMD` unchanged.

**`deploy/docker/Dockerfile`** — add after the `COPY --from=builder` line (20):
```dockerfile
COPY config/config.app.yaml.sample /etc/dagger-kubernetes/config.app.yaml
```

### Edge case
The sample contains placeholder values (`data_hostname: "data.supv.example.com"`); env vars
override them in real deploys. This is the documented behavior (docs/README.md:310-314).

---

## Finding 7 [LOW] — Strip local binaries (ldflags)

### Current behavior (already mostly done)
- `Dockerfile:24,30`: `go build -trimpath -ldflags "-s -w"` — DONE.
- `deploy/docker/Dockerfile:12`: `go build -trimpath -ldflags "-s -w"` — DONE.
- `dagger/main.go:137-147` `Build`: delegates to `dag.Golang(...).Build(...)` whose default
  ldflags are `["-s","-w"]` (confirmed `dagger/deps/golang/README.md:31`,
  `dagger/deps/golang/main.go:250-260`) — DONE.
- `docs/README.md:440-441`: bare `go build -o ... ./cmd/...` with no ldflags — NOT stripped.

### Target / exact changes
**`docs/README.md:440-441`** — add ldflags + trimpath:
```bash
go build -trimpath -ldflags "-s -w" -o dagger-kubernetes-ci ./cmd/ci
go build -trimpath -ldflags "-s -w" -o supervisor ./cmd/api
```
No Dockerfile/dagger/scripts changes (already stripped). Verify with a `grep` gate (Validation).

---

## Finding 8 [COSMETIC] — Markdown table alignment + helm README footnote indent

### Current behavior
- `docs/README.md` env-var table (lines 316-323) and full-reference table (lines 345, 355-358,
  400-403) have inconsistent column widths after the rename (trailing-space padding drift).
- `deploy/helm/dagger-kubernetes/README.md:6`: `   [^1]: Latest released version: \`0.1.0\``
  (3-space indent) — caused by `scripts/update-helm-docs.sh:30` whose sed replacement string
  `s/^.*\$/   [^1]: .../` prepends 3 spaces. The insert branch (line 33) uses column-0 `[^1]:`.

### Target / exact changes
**`deploy/helm/dagger-kubernetes/README.md:6`** — restore column 0:
```
[^1]: Latest released version: `0.1.0`
```
**`scripts/update-helm-docs.sh:30`** — fix the replacement to column 0:
```bash
sed -i "/${MARKER}/{n;s/^.*\$/[^1]: Latest released version: \`${VERSION}\`/}" "${README}"
```
(remove the 3 leading spaces in the replacement). This prevents re-break on the next run.

**`docs/README.md`** — realign the two tables so every row's column separators line up. The
implementer re-flows the env-var table (316-323) and the full-reference table (340-419) to a
consistent column width (pick the widest cell per column; pad with spaces). This is mechanical;
no content change. Verify with a markdown linter or visual diff.

### Edge case
`update-helm-docs.sh` line 33 (insert branch) already uses column-0; only the update branch
(line 30) was wrong. Fixing line 30 keeps both branches consistent.

---

## Finding 9 [MANUAL] — Human UI verification

No code change. The plan must include the exact §5.2 steps (AGENTS.local.md:149-155), updated
for cookie auth:

After redeploy, a human confirms at `https://dagger.home.webcenter.fr`:
1. **Login** works (username/password) and the header shows the global status indicator
   (green/amber/red). (Cookie is set; localStorage no longer holds tokens — verify in DevTools
   Application → Cookies: `dagger_kubernetes_access` + `dagger_kubernetes_refresh` are HttpOnly.)
2. **Navigate** Pipelines, MagicCache, Runners, Services — render correctly against real data.
3. **Refresh**: leave idle > access TTL (15m), click an action → silent refresh succeeds (no
   re-login).
4. **Logout**: click logout → cookies cleared, redirected to `/auth/login`.
5. **OAuth** (if enabled): GitHub/OIDC login lands on `/auth/callback?redirect=...` (no
   fragment), cookies set, dashboard loads.
6. **CI bearer auth still works**: a `curl -H "Authorization: Bearer <api-token>"` to
   `/api/v1/status` returns 200 (cookie auth is additive).

---

## Ordered implementation steps (safe order)

1. **Finding 2** (remove `api` binary + `.gitignore`) — isolated, no build impact.
2. **Finding 6** (Dockerfile sample copy) — isolated.
3. **Finding 7** (docs ldflags) — isolated.
4. **Finding 8** (markdown alignment + helm footnote + script fix) — isolated.
5. **Finding 3** (standalone RBAC → Role) — isolated manifest.
6. **Finding 4** (standalone supervisor.yaml tokens mount + namespace-rbac tokens Secret) —
   isolated manifests.
7. **Finding 1** (ConfigMap secrets → Secret+env; `password_secret` ref):
   a. `internal/domain/config.go` (`SecretRef`, `PasswordSecret`).
   b. `config/loader.go` (no new defaults needed; verify).
   c. `cmd/api/main.go` (`resolveRegistryBackendSecrets`).
   d. Helm: `configmap.yaml`, `secret.yaml`, `statefulset.yaml`, `values.yaml`, `_helpers.tpl`
      (if needed), helm README.
   e. `deploy/k8s/namespace-rbac.yaml` (cache.auth_token blanking).
   f. `config/config.app.yaml.sample`, `docs/README.md`.
   - Checkpoint: `go build ./... && go vet ./... && go test ./internal/domain/... ./cmd/...`.
8. **Finding 5** (cookie auth) — largest change, do last:
   a. `internal/domain/config.go` (`CookieConfig`, `CORSConfig`).
   b. `config/loader.go` (defaults).
   c. `internal/service/jwt_service.go` (`AccessTTL`/`RefreshTTL` getters).
   d. `internal/handler/server.go` (Deps/Server fields, CORS middleware, logout route).
   e. `internal/handler/auth_endpoints.go` (cookie helpers, login/refresh/callback/logout).
   f. `internal/handler/auth.go` + `middleware.go` (cookie fallback in resolveIdentity).
   g. Tests: `auth_endpoints_test.go`, `middleware_test.go` (table-driven, stdlib).
   h. UI: `stores/auth.ts`, `api/client.ts`, `auth/Callback.vue`, `auth/Login.vue`.
   i. `config/config.app.yaml.sample`, `docs/README.md`, Helm `values.yaml` + `configmap.yaml`
      (cookie/cors blocks).
   - Checkpoints: `go build ./... && go test -race ./...`; `cd ui && npm run typecheck && npm run build`;
     `rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist && go build ./...`.

## Validation gates (exact commands)

```sh
# Go
go build ./... && go vet ./...
gofmt -l . | grep . && echo UNFORMATTED || echo ok
goimports -w -local github.com/disaster/dagger-kubernetes .
go test -race -coverprofile=coverage.out -covermode=atomic ./...
golangci-lint run ./...

# No plaintext secrets in rendered ConfigMap
helm dependency update deploy/helm/dagger-kubernetes
helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug \
  --set auth.jwtSecret=test --set auth.oauthClientSecret=test --set auth.adminPassword=test \
  | grep -E 'secret:|client_secret:|auth_token:|password:' | grep -v '""' \
  && echo "LEAK" || echo "ok (no non-empty secrets in ConfigMap)"

# ldflags in docs
grep -n 'go build' docs/README.md | grep -v 'ldflags "-s -w"' && echo MISSING || echo ok

# Stale binary gone
git ls-files | grep '^api$' && echo PRESENT || echo ok

# Helm
helm lint deploy/helm/dagger-kubernetes
helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug
helm template dagger-kubernetes deploy/helm/dagger-kubernetes \
  --set tools.otelCollector.enabled=false --set tools.registry.enabled=false --debug

# UI
cd ui && npm ci && npm run typecheck && npm run build
cd .. && rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist
go build ./...

# Rename grep gates (B-whitelist preserved) — from the rename plan
grep -rn "DaggerCache\|dagger_cache_token\|dagger_cache_refresh_token" . && echo LEAK || echo ok
grep -rn "X-Dagger-Cache" . && echo LEAK || echo ok
```

## Rollback note
- **Helm**: `helm --kubeconfig /home/user/.kube/home rollback dagger-kubernetes-test -n dagger-kubernetes-test`.
- **Cookie auth rollback**: re-login is required for every user (localStorage sessions were
  invalidated by the rollout; rolling back re-enables localStorage but existing cookies are
  ignored — users re-login once).
- **Secrets rollback**: chart-managed Secrets persist across rollback; env-var wiring reverts
  with the chart. No data loss (Raft store unaffected).

## Cluster redeploy (AGENTS.local.md §6)
After all gates pass:
```sh
docker build -t docker.io/disaster/dagger-kubernetes:dev .
docker push docker.io/disaster/dagger-kubernetes:dev
export KUBECONFIG=/home/user/.kube/home
helm get values dagger-kubernetes-test -n dagger-kubernetes-test -o yaml > /tmp/dagger-kubernetes-test.values.yaml
helm upgrade --install dagger-kubernetes-test ./deploy/helm/dagger-kubernetes \
  -n dagger-kubernetes-test -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.config.raft.replicas=1 \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-kubernetes-test \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```
Then AGENTS.local.md §5.1 agent verification (pods Ready, /healthz, /readyz, authed API smoke,
logs clean), then §5.2 human verification (Finding 9 steps above). Update AGENTS.local.md §3/§7
if any deployed value changed (e.g. new `auth.cookie.*` defaults — none required, defaults apply).

## Open design decisions (resolved with defaults — implementer does not stall)

- **D1 — `existingSecret` toggle for chart Secrets**: NOT added. Chart-managed Secrets are
  reused in place across `helm upgrade` (Helm updates them), satisfying "existing-secret reuse
  across helm upgrades". Adding a per-Secret `existingSecret` toggle is a separate feature, out
  of scope. (Default: keep chart-managed Secrets.)
- **D2 — Login response body**: returns ONLY `{user}` (no tokens) to close the XSS-reads-body
  hole. Breaking for curl-based login; document (use API tokens for programmatic access).
  (Default: drop tokens from body.)
- **D3 — CSRF**: SameSite=Lax, no CSRF token. RESTful API (GET=read, mutations=non-GET) makes Lax
  sufficient; matches the existing `oauth_state` cookie. (Default: Lax, no token.)
- **D4 — `cache.registries[].password` mechanism**: `password_secret` K8s-Secret ref (mirrors
  the existing `cache.auth_token` → `engine-registry-auth` pattern). Non-K8s deploys set
  `password` directly in config. (Default: `password_secret` ref.)
- **D5 — SSE `?token=` query auth**: retained as a last-resort fallback for non-cookie clients
  (header → cookie → query). The SPA drops it (uses cookies). (Default: keep fallback.)

## Files touched (estimate: ~24)

`internal/domain/config.go`, `config/loader.go`, `cmd/api/main.go`, `internal/service/jwt_service.go`,
`internal/handler/server.go`, `internal/handler/auth_endpoints.go`, `internal/handler/auth.go`,
`internal/handler/middleware.go`, `internal/handler/auth_endpoints_test.go`,
`internal/handler/middleware_test.go`, `ui/src/stores/auth.ts`, `ui/src/api/client.ts`,
`ui/src/auth/Callback.vue`, `ui/src/auth/Login.vue`, `Dockerfile`, `deploy/docker/Dockerfile`,
`deploy/helm/dagger-kubernetes/templates/configmap.yaml`, `deploy/helm/dagger-kubernetes/templates/secret.yaml`,
`deploy/helm/dagger-kubernetes/templates/statefulset.yaml`, `deploy/helm/dagger-kubernetes/values.yaml`,
`deploy/helm/dagger-kubernetes/README.md`, `deploy/k8s/namespace-rbac.yaml`, `deploy/k8s/supervisor.yaml`,
`config/config.app.yaml.sample`, `docs/README.md`, `scripts/update-helm-docs.sh`, `.gitignore`
(+ `git rm --cached api`). ~24-27 files.
