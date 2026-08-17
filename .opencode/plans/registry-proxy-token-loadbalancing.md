# Plan: Registry-Proxy Token Control + Multi-Registry Load Balancing

**Module:** `github.com/disaster/dagger-kubernetes`
**Plan file:** `.opencode/plans/registry-proxy-token-loadbalancing.md`

## Goal

Fix the cache-ref emission bug and turn the Supervisor into a real OCI
Distribution v2 reverse proxy that (a) never exposes the raw registry address
to clients/engines, (b) controls registry credentials itself (token control),
and (c) load-balances cache traffic across multiple backend registries using a
persisted object→backend routing table so cache "charge" is distributed while
preserving OCI correctness (cross-run hits + upload-session affinity).

## Problem (verbatim)

> On UI, you display `export _EXPERIMENTAL_DAGGER_CACHE_CONFIG='type=registry,
> ref=dagger-cache-test-registry:5000/dagger-cache:v0-19-0,mode=max'`. But the
> dagger client must never speak directly with docker registry. It must speak
> with supervisor api that act as proxy for docker registry. The supervisor must
> control token and must route on right docker registry if multiple registry
> deployed for load balance charge of cache.

## Resolved design decisions

1. **Cache ref host** — emitted ref points at a **dedicated, explicitly
   configured cache vhost** (`cache.public_host`). It defaults to
   `cache.<host-of-server.public_url>` and is validated at startup to differ
   from the control-plane host (fail fast on collision). The engine reaches it
   via DNS/ingress over the existing TLS listener.
2. **Listener** — shared control-plane Hertz listener; bump the global
   `WithReadTimeout` to `0` (disabled) so multi-GB blob uploads are not killed.
   Control-API body limits stay enforced per-handler (`handleEngines` 1 MiB
   cap). Cache-proxy traffic is selected by **Host header** equality with
   `cache.public_host` (dedicated vhost ⇒ no collision with `/api/v1/...`).
3. **Routing strategy** — **least-charged** push + **routing-table lookup with
   self-healing probe** on pull miss. Charge = Supervisor's own per-backend
   manifest-size sum from periodic catalog walks (no sidecar). Routing table
   persisted in SQLite.
4. **Engine→proxy auth** — reuse the existing `DAGGER_CACHE_TOKEN` (from the
   `engine-registry-auth` K8s secret) as the bearer the engine presents to the
   Supervisor proxy. The Supervisor validates it; the real registry credentials
   are injected by the Supervisor and never reach the engine.
5. **Backend credentials** — per-backend `username`/`password` in config
   (`cache.registries[]`, env-injectable). Backward compat: when
   `cache.registries` is empty, synthesize one backend from `cache.internal_addr`
   (or derived from `cache.registry`) with empty creds.
6. **Proxy interception** — Host-header based on the dedicated cache vhost
   (path-based `/v2/` was rejected because the OCI protocol hardcodes `/v2/`;
   the dedicated vhost avoids control-plane collision).

## Scope

**In scope:** config schema, domain types, SQLite routing table, multi-backend
registry client, `RegistryRouter` service, multi-backend stats/purge/GC/status,
cache-proxy rewrite (auth + cred injection + routing + Location rewrite +
WWW-Authenticate suppression), `Cache.BuildCacheConfig` default host, main.go
wiring, dead-code removal (`BuildEngineJSON`), tests, docs/ADR.

**Out of scope:** S3 backend changes (unchanged), Helm chart values (noted as a
follow-up), cert-manager SAN provisioning for the cache vhost (operational doc
only), consistent-hash strategy (only least-charged implemented).

---

## 1. Config schema changes

### `internal/domain/config.go`

Add `RegistryBackend` and extend `CacheConfig`:

```go
// RegistryBackend is one backend OCI registry the Supervisor proxies to.
type RegistryBackend struct {
    ID           string `mapstructure:"id"`
    InternalAddr string `mapstructure:"internal_addr"` // host[:port], no scheme
    Username     string `mapstructure:"username"`
    Password     string `mapstructure:"password"`
}

type CacheConfig struct {
    Backend       string            `mapstructure:"backend"`        // "registry" | "s3"
    Registry      string            `mapstructure:"registry"`       // legacy single ref "host/repo"
    PublicHost    string            `mapstructure:"public_host"`    // dedicated cache vhost
    InternalAddr  string            `mapstructure:"internal_addr"`  // legacy single backend addr
    AuthToken     string            `mapstructure:"auth_token"`     // NEW: engine→proxy bearer
    Registries    []RegistryBackend `mapstructure:"registries"`     // NEW: multi-backend list
    S3            S3Config          `mapstructure:"s3"`
    RefPerVersion bool              `mapstructure:"ref_per_version"`
    GC            GCConfig          `mapstructure:"gc"`
}
```

### `config/loader.go`

Add defaults (every new key gets `v.SetDefault`):

```go
v.SetDefault("cache.auth_token", "")
v.SetDefault("cache.registries", []domain.RegistryBackend{})
```

`cache.public_host` keeps its `""` default; the effective value is resolved in
`cmd/api/main.go` (see §8).

### Validation (new function `config.Validate` or inline in `cmd/api/main.go`)

Implement in `cmd/api/main.go` as `validateCacheConfig(cfg *domain.Config) error`
called right after `config.Load`, before any wiring:

- If `cfg.Cache.Backend != "registry"` ⇒ skip registry validation.
- Resolve `cacheHost`:
  - If `cfg.Cache.PublicHost != ""` ⇒ use it.
  - Else derive `fmt.Sprintf("cache.%s", hostOf(cfg.Server.PublicURL))` where
    `hostOf` strips scheme and path (use `net/url.Parse`, take `u.Host`).
- `controlHost := hostOf(cfg.Server.PublicURL)`.
- If `cacheHost == controlHost` ⇒ return
  `fmt.Errorf("cache.public_host (%s) must differ from the control-plane host (%s); set a dedicated cache vhost", cacheHost, controlHost)`.
- If `cfg.Cache.AuthToken == ""` and no K8s secret is readable ⇒ log a WARN
  (proxy auth disabled; dev mode only). Not fatal.
- Build the effective backend list:
  - If `len(cfg.Cache.Registries) > 0`:
    - Each entry: `ID != ""`, `InternalAddr != ""`. Duplicate `ID` ⇒ error.
    - `InternalAddr` must match `^[A-Za-z0-9._:-]+(:[0-9]+)?$` (host[:port],
      no scheme/path) — defense against SSRF via config (CWE-918).
  - Else synthesize one backend:
    - `InternalAddr = cfg.Cache.InternalAddr`; if empty, derive from
      `cfg.Cache.Registry` via existing `registryHostFrom` (strips path).
    - `ID = "default"`.
- If the effective list is empty ⇒ error `"cache: no backend registry configured"`.
- Return the resolved `cacheHost` and effective `[]RegistryBackend` (pass them
  forward via the `cfg` mutation or local vars in `run`).

### `config/config.app.yaml.sample`

Update the `cache:` section (keep 2-space indent):

```yaml
cache:
  backend: "registry"
  registry: "cache.reg/dagger-cache"   # legacy single-registry ref (used when registries: [] is empty).
  public_host: ""                       # dedicated cache vhost engines push/pull through (Supervisor proxy). Defaults to cache.<server.public_url host>. Must differ from the control-plane host.
  internal_addr: ""                     # legacy single backend address (used when registries: [] is empty).
  auth_token: ""                        # bearer the Supervisor accepts from engines on the cache proxy. Empty = read from the engine-registry-auth K8s secret (key "token"). Never logged.
  registries: []                        # multi-backend list for load balancing. Empty = single-backend mode (legacy).
    # - id: "reg-1"
    #   internal_addr: "registry-1:5000"
    #   username: ""
    #   password: ""
    # - id: "reg-2"
    #   internal_addr: "registry-2:5000"
    #   username: ""
    #   password: ""
  s3: { bucket: "", region: "" }
  ref_per_version: true
  gc: { enabled: false, max_age: "168h", schedule: "1h", min_refs_to_keep: 3, protect_active_versions: true }
```

---

## 2. Domain types

### `internal/domain/cache.go`

Add the routing-table entity types (stdlib only — `domain` imports stdlib):

```go
// CacheRoute is one persisted manifest→backend mapping.
type CacheRoute struct {
    Repo        string
    Tag         string
    Digest      string
    BackendID   string
    StoredBytes int64
    CreatedAt   string // RFC3339
    LastSeenAt  string // RFC3339
}

// CacheUploadSession is one in-flight OCI blob upload session.
type CacheUploadSession struct {
    UploadUUID string
    Repo       string
    BackendID  string
    CreatedAt  string // RFC3339
}
```

No new interfaces in domain for the router (the router lives in `service` and
is injected into the handler via `Deps`). The `CacheBackend` interface is
unchanged.

---

## 3. Repository: routing table + multi-backend client

### `internal/repository/schema.sql` (embedded)

Append two tables to the existing embedded schema (idempotent
`CREATE TABLE IF NOT EXISTS`):

```sql
CREATE TABLE IF NOT EXISTS cache_object_routes (
    repo         TEXT NOT NULL,
    tag          TEXT NOT NULL,
    digest       TEXT NOT NULL DEFAULT '',
    backend_id   TEXT NOT NULL,
    stored_bytes INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    PRIMARY KEY (repo, tag)
);
CREATE INDEX IF NOT EXISTS idx_cache_routes_backend ON cache_object_routes(backend_id);

CREATE TABLE IF NOT EXISTS cache_blob_routes (
    digest       TEXT NOT NULL,
    backend_id   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    PRIMARY KEY (digest, backend_id)
);

CREATE TABLE IF NOT EXISTS cache_upload_sessions (
    upload_uuid TEXT PRIMARY KEY,
    repo        TEXT NOT NULL,
    backend_id  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
```

Bump the migration version: in `internal/repository/sqlite.go` `Migrate`, add a
`v3` block (mirroring the `v2` pattern) that runs `schemaSQL` is already
idempotent, so v3 just records the version. Concretely:

```go
var v3Count int
if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 3").Scan(&v3Count); err != nil {
    return fmt.Errorf("check schema_migrations v3: %w", err)
}
if v3Count == 0 {
    // schema.sql is IF NOT EXISTS; re-applying creates the new tables.
    if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
        return fmt.Errorf("apply schema v3: %w", err)
    }
    if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (3, ?)", time.Now().UTC()); err != nil {
        return fmt.Errorf("record migration v3: %w", err)
    }
}
```

### New file `internal/repository/cache_routes_repo.go`

```go
package repository

import (
    "context"
    "database/sql"
    "errors"
    "fmt"
    "time"

    "github.com/disaster/dagger-kubernetes/internal/domain"
)

type CacheRoutesRepo struct {
    db *sql.DB
}

func NewCacheRoutesRepo(db *sql.DB) *CacheRoutesRepo {
    return &CacheRoutesRepo{db: db}
}

// LookupManifest returns the route for repo+tag. ok=false when absent.
func (r *CacheRoutesRepo) LookupManifest(ctx context.Context, repo, tag string) (domain.CacheRoute, bool, error)

// UpsertManifest inserts or replaces the repo+tag → backend mapping.
func (r *CacheRoutesRepo) UpsertManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error

// LookupBlob returns a backend that holds digest. ok=false when absent.
func (r *CacheRoutesRepo) LookupBlob(ctx context.Context, digest string) (backendID string, ok bool, err error)

// UpsertBlob records that digest is present on backendID (idempotent).
func (r *CacheRoutesRepo) UpsertBlob(ctx context.Context, digest, backendID string) error

// LookupUpload returns the upload session for uuid. ok=false when absent.
func (r *CacheRoutesRepo) LookupUpload(ctx context.Context, uuid string) (domain.CacheUploadSession, bool, error)

// RecordUpload inserts an upload session.
func (r *CacheRoutesRepo) RecordUpload(ctx context.Context, uuid, repo, backendID string) error

// DeleteUpload removes an upload session (on completion or expiry).
func (r *CacheRoutesRepo) DeleteUpload(ctx context.Context, uuid string) error

// BackendCharge returns the sum of stored_bytes for backendID.
func (r *CacheRoutesRepo) BackendCharge(ctx context.Context, backendID string) (int64, error)

// AllCharges returns stored_bytes summed per backend_id.
func (r *CacheRoutesRepo) AllCharges(ctx context.Context) (map[string]int64, error)

// DeleteManifestRoute removes a repo+tag route (used by purge).
func (r *CacheRoutesRepo) DeleteManifestRoute(ctx context.Context, repo, tag string) error

// DeleteRoutesForBackend removes all manifest+blob routes for a backend
// (used when a backend is permanently removed).
func (r *CacheRoutesRepo) DeleteRoutesForBackend(ctx context.Context, backendID string) error

// ReapUploadSessions deletes upload sessions older than maxAge (housekeeping).
func (r *CacheRoutesRepo) ReapUploadSessions(ctx context.Context, maxAge time.Duration) (int, error)
```

Implementation notes:
- `UpsertManifest` uses `INSERT ... ON CONFLICT(repo, tag) DO UPDATE SET
  digest=excluded.digest, backend_id=excluded.backend_id,
  stored_bytes=excluded.stored_bytes, last_seen_at=excluded.last_seen_at`.
- `created_at`/`last_seen_at` use `time.Now().UTC().Format(time.RFC3339)`.
- Wrap SQL errors with `%w`; `sql.ErrNoRows` → `ok=false, err=nil`.
- Digest validation: callers pass digests already validated by
  `validDigest` (reuse from `registry_client.go`); the repo does not re-validate.

### `internal/repository/registry_client.go` (extend)

Add an authenticated variant and a multi-backend helper:

```go
// NewRegistryStatsClientWithAuth returns a client that sends Basic auth.
func NewRegistryStatsClientWithAuth(host, username, password string) *RegistryStatsClient
```

`RegistryStatsClient` gains `username`, `password` fields; `do()` sets
`req.SetBasicAuth(username, password)` when both are non-empty. Existing
`NewRegistryStatsClient(host)` keeps working (empty creds).

Add a `Host()` method (already exists) — unchanged.

---

## 4. Service: `RegistryRouter`

### New file `internal/service/registry_router.go`

```go
package service

import (
    "context"
    "errors"
    "fmt"
    "sync"
    "time"

    "github.com/sirupsen/logrus"

    "github.com/disaster/dagger-kubernetes/internal/domain"
    "github.com/disaster/dagger-kubernetes/internal/repository"
)

// Sentinel errors for routing.
var (
    ErrNoBackend       = errors.New("no cache backend available")
    ErrRouteNotFound   = errors.New("cache route not found")
    ErrInvalidOCIPath  = errors.New("invalid OCI request path")
)

// RegistryRouter picks a backend for each OCI request and persists the
// object→backend mapping. It is safe for concurrent use.
type RegistryRouter struct {
    backends []domain.RegistryBackend
    clients  map[string]*repository.RegistryStatsClient // backendID -> client
    routes   *repository.CacheRoutesRepo
    logger   *logrus.Logger

    mu       sync.RWMutex
    charges  map[string]int64 // backendID -> stored bytes (last refresh)
    down     map[string]bool  // backendID -> unhealthy
}

func NewRegistryRouter(
    backends []domain.RegistryBackend,
    routes *repository.CacheRoutesRepo,
    logger *logrus.Logger,
) *RegistryRouter

// Backends returns a copy of the configured backends.
func (r *RegistryRouter) Backends() []domain.RegistryBackend

// BackendByID returns the backend and ok.
func (r *RegistryRouter) BackendByID(id string) (domain.RegistryBackend, bool)

// ClientByID returns the stats client for a backend (for stats/purge/GC).
func (r *RegistryRouter) ClientByID(id string) (*repository.RegistryStatsClient, bool)

// HealthyBackends returns backends not marked down, ordered least-charged first.
func (r *RegistryRouter) HealthyBackends() []domain.RegistryBackend

// MarkDown / MarkUp toggle health (called by the proxy error handler and the
// stats probe loop).
func (r *RegistryRouter) MarkDown(backendID string)
func (r *RegistryRouter) MarkUp(backendID string)

// SetCharges replaces the charge map (called by CacheStatsService.probe after
// walking all backends).
func (r *RegistryRouter) SetCharges(charges map[string]int64)

// RouteForPull resolves a manifest pull (GET/HEAD /v2/<repo>/manifests/<ref>).
// ref may be a tag or a digest. Table lookup first; on miss, probe healthy
// backends least-charged-first via HEAD manifest; on hit, upsert the route
// (self-heal) and return the backend. Returns ErrRouteNotFound if no backend
// has it.
func (r *RegistryRouter) RouteForPull(ctx context.Context, repo, ref string) (domain.RegistryBackend, error)

// RouteForBlobPull resolves a blob pull (GET/HEAD /v2/<repo>/blobs/<digest>).
// Table lookup by digest; on miss, probe via HEAD blob; self-heal.
func (r *RegistryRouter) RouteForBlobPull(ctx context.Context, digest string) (domain.RegistryBackend, error)

// RouteForPush picks the least-charged healthy backend for a new manifest PUT.
func (r *RegistryRouter) RouteForPush(repo string) (domain.RegistryBackend, error)

// RouteForUploadStart picks the least-charged healthy backend for a new blob
// upload (POST /v2/<repo>/blobs/uploads/).
func (r *RegistryRouter) RouteForUploadStart(repo string) (domain.RegistryBackend, error)

// RouteForUploadResume resolves an in-flight upload by uuid (PATCH/PUT).
func (r *RegistryRouter) RouteForUploadResume(ctx context.Context, uuid string) (domain.RegistryBackend, error)

// RecordUploadSession persists upload_uuid → backend (called from the proxy
// modifyResponse on a successful POST uploads).
func (r *RegistryRouter) RecordUploadSession(ctx context.Context, uuid, repo, backendID string) error

// CompleteUpload deletes the session and records the blob route (called from
// the proxy post-process on a successful PUT ?digest=).
func (r *RegistryRouter) CompleteUpload(ctx context.Context, uuid, digest, backendID string) error

// RecordManifest upserts the repo+tag → backend route (called from the proxy
// post-process on a successful manifest PUT).
func (r *RegistryRouter) RecordManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error

// RefreshCharges recomputes the charge map from the routing table (fallback
// when no catalog walk has run yet).
func (r *RegistryRouter) RefreshCharges(ctx context.Context) error
```

Behavior details:
- `HealthyBackends` orders by `charges[id]` ascending (least-charged first),
  excluding `down[id]==true`. Ties broken by `backends` slice order
  (deterministic).
- `RouteForPull` / `RouteForBlobPull` probe: iterate `HealthyBackends()`; for
  each, call the per-backend `RegistryStatsClient` HEAD (manifest by repo+ref,
  blob by digest). First 200 ⇒ upsert route, return backend. 404 ⇒ continue.
  Transport error ⇒ `MarkDown(id)`, continue. All miss ⇒ `ErrRouteNotFound`.
- `RouteForPush` / `RouteForUploadStart`: `HealthyBackends()[0]`; if empty ⇒
  `ErrNoBackend`.
- `RouteForUploadResume`: table lookup; if absent ⇒ `ErrRouteNotFound` (do NOT
  self-heal — an upload session is bound to one backend; a missing session is a
  client error).
- Probes use a per-call `context.WithTimeout(ctx, 10*time.Second)`.
- All public methods take the `mu` read lock for charge/down reads; writes
  take the write lock. SQLite calls use the passed `ctx`.

---

## 5. Service: multi-backend stats / purge / GC / status

### `internal/service/cache_stats.go` (modify)

Replace the single `registry *repository.RegistryStatsClient` field with a
`router *RegistryRouter` (which exposes per-backend clients + the routes
repo). Keep `cache *Cache`.

`probe(ctx)`:
- For each backend in `router.Backends()`: ping via `router.ClientByID(id)`,
  catalog, tags, manifests — same logic as today but per-backend. Aggregate
  `TotalSize`, `ObjectCount` across all backends.
- After the walk, compute `charges := map[string]int64` (per-backend
  `stored_bytes` sum) and call `router.SetCharges(charges)`.
- Mark a backend `down` on ping failure (`router.MarkDown(id)`); `up` on
  success (`router.MarkUp(id)`).
- `CacheVersionRef.Ref` is now `fmt.Sprintf("%s/%s:%s", publicHost, repo, tag)`
  (uses the **public cache vhost**, not the internal registry host). The
  `registryHost()` helper is replaced by the configured `cache.PublicHost`
  passed into the `Cache` struct (see §6).
- `Versions` aggregate across backends; dedupe by `(repo, tag)`; `Digest` from
  whichever backend returned it.

`Purge(ctx, req)`:
- Resolve `repo` from `cache.Registry` (path portion).
- Look up the route via `router.routes.LookupManifest(repo, tag)`; if found,
  delete from that backend. If not found, probe all backends (self-heal) and
  delete from the first that has it.
- On success, `router.routes.DeleteManifestRoute(repo, tag)`.
- Map `repository.ErrManifestNotFound` → `AlreadyPurged`.

`PurgeAll(ctx)`:
- Iterate all backends' catalogs (via `router.Backends()` + per-backend client),
  delete every tag, and delete the corresponding route rows. Cap at
  `maxPurgeAllTags` total.

`RunGC(ctx)`:
- Same per-backend walk; for each tag older than `MaxAge` and not protected,
  delete from its backend and delete the route row.

`StartGCSweeper` — unchanged (calls `RunGC`).

### `internal/service/status.go` (modify)

`probeCache`:
- For `backend=="registry"`: iterate `router.Backends()`; ping each. State =
  `Down` if all down, `Degraded` if some down, `OK` if all up. Message lists
  down backend IDs (never creds).
- Replace the single `registry *RegistryStatsClient` field with
  `router *RegistryRouter`.

### `internal/service/cache.go` (modify)

`Cache` struct gains `PublicHost` already exists — keep it. The semantics
change: `BuildCacheConfig` **always** rewrites the host to `PublicHost` when
`Type=="registry"` (no longer conditional). If `PublicHost == ""`, leave the
ref as-is (legacy/dev mode where the engine talks to the registry directly —
kept for backward compat, but the startup validation in §1 will have derived a
default so this branch is only hit when explicitly empty in single-backend
dev mode).

```go
func (b *Cache) BuildCacheConfig(v *domain.Version, mode string) string {
    switch b.Type {
    case "registry":
        ref := b.CacheRefForVersion(v)
        if b.PublicHost != "" {
            path := ref
            if _, rest, ok := strings.Cut(ref, "/"); ok {
                path = rest
            }
            ref = fmt.Sprintf("%s/%s", b.PublicHost, path)
        }
        return fmt.Sprintf("type=registry,ref=%s,mode=%s", ref, mode)
    case "s3":
        return fmt.Sprintf("type=s3,bucket=%s,region=%s,mode=%s", b.S3.Bucket, b.S3.Region, mode)
    default:
        return ""
    }
}
```

(Net change vs today: the `if b.PublicHost != ""` block stays, but
`cmd/api/main.go` now always sets `PublicHost` to the resolved cache vhost, so
the rewrite always happens in production. The `""` branch is dev-only.)

**Remove `BuildEngineJSON`** (dead code — no callers; only tests reference it).
Delete the method, the `EngineJSON` and `RegistryAuthEntry` types, and the
corresponding tests in `cache_test.go` (`TestBuildEngineJSON`,
`TestBuildEngineJSONPublicHost`).

---

## 6. Handler: cache proxy rewrite

### `internal/handler/server.go` (modify)

#### `ServerConfig`

```go
type ServerConfig struct {
    ControlAddr   string
    DataAddr      string
    DataHost      string
    CacheHost     string // dedicated cache vhost (Host header to match)
    CacheToken    string // engine→proxy bearer; "" = proxy auth disabled
    CollectorURL  string
    VictoriaURL   string
    CertPath      string
    KeyPath       string
}
```

Remove `InternalReg` (replaced by the router's per-backend targets).

#### `Server` struct

Replace `cacheProxy *reverseproxy.ReverseProxy` with:

```go
cacheProxy  *reverseproxy.ReverseProxy // single instance; target chosen per-request in the director
router      *service.RegistryRouter
cacheToken  string
```

Add `Router *service.RegistryRouter` to `Deps`.

#### `buildProxies()` — cache proxy block

Replace the existing `if s.cfg.CacheHost != "" && s.cfg.InternalReg != ""`
block. The proxy no longer has a fixed target; the director picks the backend
per request. Construct with a placeholder target (the first backend) and set a
custom director + modifyResponse + errorHandler:

```go
if s.cfg.CacheHost != "" && s.router != nil && len(s.router.Backends()) > 0 {
    p, err := reverseproxy.NewSingleHostReverseProxy(fmt.Sprintf("http://%s", s.router.Backends()[0].InternalAddr))
    if err != nil {
        s.logger.WithError(err).Error("invalid cache proxy URL")
    } else {
        p.SetDirector(s.cacheProxyDirector())
        p.SetModifyResponse(s.cacheProxyModifyResponse())
        p.SetErrorHandler(func(c *app.RequestContext, err error) {
            s.logger.WithError(err).Error("cache proxy error")
            writeError(c, consts.StatusBadGateway, "cache backend unreachable")
        })
        s.cacheProxy = p
    }
}
```

#### `cacheProxyDirector()`

Returns `func(req *protocol.Request)`. Reads the chosen backend from a request
header set by `serveCacheHost` (`X-Dagger-Cache-Target` = internal_addr,
`X-Dagger-Cache-User`, `X-Dagger-Cache-Pass`), rewrites the request, strips the
client Authorization, injects backend creds, and deletes the internal headers:

```go
func (s *Server) cacheProxyDirector() func(*protocol.Request) {
    return func(req *protocol.Request) {
        // Never forward the engine's supervisor token to the backend.
        req.Header.Del("Authorization")

        target := string(req.Header.Peek("X-Dagger-Cache-Target"))
        user := string(req.Header.Peek("X-Dagger-Cache-User"))
        pass := string(req.Header.Peek("X-Dagger-Cache-Pass"))
        req.Header.Del("X-Dagger-Cache-Target")
        req.Header.Del("X-Dagger-Cache-User")
        req.Header.Del("X-Dagger-Cache-Pass")

        if target == "" {
            return // leave default target; errorHandler will surface 502
        }
        u, err := url.Parse(fmt.Sprintf("http://%s", target))
        if err == nil {
            req.URI().SetScheme(u.Scheme)
            req.URI().SetHost(u.Host)
            req.Header.SetHostBytes([]byte(u.Host))
        }
        if user != "" || pass != "" {
            // Basic auth to the backend (registry:2 htpasswd). Use stdlib
            // base64 via req.Header.SetBasicAuth-equivalent; hertz provides
            // req.Header.SetBasicAuth(user, pass).
            req.Header.SetBasicAuth(user, pass)
        }
    }
}
```

Note: hertz `protocol.RequestHeader` has `SetBasicAuth`. If not, set
`Authorization: Basic <base64(user:pass)>` manually via `fmt.Sprintf` + stdlib
`encoding/base64`. Never log `user`/`pass`.

#### `cacheProxyModifyResponse()`

Returns `func(*protocol.Response) error`. Two jobs:

1. **Rewrite `Location`** on upload-start responses (202 from
   `POST /v2/<repo>/blobs/uploads/`) so the engine's next PATCH/PUT comes back
   to the Supervisor:
   ```go
   loc := resp.Header.Get("Location")
   if loc != "" && isOCIUploadLocation(loc) {
       rewritten := rewriteUploadLocation(loc, s.cfg.CacheHost)
       resp.Header.Set("Location", rewritten)
   }
   ```
   `rewriteUploadLocation` parses the backend Location, extracts the
   `/v2/<repo>/blobs/uploads/<uuid>` path (and `?digest=` query), and rebuilds
   it as `fmt.Sprintf("https://%s%s", s.cfg.CacheHost, path)`. Preserve the
   query string.
2. **Suppress `WWW-Authenticate`** on backend 401: if `resp.StatusCode() == 401`,
   delete the `WWW-Authenticate` header and return an error so the errorHandler
   maps it to 502:
   ```go
   if resp.StatusCode() == consts.StatusUnauthorized {
       resp.Header.Del("WWW-Authenticate")
       return fmt.Errorf("cache backend auth failed")
   }
   ```
   The errorHandler is the shared one (502 "cache backend unreachable"); to
   distinguish, use a dedicated error sentinel `errBackendAuth` and branch in
   the errorHandler to write 502 "cache backend auth failed".

`isOCIUploadLocation` matches paths containing `/blobs/uploads/`.

#### `cacheHostMiddleware()` — unchanged shape

```go
func (s *Server) cacheHostMiddleware() app.HandlerFunc {
    return func(ctx context.Context, c *app.RequestContext) {
        if !strings.EqualFold(string(c.Host()), s.cfg.CacheHost) {
            c.Next(ctx)
            return
        }
        s.serveCacheHost(ctx, c)
        c.Abort()
    }
}
```

Registered only when `s.cacheProxy != nil` (unchanged).

#### `serveCacheHost(ctx, c)` — rewrite

```go
func (s *Server) serveCacheHost(ctx context.Context, c *app.RequestContext) {
    if s.cacheProxy == nil {
        writeError(c, consts.StatusBadGateway, "cache backend unreachable")
        return
    }
    if !s.requireCacheAuth(c) {
        return
    }
    backend, routeKind, err := s.routeCacheRequest(c)
    if err != nil {
        s.writeCacheRouteError(c, err)
        return
    }
    // Stash backend target + creds on the inbound request headers; the
    // director reads and deletes them. Credentials never logged.
    c.Request.Header.Set("X-Dagger-Cache-Target", backend.InternalAddr)
    c.Request.Header.Set("X-Dagger-Cache-User", backend.Username)
    c.Request.Header.Set("X-Dagger-Cache-Pass", backend.Password)

    s.cacheProxy.ServeHTTP(ctx, c)

    // Post-process: record routes for successful pushes (non-racy; next pull
    // is a separate run). Upload-session recording happens in modifyResponse
    // (pre-response) to avoid the POST→PATCH race.
    s.recordCacheRoute(ctx, c, backend, routeKind)
}
```

#### `requireCacheAuth(c) bool`

Validates the engine's cache token. Accepts `Authorization: Bearer <token>` or
`Authorization: Basic base64(<any-username>:<token>)` (extract the password
field). Compares the token to `s.cacheToken` using `subtle.ConstantTimeCompare`.
If `s.cacheToken == ""` (dev mode) ⇒ allow (log WARN once). On mismatch ⇒
`writeError(c, 401, "unauthorized")` and return false. Never forward the
client header (the director strips it).

```go
func (s *Server) requireCacheAuth(c *app.RequestContext) bool {
    if s.cacheToken == "" {
        return true // dev mode; proxy auth disabled
    }
    tok := extractCacheToken(c)
    if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cacheToken)) != 1 {
        writeError(c, consts.StatusUnauthorized, "unauthorized")
        return false
    }
    return true
}
```

`extractCacheToken` parses `Authorization`: `Bearer <t>` → `t`; `Basic <b>` →
base64-decode, split on `:`, take the password (index 1); empty otherwise.

#### `routeCacheRequest(c) (backend domain.RegistryBackend, kind routeKind, err error)`

Parses the OCI path (`string(c.Path())`, `string(c.Method())`) and dispatches
to the router. Path parsing uses a strict regex set (defense vs path traversal
/ SSRF — the target is always from config, but the path is forwarded, so
validate shape):

```go
var (
    rePing       = regexp.MustCompile(`^/v2/?$`)
    reManifest   = regexp.MustCompile(`^/v2/([^/]+)/manifests/(.+)$`)
    reBlobUpload = regexp.MustCompile(`^/v2/([^/]+)/blobs/uploads/?$`)
    reBlobUploadUUID = regexp.MustCompile(`^/v2/([^/]+)/blobs/uploads/([^/]+)$`)
    reBlob       = regexp.MustCompile(`^/v2/([^/]+)/blobs/(sha256:[a-f0-9]{64})$`)
    reTags       = regexp.MustCompile(`^/v2/([^/]+)/tags/list$`)
    reCatalog    = regexp.MustCompile(`^/v2/_catalog$`)
)
```

Dispatch table:

| Method | Path pattern | Routing |
|--------|--------------|---------|
| GET | `/v2/` | any healthy backend (least-charged) |
| GET, HEAD | `/v2/<repo>/manifests/<ref>` | `router.RouteForPull(repo, ref)` |
| PUT | `/v2/<repo>/manifests/<ref>` | `router.RouteForPush(repo)` |
| POST | `/v2/<repo>/blobs/uploads/` | `router.RouteForUploadStart(repo)` |
| PATCH, PUT | `/v2/<repo>/blobs/uploads/<uuid>` | `router.RouteForUploadResume(uuid)` |
| GET, HEAD | `/v2/<repo>/blobs/<digest>` | `router.RouteForBlobPull(digest)` |
| GET | `/v2/<repo>/tags/list` | any healthy backend |
| GET | `/v2/_catalog` | any healthy backend |

`routeKind` is an enum (`routeManifest`, `routeBlob`, `routeUploadStart`,
`routeUploadComplete`, `routeOther`) used by `recordCacheRoute` to decide what
to record. `routeUploadComplete` is a PUT to `reBlobUploadUUID` with a `?digest=`
query.

Errors:
- No match ⇒ `ErrInvalidOCIPath` → 400 `"invalid OCI request path"`.
- `ErrNoBackend` ⇒ 503 `"no cache backend available"`.
- `ErrRouteNotFound` ⇒ 404 `"cache route not found"`.
- `ErrInvalidOCIPath` ⇒ 400.

`writeCacheRouteError` maps these.

#### `recordCacheRoute(ctx, c, backend, kind)`

Runs after `ServeHTTP` returns. Inspects `c.Response.StatusCode()` and the
request path/method:

- **Manifest PUT** (`routeManifest`, method PUT, status 201/202):
  - `repo`, `tag` from `reManifest`. `digest` from response header
    `Docker-Content-Digest` (validate `validDigest`; else "").
  - `storedBytes = 0` (the next `RefreshCharges`/catalog walk recomputes).
  - `router.RecordManifest(ctx, repo, tag, digest, backend.ID, 0)`.
- **Upload complete** (`routeUploadComplete`, method PUT, `?digest=` present,
  status 201):
  - `uuid` from path, `digest` from query.
  - `router.CompleteUpload(ctx, uuid, digest, backend.ID)`.
- **Upload start** (`routeUploadStart`, method POST, status 202): recorded in
  `modifyResponse` (pre-response) — nothing here.
- All other cases: no-op.

Wrap each DB call in `func() { defer func(){ recover() } }` so a recording
failure never panics the handler (best-effort; log at WARN).

#### Upload-session recording in `modifyResponse`

`cacheProxyModifyResponse` also records the upload session on a successful
`POST /v2/<repo>/blobs/uploads/` (status 202). It needs the repo and uuid:
parse from the (already-rewritten) `Location` header or the
`Docker-Upload-UUID` response header. Then call
`router.RecordUploadSession(ctx, uuid, repo, backendID)`. The `backendID` is
not directly available in `modifyResponse` (it gets `*protocol.Response` only);
stash it on a response header in the director (`X-Dagger-Cache-Backend-ID`,
set from the `X-Dagger-Cache-Target`-derived backend) — or simpler: stash the
backend ID on the inbound request in `serveCacheHost` and read it from the
response's request reference. Since `modifyResponse` lacks the request, set
`resp.Header.Set("X-Dagger-Cache-Backend-ID", backendID)` in the director
(after looking up the backend by target) and read it in `modifyResponse`, then
delete it before the response goes to the client. The director can map
`target → backendID` via `s.router` closure (find the backend whose
`InternalAddr == target`).

#### Read timeout

In `configure()`, change:
```go
server.WithReadTimeout(10 * time.Second),
```
to:
```go
server.WithReadTimeout(0), // disabled: cache-proxy blob uploads are unbounded;
                           // control-API bodies are capped per-handler (handleEngines 1 MiB).
```
Document the trade-off in a comment.

#### Request body size

Do **not** apply `maxRequestBodyBytes` to cache-host requests. The
`maxRequestBodyBytes` cap is only in `handleEngines` (control plane), so no
change needed — just a comment noting cache-host requests are exempt.

---

## 7. `cmd/api/main.go` wiring

In `run(c *cli.Context) error`, after `config.Load` + `validateCacheConfig`:

1. Resolve `cacheHost` and `effectiveBackends` (from §1 validation).
2. Set `cfg.Cache.PublicHost = cacheHost` (so `Cache.BuildCacheConfig` rewrites).
3. Build `cacheBackend := &service.Cache{Type, Registry: cfg.Cache.Registry,
   PublicHost: cacheHost, S3: ...}` (unchanged constructor, new PublicHost).
4. Open the routing repo: `routesRepo := repository.NewCacheRoutesRepo(db)`.
   (Migration already ran via `repository.Migrate`.)
5. Build `router := service.NewRegistryRouter(effectiveBackends, routesRepo, logger)`.
6. Resolve the cache auth token:
   ```go
   cacheToken := cfg.Cache.AuthToken
   if cacheToken == "" {
       cacheToken = loadCacheTokenFromSecret(ctx, clientset, cfg.Fleet.Namespace, logger)
   }
   ```
   `loadCacheTokenFromSecret` reads K8s secret `engine-registry-auth` key
   `token` in `cfg.Fleet.Namespace`; returns "" (with a WARN) if K8s is
   unavailable or the secret is missing. Reuse the existing `clientset`
   (`createProvider` already builds one; refactor `newK8sClientset` to a shared
   call so both `createProvider` and token loading use it).
7. Wire `handler.ServerConfig{CacheHost: cacheHost, CacheToken: cacheToken, ...}`
   (drop `InternalReg`).
8. Add `Deps.Router: router`.
9. `cacheStatsSvc := service.NewCacheStatsService(cacheBackend, router, metricsClient, provider, cfg.Cache.GC, logger, metrics)` — change the registry param from `*RegistryStatsClient` to `*RegistryRouter`.
10. `statusSvc := service.NewStatusService(cfg, cacheBackend, router, fleetManager, logger)` — same signature change.

`registryHostFrom` stays (used to synthesize the legacy single backend).

---

## 8. K8s provider: `DAGGER_CACHE_TOKEN` semantics

**No code change** to `internal/repository/k8s_provider.go`. The env var name,
secret name (`engine-registry-auth`), and key (`token`) are unchanged. The
semantics shift: the token is now the **engine→Supervisor-proxy bearer**, not
a direct registry password. The Supervisor reads the same secret (§7.6) to
validate it. Document this in ADR-014 and the README.

The Helm chart / deployment must ensure the `engine-registry-auth` secret's
`token` value equals `cache.auth_token` (or both unset in dev). This is a docs
change, not a code change.

---

## 9. Edge cases & error handling (exhaustive)

| Case | Behavior | Status / action |
|------|----------|-----------------|
| `cache.public_host` == control-plane host | Startup validation fails | `config.Load` returns error; process exits |
| `cache.registries` empty + `cache.internal_addr` empty + `cache.registry` empty | Startup validation fails | error `"cache: no backend registry configured"` |
| `cache.registries` entry with empty `id` or `internal_addr` | Startup validation fails | error listing the bad entry |
| Duplicate `id` in `cache.registries` | Startup validation fails | error `"duplicate cache backend id"` |
| `internal_addr` with scheme/path | Startup validation fails | error `"cache backend internal_addr must be host[:port]"` |
| Engine sends no Authorization to proxy | `requireCacheAuth` rejects | 401 `"unauthorized"` |
| Engine sends wrong token | `requireCacheAuth` rejects (constant-time) | 401 `"unauthorized"` |
| `cache.auth_token` empty + no K8s secret | Proxy auth disabled (dev) | WARN logged once; requests allowed |
| Non-`/v2/...` path on cache vhost | `routeCacheRequest` no match | 400 `"invalid OCI request path"` |
| All backends down | `HealthyBackends()` empty | 503 `"no cache backend available"` |
| Pull miss (no route, no backend has it) | Self-heal probe exhausted | 404 `"cache route not found"` |
| Pull miss but a backend has it | Self-heal: upsert route, forward | 200 (proxied) |
| Upload resume with unknown uuid | `RouteForUploadResume` miss | 404 `"cache route not found"` (no self-heal) |
| Backend returns 401 to Supervisor | `modifyResponse` strips `WWW-Authenticate`, returns sentinel | errorHandler → 502 `"cache backend auth failed"` |
| Backend returns 404 on a routed pull | Forward the 404 to the engine | 404 (pass-through; the engine treats as cache miss) |
| Backend unreachable (transport) | `errorHandler` + `router.MarkDown(id)` | 502 `"cache backend unreachable"` |
| Malformed `Location` on upload start | `rewriteUploadLocation` falls back to original | pass-through (engine may fail; logged WARN) |
| Blob upload body > memory | Hertz reverseproxy streams; no buffering | unbounded (read timeout disabled) |
| Path traversal in OCI path (`..`, `//`) | Strict regex rejects | 400 `"invalid OCI request path"` |
| SSRF via `internal_addr` config | Validated at startup (host[:port] only); target never from client input | rejected at startup |
| Digest in path not `sha256:<hex>` | `reBlob` regex rejects | 400 `"invalid OCI request path"` |
| `Docker-Content-Digest` malformed on manifest PUT | `validDigest` fails ⇒ record `digest=""` | route still recorded (digest optional) |
| Recording DB write fails | best-effort, logged WARN, never panics | response already sent; next pull self-heals |
| Single backend configured | `HealthyBackends()` returns it; no load balancing | works (legacy behavior) |
| Backend added after objects pushed | Pull miss → self-heal probe finds it on the new backend | table converges |
| DB wiped | All routes lost; pulls self-heal by probing | cache stays available (slower first hit) |
| `cache.backend == "s3"` | Proxy not built; `BuildCacheConfig` emits s3 ref | unchanged |

---

## 10. Test plan (stdlib `testing`, table-driven, 100% coverage target)

### `internal/repository/cache_routes_repo_test.go` (new)
Table-driven cases for every method: upsert/lookup round-trip, lookup miss,
conflict upsert (replace), blob dedupe, upload session lifecycle, charge sums,
delete by backend, reap old sessions. Use a temp SQLite DB
(`repository.OpenSQLite(t.TempDir()+"/t.db")` + `Migrate`).

### `internal/repository/registry_client_test.go` (extend)
Add `NewRegistryStatsClientWithAuth` cases: Basic auth header set on requests
(verify via a stub server or `httptest.NewServer`).

### `internal/service/registry_router_test.go` (new)
- `HealthyBackends` ordering by charge (table-driven: charges map → expected order).
- `RouteForPush` least-charged; `ErrNoBackend` when all down.
- `RouteForPull` table hit (no probe); table miss → probe hit (stub client
  returns 200) → self-heal upsert; probe all miss → `ErrRouteNotFound`.
- `RouteForBlobPull` same matrix.
- `RouteForUploadResume` hit / miss (no self-heal).
- `RecordUploadSession` / `CompleteUpload` lifecycle.
- `RecordManifest` upsert + `RefreshCharges` recomputes.
- `MarkDown`/`MarkUp` excludes/Includes from healthy set.
- Stub `*RegistryStatsClient` via a thin interface or `httptest` server
  returning canned manifest/blob HEAD responses.

### `internal/service/cache_test.go` (modify)
- Update `TestBuildCacheConfigRegistryPublicHost` (still valid).
- Add `TestBuildCacheConfigRegistryRewritesByDefault` (PublicHost set ⇒
  rewritten; the production path).
- Remove `TestBuildEngineJSON`, `TestBuildEngineJSONPublicHost`.

### `internal/service/cache_stats_test.go` (modify)
- Multi-backend `probe`: two stub backends, assert aggregated `TotalSize`,
  per-backend `SetCharges` called, `MarkDown` on a failing backend.
- `Purge` routes to the backend that holds the tag (route table hit) and
  deletes the route row.
- `PurgeAll` iterates all backends.

### `internal/service/status_test.go` (modify)
- All-up ⇒ OK; one-down ⇒ Degraded; all-down ⇒ Down.

### `internal/handler/cache_proxy_test.go` (new)
- `requireCacheAuth`: Bearer correct/wrong/empty; Basic correct (password
  matches); dev mode (`cacheToken==""`) allows.
- `routeCacheRequest`: table-driven path/method matrix → expected routing
  call + error mapping (400/404/503).
- `cacheProxyDirector`: strips client `Authorization`, sets backend Basic
  auth, rewrites host, deletes internal headers.
- `cacheProxyModifyResponse`: rewrites `Location` on 202 upload; strips
  `WWW-Authenticate` on 401 → returns sentinel.
- `recordCacheRoute`: manifest PUT 201 ⇒ `RecordManifest` called; upload
  complete PUT 201 ⇒ `CompleteUpload` called; non-2xx ⇒ no-op.
- End-to-end via `ut.PerformRequest` with a stub `RegistryRouter` (interface
  it in the handler for testability — see below) and a stub backend
  (`httptest.NewServer` emulating `/v2/...`).

**Testability refactor:** introduce a small interface in the handler package
for the router methods the handler uses, so tests inject a stub:
```go
type cacheRouter interface {
    Backends() []domain.RegistryBackend
    BackendByID(id string) (domain.RegistryBackend, bool)
    RouteForPull(ctx context.Context, repo, ref string) (domain.RegistryBackend, error)
    RouteForBlobPull(ctx context.Context, digest string) (domain.RegistryBackend, error)
    RouteForPush(repo string) (domain.RegistryBackend, error)
    RouteForUploadStart(repo string) (domain.RegistryBackend, error)
    RouteForUploadResume(ctx context.Context, uuid string) (domain.RegistryBackend, error)
    RecordUploadSession(ctx context.Context, uuid, repo, backendID string) error
    CompleteUpload(ctx context.Context, uuid, digest, backendID string) error
    RecordManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error
    MarkDown(backendID string)
}
```
`Server.router` is typed as this interface; `*service.RegistryRouter` satisfies
it. This follows the existing `domain.CacheBackend` stub pattern.

### `cmd/api/main_test.go` (extend)
- `validateCacheConfig`: public_host collision ⇒ error; empty backends ⇒
  error; duplicate id ⇒ error; bad internal_addr ⇒ error; default derivation
  `cache.<host>`; single-backend synthesis from `internal_addr` / `registry`.

### `tests/integration/cache_proxy_test.go` (new)
Black-box: stand up a fake OCI registry (`httptest.NewServer` implementing
`/v2/`, manifests, blob uploads) behind the Supervisor proxy with two backends;
drive the engine-side client (stdlib `net/http` with the cache token) through
the proxy; assert push lands on the least-charged backend, pull routes back,
upload session affinity holds across PATCH/PUT, and the engine never sees
backend credentials.

---

## 11. Documentation changes

### `config/config.app.yaml.sample`
Updated `cache:` section (see §1).

### `docs/README.md`
- Update the "Remote shared cache" section: the emitted ref now points at
  `cache.public_host` (the Supervisor proxy), never the raw registry. Show the
  new example:
  `type=registry,ref=cache.supv.example.com/dagger-cache:V0-21-4,mode=max`.
- Update the architecture diagram: engines push/pull through the Supervisor
  cache proxy (Host = cache vhost), which holds backend credentials and routes
  across N registries.
- Add a "Multi-registry cache" subsection: configure `cache.registries[]`,
  explain least-charged routing + self-healing, the routing table, and the
  `cache.auth_token` / `engine-registry-auth` secret relationship.
- Note the TLS SAN requirement: the control-plane cert must include
  `cache.public_host` as a SAN.
- Note the read-timeout change and its rationale.

### `docs/design/ADR-014-registry-proxy-token-loadbalancing.md` (new)
- **Context:** the cache ref exposed the raw registry; the Supervisor must
  control tokens and load-balance across registries.
- **Decision:** dedicated cache vhost; Supervisor terminates engine auth
  (`DAGGER_CACHE_TOKEN`) and injects backend creds; least-charged push +
  routing-table pull with self-healing probe; SQLite routing table; charge via
  catalog-walk manifest-size sum.
- **Alternatives considered:** consistent hashing (split cache on backend
  failure), in-memory table (lost on restart), sidecar stats (operational
  burden), fan-out on miss (N× load), path-based `/v2/` interception (OCI
  hardcodes `/v2/`), separate listener (rejected for simplicity).
- **Consequences:** new config keys; SQLite v3 migration; `BuildEngineJSON`
  removed; read timeout disabled; TLS SAN must include the cache vhost.

### `docs/design/ADR-006-oci-registry-cache-backend.md` (update)
Add a note: "Superseded in part by ADR-014: the cache ref now targets the
Supervisor proxy vhost by default; `cache.public_host` is the dedicated cache
vhost."

---

## 12. Implementation steps (ordered)

1. **Domain + config** — add `RegistryBackend`, extend `CacheConfig`, add
   `CacheRoute`/`CacheUploadSession`; add `v.SetDefault` for new keys; write
   `validateCacheConfig` in `cmd/api/main.go` (+ tests in `main_test.go`).
2. **Schema + migration** — append the three tables to `schema.sql`; add the
   `v3` migration block in `sqlite.go`; add `cache_routes_repo.go` (+ tests).
3. **Registry client** — add `NewRegistryStatsClientWithAuth` + Basic auth in
   `do()` (+ tests).
4. **RegistryRouter** — new `internal/service/registry_router.go` (+ tests).
5. **Cache service** — remove `BuildEngineJSON` + types + tests; update
   `BuildCacheConfig` (always-rewrite semantics via main.go setting
   `PublicHost`).
6. **CacheStatsService / StatusService** — switch from single client to
   `*RegistryRouter`; multi-backend probe/purge/GC/status (+ tests).
7. **Handler** — `ServerConfig` (`CacheHost`, `CacheToken`, drop
   `InternalReg`); `Deps.Router`; `cacheRouter` interface; rewrite
   `buildProxies` cache block, `cacheProxyDirector`,
   `cacheProxyModifyResponse`, `serveCacheHost`, `requireCacheAuth`,
   `routeCacheRequest`, `recordCacheRoute`, `writeCacheRouteError`; disable
   read timeout (+ tests).
8. **main.go wiring** — resolve `cacheHost` + `effectiveBackends`, build
   `router`, load cache token (config or K8s secret), wire `Deps.Router` and
   `ServerConfig`; refactor `newK8sClientset` to shared helper.
9. **Integration test** — `tests/integration/cache_proxy_test.go` with a
   fake OCI registry.
10. **Docs** — update `config.app.yaml.sample`, `docs/README.md`, ADR-006,
    add ADR-014.

## Open questions / out of scope

- Helm chart values for `cache.registries[]` and the cache vhost ingress/TLS
  SAN — follow-up PR (noted in ADR-014).
- Consistent-hash strategy as an alternative `cache.routing.strategy` — not
  implemented (only least-charged).
- Upload-session reaping cadence — wire `ReapUploadSessions` into the existing
  30s sweep ticker in `main.go` (cheap; include in step 8).
