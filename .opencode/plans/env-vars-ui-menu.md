# Plan: UI menu for Dagger CLI connection environment variables (revised)

## Problem statement and goals

Users of the self-hosted dagger-kubernetes platform currently have to read
`docs/README.md` and hand-assemble the environment variables the Dagger CLI
needs to point at this Supervisor instead of Dagger Cloud:

- `DAGGER_CLOUD_URL`
- `DAGGER_CLOUD_TOKEN`
- `_EXPERIMENTAL_DAGGER_RUNNER_HOST`
- `_EXPERIMENTAL_DAGGER_TAG` (optional)
- `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` (optional, version-dependent)

Goal: add a UI menu/page that displays every required env var with its
**fully-resolved value, including the token plaintext**, offers one-click
copy of ready-to-paste snippets for bash/zsh, `.bashrc` sourcing, GitHub
Actions, GitLab CI, and generic `export` lines, and ships a short
user-facing doc explaining how to source the environment in an interactive
shell and in CI.

**Revision note (vs. v1):** The user explicitly relaxed the "API token
plaintext is shown exactly once" invariant so the page can serve a
fully-formed, ready-to-copy environment including the token on every visit.
This requires storing the token plaintext reversibly encrypted at rest
(see "Token-serving decision" below for the feasibility analysis and the
chosen approach).

## Token-serving decision (feasibility analysis)

### The hard constraint

The plaintext token is **not** stored today — only the SHA-256 hash is
persisted (`internal/service/token_service.go:106` `HashAPIToken`,
`internal/repository/token_repo.go:14` `tokenCols`). Therefore "serve the
plaintext on every page load" is impossible without one of:

- **(a) Regenerate on every reveal** — invalidates the existing CI token
  each time the user opens the page. Unacceptable UX: the user's CI breaks
  on every visit. Even with a confirmation flow, this defeats the purpose
  of a "copy ready-to-use environment" page.
- **(b) Store the plaintext reversibly encrypted** — requires a new
  ciphertext column + an encryption key. The plaintext is recoverable on
  demand. This is the only approach that delivers the requested UX
  (full snippet with token on every page load) without invalidating CI.
- **(c) Hybrid: "Generate + copy full snippet" one-shot action** —
  regenerates the token and returns the plaintext bundled with the
  assembled snippet in a single response. Plaintext is shown exactly once
  (the invariant is preserved), but each click invalidates the previous
  token. Considered and rejected as the primary design because the user
  explicitly asked to relax the one-time invariant for a better repeat-visit
  UX; however, the existing `POST/PUT /api/v1/tokens/me` endpoints remain
  available and the UI still offers "Generate" / "Regenerate" for users who
  prefer the one-time-plaintext flow.

### Chosen approach: (b) reversible encryption at rest

Store the token plaintext encrypted with **AES-256-GCM** in a new
`token_ciphertext` column on the `api_tokens` table. The encryption key is
managed exactly like the existing JWT secret:

- **Configurable** via a new config key `auth.token.encryption_key`
  (env: `DAGGER_CACHE_AUTH_TOKEN_ENCRYPTION_KEY`).
- **Auto-generated** (32 random bytes, hex-encoded) and persisted in the
  SQLite `meta` table under key `token_encryption_key` when not configured
  — mirroring `loadOrCreateJWTSecret` in `cmd/api/main.go:306-330`.
- **Validated** at startup: if configured explicitly, must be ≥ 32 bytes
  (matching the JWT secret rule, `minJWTSecretLen = 32`).

### Threat-model justification

- **Today (hashing):** DB compromise alone does not yield usable tokens
  (SHA-256 is one-way). An attacker must also intercept a token in transit
  or read it from a CI secret store.
- **After this change (reversible encryption):** DB compromise **plus**
  encryption-key compromise yields all token plaintexts. If the key is
  auto-generated and stored in the same DB's `meta` table, DB compromise
  alone suffices — a regression.
- **Mitigation:** the plan **recommends** configuring
  `auth.token.encryption_key` via env / K8s Secret in production (separate
  from the DB), exactly as `auth.jwt.secret` is recommended to be set
  explicitly. Auto-generation in `meta` is a dev-mode convenience; the
  startup log warns when it is used. This matches the existing JWT-secret
  posture (the DB is already a crown jewel: it holds the JWT signing key,
  bcrypt password hashes, and token hashes).
- **Net assessment:** the regression is bounded — an attacker with DB
  access can already forge JWTs (via the persisted JWT secret) and
  impersonate any user. Reversible token encryption adds one more
  recoverable secret but does not open a new attack class beyond "DB
  compromise = full compromise," which is already true. The operational
  guidance (set the key via env in production) keeps the production posture
  equivalent to the JWT-secret posture.

### CI snippet stance (reconsidered)

Because the server can now assemble a complete snippet with the plaintext,
the CI copy formats **could** embed the plaintext directly. However,
committed CI config files (`.github/workflows/*.yml`, `.gitlab-ci.yml`)
are version-controlled and a plaintext token there is a security finding
(CWE-798). The plan therefore **recommends the safest still-convenient
approach**:

- **Interactive-shell snippets** (bash exports, `.bashrc` heredoc, generic
  exports): include the **plaintext token directly**. These go into the
  user's local shell/dotfiles, which are not committed.
- **CI snippets** (GitHub Actions `env:`, GitLab CI `variables:`): use a
  **secret reference** (`${{ secrets.DAGGER_CLOUD_TOKEN }}` /
  `$DAGGER_CLOUD_TOKEN`) by default, with a prominent "Copy token value"
  button and a one-line instruction to paste it into the CI secret store
  once. Rationale: one extra paste step is standard practice and prevents
  the token from entering version control. The UI offers a "Include
  plaintext in CI snippet" toggle for users who accept the risk (e.g.
  ephemeral CI, internal-only repos).

## Current-state findings

### UI
- Vue 3 SPA embedded via `//go:embed all:ui-dist` in
  `internal/handler/ui.go`; served by `handleNoRoute` → `serveUI`
  (`internal/handler/server.go:747-751`).
- Router: `ui/src/router/index.ts`. Nav: `ui/src/App.vue`.
- `ui/src/views/Settings.vue` has an "API Token" card that
  generates/regenerates/revokes the per-user `DAGGER_CLOUD_TOKEN` and shows
  the plaintext exactly once.
- API client: `ui/src/api/client.ts` (axios, Bearer JWT); types in
  `ui/src/api/types.ts`.
- No frontend test framework (only `vue-tsc --noEmit` typecheck).

### Backend
- Hertz server wired in `internal/handler/server.go`; routes in
  `configure()` (lines 252-349). Auth helpers: `resolveIdentity`,
  `requireAuth` (`internal/handler/middleware.go`).
- Token endpoints: `GET/POST /api/v1/tokens/me`,
  `PUT /api/v1/tokens/me/regenerate`, `DELETE /api/v1/tokens/me`
  (`internal/handler/tokens.go`). `TokenService` (`internal/service/token_service.go`)
  stores only `HashAPIToken(plaintext)`; plaintext returned once by
  `Generate`/`Regenerate`.
- `Deps` bundle: `internal/handler/server.go:71-95`; `Server` fields:
  lines 111-148; wired in `cmd/api/main.go:215-249`.
- DB schema: `internal/repository/schema.sql`. `api_tokens` table (lines
  34-41): `id, user_id, token_hash, prefix, created_at, last_used_at`.
  Migration: `internal/repository/sqlite.go:64-95` — single v1 migration
  inside a transaction; `schema_migrations` table tracks versions.
- `MetaStore` (`internal/repository/sqlite.go:148-177`) reads/writes the
  `meta` table; used by `loadOrCreateJWTSecret` for the auto-generated JWT
  secret.
- `TokenRepo` (`internal/repository/token_repo.go`): `tokenCols` constant
  (line 14), `scanToken` (line 75). `Upsert` does an
  `INSERT ... ON CONFLICT(user_id) DO UPDATE`.

### Connection env vars (the contract)
Sources: `cmd/ci/main.go:61-69`, `scripts/dagger-cache.sh:13-27`,
`docs/README.md:161-170`:

| Env var | Value | Source |
|---|---|---|
| `DAGGER_CLOUD_URL` | `cfg.Server.PublicURL` | config |
| `DAGGER_CLOUD_TOKEN` | per-user API token (`dct_...`) | `TokenService` (now recoverable) |
| `_EXPERIMENTAL_DAGGER_RUNNER_HOST` | `dagger-cloud://self` | constant |
| `_EXPERIMENTAL_DAGGER_TAG` | optional engine version pin | user choice |
| `_EXPERIMENTAL_DAGGER_CACHE_CONFIG` | `type=registry,ref=<reg>:V<maj>-<min>-<patch>,mode=max` (registry) or `type=s3,bucket=...,region=...,mode=max` (s3) | `service.Cache.BuildCacheConfig(v, "max")` |

### Existing helpers (reused)
- `service.Cache.BuildCacheConfig(v *domain.Version, mode string) string`
  (`internal/service/cache.go:40`) — rewrites the registry host when
  `cache.public_host` is set; returns the s3 form for s3 backend.
- `domain.Version.CacheRefTag()` / `Slug()` produces `v<maj>-<min>-<patch>`.
- `domain.VersionResolver` (`internal/domain/version.go:17`) — `IsAllowed`,
  `ResolveMinimal`, `Floor`, `AllReleases`.
- `service.TokenService.Meta(ctx, userID)` — returns masked `*domain.APIToken`.
- `service.randomHex(n)` (`internal/service/id.go:11`) — crypto/rand hex.
- `cmd/api/main.go:loadOrCreateJWTSecret` (lines 306-330) — the pattern to
  mirror for the encryption key.

### Docs
- `docs/README.md` "Client setup" section (lines 156-180).
- `docs/design/index.md` lists ADRs 001-012.
- `config/config.app.yaml.sample` — fully-commented config reference.

## Chosen design and rationale

### Layering (respects `handler → service → domain ← repository`)

- `internal/domain/connect.go` (NEW): pure structs + new sentinel error,
  stdlib only.
- `internal/domain/apitoken.go` (MODIFY): add `TokenCiphertext` field.
- `internal/domain/config.go` (MODIFY): add `TokenConfig` sub-struct.
- `internal/domain/identity.go` (MODIFY): add `ErrTokenNotRecoverable`.
- `internal/repository/schema.sql` (MODIFY): add `token_ciphertext` column.
- `internal/repository/sqlite.go` (MODIFY): add v2 migration.
- `internal/repository/token_repo.go` (MODIFY): add ciphertext to
  `tokenCols`, `scanToken`, `Upsert`.
- `internal/service/token_service.go` (MODIFY): add encryption key, encrypt
  on upsert, new `Reveal` method.
- `internal/service/connect_service.go` (NEW): assembles the snapshot,
  optionally reveals the token.
- `internal/handler/connect.go` (NEW): Hertz handler, auth-gated,
  `?reveal=true` query param, `Cache-Control: no-store`.
- `cmd/api/main.go` (MODIFY): load/create encryption key, construct
  `ConnectService`, inject.
- `internal/handler/server.go` (MODIFY): add `connect` field + `Deps` field
  + route.
- `config/loader.go` (MODIFY): `v.SetDefault` for
  `auth.token.encryption_key`.
- `config/config.app.yaml.sample` (MODIFY): document the new key.
- UI: `ui/src/views/Connect.vue` (NEW) + `ui/src/api/client.ts` (MODIFY) +
  `ui/src/api/types.ts` (MODIFY) + `ui/src/router/index.ts` (MODIFY) +
  `ui/src/App.vue` (MODIFY).

## Detailed implementation

### 1. `internal/domain/identity.go` (MODIFY)

Add a new sentinel error:
```go
ErrTokenNotRecoverable = errors.New("api token not recoverable")
```

### 2. `internal/domain/apitoken.go` (MODIFY)

Add the ciphertext field to `APIToken`:
```go
type APIToken struct {
    ID              string     `json:"id"`
    UserID          string     `json:"user_id"`
    TokenHash       string     `json:"-"`
    TokenCiphertext string     `json:"-"` // AES-256-GCM(nonce||ciphertext||tag), base64; "" for pre-v2 tokens
    Prefix          string     `json:"prefix"`
    CreatedAt       time.Time  `json:"created_at"`
    LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
}
```

### 3. `internal/domain/config.go` (MODIFY)

Add `TokenConfig` to `AuthConfig`:
```go
type AuthConfig struct {
    Internal       InternalAuthConfig   `mapstructure:"internal"`
    OAuth          OAuthConfig          `mapstructure:"oauth"`
    JWT            JWTConfig            `mapstructure:"jwt"`
    Token          TokenConfig          `mapstructure:"token"` // NEW
    BootstrapAdmin BootstrapAdminConfig `mapstructure:"bootstrap_admin"`
}

// TokenConfig configures API-token plaintext recovery (Connect-env UI).
type TokenConfig struct {
    EncryptionKey string `mapstructure:"encryption_key"` // >= 32 bytes; empty = auto-generated + persisted in meta
}
```

### 4. `internal/domain/connect.go` (NEW)

Pure data types, stdlib only.
```go
package domain

// ConnectEnvVar is one environment variable the Dagger CLI reads.
type ConnectEnvVar struct {
    Name        string `json:"name"`
    Value       string `json:"value"`    // full value, including plaintext token when reveal=true
    Required    bool   `json:"required"`
    Secret      bool   `json:"secret"`
    Description string `json:"description"`
}

// ConnectTokenMeta is the masked view of the caller's API token.
type ConnectTokenMeta struct {
    Exists     bool   `json:"exists"`
    Prefix     string `json:"prefix"`      // e.g. "dct_ab12cd34"; "" when !Exists
    Recoverable bool   `json:"recoverable"` // true when ciphertext is present + key available
}

// ConnectEnvSnapshot is the payload of GET /api/v1/connect/env.
type ConnectEnvSnapshot struct {
    ServerURL       string           `json:"server_url"`
    DataHostname    string           `json:"data_hostname"`
    CacheBackend    string           `json:"cache_backend"`
    VersionFloor    string           `json:"version_floor"`
    AllowedVersions []string         `json:"allowed_versions"`
    SelectedVersion string           `json:"selected_version,omitempty"`
    Token           ConnectTokenMeta `json:"token"`
    EnvVars         []ConnectEnvVar  `json:"env_vars"`
}
```

### 5. `internal/repository/schema.sql` (MODIFY)

Add the column to the `api_tokens` table definition:
```sql
CREATE TABLE IF NOT EXISTS api_tokens (
    id               TEXT PRIMARY KEY,
    user_id          TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    token_hash       TEXT NOT NULL UNIQUE,
    token_ciphertext TEXT NOT NULL DEFAULT '',  -- AES-256-GCM base64; "" for pre-v2 tokens
    prefix           TEXT NOT NULL DEFAULT '',
    created_at       DATETIME NOT NULL,
    last_used_at     DATETIME
);
```

### 6. `internal/repository/sqlite.go` (MODIFY)

Add a v2 migration after the v1 block in `Migrate`:
```go
// v2: add token_ciphertext column (idempotent via IF NOT EXISTS).
var v2Count int
if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 2").Scan(&v2Count); err != nil {
    return fmt.Errorf("check schema_migrations v2: %w", err)
}
if v2Count == 0 {
    if _, err := tx.ExecContext(ctx, `ALTER TABLE api_tokens ADD COLUMN token_ciphertext TEXT NOT NULL DEFAULT ''`); err != nil {
        return fmt.Errorf("alter api_tokens v2: %w", err)
    }
    if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (2, ?)", time.Now().UTC()); err != nil {
        return fmt.Errorf("record migration v2: %w", err)
    }
}
```
Note: `ALTER TABLE ... ADD COLUMN` with `IF NOT EXISTS` is not supported by
older SQLite; the `schema_migrations` gate makes it idempotent. For
fresh DBs, the v1 `CREATE TABLE` already includes the column, so the
`ALTER` is a no-op failure on "duplicate column" — guard with a check or
use `PRAGMA table_info` to test column presence before altering. The
safest pattern: query `PRAGMA table_info(api_tokens)`, check if
`token_ciphertext` is present, and only `ALTER` when it is missing.

### 7. `internal/repository/token_repo.go` (MODIFY)

- Update `tokenCols`:
  ```go
  const tokenCols = `id, user_id, token_hash, token_ciphertext, prefix, created_at, last_used_at`
  ```
- Update `Upsert` to include `token_ciphertext` in the INSERT and the
  `ON CONFLICT` UPDATE set list.
- Update `scanToken` to scan the new column:
  ```go
  err := row.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.TokenCiphertext, &t.Prefix, &t.CreatedAt, &lastUsed)
  ```

### 8. `internal/service/token_service.go` (MODIFY)

Add the encryption key and a `Reveal` method.

```go
type TokenService struct {
    tokens domain.APITokenRepository
    encKey []byte // AES-256 key (32 bytes); nil = encryption disabled (pre-config)
    logger *logrus.Logger
}

func NewTokenService(tokens domain.APITokenRepository, logger *logrus.Logger, encKey []byte) *TokenService {
    return &TokenService{tokens: tokens, encKey: encKey, logger: logger}
}
```

- `upsertNew`: after generating `plaintext`, encrypt it and store the
  ciphertext in `t.TokenCiphertext`:
  ```go
  ct, err := encryptToken(s.encKey, plaintext)
  if err != nil {
      return "", nil, fmt.Errorf("encrypt token: %w", err)
  }
  t.TokenCiphertext = ct
  ```
- New method `Reveal`:
  ```go
  // Reveal returns the plaintext token for the user. Returns ErrNotFound
  // when no token exists, ErrTokenNotRecoverable when the ciphertext is
  // empty (pre-v2 token) or the encryption key is unavailable.
  func (s *TokenService) Reveal(ctx context.Context, userID string) (string, error) {
      t, err := s.tokens.GetByUser(ctx, userID)
      if err != nil {
          return "", err
      }
      if t.TokenCiphertext == "" {
          return "", domain.ErrTokenNotRecoverable
      }
      if len(s.encKey) == 0 {
          return "", domain.ErrTokenNotRecoverable
      }
      pt, err := decryptToken(s.encKey, t.TokenCiphertext)
      if err != nil {
          return "", fmt.Errorf("decrypt token: %w", err)
      }
      return pt, nil
  }
  ```
- New helpers `encryptToken` / `decryptToken` (AES-256-GCM, stdlib only):
  ```go
  func encryptToken(key []byte, plaintext string) (string, error) {
      if len(key) == 0 {
          return "", nil // encryption disabled
      }
      block, err := aes.NewCipher(key)
      if err != nil {
          return "", err
      }
      gcm, err := cipher.NewGCM(block)
      if err != nil {
          return "", err
      }
      nonce := make([]byte, gcm.NonceSize())
      if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
          return "", err
      }
      sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil) // nonce || ciphertext || tag
      return base64.StdEncoding.EncodeToString(sealed), nil
  }

  func decryptToken(key []byte, ctB64 string) (string, error) {
      sealed, err := base64.StdEncoding.DecodeString(ctB64)
      if err != nil {
          return "", err
      }
      block, err := aes.NewCipher(key)
      if err != nil {
          return "", err
      }
      gcm, err := cipher.NewGCM(block)
      if err != nil {
          return "", err
      }
      if len(sealed) < gcm.NonceSize() {
          return "", fmt.Errorf("ciphertext too short")
      }
      nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
      pt, err := gcm.Open(nil, nonce, ct, nil)
      if err != nil {
          return "", err
      }
      return string(pt), nil
  }
  ```
- `ImportRaw` (legacy migration path): encrypt the imported plaintext too,
  so migrated tokens are also recoverable. If `encKey` is nil (shouldn't
  happen post-config), store `""` and log a warning.

### 9. `internal/service/connect_service.go` (NEW)

```go
package service

import (
    "context"
    "errors"
    "fmt"

    "github.com/sirupsen/logrus"

    "github.com/disaster/dagger-kubernetes/internal/domain"
)

type ConnectService struct {
    cfg             *domain.Config
    cache           *Cache
    versionResolver domain.VersionResolver
    tokens          *TokenService
    logger          *logrus.Logger
}

func NewConnectService(
    cfg *domain.Config,
    cache *Cache,
    vr domain.VersionResolver,
    tokens *TokenService,
    logger *logrus.Logger,
) *ConnectService {
    return &ConnectService{cfg: cfg, cache: cache, versionResolver: vr, tokens: tokens, logger: logger}
}

// ConnectEnv builds the snapshot. When reveal=true and the token is
// recoverable, the DAGGER_CLOUD_TOKEN value is populated with the plaintext.
func (s *ConnectService) ConnectEnv(ctx context.Context, userID, version string, reveal bool) (*domain.ConnectEnvSnapshot, error) {
    snap := &domain.ConnectEnvSnapshot{
        ServerURL:    s.cfg.Server.PublicURL,
        DataHostname: s.cfg.Server.DataHost,
        CacheBackend: s.cfg.Cache.Backend,
        VersionFloor: s.cfg.Version.Floor,
        Token:        s.tokenMeta(ctx, userID, reveal),
    }
    snap.AllowedVersions = s.allowedVersions()

    tokenValue := ""
    if reveal && snap.Token.Recoverable {
        pt, err := s.tokens.Reveal(ctx, userID)
        if err != nil {
            s.logger.WithError(err).Warn("connect: token reveal unavailable")
        } else {
            tokenValue = pt
        }
    }

    envs := []domain.ConnectEnvVar{
        {Name: "DAGGER_CLOUD_URL", Value: s.cfg.Server.PublicURL, Required: true, Description: "Control-plane URL the Dagger CLI talks to (replaces Dagger Cloud)."},
        {Name: "DAGGER_CLOUD_TOKEN", Value: tokenValue, Required: true, Secret: true, Description: "Your per-user API token (dct_...)."},
        {Name: "_EXPERIMENTAL_DAGGER_RUNNER_HOST", Value: "dagger-cloud://self", Required: true, Description: "Tells the CLI to provision a remote engine via the cloud driver."},
    }

    if version != "" {
        v, err := s.versionResolver.ResolveMinimal(version)
        if err != nil {
            return nil, fmt.Errorf("parse version: %w", err)
        }
        if !s.versionResolver.IsAllowed(v) {
            return nil, fmt.Errorf("%w: version %s not allowed (floor %s)", domain.ErrValidation, v, s.versionResolver.Floor())
        }
        snap.SelectedVersion = v.String()
        envs = append(envs, domain.ConnectEnvVar{
            Name: "_EXPERIMENTAL_DAGGER_TAG", Value: v.String(), Required: false,
            Description: "Pins the engine version (recommended for cache locality).",
        })
        if cc := s.cache.BuildCacheConfig(v, "max"); cc != "" {
            envs = append(envs, domain.ConnectEnvVar{
                Name: "_EXPERIMENTAL_DAGGER_CACHE_CONFIG", Value: cc, Required: false,
                Description: "Remote shared cache ref (BuildKit registry/s3 mode).",
            })
        }
    }

    snap.EnvVars = envs
    return snap, nil
}

func (s *ConnectService) tokenMeta(ctx context.Context, userID string, reveal bool) domain.ConnectTokenMeta {
    if userID == "" {
        return domain.ConnectTokenMeta{}
    }
    t, err := s.tokens.Meta(ctx, userID)
    if err != nil {
        if errors.Is(err, domain.ErrNotFound) {
            return domain.ConnectTokenMeta{Exists: false}
        }
        s.logger.WithError(err).Warn("connect: token meta unavailable")
        return domain.ConnectTokenMeta{}
    }
    recoverable := t.TokenCiphertext != "" && s.tokens.encKeyAvailable()
    return domain.ConnectTokenMeta{Exists: true, Prefix: t.Prefix, Recoverable: recoverable}
}

func (s *ConnectService) allowedVersions() []string {
    out := make([]string, 0, 4)
    for _, v := range s.versionResolver.AllReleases() {
        out = append(out, v.String())
    }
    return out
}
```

Note: `TokenService` needs a small unexported helper `encKeyAvailable() bool`
(or expose `len(s.encKey) > 0` via a method) so `ConnectService` can compute
`Recoverable` without leaking the key.

### 10. `internal/handler/connect.go` (NEW)

```go
package handler

import (
    "context"

    "github.com/cloudwego/hertz/pkg/app"
    "github.com/cloudwego/hertz/pkg/protocol/consts"
)

// handleConnectEnv returns the connection env-var snapshot for the caller.
// Auth-gated. ?reveal=true populates the DAGGER_CLOUD_TOKEN plaintext when
// the token is recoverable. The response is never cached (no-store).
func (s *Server) handleConnectEnv(_ context.Context, c *app.RequestContext) {
    if !s.requireAuth(c) {
        return
    }
    if s.connect == nil {
        writeError(c, consts.StatusInternalServerError, "connect env unavailable")
        return
    }
    id := identityOf(c)
    if id == nil {
        writeError(c, consts.StatusUnauthorized, "unauthorized")
        return
    }
    reveal := c.Query("reveal") == "true"
    version := string(c.Query("version"))
    snap, err := s.connect.ConnectEnv(context.Background(), id.UserID, version, reveal)
    if err != nil {
        s.writeServiceError(c, err)
        return
    }
    // Never cache a response that may contain the token plaintext.
    c.Response.Header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
    c.Response.Header.Set("Pragma", "no-cache")
    writeJSON(c, snap)
}
```

**Logging guard:** the handler must never log the token value or the
response body. The existing `requestLog` middleware logs only method,
path, status, duration — not bodies. The handler itself logs nothing on
the success path. On the error path, `writeServiceError` logs the error
message, which never contains the token (errors are sentinel-wrapped).

### 11. `internal/handler/server.go` (MODIFY)

- Add to `Deps` (after `StatusProvider`):
  ```go
  Connect *service.ConnectService
  ```
- Add to `Server` (after `status`):
  ```go
  connect *service.ConnectService
  ```
- Assign in `NewServer`: `connect: deps.Connect,`.
- Register route in `configure()` (after `h.GET("/api/v1/status", ...)`):
  ```go
  h.GET("/api/v1/connect/env", s.handleConnectEnv)
  ```

### 12. `cmd/api/main.go` (MODIFY)

After the JWT secret is loaded (line ~148), add the token encryption key:
```go
tokenEncKey, err := loadOrCreateTokenEncryptionKey(ctx, metaStore, cfg.Auth.Token.EncryptionKey, logger)
if err != nil {
    return fmt.Errorf("load token encryption key: %w", err)
}
```
New function `loadOrCreateTokenEncryptionKey` (mirrors
`loadOrCreateJWTSecret`):
```go
const minTokenEncKeyLen = 32

func loadOrCreateTokenEncryptionKey(ctx context.Context, ms *repository.MetaStore, configured string, logger *logrus.Logger) ([]byte, error) {
    if configured != "" {
        if len(configured) < minTokenEncKeyLen {
            return nil, fmt.Errorf("auth.token.encryption_key too short (%d bytes): requires at least %d bytes", len(configured), minTokenEncKeyLen)
        }
        return []byte(configured), nil
    }
    const key = "token_encryption_key"
    if existing, err := ms.Get(ctx, key); err == nil {
        return []byte(existing), nil
    } else if !errors.Is(err, domain.ErrNotFound) {
        return nil, fmt.Errorf("get token encryption key: %w", err)
    }
    b := make([]byte, 32)
    if _, err := rand.Read(b); err != nil {
        return nil, fmt.Errorf("generate token encryption key: %w", err)
    }
    secret := hex.EncodeToString(b)
    if err := ms.Set(ctx, key, secret); err != nil {
        return nil, fmt.Errorf("persist token encryption key: %w", err)
    }
    logger.Warn("generated and persisted token encryption key (configure auth.token.encryption_key in production)")
    return []byte(secret), nil
}
```

Update the `tokensSvc` construction to pass the key:
```go
tokensSvc := service.NewTokenService(tokenRepo, logger, tokenEncKey)
```

After `statusSvc`, add:
```go
connectSvc := service.NewConnectService(cfg, cacheBackend, versionResolver, tokensSvc, logger)
```
And add to `Deps{...}`:
```go
Connect: connectSvc,
```

### 13. `config/loader.go` (MODIFY)

Add the default:
```go
v.SetDefault("auth.token.encryption_key", "")
```

### 14. `config/config.app.yaml.sample` (MODIFY)

Under the `auth:` block, add:
```yaml
  # API-token plaintext recovery (Connect-env UI). AES-256-GCM key (>= 32 bytes).
  # Empty = auto-generated and persisted in the DB meta table (dev mode).
  # Configure via env (DAGGER_CACHE_AUTH_TOKEN_ENCRYPTION_KEY) in production
  # so DB compromise alone does not yield token plaintexts.
  token:
    encryption_key: ""
```

### 15. `internal/handler/middleware_test.go` (MODIFY)

Register the route in `newAuthEngine`:
```go
e.GET("/api/v1/connect/env", s.handleConnectEnv)
```

### 16. Tests

#### `internal/service/token_service_test.go` (MODIFY — add cases)
- `TestRevealSuccess` — token created with encKey, `Reveal` returns the
  original plaintext.
- `TestRevealNotFound` — no token → `ErrNotFound`.
- `TestRevealPreV2Token` — token with `TokenCiphertext=""` →
  `ErrTokenNotRecoverable`.
- `TestRevealNoKey` — `encKey=nil` → `ErrTokenNotRecoverable`.
- `TestEncryptDecryptRoundTrip` — `encryptToken` then `decryptToken`
  returns the original; wrong key fails.
- `TestUpsertStoresCiphertext` — after `Generate`, the repo row has a
  non-empty `TokenCiphertext`.

#### `internal/service/connect_service_test.go` (NEW)
Table-driven. Cases:
- `TestConnectEnvNoVersionMasked` — `reveal=false`; token value `""`;
  `token.recoverable=true` (when encKey present).
- `TestConnectEnvNoVersionRevealed` — `reveal=true`; token value is the
  plaintext.
- `TestConnectEnvWithVersion` — 5 env vars; cache config matches
  `service.Cache.BuildCacheConfig`.
- `TestConnectEnvS3Backend` — s3 cache config form.
- `TestConnectEnvRegistryPublicHost` — cache config ref uses public host.
- `TestConnectEnvInvalidVersion` → `ErrValidation`.
- `TestConnectEnvDisallowedVersion` → error.
- `TestConnectEnvTokenMissing` — `token.exists=false`, `recoverable=false`.
- `TestConnectEnvTokenNotRecoverable` — pre-v2 token (ciphertext `""`);
  `recoverable=false`; `reveal=true` → token value `""`.
- `TestConnectEnvEmptyUserID` — `token.exists=false`.
- `TestConnectEnvAllowedVersions` — matches resolver's `AllReleases`.

#### `internal/handler/connect_test.go` (NEW)
- `TestConnectEnvRequiresAuth` — no Authorization → 401.
- `TestConnectEnvDefaultMasked` — JWT user, no `?reveal` → 200; token
  value `""`; `token.recoverable=true`.
- `TestConnectEnvRevealed` — `?reveal=true` → 200; token value is the
  plaintext; `Cache-Control: no-store` header present.
- `TestConnectEnvWithVersion` — `?version=v0.21.4` → 200; includes TAG +
  CACHE_CONFIG.
- `TestConnectEnvInvalidVersion` — `?version=notaversion` → 400.
- `TestConnectEnvDisallowedVersion` — `?version=v0.10.0` → 400.
- `TestConnectEnvTokenMissing` — user without token → `token.exists=false`.
- `TestConnectEnvTokenNotRecoverable` — pre-v2 token → `recoverable=false`;
  `?reveal=true` → token value `""`.
- `TestConnectEnvAuthDisabled` — synthetic anonymous identity → 200;
  `token.exists=false`.
- `TestConnectEnvCacheControlHeader` — every response has
  `Cache-Control: no-store`.
- `TestConnectEnvNoTokenInLogs` — (best-effort) capture log output,
  assert the plaintext does not appear in any log line.

#### `internal/repository/token_repo_test.go` (MODIFY — add cases)
- `TestUpsertStoresCiphertext` — `Upsert` with `TokenCiphertext` set;
  `GetByUser` returns it.
- `TestGetByUserPreV2Column` — a row with `token_ciphertext=""` (simulated)
  scans to `TokenCiphertext=""` without error.

#### `internal/repository/sqlite_migrate_test.go` (MODIFY — add case)
- `TestMigrateV2AddsColumn` — start from a v1-only DB (no
  `token_ciphertext`), run `Migrate`, assert the column exists and
  `schema_migrations` has version 2.

### 17. UI: `ui/src/api/types.ts` (MODIFY)

```ts
export interface ConnectEnvVar {
  name: string
  value: string
  required: boolean
  secret: boolean
  description: string
}
export interface ConnectTokenMeta {
  exists: boolean
  prefix: string
  recoverable: boolean
}
export interface ConnectEnvSnapshot {
  server_url: string
  data_hostname: string
  cache_backend: string
  version_floor: string
  allowed_versions: string[]
  selected_version?: string
  token: ConnectTokenMeta
  env_vars: ConnectEnvVar[]
}
```

### 18. UI: `ui/src/api/client.ts` (MODIFY)

```ts
export async function fetchConnectEnv(version?: string, reveal?: boolean): Promise<ConnectEnvSnapshot> {
  const params: Record<string, string> = {}
  if (version) params.version = version
  if (reveal) params.reveal = 'true'
  const { data } = await api.get('/api/v1/connect/env', { params })
  return data as ConnectEnvSnapshot
}
```

### 19. UI: `ui/src/views/Connect.vue` (NEW)

- **Version picker**: `<select>` from `allowed_versions`, default
  "No pin (use CLI default)". On change, re-fetch.
- **Reveal toggle**: a checkbox "Show token plaintext". When toggled on,
  re-fetch with `reveal=true`. When off, fetch with `reveal=false` (token
  value is `""`; show masked prefix `token.prefix + "…"`).
- **Env-var table**: one row per `env_vars[i]`. Non-secret rows show the
  value in a read-only `<code>`. The `DAGGER_CLOUD_TOKEN` row:
  - When `reveal=false`: shows `token.prefix + "…"` (masked) with a note
    "Check 'Show token plaintext' to reveal."
  - When `reveal=true` and `token.recoverable=true`: shows the plaintext
    in a read-only `<code>` (red background to signal a secret).
  - When `token.recoverable=false`: shows a warning "Token not recoverable
    (created before this feature). Regenerate your token on the Settings
    page to enable full-snippet copy." with a link to `/settings`.
  - When `token.exists=false`: shows "No token. Generate one on the
    Settings page." with a link to `/settings`.
- **Copy buttons** (built client-side from the current table values):
  1. **Bash/zsh exports** — `export NAME='value'` lines (skips empty
     non-required rows; skips the token row if value is empty).
  2. **`.bashrc` snippet** — `cat >> ~/.dagger-cache.env <<'EOF' ... EOF`
     heredoc + `echo 'source ~/.dagger-cache.env' >> ~/.bashrc`.
  3. **GitHub Actions `env:` block** — YAML with
     `DAGGER_CLOUD_TOKEN: ${{ secrets.DAGGER_CLOUD_TOKEN }}` by default.
     A toggle "Include plaintext token" (off by default) replaces it with
     the literal value. When the toggle is on, show a red warning.
  4. **GitLab CI `variables:` block** — YAML with
     `DAGGER_CLOUD_TOKEN: $DAGGER_CLOUD_TOKEN` by default; same toggle.
  5. **Generic `export` lines** — same as (1), unquoted.
- **"Copy token value" button** — copies just the `DAGGER_CLOUD_TOKEN`
  value (for pasting into a CI secret store). Only enabled when
  `reveal=true` and `token.recoverable=true`.
- **Doc section** (collapsible `<details>`): the short user-facing doc
  (see §20).
- **Copy implementation**: `navigator.clipboard.writeText` with a hidden
  `<textarea>` + `document.execCommand('copy')` fallback. Transient
  "Copied!" badge.

Edge cases:
- `token.recoverable=false` → all copy buttons that need the token are
  disabled with a tooltip.
- Auth-disabled mode → token row shows "Auth disabled — tokens
  unavailable"; non-secret env vars still shown.
- `cache_backend === 's3'` → cache config uses the s3 form.
- `allowed_versions` empty → version picker hidden.

### 20. UI: `ui/src/router/index.ts` (MODIFY)

```ts
{ path: '/connect', name: 'connect', component: () => import('@/views/Connect.vue') },
```

### 21. UI: `ui/src/App.vue` (MODIFY)

Add nav link (between "Settings" and the admin block):
```html
<router-link to="/connect">Connect</router-link>
```

### 22. Docs

#### `docs/README.md` (MODIFY)
- Add **"Connect your environment (UI)"** subsection after "Client setup":
  log in → click "Connect" → pick a version (optional) → check "Show
  token plaintext" → click the copy button for your target. Include the
  sourcing recipe:
  ```bash
  # 1. On the Connect page, check "Show token plaintext", click "Copy .bashrc snippet", paste into a shell.
  # 2. Reload your shell (or: source ~/.dagger-cache.env).
  dagger call github.com/your-org/ci@v1.0.0 build
  ```
- Add a note under "Authentication" → "Per-user API tokens": tokens are
  now stored reversibly encrypted (AES-256-GCM) so the Connect page can
  reveal them; configure `auth.token.encryption_key` via env in
  production. Pre-existing tokens (created before the upgrade) are not
  recoverable — regenerate to enable.
- Update the "Pipeline UI" feature list to mention the Connect page.
- Update the config reference table with `auth.token.encryption_key`.

#### `docs/design/ADR-013-connect-env-menu.md` (NEW)
Standard ADR. Key decisions:
1. Store API-token plaintext reversibly encrypted (AES-256-GCM) in a new
   `token_ciphertext` column; key managed like the JWT secret
   (configurable via `auth.token.encryption_key`, auto-generated in
   `meta` as fallback).
2. `GET /api/v1/connect/env` is auth-gated; `?reveal=true` populates the
   token plaintext; `Cache-Control: no-store` on every response; the
   token value is never logged.
3. Default view is masked (prefix only); reveal is an explicit user
   action (UI checkbox).
4. CI snippets use secret references by default (plaintext toggle
   available with a warning); interactive-shell snippets include the
   plaintext directly.
5. Reuses `service.Cache.BuildCacheConfig` and `domain.VersionResolver`
   (single source of truth for the cache-config value).
6. Threat-model trade-off: DB compromise + key compromise → token
   plaintext leaked (vs. hashing where DB alone is insufficient).
   Mitigation: configure the key via env/K8s Secret in production.
   Pre-existing tokens are not recoverable (no ciphertext) — regenerate
   to enable.
7. Alternatives rejected: (a) regenerate-on-reveal (breaks CI on every
   view); (c) hybrid one-shot (preserves invariant but invalidates token
   on each use — kept as the existing Settings flow for users who prefer
   it).

#### `docs/design/index.md` (MODIFY)
Add row:
```
| 013  | [Connect-env UI menu](ADR-013-connect-env-menu.md) |
```

## Validation and edge cases

- **Token plaintext served only to the authenticated owner**:
  `requireAuth` gates the endpoint; `identityOf(c).UserID` scopes the
  reveal. Verified by `TestConnectEnvRequiresAuth`.
- **Token never logged**: the handler logs nothing on the success path;
  `writeServiceError` logs only sentinel-wrapped errors (no token in the
  error message). `TestConnectEnvNoTokenInLogs` asserts the plaintext does
  not appear in captured log output.
- **Token never cached**: `Cache-Control: no-store, no-cache,
  must-revalidate` + `Pragma: no-cache` on every response. Verified by
  `TestConnectEnvCacheControlHeader`.
- **Pre-v2 tokens**: `TokenCiphertext=""` → `recoverable=false`; reveal
  returns `""`; UI shows a "Regenerate to enable" link. Verified by
  `TestConnectEnvTokenNotRecoverable`.
- **Encryption key unavailable** (nil): `Reveal` returns
  `ErrTokenNotRecoverable`; `recoverable=false`.
- **Version validation**: invalid/disallowed → 400 via `writeServiceError`
  mapping `domain.ErrValidation`.
- **s3 backend**: cache config uses the s3 form; no crash when
  `cache.registry` is empty.
- **`cache.public_host` set**: cache config ref uses the public host.
- **Synthetic identities** (auth-disabled, legacy): `userID` is "" →
  `token.exists=false`; non-secret env vars still rendered.
- **Empty `allowed_versions`**: UI hides the version picker; endpoint
  still returns the 3 required env vars.
- **Migration idempotency**: v2 migration guarded by
  `schema_migrations` version 2; `ALTER TABLE` only when the column is
  missing (checked via `PRAGMA table_info`).
- **CSP**: existing `frame-ancestors 'none'` + `no-referrer` headers
  apply; `navigator.clipboard` works under CSP `default-src 'self'`.

## Testing plan

| File | Cases |
|---|---|
| `internal/service/token_service_test.go` (modify) | RevealSuccess, RevealNotFound, RevealPreV2Token, RevealNoKey, EncryptDecryptRoundTrip, UpsertStoresCiphertext |
| `internal/service/connect_service_test.go` (new) | no-version-masked, no-version-revealed, with-version, s3, registry+public_host, invalid-version, disallowed-version, token-missing, token-not-recoverable, empty-userID, allowed-versions |
| `internal/handler/connect_test.go` (new) | requires-auth, default-masked, revealed, with-version, invalid-version, disallowed-version, token-missing, token-not-recoverable, auth-disabled, cache-control-header, no-token-in-logs |
| `internal/repository/token_repo_test.go` (modify) | UpsertStoresCiphertext, GetByUserPreV2Column |
| `internal/repository/sqlite_migrate_test.go` (modify) | MigrateV2AddsColumn |
| `internal/handler/middleware_test.go` (modify) | register `/api/v1/connect/env` in `newAuthEngine` |
| UI | `npm run typecheck`; manual smoke test against `cd deploy/docker && docker compose up` |

Coverage target: 100% for the new/modified code (per AGENTS.md).

## Security posture (revised)

- The new endpoint is **auth-gated** (`requireAuth`); the token plaintext
  is returned **only to the authenticated owner** scoped by
  `identityOf(c).UserID`.
- The endpoint sets **`Cache-Control: no-store`** on every response;
  the token plaintext is never cached by browsers or proxies.
- The token plaintext is **never logged**: the handler logs nothing on
  the success path; error logging uses sentinel-wrapped errors that do
  not contain the token.
- The **default view is masked** (prefix only); revealing the plaintext
  is an explicit user action (UI checkbox → `?reveal=true`).
- The token plaintext is now stored **reversibly encrypted** (AES-256-GCM)
  at rest. This is a deliberate trade-off: it enables the requested UX
  (full snippet with token on every visit) at the cost of making token
  plaintexts recoverable by an attacker who compromises both the DB and
  the encryption key. **Mitigation:** configure
  `auth.token.encryption_key` via env / K8s Secret in production
  (separate from the DB), exactly as `auth.jwt.secret` is recommended to
  be set explicitly. Auto-generation in the `meta` table is a dev-mode
  convenience with a startup warning.
- **Pre-existing tokens** (created before this change) have no
  ciphertext and are **not recoverable**; the UI shows a "Regenerate to
  enable" link. This is a one-time migration cost.
- **CI snippets** use **secret references** by default
  (`${{ secrets.DAGGER_CLOUD_TOKEN }}` / `$DAGGER_CLOUD_TOKEN`) to keep
  tokens out of version control; a UI toggle allows embedding the
  plaintext with a red warning for users who accept the risk.
- **Flag (unchanged from v1):** if the deployment runs with
  `auth.internal.enabled: false` (dev mode), the UI is unauthenticated
  and the endpoint returns the server URL, cache topology, and (when
  `?reveal=true`) the anonymous user's token plaintext to anonymous
  callers. This is the existing posture; production must enable auth.
  ADR-013 documents this explicitly.

## Ordered implementation checklist

1. `internal/domain/identity.go`: add `ErrTokenNotRecoverable`.
2. `internal/domain/apitoken.go`: add `TokenCiphertext` field.
3. `internal/domain/config.go`: add `TokenConfig` to `AuthConfig`.
4. `internal/domain/connect.go`: new structs.
5. `internal/repository/schema.sql`: add `token_ciphertext` column.
6. `internal/repository/sqlite.go`: add v2 migration (PRAGMA-guarded
   `ALTER TABLE`).
7. `internal/repository/token_repo.go`: update `tokenCols`, `scanToken`,
   `Upsert`.
8. `internal/repository/token_repo_test.go` + `sqlite_migrate_test.go`:
   add cases; run `go test ./internal/repository`.
9. `internal/service/token_service.go`: add `encKey`, `Reveal`,
   `encryptToken`/`decryptToken`; update `NewTokenService` signature,
   `upsertNew`, `ImportRaw`.
10. `internal/service/token_service_test.go`: add cases; run
    `go test ./internal/service`.
11. `internal/service/connect_service.go` + `connect_service_test.go`;
    run `go test ./internal/service`.
12. `internal/handler/connect.go`.
13. `internal/handler/server.go`: add `Connect` to `Deps` + `Server` +
    `NewServer` + route in `configure()`.
14. `cmd/api/main.go`: add `loadOrCreateTokenEncryptionKey`, update
    `NewTokenService` call, construct `ConnectService`, inject into
    `Deps`.
15. `config/loader.go`: `v.SetDefault("auth.token.encryption_key", "")`.
16. `config/config.app.yaml.sample`: document the new key.
17. `internal/handler/middleware_test.go`: register route in
    `newAuthEngine`.
18. `internal/handler/connect_test.go`: add cases; run
    `go test ./internal/handler`.
19. `gofmt`/`goimports` (local prefix
    `github.com/disaster/dagger-kubernetes`); `golangci-lint run ./...`.
20. UI: `ui/src/api/types.ts` + `ui/src/api/client.ts`.
21. UI: `ui/src/views/Connect.vue`.
22. UI: `ui/src/router/index.ts` + `ui/src/App.vue`.
23. UI: `cd ui && npm install && npm run build` (verify the embed path
    copies to `internal/handler/ui-dist`).
24. UI: `npm run typecheck`.
25. Docs: `docs/design/ADR-013-connect-env-menu.md`;
    update `docs/design/index.md`.
26. Docs: update `docs/README.md` (Connect subsection, Authentication
    note, config reference, Pipeline UI feature list).
27. Manual smoke test: `cd deploy/docker && docker compose up -d --build`;
    log in; visit `/connect`; toggle "Show token plaintext"; copy each
    snippet; verify `dagger call ...` works with the pasted env; verify
    a pre-existing token (created before upgrade) shows "not
    recoverable" and that regenerating fixes it.
