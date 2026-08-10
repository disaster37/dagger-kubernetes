# Multi-User RBAC for dagger-cache

**Status:** Finalized — implementation-ready
**Target:** `github.com/disaster/dagger-kubernetes` (supervisor control plane + Vue 3 UI)

## 0. Context & Goals

Today the platform has no users: auth is a flat bearer-token file
(`service.TokenValidator`), all state is in-memory (leases) or flat files,
`GET /api/v1/traces` is a hardcoded stub, and the UI auth store is a stub
(`role: 'viewer'` hardcoded, Login.vue never calls the backend, Callback.vue
treats the OAuth `code` as a token).

This plan adds:

1. Persistent users (SQLite, `modernc.org/sqlite` pure-Go driver) with roles `admin` / `user`.
2. Groups (many-to-many with users) carrying `max_runner_sessions`, `agent_available`, and an `auto_assign_pattern` regex.
3. **Projects** — CI pipelines (identified by repo slug) assigned to groups manually, pre-created, or auto-matched by regex. Traces become visible per group.
4. AuthN: username/password login → JWT (access + refresh), GitHub OAuth → JWT, per-user API tokens for CI (`DAGGER_CLOUD_TOKEN` compatible).
5. RBAC middleware; admin CRUD for users/groups/projects; self-service token + profile UI.
6. Zero-breakage migration from flat-file tokens (compat fallback + `migrate-tokens` command).

## 1. Key Decisions (resolved with stakeholder)

| # | Decision | Rationale |
|----|----------|-----------|
| D1 | **One API token per user** (not per group). CI runs *as the user*. | User decision: "CI is like a regular user with its own token". |
| D2 | Pipelines attach to groups via **projects** (repo slug → group), assigned by admin UI, pre-created, or **regex auto-assign** (one pattern per group, first match by group `id` order wins). | User decision. Token can't carry group context (Dagger CLI sends only `Authorization: Bearer`). |
| D3 | **Quota semantics:** a group's active sessions = active leases of *all its members* (a multi-group user's session counts against each of their groups). Admission to `POST /v1/engines`: user must belong to ≥1 group with `agent_available=true`, and is admitted if *any* such group has remaining capacity (`max_runner_sessions`; `0` = unlimited). Admins bypass quota entirely. Users with no groups → 403. | Permissive admission, conservative counting; computable from in-memory lease store + membership query. |
| D4 | Trace visibility: admin = all; user = traces where `group_id` ∈ user's groups **or** `user_id` = self (owner always sees own pipelines even when unassigned). Unknown/missing trace metadata → admin-only (fail closed). | Default-deny with sane owner UX. |
| D5 | Trace attribution is two-phase: `POST /v1/engines` records `trace_id → user_id`; `POST /v1/traces` OTLP ingest parses the body best-effort (then still forwards to collector) extracting root-span attrs (`dagger.io/ci.repo`, engine version, status, duration), upserts the project, resolves the group, and enriches `trace_meta`. Group is set once (first resolution wins; later project reassignment affects only future traces). | Repo slug only exists in span data, not at engine provision time. Hertz buffers request bodies, so reading `c.Body()` does not consume it for the reverse proxy. |
| D6 | JWT: HS256 via `github.com/golang-jwt/jwt/v5`; access 15m, refresh 7d, refresh rotated on use, stateless. Secret from `auth.jwt.secret`; if empty, auto-generated on first boot and persisted in SQLite `meta` table. Claims: `uid`, `username`, `role`, `groups[]`, `typ` (access/refresh/oauth_state). | Requirement. Groups are re-fetched from DB at resolve time (claims can be stale); claims groups are informational. |
| D7 | **Bootstrap admin:** on first boot with empty `users` table, create admin from `auth.bootstrap_admin.username` (default `admin`) / `.password`; if password empty, generate random 16-byte hex and log it once at WARN. Idempotent (only when user count = 0). | No CLI-only bootstrap chicken-and-egg. |
| D8 | **Legacy compat:** when a bearer token is neither JWT nor API token, and `auth.internal.tokens_file` is still configured and contains it, the request runs as synthetic identity `legacy` (role `admin`, no groups, bypasses quota — exactly today's full-access behavior). `supervisor migrate-tokens` imports each token line as a real user + API token; operator then removes `tokens_file`. | Zero CI breakage; explicit cutover. |
| D9 | `auth.internal.enabled=false` keeps its current meaning: **auth fully disabled** (anonymous admin identity, dev mode). | Backward compatible with existing no-auth deployments. |
| D10 | OAuth callback moves to the **backend**: `GET /api/v1/auth/oauth/github/callback`, which 302s to the SPA route `/auth/callback#access_token=…&refresh_token=…` (fragment, never logged). `auth.oauth.redirect_url` default changes accordingly; the GitHub OAuth App must be re-pointed. `allowed_orgs` enforced (empty = allow all); optional `auth.oauth.default_group` auto-membership for new OAuth users. | Server-side code exchange; existing SPA stubs rewired. |
| D11 | Leases gain `UserID` (+ `GroupID`, set only when the user has exactly one group; display aid). Quota always counts via membership, never via `Lease.GroupID`. | Minimal blast radius on `SessionStore` interface. |
| D12 | `handler.NewServer` is refactored to `(cfg *ServerConfig, deps Deps)` with a `Deps` struct bundling collaborators (current 11-param signature + ~6 new ones is unmaintainable and every test constructs it). | Mechanical test churn now vs. permanent pain. |
| D13 | Token format `dct_<32 random bytes hex>` (`crypto/rand`); store SHA-256 hex (tokens are high-entropy; bcrypt is for passwords only); plaintext returned once; `prefix` (first 12 chars) stored for display. Regeneration replaces the hash → old token invalid immediately. | Standard practice. |
| D14 | `extractToken` additionally accepts `?token=` query param (fallback after `Authorization`) so the SSE `/live` endpoint can be authenticated by EventSource clients. | EventSource cannot set headers. |
| D15 | IDs: 16 random bytes hex (32 chars) via `crypto/rand` for users/groups/projects/tokens. Timestamps stored UTC. | No new dependency (`google/uuid` stays indirect). |

## 2. File Changes Summary

### Created — backend
| Path | Purpose |
|------|---------|
| `internal/domain/user.go` | `Role`, `User`, `UserRepository` |
| `internal/domain/group.go` | `Group`, `GroupRepository` |
| `internal/domain/project.go` | `Project`, `ProjectRepository` |
| `internal/domain/apitoken.go` | `APIToken`, `APITokenRepository` |
| `internal/domain/identity.go` | `Identity`, `AuthMethod`, sentinel errors |
| `internal/domain/tracemeta.go` | `TraceMeta`, `TraceFilter`, `TraceMetaRepository` |
| `internal/repository/schema.sql` | Embedded SQLite schema (v1) |
| `internal/repository/sqlite.go` | `OpenSQLite`, schema migration, `MetaStore` |
| `internal/repository/user_repo.go` | SQLite `UserRepository` |
| `internal/repository/group_repo.go` | SQLite `GroupRepository` |
| `internal/repository/project_repo.go` | SQLite `ProjectRepository` |
| `internal/repository/token_repo.go` | SQLite `APITokenRepository` |
| `internal/repository/tracemeta_repo.go` | SQLite `TraceMetaRepository` |
| `internal/service/user_service.go` | User CRUD + password logic |
| `internal/service/group_service.go` | Group CRUD + membership |
| `internal/service/project_service.go` | Project CRUD + assignment |
| `internal/service/token_service.go` | API token generate/regenerate/revoke/validate |
| `internal/service/jwt_service.go` | JWT issue/parse |
| `internal/service/auth_service.go` | Login, refresh, bearer → `Identity` resolution (JWT → API token → legacy fallback) |
| `internal/service/quota_service.go` | Group engine-quota admission + usage |
| `internal/service/attribution_service.go` | Project upsert + regex auto-assign + trace_meta writes |
| `internal/service/otlp_extract.go` | Pure OTLP/HTTP JSON → trace summaries extraction |
| `internal/service/oauth_github.go` | GitHub OAuth flow |
| `internal/service/legacy_import.go` | Flat-file token importer (used by CLI) |
| `internal/handler/middleware.go` | Identity resolution middleware, `requireAuth`/`requireAdmin`, trace authorization |
| `internal/handler/auth_endpoints.go` | login/refresh/me/providers/password/oauth handlers |
| `internal/handler/users.go` | Admin user CRUD handlers |
| `internal/handler/groups.go` | Admin group CRUD + members handlers |
| `internal/handler/projects.go` | Admin project handlers |
| `internal/handler/tokens.go` | Self-service token handlers |
| `tests/integration/rbac_test.go` | End-to-end RBAC/quota/visibility scenarios |

### Created — tests (unit)
`internal/repository/sqlite_test.go`, `user_repo_test.go`, `group_repo_test.go`,
`project_repo_test.go`, `token_repo_test.go`, `tracemeta_repo_test.go`,
`internal/service/user_service_test.go`, `group_service_test.go`,
`project_service_test.go`, `token_service_test.go`, `jwt_service_test.go`,
`auth_service_test.go`, `quota_service_test.go`, `attribution_service_test.go`,
`otlp_extract_test.go`, `oauth_github_test.go`, `legacy_import_test.go`,
`internal/handler/middleware_test.go`, `auth_endpoints_test.go`, `users_test.go`,
`groups_test.go`, `projects_test.go`, `tokens_test.go`.

### Created — frontend
| Path | Purpose |
|------|---------|
| `ui/src/api/types.ts` | Shared TS interfaces |
| `ui/src/views/admin/Users.vue` | Admin user management |
| `ui/src/views/admin/Groups.vue` | Admin group management (quota, agent_available, pattern, members) |
| `ui/src/views/admin/Projects.vue` | Admin project→group assignment |

### Created — docs/deploy
| Path | Purpose |
|------|---------|
| `docs/design/ADR-010-sqlite-multiuser-rbac.md` | ADR: SQLite + RBAC architecture |

### Modified
| Path | Change |
|------|--------|
| `go.mod` / `go.sum` | Add `modernc.org/sqlite`, `github.com/golang-jwt/jwt/v5`; promote `golang.org/x/crypto`, `golang.org/x/oauth2` to direct |
| `internal/domain/config.go` | `DatabaseConfig`, `JWTConfig`, `BootstrapAdminConfig`; `OAuthConfig.DefaultGroup` |
| `internal/domain/session.go` | `Lease.UserID`, `Lease.GroupID`; `SessionStore.Register` +`userID`; new `CountByUser` |
| `internal/domain/auth.go` | Keep `TokenValidator` (used by legacy fallback); doc comment |
| `config/loader.go` + `config/loader_test.go` | New keys + defaults |
| `config/config.app.yaml.sample`, `config/config.app.yaml` | New sections (mandatory per AGENTS.md) |
| `internal/repository/*` (none structurally) | — |
| `internal/service/session.go` + `session_test.go` | New `Register` signature, `CountByUser` |
| `internal/service/auth.go` | Unchanged behavior; now consumed by `AuthService` as legacy fallback |
| `internal/service/fleet_test.go`, `k8s_manager_test.go` | `Register` signature updates |
| `internal/handler/server.go` | `Deps` refactor, new routes, `handleEngines` quota, `handleOTel` attribution, `ServerConfig` field changes |
| `internal/handler/auth.go` | Rewrite around `Identity` (keep `extractToken` + query-param fallback) |
| `internal/handler/traces.go` | Real scoped list; trace authorization on detail/logs/live |
| `internal/handler/logs.go` | Trace authorization |
| `internal/handler/server_test.go`, `auth_test.go` | `Deps` + identity-based tests |
| `cmd/api/main.go` | DB open/migrate/close, bootstrap admin, service wiring, `migrate-tokens` subcommand |
| `tests/integration/api_test.go` | `Deps` construction; token-based flow via seeded user+token |
| `ui/src/stores/auth.ts` | Real user/role/groups state, login/refresh/fetchMe actions |
| `ui/src/api/client.ts` | 401→refresh→retry interceptor; all new API functions |
| `ui/src/router/index.ts` | Global guards (`requiresAuth`, `requiresAdmin`), admin routes |
| `ui/src/auth/Login.vue` | Username/password form + GitHub button (calls real endpoints) |
| `ui/src/auth/Callback.vue` | Parse tokens from URL fragment, `fetchMe`, redirect |
| `ui/src/App.vue` | Admin nav links, role badge |
| `ui/src/views/Settings.vue` | Profile, groups, API token management, change password |
| `ui/src/views/Pipelines.vue` | Real data shape: group/project columns, admin group filter |
| `deploy/helm/dagger-kubernetes/values.yaml`, `templates/configmap.yaml`, `templates/secret.yaml`, `templates/deployment.yaml` | DB path, JWT secret, SQLite PVC |
| `deploy/k8s/supervisor.yaml` | DB volume + env |
| `deploy/docker/docker-compose.yaml` | DB volume + bootstrap/JWT env |
| `docs/README.md` | Auth section rewrite; groups/projects/quota/migration docs |
| `docs/design/index.md` | ADR-010 row |

### Deleted
None. (`auth.internal.tokens_file` support is retained as legacy fallback; removal is a future release decision.)

## 3. Data Model (Go)

All in `internal/domain` (stdlib-only imports).

```go
// user.go
type Role string

const (
    RoleAdmin Role = "admin"
    RoleUser  Role = "user"
)

func ParseRole(s string) (Role, error) // rejects anything else

type User struct {
    ID            string    `json:"id"`
    Username      string    `json:"username"`
    Role          Role      `json:"role"`
    PasswordHash  string    `json:"-"`
    OAuthProvider string    `json:"oauth_provider,omitempty"`
    OAuthID       string    `json:"oauth_id,omitempty"`
    CreatedAt     time.Time `json:"created_at"`
    UpdatedAt     time.Time `json:"updated_at"`
}

type UserRepository interface {
    Create(ctx context.Context, u *User) error
    Get(ctx context.Context, id string) (*User, error)
    GetByUsername(ctx context.Context, username string) (*User, error)
    GetByOAuth(ctx context.Context, provider, oauthID string) (*User, error)
    List(ctx context.Context) ([]*User, error)
    Update(ctx context.Context, u *User) error // role, password_hash, oauth fields, updated_at
    Delete(ctx context.Context, id string) error
    Count(ctx context.Context) (int, error)
}
```

```go
// group.go
type Group struct {
    ID                string    `json:"id"`
    Name              string    `json:"name"`
    Description       string    `json:"description"`
    MaxRunnerSessions int       `json:"max_runner_sessions"` // 0 = unlimited
    AgentAvailable    bool      `json:"agent_available"`
    AutoAssignPattern string    `json:"auto_assign_pattern,omitempty"` // regex vs project name; empty = off
    CreatedAt         time.Time `json:"created_at"`
    UpdatedAt         time.Time `json:"updated_at"`
}

type GroupRepository interface {
    Create(ctx context.Context, g *Group) error
    Get(ctx context.Context, id string) (*Group, error)
    GetByName(ctx context.Context, name string) (*Group, error)
    List(ctx context.Context) ([]*Group, error)
    Update(ctx context.Context, g *Group) error
    Delete(ctx context.Context, id string) error
    SetMembers(ctx context.Context, groupID string, userIDs []string) error // full replace, tx
    Members(ctx context.Context, groupID string) ([]*User, error)
    GroupsForUser(ctx context.Context, userID string) ([]*Group, error)
    AllMemberships(ctx context.Context) (map[string][]string, error) // groupID -> userIDs
}
```

```go
// project.go
type Project struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"` // canonical CI repo slug, e.g. "github.com/acme/api"
    GroupID   string    `json:"group_id,omitempty"` // empty = unassigned
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

type ProjectRepository interface {
    Create(ctx context.Context, p *Project) error
    Get(ctx context.Context, id string) (*Project, error)
    GetByName(ctx context.Context, name string) (*Project, error)
    List(ctx context.Context) ([]*Project, error)
    Update(ctx context.Context, p *Project) error
    Delete(ctx context.Context, id string) error
}
```

```go
// apitoken.go
type APIToken struct {
    ID         string     `json:"id"`
    UserID     string     `json:"user_id"`
    TokenHash  string     `json:"-"`
    Prefix     string     `json:"prefix"` // e.g. "dct_ab12cd34" (first 12 chars of plaintext)
    CreatedAt  time.Time  `json:"created_at"`
    LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type APITokenRepository interface {
    Upsert(ctx context.Context, t *APIToken) error // replaces any existing token for UserID (tx)
    GetByHash(ctx context.Context, tokenHash string) (*APIToken, error)
    GetByUser(ctx context.Context, userID string) (*APIToken, error)
    Delete(ctx context.Context, userID string) error
    TouchLastUsed(ctx context.Context, id string, at time.Time) error
}
```

```go
// identity.go
type AuthMethod string

const (
    AuthNone      AuthMethod = "none"       // auth disabled
    AuthJWT       AuthMethod = "jwt"
    AuthAPIToken  AuthMethod = "api_token"
    AuthLegacyTok AuthMethod = "legacy_token"
)

type Identity struct {
    UserID   string
    Username string
    Role     Role
    GroupIDs []string // fresh from DB at resolve time (empty for legacy/anonymous)
    Method   AuthMethod
}

func (i *Identity) IsAdmin() bool
func (i *Identity) HasGroup(groupID string) bool

// Sentinel errors (matched with errors.Is in handlers):
var (
    ErrUnauthenticated   = errors.New("unauthenticated")
    ErrForbidden         = errors.New("forbidden")
    ErrNoGroups          = errors.New("user is not a member of any group")
    ErrAgentUnavailable  = errors.New("engines not available for any of the user's groups")
    ErrQuotaExhausted    = errors.New("group runner session quota exhausted")
    ErrTokenExists       = errors.New("api token already exists")
    ErrNotFound          = errors.New("not found")
    ErrInvalidCredential = errors.New("invalid credentials")
)
```

```go
// tracemeta.go
type TraceMeta struct {
    TraceID     string    `json:"trace_id"`
    UserID      string    `json:"user_id,omitempty"`
    GroupID     string    `json:"group_id,omitempty"`
    ProjectName string    `json:"project_name,omitempty"`
    Status      string    `json:"status,omitempty"`
    Version     string    `json:"version,omitempty"`
    CIProvider  string    `json:"ci_provider,omitempty"`
    CIRepo      string    `json:"ci_repo,omitempty"`
    DurationMS  int64     `json:"duration_ms"`
    StartedAt   time.Time `json:"started_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// TraceListResult enriches TraceMeta with joined display names.
type TraceListResult struct {
    TraceMeta
    GroupName string `json:"group_name,omitempty"`
    Username  string `json:"username,omitempty"`
}

type TraceFilter struct {
    GroupIDs          []string // empty = no group restriction (admin)
    IncludeUnassigned bool     // admin: true (all); user: only their own user_id fallback
    UserID            string   // owner fallback for unassigned traces
    Limit             int      // default 100, max 500
}

type TraceMetaRepository interface {
    UpsertProvision(ctx context.Context, traceID, userID string) error
    UpsertIngest(ctx context.Context, m *TraceMeta) error // never overwrites a set group_id
    Get(ctx context.Context, traceID string) (*TraceMeta, error)
    List(ctx context.Context, f TraceFilter) ([]*TraceListResult, error)
}
```

```go
// session.go — modified
type Lease struct {
    CertFP       string
    Version      string
    ReplicaPod   string
    InstanceID   string
    LastActivity time.Time
    InFlight     int
    TraceID      string
    UserID       string // new: owning user ("" for legacy/anonymous)
    GroupID      string // new: set only when owner has exactly one group (display aid)
}

type SessionStore interface {
    Register(certFP, version, replicaPod, instanceID, traceID, userID string) *Lease
    Get(certFP string) (*Lease, error)
    Touch(certFP string) error
    IncInFlight(certFP string) error
    DecInFlight(certFP string) error
    Remove(certFP string)
    PinnedSessionsOnReplica(podName string) int
    CountByUser(userID string) int // new
    List() []*Lease
}
```

```go
// config.go — additions
type Config struct {
    // ... existing fields ...
    Database DatabaseConfig `mapstructure:"database"`
}

type AuthConfig struct {
    Internal       InternalAuthConfig   `mapstructure:"internal"`
    OAuth          OAuthConfig          `mapstructure:"oauth"`
    JWT            JWTConfig            `mapstructure:"jwt"`
    BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
}

type OAuthConfig struct {
    // ... existing fields ...
    DefaultGroup string `mapstructure:"default_group"` // auto-membership for new OAuth users; empty = none
}

type DatabaseConfig struct {
    Path string `mapstructure:"path"`
}

type JWTConfig struct {
    Secret     string        `mapstructure:"secret"`
    AccessTTL  time.Duration `mapstructure:"access_ttl"`
    RefreshTTL time.Duration `mapstructure:"refresh_ttl"`
}

type BootstrapAdminConfig struct {
    Username string `mapstructure:"username"`
    Password string `mapstructure:"password"`
}
```

## 4. Database Schema (`internal/repository/schema.sql`)

SQLite via `modernc.org/sqlite` (driver name `"sqlite"`), std `database/sql`.

```sql
-- v1
CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    role           TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    password_hash  TEXT NOT NULL DEFAULT '',
    oauth_provider TEXT NOT NULL DEFAULT '',
    oauth_id       TEXT NOT NULL DEFAULT '',
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oauth
    ON users(oauth_provider, oauth_id) WHERE oauth_provider != '';

CREATE TABLE IF NOT EXISTS groups (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE COLLATE NOCASE,
    description         TEXT NOT NULL DEFAULT '',
    max_runner_sessions INTEGER NOT NULL DEFAULT 0,   -- 0 = unlimited
    agent_available     INTEGER NOT NULL DEFAULT 1,
    auto_assign_pattern TEXT NOT NULL DEFAULT '',
    created_at          DATETIME NOT NULL,
    updated_at          DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS user_groups (
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    group_id   TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, group_id)
);
CREATE INDEX IF NOT EXISTS idx_user_groups_group ON user_groups(group_id);

CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL,
    last_used_at DATETIME
);

CREATE TABLE IF NOT EXISTS projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    group_id   TEXT REFERENCES groups(id) ON DELETE SET NULL, -- NULL = unassigned
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_group ON projects(group_id);

CREATE TABLE IF NOT EXISTS trace_meta (
    trace_id     TEXT PRIMARY KEY,
    user_id      TEXT REFERENCES users(id)  ON DELETE SET NULL,
    group_id     TEXT REFERENCES groups(id) ON DELETE SET NULL, -- NULL = unassigned
    project_name TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    ci_provider  TEXT NOT NULL DEFAULT '',
    ci_repo      TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    started_at   DATETIME,
    updated_at   DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trace_meta_group ON trace_meta(group_id);
CREATE INDEX IF NOT EXISTS idx_trace_meta_user  ON trace_meta(user_id);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at DATETIME NOT NULL
);
```

Notes:
- `group_id`/`user_id` in `projects`/`trace_meta` are nullable (NULL = unassigned); repos scan into `sql.NullString` and map to `""`.
- FK enforcement requires `PRAGMA foreign_keys=ON` per connection (set in DSN).
- Migration runner: single embedded `schema.sql` executed inside one transaction when `schema_migrations` lacks version 1; insert row after success. Future versions add ordered `v{n}` blocks.

## 5. Config Changes

### `config/loader.go` — new defaults (add to existing block)

```go
v.SetDefault("database.path", "/var/lib/dagger-cache/dagger-cache.db")

v.SetDefault("auth.jwt.secret", "")
v.SetDefault("auth.jwt.access_ttl", 15*time.Minute)
v.SetDefault("auth.jwt.refresh_ttl", 168*time.Hour) // 7d

v.SetDefault("auth.bootstrap_admin.username", "admin")
v.SetDefault("auth.bootstrap_admin.password", "")

v.SetDefault("auth.oauth.default_group", "")
```

Existing `auth.internal.enabled` (default `true`) and `auth.internal.tokens_file` keep their keys; semantics documented in ADR-010: `enabled=false` = auth disabled (dev mode); `tokens_file` = legacy fallback + migration source only.

### YAML (`config/config.app.yaml.sample` — full reference; mirror essentials in `config/config.app.yaml`)

```yaml
# --- Database ----------------------------------------------------------------
database:
  path: "/var/lib/dagger-cache/dagger-cache.db"  # SQLite file (users, groups, tokens, projects, trace metadata).

auth:
  internal:
    enabled: true                                   # false = auth fully disabled (dev mode).
    tokens_file: "/etc/dagger-cache/tokens"         # DEPRECATED legacy fallback; migrate with `supervisor migrate-tokens` then remove.
  bootstrap_admin:
    username: "admin"                               # created on first boot when no users exist.
    password: ""                                    # empty = random password logged once at first boot.
  jwt:
    secret: ""                                      # HS256 signing key; empty = auto-generated and persisted in the DB.
    access_ttl: "15m"
    refresh_ttl: "168h"
  oauth:
    enabled: false
    provider: "github"
    client_id: "${OAUTH_CLIENT_ID}"
    client_secret: "${OAUTH_CLIENT_SECRET}"
    redirect_url: "https://supv.example.com/api/v1/auth/oauth/github/callback"  # NOTE: backend endpoint now
    allowed_orgs: ["acme"]
    default_group: ""                               # new OAuth users auto-join this group (must exist); empty = none.
```

Env overrides follow existing convention, e.g. `DAGGER_CACHE_AUTH_JWT_SECRET`, `DAGGER_CACHE_DATABASE_PATH`, `DAGGER_CACHE_AUTH_BOOTSTRAP_ADMIN_PASSWORD`.

## 6. Backend Implementation

Conventions (AGENTS.md): `fmt.Sprintf` only, errors wrapped with `%w`,
`logrus.WithFields`, dependency rule `handler → service → domain ← repository`,
stdlib-only `domain`, 100% coverage target, stdlib `testing` only.

### 6.1 `internal/repository/sqlite.go`

```go
//go:embed schema.sql
var schemaSQL string

func OpenSQLite(path string) (*sql.DB, error)
// DSN: fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
// MkdirAll(filepath.Dir(path), 0o755); sql.Open("sqlite", dsn); db.Ping();
// db.SetMaxOpenConns(4) (WAL allows concurrent readers; small pool avoids lock churn).
// Errors: fmt.Errorf("open sqlite %s: %w", path, err)

func Migrate(ctx context.Context, db *sql.DB) error
// tx; SELECT COUNT(*) FROM schema_migrations WHERE version=1 (create schema_migrations
// first via CREATE TABLE IF NOT EXISTS); if absent exec schemaSQL, insert migration row.

type MetaStore struct{ db *sql.DB }
func NewMetaStore(db *sql.DB) *MetaStore
func (m *MetaStore) Get(ctx context.Context, key string) (string, error) // sql.ErrNoRows -> ("", domain.ErrNotFound)
func (m *MetaStore) Set(ctx context.Context, key, value string) error    // upsert
```

All repos take `*sql.DB` in their constructor, e.g. `NewUserRepo(db *sql.DB) *UserRepo`, with `var _ domain.UserRepository = (*UserRepo)(nil)` guards. Error mapping: `sql.ErrNoRows` → `domain.ErrNotFound` wrapped (`fmt.Errorf("get user %s: %w", id, domain.ErrNotFound)`); unique-constraint violations → `fmt.Errorf("...: %w", err)` with message including the duplicate value. `COLLATE NOCASE` gives case-insensitive uniqueness for usernames/group/project names.

Repo method shapes (representative):

```go
// user_repo.go
func (r *UserRepo) Create(ctx context.Context, u *domain.User) error
func (r *UserRepo) Get(ctx context.Context, id string) (*domain.User, error)
// ... scan helper scanUser(row scanner) (*domain.User, error)

// group_repo.go — SetMembers runs in a tx: DELETE FROM user_groups WHERE group_id=?; INSERT each.
// AllMemberships: SELECT group_id, user_id FROM user_groups -> map.

// tracemeta_repo.go
func (r *TraceMetaRepo) UpsertProvision(ctx, traceID, userID string) error
// INSERT INTO trace_meta(trace_id,user_id,updated_at) VALUES(?,?,?)
// ON CONFLICT(trace_id) DO UPDATE SET user_id=COALESCE(NULLIF(trace_meta.user_id,''), excluded.user_id), updated_at=excluded.updated_at
func (r *TraceMetaRepo) UpsertIngest(ctx, m *domain.TraceMeta) error
// ON CONFLICT(trace_id) DO UPDATE SET
//   group_id = COALESCE(trace_meta.group_id, excluded.group_id),
//   project_name/status/version/ci_provider/ci_repo/duration_ms/started_at = take non-empty newer values,
//   updated_at = excluded.updated_at
func (r *TraceMetaRepo) List(ctx, f domain.TraceFilter) ([]*domain.TraceListResult, error)
// WHERE clause built from filter:
//   admin (GroupIDs empty & IncludeUnassigned): no restriction
//   user: (group_id IN (?...) OR (group_id IS NULL AND user_id = ?))
// LEFT JOIN groups g ON g.id = tm.group_id LEFT JOIN users u ON u.id = tm.user_id
// ORDER BY COALESCE(tm.started_at, tm.updated_at) DESC LIMIT ?
```

### 6.2 `internal/service/user_service.go`

```go
type UserService struct { users domain.UserRepository; groups domain.GroupRepository; logger *logrus.Logger }
func NewUserService(users domain.UserRepository, groups domain.GroupRepository, logger *logrus.Logger) *UserService

func (s *UserService) Create(ctx context.Context, username, password string, role domain.Role) (*domain.User, error)
// validate: username regex ^[a-zA-Z0-9][a-zA-Z0-9._-]{1,63}$ ; password len >= 8;
// role via domain.ParseRole; bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost);
// ID = newID(); users.Create. Duplicate username -> wrap ErrForbidden? No: return wrapped err, handler maps unique violation to 409.

func (s *UserService) Authenticate(ctx context.Context, username, password string) (*domain.User, error)
// GetByUsername; user.PasswordHash == "" -> ErrInvalidCredential;
// bcrypt.CompareHashAndPassword -> ErrInvalidCredential on mismatch (never reveal which part failed).

func (s *UserService) Get / List / UpdateRole(ctx, id string, role domain.Role) / Delete(ctx, id string)
// Delete cascades tokens+memberships via FK; also log with fields user_id/username.
func (s *UserService) ResetPassword(ctx, id, newPassword string) error      // admin-set; validate len
func (s *UserService) ChangePassword(ctx, id, current, newPassword string) error // verify current first
func (s *UserService) EnsureOAuthUser(ctx context.Context, provider, oauthID, username string) (*domain.User, bool, error)
// GetByOAuth -> (user,false,nil); else create with unique username (suffix "-2","-3"... on collision),
// empty password hash, role user -> (user,true,nil)
```

### 6.3 `internal/service/group_service.go`

```go
type GroupService struct { groups domain.GroupRepository; logger *logrus.Logger }
func NewGroupService(...) *GroupService
func (s *GroupService) Create(ctx, in GroupInput) (*domain.Group, error)
// GroupInput{Name, Description string; MaxRunnerSessions int; AgentAvailable bool; AutoAssignPattern string}
// validate: name regex ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$; MaxRunnerSessions >= 0;
// if AutoAssignPattern != "" -> regexp.Compile must succeed (reject with wrapped error).
func (s *GroupService) Get / List / Update(ctx, id string, in GroupInput) / Delete(ctx, id string)
// Update revalidates pattern; Delete relies on FK ON DELETE (memberships, projects->NULL, trace_meta->NULL).
func (s *GroupService) SetMembers(ctx, groupID string, userIDs []string) error // verify each user exists first
func (s *GroupService) Members(ctx, groupID string) ([]*domain.User, error)
func (s *GroupService) GroupsForUser(ctx, userID string) ([]*domain.Group, error)
```

### 6.4 `internal/service/project_service.go`

```go
type ProjectService struct { projects domain.ProjectRepository; groups domain.GroupRepository; logger *logrus.Logger }
func NewProjectService(...) *ProjectService
func (s *ProjectService) Create(ctx, name, groupID string) (*domain.Project, error) // verify group exists when groupID != ""
func (s *ProjectService) Get / List / Delete
func (s *ProjectService) Assign(ctx, id, groupID string) (*domain.Project, error) // groupID "" = unassign
func (s *ProjectService) GetOrCreateByName(ctx, name string) (*domain.Project, error)
// race-safe: try GetByName; on ErrNotFound Create; on unique-violation retry GetByName once.
```

### 6.5 `internal/service/token_service.go`

```go
type TokenService struct { tokens domain.APITokenRepository; logger *logrus.Logger }
func NewTokenService(...) *TokenService

func (s *TokenService) Generate(ctx context.Context, userID string) (plaintext string, meta *domain.APIToken, err error)
// if GetByUser succeeds -> ("", nil, domain.ErrTokenExists)
// plaintext = newPlaintextToken(); upsert; return plaintext once.
func (s *TokenService) Regenerate(ctx, userID string) (string, *domain.APIToken, error) // upsert replaces hash; old token dead immediately
func (s *TokenService) Revoke(ctx, userID string) error
func (s *TokenService) Meta(ctx, userID string) (*domain.APIToken, error) // ErrNotFound when none
func (s *TokenService) Validate(ctx context.Context, plaintext string) (*domain.APIToken, error)
// sha256 hex -> GetByHash; on success TouchLastUsed (best-effort: log error, don't fail auth)

func newPlaintextToken() string // b:=make([]byte,32); crypto/rand.Read; fmt.Sprintf("dct_%s", hex.EncodeToString(b))
func HashAPIToken(plaintext string) string // sha256 hex
```

### 6.6 `internal/service/jwt_service.go`

```go
type Claims struct {
    UserID   string   `json:"uid"`
    Username string   `json:"username"`
    Role     string   `json:"role"`
    GroupIDs []string `json:"groups"`
    Type     string   `json:"typ"` // "access" | "refresh" | "oauth_state"
    jwt.RegisteredClaims
}

type JWTService struct { secret []byte; accessTTL, refreshTTL time.Duration; clock func() time.Time }
func NewJWTService(secret []byte, accessTTL, refreshTTL time.Duration) *JWTService
func (s *JWTService) IssuePair(u *domain.User, groupIDs []string) (access, refresh string, err error)
func (s *JWTService) ParseAccess(token string) (*Claims, error)   // validates sig, exp, typ=="access"
func (s *JWTService) ParseRefresh(token string) (*Claims, error)  // typ=="refresh"
func (s *JWTService) IssueOAuthState(redirectPath string) (string, error) // typ=="oauth_state", 10m TTL
func (s *JWTService) ParseOAuthState(token string) (*Claims, error)
// Parse uses jwt.ParseWithClaims + jwt.WithValidMethods([]string{"HS256"});
// errors wrapped: fmt.Errorf("parse access token: %w", err)
```

### 6.7 `internal/service/auth_service.go`

```go
type AuthServiceConfig struct {
    Disabled      bool   // auth.internal.enabled == false
    LegacyEnabled bool   // tokens_file configured
}

type AuthService struct {
    cfg      AuthServiceConfig
    users    *UserService
    groups   domain.GroupRepository
    tokens   *TokenService
    jwt      *JWTService
    legacy   domain.TokenValidator // existing flat-file validator (nil when no tokens_file)
    logger   *logrus.Logger
}
func NewAuthService(cfg AuthServiceConfig, users *UserService, groups domain.GroupRepository,
    tokens *TokenService, jwtSvc *JWTService, legacy domain.TokenValidator, logger *logrus.Logger) *AuthService

func (a *AuthService) Resolve(ctx context.Context, bearer string) (*domain.Identity, error)
// 1. cfg.Disabled -> &Identity{UserID:"anonymous", Username:"anonymous", Role: admin, Method: AuthNone}
// 2. bearer == "" -> ErrUnauthenticated
// 3. strings.HasPrefix(bearer, "dct_") -> tokens.Validate -> load user -> groups -> Identity{Method: AuthAPIToken}
//    (user deleted between hash lookup and load -> ErrUnauthenticated)
// 4. jwt.ParseAccess -> load user by claims.UserID (must still exist; role read fresh from DB)
//    -> groups fresh from groupRepo.GroupsForUser -> Identity{Method: AuthJWT}
// 5. legacy != nil && legacy.ValidateToken(bearer) ok ->
//    &Identity{UserID:"legacy", Username:"legacy", Role: admin, Method: AuthLegacyTok}
// 6. ErrUnauthenticated
// All failures logged at Debug with method field; never log the token.

func (a *AuthService) Login(ctx context.Context, username, password string) (access, refresh string, u *domain.User, err error)
// users.Authenticate -> groups -> jwt.IssuePair; log login with username field (no password).
func (a *AuthService) Refresh(ctx context.Context, refreshToken string) (access, refresh string, err error)
// ParseRefresh -> load user (must exist) -> groups -> IssuePair (rotation).
```

### 6.8 `internal/service/quota_service.go`

```go
type QuotaService struct {
    sessions domain.SessionStore
    groups   domain.GroupRepository
    logger   *logrus.Logger
}
func NewQuotaService(sessions domain.SessionStore, groups domain.GroupRepository, logger *logrus.Logger) *QuotaService

func (q *QuotaService) CheckEngineAccess(ctx context.Context, id *domain.Identity) error
// id.IsAdmin() -> nil
// gs := groups.GroupsForUser(id.UserID); len==0 -> domain.ErrNoGroups
// available := filter(gs, AgentAvailable); len==0 -> domain.ErrAgentUnavailable
// usage := q.usageSnapshot(ctx) // map groupID -> active session count
// for g in available: if g.MaxRunnerSessions == 0 || usage[g.ID] < g.MaxRunnerSessions -> nil
// -> domain.ErrQuotaExhausted (log WithFields user_id, groups, usage)

func (q *QuotaService) UsageByGroup(ctx context.Context) (map[string]int, error) // for admin UI / GET /api/v1/groups
func (q *QuotaService) usageSnapshot(ctx context.Context) map[string]int
// memberships := groups.AllMemberships(); leases := sessions.List()
// perLease := count leases by UserID; for groupID,userIDs in memberships: sum perLease[uid]
// (a multi-group user's lease counts against EACH of their groups — decision D3)
```

### 6.9 `internal/service/attribution_service.go`

```go
type AttributionService struct {
    projects  *ProjectService
    groups    domain.GroupRepository
    traceMeta domain.TraceMetaRepository
    logger    *logrus.Logger
}
func NewAttributionService(...) *AttributionService

func (a *AttributionService) Provision(ctx context.Context, traceID, userID string)
// best-effort: traceMeta.UpsertProvision; errors logged WithError + trace_id, never returned.

func (a *AttributionService) Ingest(ctx context.Context, traceID, userID, ciRepo, ciProvider, version, status string, durationMS int64, startedAt time.Time)
// if ciRepo != "": proj := projects.GetOrCreateByName(ciRepo)
//   groupID := proj.GroupID
//   if groupID == "": groupID = a.autoAssign(ctx, proj.Name) // regex scan
//     autoAssign: gs,_ := groups.List(); for each g with AutoAssignPattern != "" (sorted by ID):
//       regexp.Compile (skip+warn on invalid); if MatchString(name) -> persist proj.GroupID=g.ID via projects.Assign; return g.ID
// traceMeta.UpsertIngest(&TraceMeta{...}) // group set-once handled in SQL COALESCE
// all errors logged, not returned (ingest must never break telemetry forwarding).
```

### 6.10 `internal/service/otlp_extract.go`

```go
type TraceIngestSummary struct {
    TraceID    string
    CIRepo     string // from root span attr "dagger.io/ci.repo"
    CIProvider string // "dagger.io/ci"
    Version    string // "dagger.io/engine.version"
    Status     string // root span status code mapping (reuse trace_store mapping semantics)
    DurationMS int64  // root span end-start when both present
    StartedAt  time.Time
}

func ExtractTraceSummaries(body []byte) []TraceIngestSummary
// json.Unmarshal into minimal struct mirroring OTLP/HTTP JSON:
// {resourceSpans:[{scopeSpans:[{spans:[{traceId,spanId,parentSpanId,name,status:{code},
//   startTimeUnixNano,endTimeUnixNano,attributes:[{key,value:{stringValue}}]}]}]}]}
// (tolerant: accept top-level "batches" shape too, matching trace_store.extractSpans style).
// Root span = span whose parentSpanID is absent among this payload's span IDs.
// One summary per distinct root trace; parse errors -> nil (caller proceeds without metadata).
```

### 6.11 `internal/service/oauth_github.go`

```go
type GitHubOAuthService struct {
    clientID, clientSecret, redirectURL string
    allowedOrgs []string
    defaultGroup string
    tokenURL, apiBaseURL string // overridable in tests (defaults: github.com endpoints)
    http *http.Client            // 10s timeout
    users *UserService
    groups domain.GroupRepository
    jwt *JWTService
    logger *logrus.Logger
}
func NewGitHubOAuthService(cfg domain.OAuthConfig, users *UserService, groups domain.GroupRepository,
    jwtSvc *JWTService, logger *logrus.Logger) *GitHubOAuthService

func (s *GitHubOAuthService) LoginURL(state string) string
// fmt.Sprintf("https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&scope=read:org&state=%s", ...)

func (s *GitHubOAuthService) Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error)
// 1. POST tokenURL {client_id, client_secret, code, redirect_uri} Accept: application/json -> access_token
// 2. GET apiBaseURL/user -> id (int), login
// 3. GET apiBaseURL/user/orgs -> []{login}
// 4. if len(allowedOrgs) > 0 and no intersection -> ErrForbidden ("org not allowed")
// 5. user, created := users.EnsureOAuthUser(ctx, "github", strconv.Itoa(id), login)
// 6. if created && defaultGroup != "": g, err := groups.GetByName(defaultGroup); if ok -> groups.SetMembers append
//    (best-effort: log on error)
// 7. groups fresh -> jwt.IssuePair
// All GitHub calls carry Authorization: Bearer <access_token> and User-Agent: dagger-cache.
```

### 6.12 `internal/service/legacy_import.go`

```go
type LegacyImportResult struct { Imported, Skipped int; Usernames []string }

func ImportTokensFile(ctx context.Context, path string, users *UserService, tokens *TokenService,
    groups *GroupService, logger *logrus.Logger) (*LegacyImportResult, error)
// Read file (os.ReadFile; wrap errors); split lines; trim; skip "" and "#".
// Ensure group "legacy" exists (Create with AgentAvailable=true, MaxRunnerSessions=0; ignore ErrExists-equivalent).
// For each token i: username := fmt.Sprintf("legacy-%d", i+1)
//   skip if tokens.Validate(ctx, token) already succeeds (idempotency by hash)
//   create user (role user, random 32-hex password never disclosed), upsert API token with RAW token
//   (token_service helper ImportRaw(ctx, userID, rawToken) — same hashing path), add to "legacy" group.
// Log summary WithFields imported/skipped.
```

### 6.13 Handler layer

#### `internal/handler/middleware.go`

```go
const identityKey = "auth_identity"

func (s *Server) resolveIdentity(c *app.RequestContext) (*domain.Identity, bool)
// bearer, err := extractToken(c) — extraction failure degrades to "" (auth-disabled parity)
// id, err := s.auth.Resolve(ctx, bearer); err -> writeError(c, 401, "unauthorized"); false
// c.Set(identityKey, id); return id, true

func (s *Server) requireAuth(c *app.RequestContext) bool      // resolveIdentity wrapper (keeps call sites identical)
func (s *Server) requireAdmin(c *app.RequestContext) (*domain.Identity, bool) // 401 then 403 "forbidden"
func identityOf(c *app.RequestContext) *domain.Identity       // c.Get(identityKey).(*domain.Identity)

func (s *Server) authorizeTrace(c *app.RequestContext, traceID string) (*domain.TraceMeta, bool)
// meta, err := s.traceMeta.Get(ctx, traceID)
// if err (not found): admin -> (nil, true) [fail-closed for non-admin: 404 "trace not found"]
// if found: admin ok; owner (meta.UserID == id.UserID) ok; member (meta.GroupID != "" && id.HasGroup(meta.GroupID)) ok;
//   else 404 "trace not found" (404, not 403: don't leak existence)

func (s *Server) writeServiceError(c *app.RequestContext, err error)
// maps domain sentinels: ErrNotFound->404, ErrTokenExists->409, ErrInvalidCredential->401,
// ErrForbidden->403, ErrNoGroups/ErrAgentUnavailable->403, ErrQuotaExhausted->429,
// unique-violation messages->409, default->500; logs 5xx WithError.
```

#### `internal/handler/auth.go` (rewrite)

`extractToken` kept; add query fallback (D14): after header schemes fail,
`if t := string(c.Query("token")); t != "" { return t, nil }`.
`authenticate`/old `requireAuth` removed (moved to middleware.go).

#### `internal/handler/auth_endpoints.go`

```go
func (s *Server) handleLogin(ctx, c)        // POST /api/v1/auth/login (public)
func (s *Server) handleRefresh(ctx, c)      // POST /api/v1/auth/refresh (public)
func (s *Server) handleMe(ctx, c)           // GET /api/v1/auth/me (auth) -> user + groups + role
func (s *Server) handleProviders(ctx, c)    // GET /api/v1/auth/providers (public) -> {"internal":true,"oauth_github":bool}
func (s *Server) handleChangePassword(ctx, c) // PUT /api/v1/auth/password (auth)
func (s *Server) handleOAuthLogin(ctx, c)   // GET /api/v1/auth/oauth/github/login (public) -> 302
func (s *Server) handleOAuthCallback(ctx, c) // GET /api/v1/auth/oauth/github/callback (public) -> 302 to SPA fragment
// handleOAuthCallback: validate state JWT; s.oauth.Complete(code);
// on error 302 to "/auth/login?error=oauth"; on success
// c.Redirect(consts.StatusFound, []byte(fmt.Sprintf("/auth/callback#access_token=%s&refresh_token=%s", a, r)))
// oauth == nil (disabled) -> 404.
```

#### `internal/handler/users.go` (admin unless noted)

```go
handleUsersList      // GET /api/v1/users — includes groups per user + token metadata (masked)
handleUserCreate     // POST /api/v1/users {username,password,role}
handleUserGet        // GET /api/v1/users/:id
handleUserUpdate     // PUT /api/v1/users/:id {role}
handleUserDelete     // DELETE /api/v1/users/:id (409 when deleting self)
handleUserResetPassword // PUT /api/v1/users/:id/password {password}
handleUserGroups     // PUT /api/v1/users/:id/groups {group_ids:[...]} (full replace)
handleUserTokenMeta  // GET /api/v1/users/:id/token (masked metadata)
handleUserTokenRevoke// DELETE /api/v1/users/:id/token
```

#### `internal/handler/groups.go` (admin)

```go
handleGroupsList   // GET /api/v1/groups — includes member_count + active_sessions (quota usage)
handleGroupCreate  // POST /api/v1/groups {name,description,max_runner_sessions,agent_available,auto_assign_pattern}
handleGroupGet     // GET /api/v1/groups/:id
handleGroupUpdate  // PUT /api/v1/groups/:id (same body)
handleGroupDelete  // DELETE /api/v1/groups/:id
handleGroupMembers // GET /api/v1/groups/:id/members
handleGroupSetMembers // PUT /api/v1/groups/:id/members {user_ids:[...]}
```

#### `internal/handler/projects.go` (admin)

```go
handleProjectsList   // GET /api/v1/projects (group name joined)
handleProjectCreate  // POST /api/v1/projects {name, group_id}
handleProjectUpdate  // PUT /api/v1/projects/:id {group_id} ("" = unassign)
handleProjectDelete  // DELETE /api/v1/projects/:id
```

#### `internal/handler/tokens.go` (self-service, any authenticated)

```go
handleMyTokenMeta       // GET /api/v1/tokens/me — masked metadata or 404
handleMyTokenCreate     // POST /api/v1/tokens/me — 201 {"token":"dct_…"} once; 409 if exists
handleMyTokenRegenerate // PUT /api/v1/tokens/me/regenerate — 200 {"token":"dct_…"} once
handleMyTokenRevoke     // DELETE /api/v1/tokens/me — 204
```

#### `internal/handler/server.go` changes

```go
type Deps struct {
    Logger          *logrus.Logger
    Metrics         *observ.Metrics
    MintingCA       domain.MintingCA
    FleetManager    *service.Manager
    Sessions        domain.SessionStore
    CacheBackend    domain.CacheBackend
    VersionResolver domain.VersionResolver
    Auth            *service.AuthService
    Users           *service.UserService
    Groups          *service.GroupService
    Projects        *service.ProjectService
    Tokens          *service.TokenService
    Quota           *service.QuotaService
    Attribution     *service.AttributionService
    TraceMeta       domain.TraceMetaRepository
    Traces          domain.TraceRepository
    Logs            domain.LogRepository
    OAuth           *service.GitHubOAuthService // nil when disabled
}

func NewServer(cfg *ServerConfig, deps Deps) *Server
// ServerConfig: drop TokensFile (legacy handled inside AuthService).
```

Route additions in `configure()`:

```go
// public
h.POST("/api/v1/auth/login", s.handleLogin)
h.POST("/api/v1/auth/refresh", s.handleRefresh)
h.GET("/api/v1/auth/providers", s.handleProviders)
h.GET("/api/v1/auth/oauth/github/login", s.handleOAuthLogin)
h.GET("/api/v1/auth/oauth/github/callback", s.handleOAuthCallback)
// self
h.GET("/api/v1/auth/me", s.handleMe)
h.PUT("/api/v1/auth/password", s.handleChangePassword)
h.GET("/api/v1/tokens/me", s.handleMyTokenMeta)
h.POST("/api/v1/tokens/me", s.handleMyTokenCreate)
h.PUT("/api/v1/tokens/me/regenerate", s.handleMyTokenRegenerate)
h.DELETE("/api/v1/tokens/me", s.handleMyTokenRevoke)
// admin
h.GET("/api/v1/users", s.handleUsersList)          // + POST, GET/PUT/DELETE /:id, PUT /:id/password, PUT /:id/groups, GET/DELETE /:id/token
h.GET("/api/v1/groups", s.handleGroupsList)        // + POST, GET/PUT/DELETE /:id, GET/PUT /:id/members
h.GET("/api/v1/projects", s.handleProjectsList)    // + POST, PUT/DELETE /:id
```

`handleEngines` modifications (existing flow preserved):

```
id, ok := s.requireAuth(c)            // now identity-aware
...body/version checks unchanged...
if err := s.quota.CheckEngineAccess(ctx, id); err != nil {
    s.writeServiceError(c, err)       // 403 / 429 with sentinel messages
    return
}
...fleetManager.Acquire unchanged...
s.sessions.Register(fp, verStr, pod, instanceID, req.TraceID, id.UserID)
s.attribution.Provision(ctx, req.TraceID, id.UserID)   // best-effort
resp.UserID = id.Username
```

`handleOTel` modifications:

```
id, ok := s.requireAuth(c)
if body, err := c.Body(); err == nil {                 // Hertz buffers; proxy still works after read
    for _, sum := range service.ExtractTraceSummaries(body) {
        s.attribution.Ingest(ctx, sum.TraceID, id.UserID, sum.CIRepo, sum.CIProvider, sum.Version, sum.Status, sum.DurationMS, sum.StartedAt)
    }
}
...existing proxy logic unchanged...
```

`handleTracesList` rewrite (replaces stub):

```
id, ok := s.requireAuth(c)
f := domain.TraceFilter{Limit: clampLimit(c.Query("limit"))} // default 100, max 500
if !id.IsAdmin() { f.GroupIDs = id.GroupIDs; f.UserID = id.UserID }
// admin may pass ?group_id=<id> (repeatable) to narrow; "unassigned" keyword -> admin-only unassigned view
res, err := s.traceMeta.List(ctx, f) -> writeJSON
```

`handleTracesDetail` / `handleTracesLogs` / `handleTracesLive` / `handleLogsRoutes`: gate with `s.authorizeTrace(c, traceID)` before delegating to existing logic.

#### `cmd/api/main.go` changes

```go
// in run():
db, err := repository.OpenSQLite(cfg.Database.Path)      // fatal on error
defer db.Close()
if err := repository.Migrate(ctx, db); err != nil { fatal }

jwtSecret := []byte(cfg.Auth.JWT.Secret)
if len(jwtSecret) == 0 {
    jwtSecret, err = loadOrCreateJWTSecret(ctx, repository.NewMetaStore(db)) // 32 random bytes hex; log info "generated"
}
jwtSvc := service.NewJWTService(jwtSecret, cfg.Auth.JWT.AccessTTL, cfg.Auth.JWT.RefreshTTL)

userRepo := repository.NewUserRepo(db); groupRepo := ...; projectRepo := ...; tokenRepo := ...; traceMetaRepo := ...
usersSvc := service.NewUserService(...); groupsSvc := ...; projectsSvc := ...; tokensSvc := ...
authSvc := service.NewAuthService(service.AuthServiceConfig{
    Disabled: !cfg.Auth.Internal.Enabled,
    LegacyEnabled: cfg.Auth.Internal.TokensFile != "",
}, usersSvc, groupRepo, tokensSvc, jwtSvc,
   service.NewTokenValidator(cfg.Auth.Internal.TokensFile, cfg.Auth.Internal.Enabled, logger), logger)

if err := bootstrapAdmin(ctx, cfg, usersSvc, logger); err != nil { fatal }
// bootstrapAdmin: usersSvc.Count==0 -> password := cfg...Password or random; create; log WARN once with
// fields username + generated=true when random (the only place a credential is ever logged).

quotaSvc, attributionSvc wired; oauth := nil; if cfg.Auth.OAuth.Enabled { oauth = service.NewGitHubOAuthService(...) }

server := handler.NewServer(&handler.ServerConfig{...}, handler.Deps{...})

// new CLI command alongside existing Action:
app.Commands = []*cli.Command{{
    Name: "migrate-tokens",
    Usage: "import flat-file tokens as users with API tokens",
    Flags: []cli.Flag{ &cli.StringFlag{Name:"config", Value:"config/config.app.yaml"},
                       &cli.StringFlag{Name:"tokens-file"}, &cli.BoolFlag{Name:"dry-run"} },
    Action: runMigrateTokens, // load cfg, open DB, service.ImportTokensFile; print result
}}
```

Shutdown: `db.Close()` after `server.Shutdown`.

## 7. Frontend Implementation

Stack unchanged: Vue 3 Composition API + TS, Pinia, Vue Router, Axios. No new deps.

### `ui/src/api/types.ts` (new)

```ts
export type Role = 'admin' | 'user'
export interface AuthUser { id: string; username: string; role: Role; groups: GroupSummary[]; oauth_provider?: string }
export interface GroupSummary { id: string; name: string }
export interface Group extends GroupSummary { description: string; max_runner_sessions: number; agent_available: boolean; auto_assign_pattern: string; member_count?: number; active_sessions?: number; created_at: string }
export interface UserRow { id: string; username: string; role: Role; oauth_provider?: string; groups: GroupSummary[]; created_at: string; token?: TokenMeta | null }
export interface TokenMeta { id: string; prefix: string; created_at: string; last_used_at: string | null }
export interface Project { id: string; name: string; group_id: string; group_name?: string; created_at: string }
export interface TraceRow { trace_id: string; status: string; version: string; duration_ms: number; ci_provider: string; ci_repo: string; project_name: string; group_id: string; group_name: string; username: string; started_at: string }
export interface Providers { internal: boolean; oauth_github: boolean }
```

### `ui/src/stores/auth.ts` (rewrite)

State: `token`, `refreshToken` (localStorage as today), `user: AuthUser | null`.
Getters: `isAuthenticated` (`!!token`), `isAdmin` (`user?.role === 'admin'`), `groups`.
Actions:
- `login(username, password)` → `POST /api/v1/auth/login` → store tokens, set `user` from response.
- `setTokens(access, refresh)` (used by OAuth callback).
- `fetchMe()` → `GET /api/v1/auth/me` → set `user`; on 401 clear.
- `refreshSession()` → `POST /api/v1/auth/refresh` with `{refresh_token}` → update both tokens; single in-flight promise to dedupe concurrent calls.
- `logout()` → clear state + localStorage, redirect handled by router guard.

### `ui/src/api/client.ts` (modified)

- Request interceptor unchanged (Bearer).
- Response interceptor: on 401, if request URL is not `/api/v1/auth/login`/`/api/v1/auth/refresh` and `refreshToken` exists → `await auth.refreshSession()` then retry original request once; on refresh failure → `auth.logout()` + `window.location.href = '/auth/login'`.
- New functions (all typed via `types.ts`): `fetchProviders`, `fetchMe`, `loginRequest`, `refreshRequest`, `changePassword`, `listUsers/createUser/updateUser/deleteUser/resetPassword/setUserGroups`, `listGroups/createGroup/updateGroup/deleteGroup/getGroupMembers/setGroupMembers`, `listProjects/createProject/updateProject/deleteProject`, `getMyToken/createMyToken/regenerateMyToken/revokeMyToken`, `fetchTraces` (updated return type + optional `group_id` param).
- `connectLiveTrace` switched to SSE-friendly URL with token query param: `new EventSource(\`/api/v1/traces/${id}/live?token=${encodeURIComponent(auth.token)}\`)` (backend SSE handler already exists; old WebSocket usage was dead code).

### `ui/src/router/index.ts`

- Route meta: `{ public: true }` on `/auth/login`, `/auth/callback`; `{ admin: true }` on new admin routes.
- New routes: `/admin/users`, `/admin/groups`, `/admin/projects` (lazy imports from `@/views/admin/*`).
- Global `beforeEach`: `const auth = useAuthStore()`; if target not public and `!auth.isAuthenticated` → redirect `/auth/login?redirect=<to.fullPath>`; if `auth.isAuthenticated && !auth.user` → `await auth.fetchMe()` (bootstraps session from stored token); if target meta.admin and `!auth.isAdmin` → redirect `/pipelines`.

### `ui/src/auth/Login.vue` (rewrite)

- Username + password fields → `auth.login()`; error box on failure.
- On mount `fetchProviders()`; when `oauth_github` → "Login with GitHub" button → `window.location.href = '/api/v1/auth/oauth/github/login'`.
- Honors `?error=oauth` query (shows message) and `?redirect` after success.

### `ui/src/auth/Callback.vue` (rewrite)

`onMounted`: parse `window.location.hash` (`#access_token=…&refresh_token=…`) via `URLSearchParams`; if present → `auth.setTokens(...)` → `await auth.fetchMe()` → `history.replaceState` to strip hash → push `/pipelines`; else push `/auth/login`.

### `ui/src/App.vue`

Nav: existing links + when `auth.isAdmin`: Users, Groups, Projects (route links). Right side: username + role badge + Logout (calls `auth.logout()` then router to `/auth/login`).

### `ui/src/views/Settings.vue` (rewrite — user self-service)

Sections:
1. **Profile**: username, role, OAuth provider.
2. **My Groups**: read-only list from `auth.user.groups`.
3. **API Token**: on mount `getMyToken()`; if 404 → "Generate token" button (`createMyToken`); else masked meta (`prefix…`, created, last used) + **Regenerate** (confirm dialog: "your current CI token stops working immediately") + **Revoke**. Plaintext shown exactly once in a copyable `<code>` box with warning text. Usage hint: `DAGGER_CLOUD_TOKEN=<token>`.
4. **Change password**: current + new (min 8) → `changePassword`.
Admin quick-links to `/admin/*` when admin.

### `ui/src/views/admin/Users.vue` (new)

Table: username, role, groups (chips), token (prefix or "—"), created. Actions per row: role select (PUT), reset password (prompt for new), manage groups (checkbox list modal → `setUserGroups`), revoke token, delete (confirm; self-delete disabled). Create form: username/password/role.

### `ui/src/views/admin/Groups.vue` (new)

Table: name, members, active sessions / max (e.g. `3 / 8`, `∞` when max=0), agent_available toggle, pattern. Create/edit form with all fields incl. `auto_assign_pattern` (regex, validated server-side; show server error). Members editor: user checkbox list → `setGroupMembers`. Delete with confirm (warn: projects become unassigned).

### `ui/src/views/admin/Projects.vue` (new)

Table: name, group (select of groups + "Unassigned"), created. Inline group assignment via `updateProject`. Create form (name + optional group). Delete with confirm.

### `ui/src/views/Pipelines.vue` (modified)

- Uses updated `fetchTraces()` returning `TraceRow[]`; columns: Trace ID, Status, Version, Duration, CI, **Group**, **Project** (falls back to `ci_repo`).
- Admin-only filter bar: group select (All / Unassigned / each group) → query param.
- Empty-state text unchanged.

### UI build/embed

No change to pipeline: `vite build` → `ui/dist` → copied to `internal/handler/ui-dist/` (Dockerfile). SPA fallback in `serveUI` already serves `/auth/callback`, `/admin/*` as index.html.

## 8. API Endpoints (full spec)

Auth column: `public` = no auth; `auth` = any valid identity; `admin` = role admin.
Error shape everywhere: `{"message": "..."}` (existing `ErrorResponse`).

| Method | Path | Auth | Request body | Success response |
|--------|------|------|--------------|------------------|
| POST | `/api/v1/auth/login` | public | `{"username":"alice","password":"…"}` | `200 {"access_token","refresh_token","user":{id,username,role,groups[]}}` |
| POST | `/api/v1/auth/refresh` | public | `{"refresh_token":"…"}` | `200 {"access_token","refresh_token"}` |
| GET | `/api/v1/auth/providers` | public | — | `200 {"internal":true,"oauth_github":false}` |
| GET | `/api/v1/auth/me` | auth | — | `200 {id,username,role,oauth_provider,groups:[{id,name}]}` |
| PUT | `/api/v1/auth/password` | auth | `{"current_password","new_password"}` | `204` |
| GET | `/api/v1/auth/oauth/github/login` | public | — | `302` → GitHub authorize URL |
| GET | `/api/v1/auth/oauth/github/callback` | public | query `code`,`state` | `302` → `/auth/callback#access_token=…&refresh_token=…` (or `/auth/login?error=oauth`) |
| GET | `/api/v1/users` | admin | — | `200 [UserRow]` |
| POST | `/api/v1/users` | admin | `{"username","password","role"}` | `201 UserRow`; 409 duplicate username; 400 validation |
| GET | `/api/v1/users/:id` | admin | — | `200 UserRow` |
| PUT | `/api/v1/users/:id` | admin | `{"role":"user"|"admin"}` | `200 UserRow` |
| DELETE | `/api/v1/users/:id` | admin | — | `204`; 409 deleting self |
| PUT | `/api/v1/users/:id/password` | admin | `{"password":"…"}` | `204` |
| PUT | `/api/v1/users/:id/groups` | admin | `{"group_ids":["…"]}` | `200 [GroupSummary]` (new memberships) |
| GET | `/api/v1/users/:id/token` | admin | — | `200 TokenMeta`; 404 none |
| DELETE | `/api/v1/users/:id/token` | admin | — | `204` |
| GET | `/api/v1/groups` | admin | — | `200 [Group + member_count + active_sessions]` |
| POST | `/api/v1/groups` | admin | `{"name","description","max_runner_sessions":0,"agent_available":true,"auto_assign_pattern":""}` | `201 Group`; 409 dup name; 400 bad pattern |
| GET | `/api/v1/groups/:id` | admin | — | `200 Group` |
| PUT | `/api/v1/groups/:id` | admin | same as create | `200 Group` |
| DELETE | `/api/v1/groups/:id` | admin | — | `204` |
| GET | `/api/v1/groups/:id/members` | admin | — | `200 [UserRow-lite]` |
| PUT | `/api/v1/groups/:id/members` | admin | `{"user_ids":["…"]}` | `204` |
| GET | `/api/v1/projects` | admin | — | `200 [Project]` |
| POST | `/api/v1/projects` | admin | `{"name":"github.com/acme/api","group_id":""}` | `201 Project` |
| PUT | `/api/v1/projects/:id` | admin | `{"group_id":"…"}` (`""` = unassign) | `200 Project` |
| DELETE | `/api/v1/projects/:id` | admin | — | `204` |
| GET | `/api/v1/tokens/me` | auth | — | `200 TokenMeta`; 404 none |
| POST | `/api/v1/tokens/me` | auth | — | `201 {"token":"dct_…"}` once; 409 exists |
| PUT | `/api/v1/tokens/me/regenerate` | auth | — | `200 {"token":"dct_…"}` once |
| DELETE | `/api/v1/tokens/me` | auth | — | `204` |

Modified existing endpoints:

| Method | Path | Change |
|--------|------|--------|
| POST | `/v1/engines` | Identity resolution; quota check (403 `ErrNoGroups`/`ErrAgentUnavailable`, 429 `ErrQuotaExhausted`); lease records user; trace provision attribution; response `user_id` populated |
| POST | `/v1/traces`, `/v1/logs`, `/v1/metrics` | Identity resolution; traces signal additionally runs best-effort attribution before proxying |
| GET | `/api/v1/traces` | Real scoped list from `trace_meta` (admin: all + optional `?group_id=` filter incl. `unassigned`; user: own groups + own unassigned); `?limit=` default 100 max 500 |
| GET | `/api/v1/traces/:traceID` / `…/logs` / `…/live` / `/api/v1/logs/:traceID` | `authorizeTrace` gate (owner/member/admin; unknown meta → admin-only; else 404) |
| GET | `/api/v1/fleet`, `/api/v1/cache`, `/v1/versions` | Any authenticated identity (unchanged surface) |
| Cache-host proxy | (Host-based) | Identity resolution replaces flat token check |

Status code conventions: 400 validation, 401 unauthenticated, 403 authenticated-but-not-allowed, 404 not found / hidden, 409 conflict (duplicate / token exists / self-delete), 429 quota.

## 9. Middleware Changes

1. **Identity resolution replaces token check.** `requireAuth` keeps its exact call-site signature (`bool`) but now stores `*domain.Identity` via `c.Set(identityKey, id)`; all downstream handlers retrieve it with `identityOf(c)`. Resolution order inside `AuthService.Resolve`: auth-disabled shortcut → API token (`dct_` prefix) → JWT access → legacy flat-file fallback → 401.
2. **Role gate.** `requireAdmin` = `requireAuth` + role check; writes 403 `{"message":"forbidden"}`. Used by all `/api/v1/users|groups|projects` handlers.
3. **Resource gate.** `authorizeTrace` (see 6.13) applied to every trace-scoped read, including SSE `/live` and Loki log routes. Fail-closed: missing metadata ⇒ admin-only.
4. **Quota gate.** `QuotaService.CheckEngineAccess` invoked in `handleEngines` before fleet acquisition; maps sentinels to 403/429.
5. **Unchanged:** `requestLog` middleware, `cacheHostMiddleware` position, data-plane mTLS (lease lookup now carries user attribution but flow is identical), SSE writer usage.
6. **Auth-disabled parity (D9):** with `auth.internal.enabled=false`, every request resolves to anonymous admin — identical observable behavior to today, including `/v1/engines` (quota bypassed for admin identity).

## 10. Migration (flat-file → multi-user)

Rollout is backward compatible; no big-bang.

**Phase 0 — deploy new binary.** DB auto-created at `database.path`; bootstrap admin created (random password logged once if unset). Existing CI keeps working:
- `DAGGER_CLOUD_TOKEN` values from the tokens file still authenticate via the **legacy fallback** and run as `legacy` admin identity (full access, quota bypass, sees all pipelines) — exactly today's behavior.
- UI users can log in with the bootstrap admin and manage users/groups/projects.

**Phase 1 — import tokens.** Operator runs:

```bash
supervisor migrate-tokens --config config.app.yaml [--tokens-file /etc/dagger-cache/tokens] [--dry-run]
```

Each token line becomes user `legacy-N` (role `user`) with that exact token as its API token, member of an auto-created `legacy` group (`agent_available=true`, unlimited). Idempotent (skips tokens already present by hash). Operator then reassigns these users/groups and project rules in the UI.

**Phase 2 — cutover.** Remove/empty `auth.internal.tokens_file` in config (Helm: `auth.tokens: []`, delete the mounted Secret). Legacy fallback disappears; only imported tokens, JWTs, and OAuth remain. Rotate bootstrap admin password; set `auth.jwt.secret` explicitly in secret storage (Helm Secret / env) before dropping the auto-generated one.

**Phase 3 — configure attribution.** Admin creates real groups, assigns users, sets `auto_assign_pattern` per group (e.g. `^github\.com/acme/.*`), pre-creates projects as needed. Historical traces without metadata remain admin-only (fail-closed by design).

**Deployment artifact changes:**
- Helm: `values.yaml` gains `supervisor.config.database.path`, `supervisor.config.auth.jwt.*`, `supervisor.persistence` (PVC for `/var/lib/dagger-cache`, default `enabled: false` + emptyDir with explicit warning that users/tokens are ephemeral); `configmap.yaml` renders new keys; `secret.yaml` optionally renders JWT secret; `deployment.yaml` mounts the volume; `auth.tokens` marked deprecated in comments.
- `deploy/k8s/supervisor.yaml`: add DB volume + `DAGGER_CACHE_DATABASE_PATH`.
- `deploy/docker/docker-compose.yaml`: mount `./data:/var/lib/dagger-cache`, add `DAGGER_CACHE_AUTH_BOOTSTRAP_ADMIN_PASSWORD` example, keep `deploy/docker/data/tokens` for the compat phase.
- GitHub OAuth apps: redirect URL must be changed to `…/api/v1/auth/oauth/github/callback` (doc callout).

## 11. Testing Strategy

Standard `testing` only, table-driven, `logrus` test logger via existing `observ.NewTestLogger()`, target 100% package coverage (AGENTS.md).

**Repository (SQLite):** each repo tested against `t.TempDir()` DB files. Cases: CRUD round-trips, case-insensitive uniqueness (username `Alice`/`alice` → conflict), FK cascades (delete user removes tokens/memberships; delete group nulls project/trace group_id), `SetMembers` replace semantics, `UpsertIngest` group set-once (second ingest with different group must not overwrite), `List` filter matrix (admin all / user groups+own-unassigned / limit), `Migrate` idempotency (run twice), WAL/pragma sanity (`PRAGMA journal_mode` returns `wal`).

**Services:**
- `user_service`: validation matrix (bad usernames, short passwords, bad role), bcrypt verify, duplicate username, `EnsureOAuthUser` collision suffixing.
- `group_service`: pattern compile rejection, name dup, membership ops.
- `token_service`: generate/409-on-second, regenerate invalidates old hash, validate round-trip, revoke, `LastUsedAt` touch.
- `jwt_service`: issue/parse access+refresh, wrong-typ rejection, expired, tampered signature, `WithValidMethods` (alg=none attack), state token TTL.
- `auth_service`: resolution order table — disabled mode, empty bearer, `dct_` token, JWT, legacy fallback hit/miss, deleted-user-with-valid-JWT, orphaned token hash.
- `quota_service`: admin bypass; no groups → `ErrNoGroups`; all groups `agent_available=false` → `ErrAgentUnavailable`; capacity matrix incl. `0=unlimited`, multi-group double-counting, usage snapshot correctness (stub `SessionStore` + stub `GroupRepository`).
- `attribution_service`: project auto-create, explicit assignment wins over regex, first-match-by-group-id regex ordering, invalid regex skipped without panic, group set-once.
- `otlp_extract`: real-shaped OTLP JSON fixtures (root span attrs, missing attrs, multiple traces per batch, malformed JSON → nil).
- `oauth_github`: `httptest.Server` standing in for GitHub (token exchange, user, orgs); allowed-orgs pass/fail; default-group auto-join; username collision.
- `legacy_import`: file parsing (comments/blanks), idempotent re-run, dry-run.

**Handlers:** `ut.PerformRequest` engines as in `server_test.go`. Full endpoint matrix per handler file: auth (login ok/bad creds/refresh rotation), RBAC (user hitting admin routes → 403; anonymous → 401), users/groups/projects CRUD incl. 409/400/404, tokens self-service (plaintext returned once, 409), traces visibility (admin vs member vs non-member vs unknown-trace fail-closed), engines quota 403/429 paths with stubbed services, OAuth callback redirect (fragment present, state mismatch → login redirect).

**Existing test updates:** `server_test.go`/`auth_test.go`/integration `api_test.go` rebuilt around `handler.Deps`; `session_test.go`, `fleet_test.go`, `k8s_manager_test.go` updated for `Register(..., userID)`; integration `TestProvisionEngineFlow` seeds a user + API token through services and authenticates with it (proving the real Dagger client contract: plain `Authorization: Bearer <token>`).

**Integration (`tests/integration/rbac_test.go`):** black-box scenario against a booted server with temp DB: bootstrap admin exists → login → create group (max_runner_sessions=1, agent_available=true) + user + membership → user token → `POST /v1/engines` 201 → second provision 429 (quota) → second group membership relaxes/admits per D3 → OTLP ingest fixture attributes trace to group via regex project rule → user sees trace in scoped list, non-member gets 404 → admin sees all → regenerate token invalidates old token (401) → flat-file legacy token still works until tokens_file removed.

**Frontend:** `npm run typecheck` (`vue-tsc --noEmit`) + `npm run build` gate. Manual verification checklist (login, refresh-on-401, admin nav gating, token show-once, group/project admin flows) documented in the PR.

**Repo-level gates:** `gofmt`/`goimports` (local prefix `github.com/disaster/dagger-kubernetes`), `go vet ./...`, `go test -race ./...`, Docker build (UI embed), `helm template` smoke.

## 12. Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| OTLP body read interferes with reverse proxy forwarding | Hertz buffers request bodies; integration test asserts collector forwarding still works post-read |
| SQLite write contention under ingest load | WAL + busy_timeout 5s + small pool; ingest writes are best-effort (logged, never block telemetry) |
| Stale JWT groups after membership change | Groups re-fetched from DB on every resolve; access TTL kept short (15m) |
| Multi-group quota double-count surprises operators | Documented in README + ADR; admin Groups view shows live `active_sessions / max` |
| Legacy fallback left enabled forever | README migration checklist + `migrate-tokens --dry-run`; tokens_file marked DEPRECATED in sample config |
| Random bootstrap password lost from logs | Documented: set `auth.bootstrap_admin.password` explicitly in production; password can be reset via API afterwards |
| OAuth redirect URL change breaks existing GitHub apps | Explicit doc callout + `/auth/login?error=oauth` surfacing |
| EventSource auth | `?token=` fallback (D14) limited to SSE route usage; tokens in URLs accepted there only |

## 13. Open Questions (explicitly deferred)

- Login rate limiting / lockout (future ADR; note in README security section).
- Refresh-token revocation list (stateless today; password change does not invalidate existing JWTs until expiry).
- Trace backfill of group metadata after project reassignment (set-once is intentional).
- Removing `auth.internal.tokens_file` support entirely (separate future release).

## 14. Ordered Task List

1. **Deps & domain**: `go get modernc.org/sqlite github.com/golang-jwt/jwt/v5`; promote x/crypto, x/oauth2; add all `internal/domain/*.go` from §3 (incl. `session.go` changes). `go build ./internal/domain`.
2. **Config**: `domain/config.go` additions, `config/loader.go` defaults, loader tests.
3. **SQLite layer**: `schema.sql`, `sqlite.go` (+MetaStore), all five repos, repo tests. `go test ./internal/repository`.
4. **Core services**: jwt, token, user, group, project services + tests.
5. **Auth & quota services**: auth_service (resolve/login/refresh), quota_service + tests.
6. **Attribution**: otlp_extract, attribution_service + tests.
7. **OAuth & legacy**: oauth_github, legacy_import + tests.
8. **Session store**: `service/session.go` new signature + `CountByUser`; fix `session_test.go`, `fleet_test.go`, `k8s_manager_test.go`.
9. **Handlers**: middleware.go, auth.go rewrite, auth_endpoints, users, groups, projects, tokens, traces/logs scoping, server.go `Deps` refactor + routes + engines/otel changes; fix `server_test.go`, `auth_test.go`; add all new handler tests.
10. **Wiring**: `cmd/api/main.go` (DB, bootstrap, services, `migrate-tokens` command).
11. **Integration tests**: update `api_test.go`, add `rbac_test.go`. `go test -race ./...`.
12. **Frontend**: types.ts → stores/auth.ts → api/client.ts → router → Login/Callback → App.vue → Settings.vue → admin views → Pipelines.vue. `npm run typecheck && npm run build`.
13. **Deploy artifacts**: Helm values/configmap/secret/deployment, k8s manifest, docker-compose.
14. **Docs**: `config/config.app.yaml.sample` + `config/config.app.yaml`, `docs/README.md` auth/groups/projects/migration rewrite, `docs/design/ADR-010-sqlite-multiuser-rbac.md`, `docs/design/index.md`.
15. **Final gates**: gofmt/goimports, `go vet ./...`, full `go test -race ./...`, UI build, Docker build, helm template.

## 15. Validation Checklist

- [ ] `go build ./...` and `go test -race ./...` green (incl. new packages at coverage target)
- [ ] Integration scenario §11 passes against real HTTP server + temp SQLite
- [ ] Fresh boot with empty dir: admin bootstrapped, UI login works, token generate → `POST /v1/engines` with `DAGGER_CLOUD_TOKEN` succeeds
- [ ] Legacy tokens file still authenticates (compat) and disappears when unconfigured
- [ ] Quota: 429 at limit, 403 without `agent_available`, admin bypass
- [ ] Visibility: non-member gets 404 on foreign trace; admin sees all; owner sees unassigned own traces
- [ ] OAuth (with stub or real GitHub app): callback lands in SPA with working session
- [ ] `vue-tsc` + UI build green; Docker image builds; helm template renders
