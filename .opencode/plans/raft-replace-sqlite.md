# Plan: Replace SQLite with a Hashicorp Raft-backed distributed store

- **Module:** `github.com/disaster/dagger-kubernetes`
- **Working dir:** `/projects/dagger-cache`
- **Go version:** `go 1.26.5` (from `go.mod`)
- **Scope:** Storage-engine swap under the `repository` layer. Public Go APIs
  (`domain.*` interfaces, `service.*` signatures, `handler.*` endpoints) are
  preserved. The session/lease store (`service.Store`) stays in-memory (it is
  local, TTL-based, and explicitly NOT persisted — see ADR-010 / `session.go`).
- **Directive (binding):** SQLite is **fully removed** from the project. There
  is **NO migration path** for existing SQLite data — this is a greenfield
  storage swap. Operators start fresh; existing SQLite data is intentionally
  not carried over. The `modernc.org/sqlite` dependency is removed from
  `go.mod`/`go.sum`. No `migrate-sqlite` subcommand, no `MigrateSQLite`
  function, no `MigrationSummary` type, no `sqlite_migrated` marker, no startup
  auto-detect of legacy SQLite files.

## 1. Summary & architectural overview

Today the supervisor persists multi-user RBAC state (users, groups,
memberships, API tokens, projects, trace metadata, the `meta` KV table, and
the cache object/blob/upload routing tables) in a single SQLite file via
`modernc.org/sqlite` (pure-Go, no CGO). All repository implementations live in
`internal/repository/` and take a `*sql.DB`.

This plan **removes SQLite entirely** and replaces it with a **Hashicorp
Raft** replicated state machine. **Raft always runs**, even for a single-node
deployment (mandate). There is no non-Raft code path. The same binary, the
same config shape, and the same domain/service/handler layers serve a
one-node cluster and an N-node cluster identically.

The new Raft-backed store starts **fresh (empty state)**. A fresh Raft
cluster with an empty FSM is the ONLY supported state. The bootstrap-admin
flow (admin user creation when `users.Count() == 0`) handles fresh-state
provisioning exactly as it would for an empty SQLite DB.

### Components

- **FSM** (`internal/repository/fsm.go`): an in-memory Go struct of maps
  protected by a `sync.RWMutex`, holding every row that SQLite held. Reads are
  served directly from the FSM (O(1) map lookups). Writes are deterministic
  mutations applied from the Raft log.
- **LogStore + StableStore**: `github.com/hashicorp/raft-boltdb/v2`
  (`boltdb.New`), a single `raft.db` file. Pure-Go (bbolt, no CGO) — preserves
  the existing static-binary / no-CGO story that motivated `modernc.org/sqlite`.
- **SnapshotStore**: `raft.NewFileSnapshotStore` (durable files under
  `<data_dir>/snapshots/`). File snapshots survive restart and support log
  compaction.
- **Transport**: `raft.NewTCPTransport` bound to a dedicated `raft.bind_addr`
  (separate from the control `:8080` and data `:8443` listeners). Optional TLS
  via `raft.NewNetworkTransportWithConfig` + `tls.Config` (gated by
  `raft.tls.enabled`, off by default for v1; documented as a hardening
  follow-up). `raft.NewInmemTransport` is used in tests.
- **Leadership**: writes go through `raft.Apply` on the leader. A follower
  returns `domain.ErrNotLeader`. Reads are served from the **local** FSM on
  any node (stale reads bounded by Raft replication latency, typically
  <100ms — the Consul "stale read" model). For the single-node default this is
  moot (the node is always leader and the FSM is authoritative).
- **Bootstrap**: every node is configured with its own `raft.node_id` +
  `raft.bind_addr` and the full `raft.peers` list. On first boot (no existing
  data dir) a node calls `raft.BootstrapCluster` with the full voter
  configuration; `ErrCantBootstrap` (existing state) is ignored. All nodes
  bootstrapping with the same peer list is the documented hashicorp pattern.
  Single-node = a one-element peer list (or empty, in which case the node
  bootstraps with itself alone).
- **Node identity**: a stable `node-id` file is generated on first boot
  (`<data_dir>/node-id`, UUIDv4) and reused across restarts. Raft membership
  requires stable IDs; regenerating on every restart would break the cluster.
- **Startup barrier**: after `raft.NewRaft`, the supervisor waits on
  `raft.LeaderCh()` for up to `raft.leader_wait_timeout` (default 30s) so that
  the first `MetaStore.Set` (JWT secret / token-encryption key) can succeed.
  On timeout the supervisor fails to start with a clear error.

### Data NOT moved to Raft (explicitly out of scope, behavior preserved)

- `service.Store` (leases) — in-memory, TTL, local to a node. Stays as-is.
- `repository.LiveHub` (SSE pub/sub) — in-memory. Stays as-is.
- `repository.MintingCA` / CA providers — disk-backed at `tls.ca_path`. Stays.
- `repository.SpanTreeReconstructor` / `LogsClient` / `MetricsClient` —
  external (Tempo/Loki/Victoria). Stays.
- Fleet provider (K8s StatefulSets) — Kubernetes is the source of truth. Stays.

## 2. File plan (exact paths)

### New files

| Path | Purpose |
|---|---|
| `internal/repository/raft_store.go` | `RaftStore` (raft node wrapper: construct, bootstrap, `apply`, leader-wait, node-id, shutdown). |
| `internal/repository/raft_store_test.go` | Unit tests for `RaftStore` (single-node inmem: apply, leader wait, not-leader, shutdown). |
| `internal/repository/fsm.go` | `FSM`, `fsmState`, `command` + `commandKind` enum, payload structs, `Apply`, `Snapshot`, `Restore`, `applyCommand` (core deterministic logic), and all typed read helpers (`readUserByID`, etc.). |
| `internal/repository/fsm_snapshot.go` | `fsmSnapshot` (`raft.FSMSnapshot`: `Persist`, `Release`) + `fsmSnapshotPayload` struct. |
| `internal/repository/fsm_test.go` | Table-driven unit tests for `applyCommand` (every kind) + `Snapshot`/`Restore` round-trip, exercising the FSM directly (no Raft) for speed. |
| `internal/repository/meta_store.go` | Raft-backed `MetaStore` (replaces the one in `sqlite.go`); same `Get`/`Set` signatures so `cmd/api/main.go` and its tests compile. |
| `internal/repository/raft_test_helpers_test.go` | `newTestRaftStore(t)` (single-node Raft with `raft.NewInmemStore` + `raft.NewInmemSnapshotStore` + `raft.NewInmemTransport`) and `newLocalFSM(t)` (bare FSM for pure-logic tests). Replaces `sqlite_test.go`'s `newTestDB`. |
| `docs/design/ADR-015-raft-replaces-sqlite.md` | New ADR (style matches ADR-009/010). |
| `.opencode/plans/raft-replace-sqlite.md` | This plan. |

### Deleted files (SQLite fully removed)

| Path | Reason |
|---|---|
| `internal/repository/sqlite.go` | Removed entirely. No `OpenSQLite`, no `Migrate`, no `MetaStore` (moved to `meta_store.go`), no `//go:embed schema.sql`, no `modernc.org/sqlite` import. |
| `internal/repository/schema.sql` | Removed entirely. No longer embedded; the FSM has no SQL schema. |
| `internal/repository/sqlite_test.go` | Removed entirely. Replaced by `raft_test_helpers_test.go`. |
| `internal/repository/sqlite_migrate_test.go` | Removed entirely. SQLite-specific tests; no SQLite remains. |

### Modified files

| Path | Change |
|---|---|
| `internal/repository/user_repo.go` | `UserRepo{store *RaftStore}`; reads delegate to `store.fsm.read*`; writes via `store.apply`. Drop `*sql.DB`, `userCols`, `scanUser`, `queryUser`. Keep `var _ domain.UserRepository = (*UserRepo)(nil)`. |
| `internal/repository/group_repo.go` | Same re-backing. `SetMembers` becomes one `kindSetMembers` command (atomic in the log — no SQL transaction needed). `Members`/`GroupsForUser`/`AllMemberships` read from FSM. |
| `internal/repository/project_repo.go` | Same re-backing. |
| `internal/repository/token_repo.go` | Same re-backing. `Upsert` enforces one-token-per-user in the FSM (returns `domain.ErrConflict` wrapped, preserving the handler's 409 mapping). |
| `internal/repository/tracemeta_repo.go` | Same re-backing. `UpsertProvision`/`UpsertIngest` preserve the exact COALESCE "first-non-empty-wins" semantics in Go. `List` does the join + filter in-memory (group/user name lookup via FSM maps). |
| `internal/repository/cache_routes_repo.go` | Same re-backing. `BackendCharge`/`AllCharges` aggregate in-memory. `ReapUploadSessions` carries a cutoff timestamp in the command. Constructor signature changes from `NewCacheRoutesRepo(db *sql.DB)` to `NewCacheRoutesRepo(store *RaftStore)` — `service.RegistryRouter` takes the concrete type, so it is unaffected. |
| `internal/repository/*_repo_test.go` | Switch from `newTestDB(t)` + `NewXxxRepo(db)` to `newTestRaftStore(t)` + `NewXxxRepo(store)`. Preserve every existing test case (these encode the contract). |
| `internal/repository/cache_routes_repo_test.go` | Same switch. |
| `internal/domain/config.go` | Replace `DatabaseConfig{Path string}` with `DatabaseConfig{Dir string}`. Add `RaftConfig` struct (see §8). Add `Raft RaftConfig `mapstructure:"raft"`` to `Config`. |
| `internal/domain/identity.go` | Add sentinels `ErrNotLeader = errors.New("not the raft leader")`, `ErrConflict = errors.New("resource already exists")`, and `ErrRaftTimeout = errors.New("raft apply timeout")`. |
| `config/loader.go` | Remove `v.SetDefault("database.path", ...)`. Add `v.SetDefault("database.dir", "/var/lib/dagger-cache")` and all `raft.*` defaults (see §8). |
| `config/config.app.yaml` | Replace `database.path` with `database.dir`; add `raft:` section. |
| `config/config.app.yaml.sample` | Same; keep in sync with `loader.go` (mandatory per AGENTS.md). |
| `cmd/api/main.go` | Replace `OpenSQLite`+`Migrate` with `repository.NewRaftStore(...)` + `store.WaitForLeader(ctx)`. Update all `NewXxxRepo(db)` → `NewXxxRepo(store)`. Add a leader-observation goroutine (logs + metrics on leadership change). Shutdown: `store.Close()`. **No `migrate-sqlite` subcommand. No SQLite open. No `//go:embed schema.sql`.** |
| `cmd/api/main_test.go` | `newMetaStore(t)` builds a `newTestRaftStore(t)` + `repository.NewMetaStore(store)`. |
| `internal/handler/middleware.go` | In `writeServiceError`: add `case errors.Is(err, domain.ErrNotLeader): writeError(c, StatusServiceUnavailable, "not the leader")`, `case errors.Is(err, domain.ErrConflict): writeError(c, StatusConflict, "resource already exists")`, and `case errors.Is(err, domain.ErrRaftTimeout): writeError(c, StatusGatewayTimeout, "raft apply timeout")`. Remove the `isUniqueViolation` SQLite-string-match fallback (no SQLite errors remain). |
| `internal/handler/test_helper_test.go` | `newTestEnv` uses `newTestRaftStore`-equivalent (construct via `repository.NewRaftStore` with inmem transport, or a shared test helper exported from `repository`). |
| `internal/service/test_helper_test.go` | `newServiceDB` returns repos backed by a test Raft store. |
| `tests/integration/{api,rbac,cache_proxy,cache_status}_test.go` | Replace `OpenSQLite`+`Migrate`+`NewXxxRepo(db)` with a single-node Raft store + repos. |
| `deploy/helm/dagger-kubernetes/values.yaml` | `supervisor.config.database.dir` (replace `path`); add `supervisor.config.raft.*`; add raft container port; bump PVC default to 2Gi (snapshots + bolt log). |
| `deploy/helm/dagger-kubernetes/templates/configmap.yaml` | Render `database.dir` + `raft:` block. |
| `deploy/helm/dagger-kubernetes/templates/deployment.yaml` | Add `raft` container port (`containerPort: 8081`); add `DAGGER_CACHE_RAFT_*` env passthrough; mount `db` volume (already present) at `/var/lib/dagger-cache`. |
| `deploy/helm/dagger-kubernetes/templates/pvc.yaml` | No structural change (size comes from values). |
| `docs/design/index.md` | Add row for ADR-015. |
| `docs/README.md` | Update the "Configuration → Database" section: describe Raft, single-node default, multi-node peers, the fresh-start behavior, and the full removal of SQLite. **No migration runbook.** |
| `go.mod` / `go.sum` | Add `github.com/hashicorp/raft`, `github.com/hashicorp/raft-boltdb/v2` (and transitive `go.etcd.io/bbolt`, `github.com/hashicorp/go-msgpack/codec`, `github.com/hashicorp/go-hclog`, `github.com/hashicorp/go-immutable-radix`, `github.com/hashicorp/golang-lru/v2`). **Remove `modernc.org/sqlite`** (and its transitive deps if unused elsewhere) via `go mod tidy`. |

### Files NOT changed (verified)

- `internal/service/*` (production) — consume `domain.*` interfaces; untouched.
  (`service/registry_router.go` keeps `*repository.CacheRoutesRepo`; only the
  repo's backing changes.)
- `internal/handler/*` (production) — untouched except `middleware.go`'s error
  mapper (three new cases, SQLite-string-match fallback removed).
- `internal/domain/*` — untouched except `config.go` (new fields) and
  `identity.go` (three new sentinels). `domain` stays stdlib-only.
- `cmd/ci/main.go` — no DB access; untouched.

## 3. FSM data structures

### 3.1 In-memory state (`fsmState`)

```go
// internal/repository/fsm.go
package repository

import (
    "encoding/json"
    "fmt"
    "io"
    "sort"
    "strings"
    "sync"
    "time"

    "github.com/hashicorp/raft"

    "github.com/disaster/dagger-kubernetes/internal/domain"
)

type fsmState struct {
    mu sync.RWMutex

    users         map[string]*domain.User // id -> user
    usersByName   map[string]string       // lower(username) -> id
    usersByOAuth  map[string]string       // "provider\x00oauthID" -> id

    groups        map[string]*domain.Group // id -> group
    groupsByName  map[string]string        // lower(name) -> id
    memberships        map[string]map[string]struct{} // groupID -> set(userID)
    membershipsByUser  map[string]map[string]struct{} // userID -> set(groupID)

    tokens         map[string]*domain.APIToken // id -> token
    tokensByHash   map[string]string          // tokenHash -> id
    tokensByUser   map[string]string          // userID -> id (one per user)

    projects        map[string]*domain.Project // id -> project
    projectsByName  map[string]string          // lower(name) -> id

    traces map[string]*domain.TraceMeta // traceID -> meta

    meta map[string]string // key -> value (jwt_secret, token_encryption_key)

    // cache routing (v3 schema)
    cacheObjectRoutes   map[string]*domain.CacheRoute            // "repo\x00tag" -> route
    cacheBlobRoutes     map[string]map[string]string             // digest -> backendID -> createdAt
    cacheUploadSessions map[string]*domain.CacheUploadSession   // uuid -> session
}
```

Notes:
- **Case-insensitive uniqueness** (SQLite `COLLATE NOCASE` on `users.username`,
  `groups.name`, `projects.name`) is reproduced by keying the `*ByName` maps on
  `strings.ToLower(...)`.
- **Reverse indices** (`membershipsByUser`, `tokensByUser`, `*ByHash`,
  `*ByName`, `usersByOAuth`) are maintained as invariants inside `applyCommand`
  under the write lock so reads are O(1).
- All read helpers return **deep copies** of entities (the `*Lease` pattern
  from `service.Store.List` is the model) so callers can read fields without
  racing with `Apply`.
- The `meta` map holds only runtime keys (`jwt_secret`,
  `token_encryption_key`). There is no `sqlite_migrated` marker — SQLite is
  fully removed and there is no migration.

### 3.2 Commands (Apply log entries)

```go
type commandKind byte

const (
    kindUpsertUser commandKind = iota + 1
    kindDeleteUser
    kindUpsertGroup
    kindDeleteGroup
    kindSetMembers // {GroupID, UserIDs}
    kindUpsertProject
    kindDeleteProject
    kindUpsertToken
    kindDeleteToken // {UserID}
    kindTouchToken  // {ID, At}
    kindUpsertTraceProvision // {TraceID, UserID, Version}
    kindUpsertTraceIngest    // {TraceMeta}
    kindSetMeta // {Key, Value}
    kindUpsertManifestRoute      // {CacheRoute}
    kindDeleteManifestRoute      // {Repo, Tag}
    kindDeleteRoutesForBackend   // {BackendID}
    kindUpsertBlobRoute          // {Digest, BackendID}
    kindRecordUpload             // {CacheUploadSession}
    kindDeleteUpload             // {UUID}
    kindReapUploads              // {CutoffRFC3339}
)

type command struct {
    Kind commandKind   `json:"k"`
    Data json.RawMessage `json:"d"`
}
```

Each kind has a small payload struct (e.g. `cmdSetMembers{ GroupID string; UserIDs []string }`).
The repository marshals the payload into `command.Data`; the FSM unmarshals it
inside `applyCommand`.

### 3.3 Serialization choice: JSON

- **Commands and snapshots: `encoding/json`.**
- Justification:
  - All `domain.*` entities already carry `json:"..."` tags (`User`, `Group`,
    `Project`, `APIToken`, `TraceMeta`, `CacheRoute`, `CacheUploadSession`).
  - JSON is debuggable (operators can decode a snapshot file or a raft log
    payload during incident response).
  - `encoding/json` is stdlib — keeps `domain` stdlib-only and avoids a new
    dependency in the FSM hot path.
  - The dataset is small (ADR-010: "fits comfortably in a 1 GiB PVC"); JSON's
    size overhead vs gob/msgpack is irrelevant here.
  - Raft internally uses `hashicorp/go-msgpack/codec` for its own protocol, but
    the `Apply` payload and snapshot bytes are entirely our choice — JSON is
    the most common choice in hashicorp's own examples.
- gob would require type registration and is less debuggable; msgpack would add
  a dependency for no real gain. JSON is the recommended choice.
- **Future optimization (out of scope):** switch snapshots to gob if snapshot
  size becomes a concern; documented in ADR-015.

## 4. Function signatures (new code)

### 4.1 `RaftStore` (`internal/repository/raft_store.go`)

```go
package repository

type RaftStore struct {
    raft    *raft.Raft
    fsm     *FSM
    timeout time.Duration
    logger  *logrus.Logger
    dir     string
}

// NewRaftStore constructs and starts a Raft node. It:
//   - loads/generates a stable node ID at <dir>/node-id
//   - opens raft-boltdb log+stable store at <dir>/raft.db
//   - opens a file snapshot store at <dir>/snapshots
//   - creates the TCP transport bound to cfg.BindAddr
//   - bootstraps the cluster from cfg.Peers (single-node when len(Peers)==0)
//   - returns the store; caller must WaitForLeader before issuing writes.
func NewRaftStore(cfg RaftStoreConfig, logger *logrus.Logger) (*RaftStore, error)

type RaftStoreConfig struct {
    Dir            string         // data directory (raft.db, snapshots/, node-id)
    NodeID         string         // stable server ID ("" = load/generate from <dir>/node-id)
    BindAddr       string         // transport bind address (host:port)
    Peers          []RaftPeer     // full voter list; empty = single-node (self only)
    ApplyTimeout   time.Duration  // default 5s
    SnapshotThreshold uint64      // default 1000 (raft config)
    SnapshotInterval   time.Duration // default 10m
    TrailingLogs        uint64      // default 256
    TLS               *tls.Config   // nil = plaintext transport (v1 default)
}

type RaftPeer struct {
    ID      string
    Address string // host:port
}

// WaitForLeader blocks until the node becomes leader or ctx expires. Required
// before the first write (e.g. MetaStore.Set at startup).
func (s *RaftStore) WaitForLeader(ctx context.Context) error

// apply marshals cmd, calls raft.Apply, and maps the result to a Go error.
// Returns domain.ErrNotLeader when this node is not the leader, or when
// raft.Apply reports ErrNotLeader. Returns the FSM's response error when the
// FSM returned one (domain.ErrNotFound / domain.ErrConflict / ...).
// Returns domain.ErrRaftTimeout when raft.Apply reports a deadline.
func (s *RaftStore) apply(cmd *command) error

// fsmRead returns the FSM for direct reads. Read helpers take the RLock
// internally; callers must not hold it across a write.
func (s *RaftStore) fsmRead() *FSM

// IsLeader reports whether this node is the Raft leader.
func (s *RaftStore) IsLeader() bool

// LeaderCh returns raft.LeaderCh() for the leader-observation goroutine.
func (s *RaftStore) LeaderCh() <-chan bool

// Close shuts the raft node and closes the transport and bolt store.
func (s *RaftStore) Close() error
```

### 4.2 `FSM` (`internal/repository/fsm.go`)

```go
type FSM struct {
    state *fsmState
}

func NewFSM() *FSM

// raft.FSM interface
func (f *FSM) Apply(log *raft.Log) interface{}      // unmarshals command, calls applyCommand, returns error or nil
func (f *FSM) Snapshot() (raft.FSMSnapshot, error)  // deep-copies state under RLock
func (f *FSM) Restore(rc io.ReadCloser) error       // JSON-decodes snapshot, replaces state under Lock

// applyCommand is the deterministic core. It is called by Apply AND directly
// by unit tests (fsm_test.go) without a Raft instance.
func (f *FSM) applyCommand(cmd *command) error

// Typed read helpers (all return copies; all take RLock internally):
func (f *FSM) readUserByID(id string) (*domain.User, error)
func (f *FSM) readUserByUsername(username string) (*domain.User, error)
func (f *FSM) readUserByOAuth(provider, oauthID string) (*domain.User, error)
func (f *FSM) listUsers() []*domain.User
func (f *FSM) countUsers() int

func (f *FSM) readGroupByID(id string) (*domain.Group, error)
func (f *FSM) readGroupByName(name string) (*domain.Group, error)
func (f *FSM) listGroups() []*domain.Group
func (f *FSM) members(groupID string) []*domain.User
func (f *FSM) groupsForUser(userID string) []*domain.Group
func (f *FSM) allMemberships() map[string][]string // groupID -> sorted userIDs

func (f *FSM) readProjectByID(id string) (*domain.Project, error)
func (f *FSM) readProjectByName(name string) (*domain.Project, error)
func (f *FSM) listProjects() []*domain.Project

func (f *FSM) readTokenByHash(hash string) (*domain.APIToken, error)
func (f *FSM) readTokenByUser(userID string) (*domain.APIToken, error)

func (f *FSM) readTrace(traceID string) (*domain.TraceMeta, error)
func (f *FSM) listTraces(f domain.TraceFilter) []*domain.TraceListResult // in-memory join + filter + sort + clamp

func (f *FSM) readMeta(key string) (string, error) // ErrNotFound when missing
func (f *FSM) setMeta(key, value string) error     // only via applyCommand(kindSetMeta)

func (f *FSM) lookupManifestRoute(repo, tag string) (domain.CacheRoute, bool)
func (f *FSM) lookupBlobRoute(digest string) (string, bool)
func (f *FSM) lookupUpload(uuid string) (domain.CacheUploadSession, bool)
func (f *FSM) backendCharge(backendID string) int64
func (f *FSM) allCharges() map[string]int64
```

### 4.3 Snapshot (`internal/repository/fsm_snapshot.go`)

```go
type fsmSnapshotPayload struct {
    Users         []*domain.User
    Groups        []*domain.Group
    Memberships   []membershipEdge // {GroupID, UserID}
    Tokens        []*domain.APIToken
    Projects      []*domain.Project
    Traces        []*domain.TraceMeta
    Meta          map[string]string
    ObjectRoutes  []*domain.CacheRoute
    BlobRoutes    []blobRouteEdge  // {Digest, BackendID, CreatedAt}
    Uploads       []*domain.CacheUploadSession
}

type fsmSnapshot struct {
    payload fsmSnapshotPayload
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error // JSON-encode payload, sink.Close()
func (s *fsmSnapshot) Release()
```

### 4.4 `MetaStore` (`internal/repository/meta_store.go`)

```go
// MetaStore reads/writes arbitrary key/value pairs via Raft. Drop-in
// replacement for the SQLite-backed MetaStore used by cmd/api/main.go for the
// JWT secret and token-encryption key.
type MetaStore struct {
    store *RaftStore
}

func NewMetaStore(store *RaftStore) *MetaStore

func (m *MetaStore) Get(ctx context.Context, key string) (string, error) // reads FSM; ErrNotFound when missing
func (m *MetaStore) Set(ctx context.Context, key, value string) error   // apply(kindSetMeta)
```

## 5. Repository → Raft operation mapping

Reads = local FSM read (no `raft.Apply`). Writes = `store.apply(&command{...})`.
The `context.Context` is accepted by every method (signature unchanged) but
Raft's `Apply` timeout is governed by `RaftStore.timeout`, not the ctx deadline
(Raft does not take a context). If `ctx` is already cancelled before the call,
the repo returns `ctx.Err()` without applying (cheap pre-check).

### 5.1 `UserRepo`

| Method | Path |
|---|---|
| `Create(u)` | `apply(kindUpsertUser, u)` — FSM rejects duplicate `lower(username)` and duplicate OAuth key with `domain.ErrConflict` (wrapped). |
| `Get(id)` | `fsm.readUserByID(id)` → `ErrNotFound` when missing. |
| `GetByUsername(name)` | `fsm.readUserByUsername(name)`. |
| `GetByOAuth(provider, oauthID)` | `fsm.readUserByOAuth(...)`. |
| `List()` | `fsm.listUsers()` (sorted by `CreatedAt` ASC, matching SQL `ORDER BY created_at`). |
| `Update(u)` | `apply(kindUpsertUser, u)` — FSM rejects if the ID is missing (`ErrNotFound`) or the new username collides (`ErrConflict`). |
| `Delete(id)` | `apply(kindDeleteUser, id)` — cascades tokens + memberships in the FSM (mirrors SQL `ON DELETE CASCADE` / `ON DELETE SET NULL`). |
| `Count()` | `fsm.countUsers()`. |

### 5.2 `GroupRepo`

| Method | Path |
|---|---|
| `Create(g)` | `apply(kindUpsertGroup)` — rejects duplicate `lower(name)` → `ErrConflict`. |
| `Get/GetByName/List/Update/Delete` | analogous to users. Delete cascades memberships (mirrors `ON DELETE CASCADE` on `user_groups`) and nulls `projects.group_id` + `trace_meta.group_id` (mirrors `ON DELETE SET NULL`). |
| `SetMembers(groupID, userIDs)` | `apply(kindSetMembers, {groupID, userIDs})` — **atomic full-replace** in one log entry (replaces the SQL transaction). Unknown userIDs → `ErrNotFound` (validation already done by `GroupService.SetMembers`, but the FSM re-checks deterministically). |
| `Members(groupID)` | `fsm.members(groupID)` (sorted by username ASC). |
| `GroupsForUser(userID)` | `fsm.groupsForUser(userID)` (sorted by group ID ASC). |
| `AllMemberships()` | `fsm.allMemberships()` (groupID → sorted userIDs, stable ordering preserved for quota accounting). |

### 5.3 `ProjectRepo` — analogous (Create rejects duplicate `lower(name)`).

### 5.4 `TokenRepo`

| Method | Path |
|---|---|
| `Upsert(t)` | `apply(kindUpsertToken)` — FSM enforces one-token-per-user (replaces `ON CONFLICT(user_id) DO UPDATE`) and unique `token_hash` (`ErrConflict` on collision). |
| `GetByHash(hash)` | `fsm.readTokenByHash(hash)` → `ErrNotFound`. |
| `GetByUser(userID)` | `fsm.readTokenByUser(userID)`. |
| `Delete(userID)` | `apply(kindDeleteToken, {userID})` — no-op if absent (matches current behavior). |
| `TouchLastUsed(id, at)` | `apply(kindTouchToken, {id, at})` — no error if the token is gone (matches current best-effort touch). |

### 5.5 `TraceMetaRepo`

| Method | Path |
|---|---|
| `UpsertProvision(traceID, userID, version)` | `apply(kindUpsertTraceProvision)` — FSM implements the exact COALESCE: `user_id = first-non-empty-wins`, `version = first-non-empty-wins`, `updated_at = now`. |
| `UpsertIngest(m)` | `apply(kindUpsertTraceIngest, m)` — FSM implements the full COALESCE matrix from the SQL `ON CONFLICT DO UPDATE` (group_id set-once; project_name/status/version/ci_provider/ci_repo take newer non-empty; duration_ms takes newer non-zero; started_at COALESCE; updated_at = now). |
| `Get(traceID)` | `fsm.readTrace(traceID)`. |
| `List(f)` | `fsm.listTraces(f)` — in-memory: iterate `traces`, apply the `TraceFilter` (GroupIDs / UnassignedOnly / UserID / IncludeUnassigned), join `groups` + `users` for display names, sort by `COALESCE(started_at, updated_at) DESC`, clamp to `[1, MaxTraceLimit]`. |

### 5.6 `CacheRoutesRepo`

| Method | Path |
|---|---|
| `LookupManifest(repo, tag)` | `fsm.lookupManifestRoute` (returns `ok=false`, no error, when absent — matches current). |
| `UpsertManifest(...)` | `apply(kindUpsertManifestRoute, CacheRoute)` — FSM sets `created_at` if new, updates `last_seen_at` always (matches SQL). |
| `LookupBlob(digest)` | `fsm.lookupBlobRoute` (most recent by `created_at DESC`). |
| `UpsertBlob(digest, backendID)` | `apply(kindUpsertBlobRoute)` — idempotent (matches `INSERT OR IGNORE`). |
| `LookupUpload(uuid)` | `fsm.lookupUpload`. |
| `RecordUpload(uuid, repo, backendID)` | `apply(kindRecordUpload)` — `INSERT OR REPLACE` semantics. |
| `DeleteUpload(uuid)` | `apply(kindDeleteUpload)`. |
| `BackendCharge(backendID)` | `fsm.backendCharge` (sum of `stored_bytes`). |
| `AllCharges()` | `fsm.allCharges()`. |
| `DeleteManifestRoute(repo, tag)` | `apply(kindDeleteManifestRoute)`. |
| `DeleteRoutesForBackend(backendID)` | `apply(kindDeleteRoutesForBackend)` — deletes manifest + blob routes for the backend. |
| `ReapUploadSessions(maxAge)` | `apply(kindReapUploads, {cutoffRFC3339})` — FSM deletes sessions with `created_at < cutoff` and returns the count via `Apply.Response()` (the repo extracts the int). |

### 5.7 `MetaStore`

| Method | Path |
|---|---|
| `Get(key)` | `fsm.readMeta(key)` → `ErrNotFound`. |
| `Set(key, value)` | `apply(kindSetMeta, {key, value})` — upsert. |

### 5.8 Apply/commit/response flow

```go
func (s *RaftStore) apply(cmd *command) error {
    if s.raft.State() != raft.Leader {
        return domain.ErrNotLeader
    }
    data, err := json.Marshal(cmd)
    if err != nil {
        return fmt.Errorf("marshal raft command: %w", err)
    }
    f := s.raft.Apply(data, s.timeout)
    if err := f.Error(); err != nil {
        if errors.Is(err, raft.ErrNotLeader) {
            return domain.ErrNotLeader
        }
        if errors.Is(err, context.DeadlineExceeded) {
            return domain.ErrRaftTimeout
        }
        return fmt.Errorf("raft apply: %w", err)
    }
    // FSM.Apply returns an error (or nil). Propagate it so the service layer
    // sees the same sentinels it sees today (ErrNotFound, ErrConflict, ...).
    if resp, ok := f.Response().(error); ok && resp != nil {
        return resp
    }
    return nil
}
```

For `ReapUploadSessions` (which needs the count), `FSM.Apply` returns an `int`
instead of an `error`; the repo checks the type and extracts it. (Documented
exception; all other commands return `error`/`nil`.)

## 6. Edge cases & failure modes

- **Leadership change during a write**: `raft.Apply` returns
  `raft.ErrNotLeader` → `domain.ErrNotLeader` → handler 503. Clients retry.
  No partial state: a log entry is either committed and applied on a quorum or
  not; the FSM never sees a half-applied command.
- **Not-the-leader on a write**: short-circuited before `Apply`
  (`s.raft.State() != raft.Leader`) → `ErrNotLeader` → 503. The handler does
  not currently redirect to the leader (no leader-address header in v1); a
  future enhancement can add `LeaderAddr` to the 503 body. Documented.
- **Reads on a follower**: served from the local FSM (stale). Staleness is
  bounded by Raft replication latency. For this app (RBAC, trace metadata,
  cache routing) stale reads are acceptable and match the single-node
  behavior. A future `raft.ReadIndex`-based consistent-read option is
  documented in ADR-015 as a hardening knob.
- **Snapshot/restore**: `Snapshot()` deep-copies the maps under RLock and
  returns a `fsmSnapshot` that JSON-encodes the payload to the sink.
  `Restore()` decodes the payload and **replaces** the entire state under Lock.
  Restore is not concurrent with `Apply` (Raft guarantees this).
- **Log compaction**: `raft.DefaultConfig()` with `SnapshotThreshold=1000`,
  `SnapshotInterval=10m`, `TrailingLogs=256`. After a snapshot, the bolt log
  is truncated up to the snapshot index. This bounds `raft.db` growth.
- **Node removal**: out of scope for v1 (no `raft.RemoveServer` workflow).
  Operator removes a node by shrinking `raft.peers` and deleting the pod; the
  remaining cluster continues with a quorum. A future `supervisor raft
  remove-peer` command can call `raft.RemoveServer`. Documented.
- **Bootstrap timing**: `BootstrapCluster` returns `ErrCantBootstrap` when the
  data dir already has state — ignored. On a fresh dir, the node bootstraps
  with the configured peer list. Multi-node: all nodes bootstrap with the same
  list (the documented pattern); Raft elects the leader.
- **Single-node startup**: `Peers=[]` (or `Peers=[self]`) → bootstrap with
  self as the only voter. `WaitForLeader` returns almost immediately. The node
  is always leader; writes always succeed locally.
- **Node ID generation**: on first boot, if `<dir>/node-id` is absent,
  generate a UUIDv4 and persist it (`0600`). On subsequent boots, read it.
  `cfg.NodeID` overrides the file (operator-supplied). A node that loses its
  `node-id` file must be treated as a new node and re-added via the peer list
  (documented operational note).
- **Fresh empty state (the only supported state)**: reads return `ErrNotFound`
  / empty lists (identical to an empty SQLite DB). Writes require leadership
  (satisfied by `WaitForLeader` at startup). The bootstrap-admin flow
  (`cmd/api/main.go: bootstrapAdmin`) checks `users.Count() == 0` and creates
  the admin — works exactly as today. **There is no migration path; operators
  starting fresh is the documented behavior.**
- **Concurrent writers**: Raft serializes all writes in the log; the FSM
  applies them one-by-one. The read-then-write races that SQL UNIQUE
  constraints used to catch are now caught deterministically inside
  `applyCommand` (the FSM sees the post-apply state of all prior entries).
- **`context.Context` cancellation**: each repo write checks `ctx.Err()` before
  `apply`; if cancelled, returns `ctx.Err()` without touching Raft. Reads are
  in-memory and effectively instantaneous (no ctx check needed, but kept for
  signature compatibility).

## 7. Error handling & validation

| Source | Maps to | HTTP |
|---|---|---|
| `domain.ErrNotFound` (FSM read miss / delete-missing in FSM) | `writeServiceError` | 404 |
| `domain.ErrConflict` (FSM uniqueness violation, replaces SQLite `UNIQUE constraint failed`) | new case in `writeServiceError` | 409 |
| `domain.ErrTokenExists` (unchanged, raised by `TokenService.Generate`) | existing case | 409 |
| `domain.ErrNotLeader` (write on a follower / leadership lost) | new case in `writeServiceError` | 503 |
| `domain.ErrRaftTimeout` (`raft.Apply` deadline, not `ErrNotLeader`) | new case in `writeServiceError` | 504 |
| `domain.ErrValidation` (unchanged, service-layer) | existing | 400 |
| Other wrapped errors | existing default | 500 |

- The previous SQLite `isUniqueViolation` string-match fallback in
  `internal/handler/middleware.go` is **removed** — no SQLite errors remain.
  The FSM returns `domain.ErrConflict` directly.
- **Apply timeout**: `RaftStore.timeout` default 5s (`raft.apply_timeout`
  config). Tunable. On a healthy single-node cluster, applies return in <1ms.
- **Retry policy**: none in v1 (matches today). Clients retry 503/504. The
  supervisor does not retry internally. Documented.
- **Backpressure**: the FSM is in-memory; very large datasets would pressure
  RAM. ADR-010 bounds the dataset to ~1 GiB. Snapshots cap log growth. No
  explicit write-rate limiting in v1 (Raft's serial apply is the natural
  backpressure). Documented.

## 8. Configuration

### 8.1 New / changed config keys (viper `mapstructure`, env prefix `DAGGER_CACHE_`)

**Removed:** `database.path`.

**Added/changed:**

| Key | Type | Default | Env |
|---|---|---|---|
| `database.dir` | string | `/var/lib/dagger-cache` | `DAGGER_CACHE_DATABASE_DIR` |
| `raft.node_id` | string | `""` (auto-generate + persist at `<dir>/node-id`) | `DAGGER_CACHE_RAFT_NODE_ID` |
| `raft.bind_addr` | string | `:8081` | `DAGGER_CACHE_RAFT_BIND_ADDR` |
| `raft.peers` | `[]RaftPeer` | `[]` (single-node) | `DAGGER_CACHE_RAFT_PEERS_0_ID` / `..._0_ADDRESS` (viper slice env) |
| `raft.apply_timeout` | duration | `5s` | `DAGGER_CACHE_RAFT_APPLY_TIMEOUT` |
| `raft.leader_wait_timeout` | duration | `30s` | `DAGGER_CACHE_RAFT_LEADER_WAIT_TIMEOUT` |
| `raft.snapshot_threshold` | uint64 | `1000` | `DAGGER_CACHE_RAFT_SNAPSHOT_THRESHOLD` |
| `raft.snapshot_interval` | duration | `10m` | `DAGGER_CACHE_RAFT_SNAPSHOT_INTERVAL` |
| `raft.trailing_logs` | uint64 | `256` | `DAGGER_CACHE_RAFT_TRAILING_LOGS` |
| `raft.tls.enabled` | bool | `false` (v1) | `DAGGER_CACHE_RAFT_TLS_ENABLED` |

`RaftPeer` reuses `domain.RegistryBackend`-style struct: `{ID, Address string}`.

### 8.2 `domain/config.go` additions

```go
type DatabaseConfig struct {
    Dir string `mapstructure:"dir"` // was: Path string
}

type RaftConfig struct {
    NodeID            string        `mapstructure:"node_id"`
    BindAddr          string        `mapstructure:"bind_addr"`
    Peers             []RaftPeer    `mapstructure:"peers"`
    ApplyTimeout      time.Duration `mapstructure:"apply_timeout"`
    LeaderWaitTimeout time.Duration `mapstructure:"leader_wait_timeout"`
    SnapshotThreshold uint64        `mapstructure:"snapshot_threshold"`
    SnapshotInterval  time.Duration `mapstructure:"snapshot_interval"`
    TrailingLogs      uint64        `mapstructure:"trailing_logs"`
    TLS               RaftTLSConfig `mapstructure:"tls"`
}

type RaftPeer struct {
    ID      string `mapstructure:"id"`
    Address string `mapstructure:"address"`
}

type RaftTLSConfig struct {
    Enabled bool `mapstructure:"enabled"`
}

// In Config:
type Config struct {
    // ... existing ...
    Database DatabaseConfig `mapstructure:"database"`
    Raft     RaftConfig     `mapstructure:"raft"`
}
```

### 8.3 `config/loader.go` defaults (mandatory `v.SetDefault` for every field)

```go
v.SetDefault("database.dir", "/var/lib/dagger-cache")
v.SetDefault("raft.node_id", "")
v.SetDefault("raft.bind_addr", ":8081")
v.SetDefault("raft.peers", []domain.RaftPeer{})
v.SetDefault("raft.apply_timeout", 5*time.Second)
v.SetDefault("raft.leader_wait_timeout", 30*time.Second)
v.SetDefault("raft.snapshot_threshold", uint64(1000))
v.SetDefault("raft.snapshot_interval", 10*time.Minute)
v.SetDefault("raft.trailing_logs", uint64(256))
v.SetDefault("raft.tls.enabled", false)
```

### 8.4 Sample YAML (`config/config.app.yaml.sample`, 2-space indent)

```yaml
# --- Database ----------------------------------------------------------------
# Raft data directory: holds raft.db (boltdb log+stable), snapshots/, node-id.
# Persist on a volume (PVC / bind-mount) in production.
# NOTE: this is a fresh-start store. There is NO migration from legacy SQLite
# data; existing SQLite data is intentionally not carried over.
database:
  dir: "/var/lib/dagger-cache"

# --- Raft (distributed store) ------------------------------------------------
# Raft ALWAYS runs, even for a single-node deployment. For single-node, leave
# peers empty (the node bootstraps with itself as the only voter). For a
# multi-node cluster, list every voter (including self) on every node.
raft:
  node_id: ""              # "" = load/generate a stable UUID at <dir>/node-id.
  bind_addr: ":8081"       # dedicated Raft transport port (not control/data).
  peers: []                # single-node default. Multi-node example:
    # - id: "node-1"
    #   address: "supervisor-0.supervisor-headless:8081"
    # - id: "node-2"
    #   address: "supervisor-1.supervisor-headless:8081"
  apply_timeout: "5s"
  leader_wait_timeout: "30s"
  snapshot_threshold: 1000
  snapshot_interval: "10m"
  trailing_logs: 256
  tls:
    enabled: false         # v1: plaintext transport. Enable in a future release.
```

## 9. Testing plan (stdlib `testing` only; 100% coverage target)

### 9.1 FSM unit tests (`internal/repository/fsm_test.go`)

Table-driven, exercising `FSM.applyCommand` directly (no Raft) for speed and
determinism:
- Every `commandKind`: happy path + error path (`ErrNotFound`, `ErrConflict`).
- Case-insensitive uniqueness for users/groups/projects.
- `kindDeleteUser` cascades tokens + memberships + nulls projects/trace group_id.
- `kindSetMembers` full-replace semantics + unknown-userID rejection.
- `kindUpsertToken` one-per-user + hash uniqueness.
- `kindUpsertTraceProvision` / `kindUpsertTraceIngest` COALESCE matrices
  (first-non-empty-wins for user_id, version, group_id; newer-non-empty for
  status/project_name/etc; duration_ms newer-non-zero; started_at COALESCE).
- `kindReapUploads` cutoff boundary + count return.
- `Snapshot()` → `Persist` → `Restore` round-trip preserves all maps.
- `Restore` replaces state (not merges).

### 9.2 `RaftStore` tests (`internal/repository/raft_store_test.go`)

- `newTestRaftStore(t)`: single-node Raft with `raft.NewInmemStore`,
  `raft.NewInmemSnapshotStore`, `raft.NewInmemTransport`. Fast, no disk.
- `WaitForLeader` returns; `IsLeader()` true.
- `apply` on leader succeeds; `apply` after `raft.LeadershipTransfer` returns
  `ErrNotLeader` (transfer leadership away, then back).
- `apply` timeout path: configure `ApplyTimeout` very small + block the FSM
  (or use a slow command) → `ErrRaftTimeout`.
- `Close` is idempotent.
- Node-id file: generated on first boot, reused on second.

### 9.3 Repository tests (`internal/repository/*_repo_test.go`)

Switch every `newTestDB(t)` → `newTestRaftStore(t)` and `NewXxxRepo(db)` →
`NewXxxRepo(store)`. **Preserve every existing test case verbatim** — these
encode the contract (case-insensitive uniqueness, FK cascades, `SetMembers`
replace, `UpsertIngest` set-once, `List` filter matrix, `ReapUploadSessions`,
`MetaStore` upsert). Add cases for the new `ErrConflict` sentinel on duplicate
inserts (replacing the SQLite `UNIQUE constraint failed` string-match tests in
`handler/middleware_test.go` — the string-match fallback is removed, so those
specific tests are deleted or rewritten to assert on `ErrConflict` directly).

### 9.4 Handler / service / integration tests

- `internal/handler/test_helper_test.go`, `internal/service/test_helper_test.go`,
  `tests/integration/*_test.go`: switch to `newTestRaftStore`-equivalent. All
  existing assertions (RBAC 403/401, CRUD 409/400/404, quota 429, visibility
  fail-closed, OAuth, cache proxy routing) must pass unchanged.
- `cmd/api/main_test.go`: `newMetaStore` uses a test Raft store; existing
  JWT-secret / token-encryption-key tests pass unchanged (they only use
  `MetaStore.Get`/`Set`).
- New handler tests: a write that returns `domain.ErrNotLeader` → 503, and a
  write that returns `domain.ErrRaftTimeout` → 504 (inject via stub repos
  returning the sentinels).

### 9.5 Coverage

Target 100% per package. The FSM's `applyCommand` switch must have a case per
`commandKind` plus a default (returns an error for unknown kinds). `Restore`
error paths (corrupt JSON) must be covered. `RaftStore.apply` error paths
(marshal failure, `ErrNotLeader`, timeout, FSM response error) must each have
a test.

## 10. Documentation updates (mandatory per AGENTS.md)

- **`docs/design/ADR-015-raft-replaces-sqlite.md`** (new): Context (SQLite is
  single-node, blocks horizontal scaling), Decision (always-on Raft, FSM,
  bolt log, file snapshots, TCP transport, leader-local writes + stale reads,
  JSON serialization, **fresh-start with no migration**), Resolved decisions
  table (D1 always-on Raft, D2 bolt, D3 file snapshots, D4 JSON, D5 stale reads,
  D6 SQLite fully removed / no migration, D7 sessions stay in-memory),
  Consequences, Risks, Testing. Style matches ADR-009/010.
- **`docs/design/index.md`**: add `| 015 | [Raft replaces SQLite](ADR-015-raft-replaces-sqlite.md) |`.
- **`docs/README.md`**: update the "Configuration → Database" section to
  describe Raft, the single-node default, multi-node peers, the **fresh-start
  behavior** (no migration; existing SQLite data is intentionally not carried
  over), and the new `database.dir` / `raft.*` keys. **No migration runbook.**
  State explicitly that SQLite is fully removed from the project.
- **`config/config.app.yaml.sample`**: full new `database` + `raft` blocks
  (§8.4), in sync with `loader.go`. Include the fresh-start note.
- **`config/config.app.yaml`**: same.
- **`deploy/helm/dagger-kubernetes/README.md`**: document the raft port +
  `supervisor.config.raft.*` values + the single-node default + the
  multi-node StatefulSet follow-up. Note that the store starts fresh (no
  migration from any prior SQLite-backed release).

## 11. Implementation sequence (ordered, with dependencies)

1. **Config + domain sentinels.** Edit `internal/domain/config.go`
   (`DatabaseConfig.Dir`, `RaftConfig`, `RaftPeer`, `RaftTLSConfig`),
   `internal/domain/identity.go` (`ErrNotLeader`, `ErrConflict`,
   `ErrRaftTimeout`), `config/loader.go` (defaults), `config/config.app.yaml`
   + `.sample`. *No runtime impact yet.*
2. **FSM + snapshot.** Write `internal/repository/fsm.go` +
   `fsm_snapshot.go` + `fsm_test.go`. Pure logic, no Raft. Must pass 100%.
3. **RaftStore.** Write `internal/repository/raft_store.go` +
   `raft_store_test.go` + `raft_test_helpers_test.go` (`newTestRaftStore`).
   Depends on (2).
4. **MetaStore.** Write `internal/repository/meta_store.go`. Depends on (3).
5. **Re-back repositories.** Rewrite `user_repo.go`, `group_repo.go`,
   `project_repo.go`, `token_repo.go`, `tracemeta_repo.go`,
   `cache_routes_repo.go` to take `*RaftStore`. Update their `_test.go` files
   to use `newTestRaftStore`. Depends on (2)+(3). Run `go test ./internal/repository/...`.
6. **Delete old SQLite plumbing.** Remove `sqlite.go`, `schema.sql`,
   `sqlite_test.go`, `sqlite_migrate_test.go`. Depends on (5) being green.
7. **Handler error mapper.** Add `ErrNotLeader`→503, `ErrConflict`→409,
   `ErrRaftTimeout`→504 to `internal/handler/middleware.go`; remove the
   `isUniqueViolation` SQLite-string-match fallback. Update
   `middleware_test.go` (delete/rewrite the string-match test). Depends on (1).
8. **Wire `cmd/api/main.go`.** Replace SQLite open/migrate with
   `NewRaftStore` + `WaitForLeader`; update all repo constructors; add
   leader-observation goroutine; shutdown `store.Close()`. **No
   `migrate-sqlite` subcommand. No SQLite open. No `//go:embed schema.sql`.**
   Update `cmd/api/main_test.go`. Depends on (3)+(4)+(5).
9. **Service + handler + integration tests.** Update
   `internal/service/test_helper_test.go`,
   `internal/handler/test_helper_test.go`, `tests/integration/*_test.go`.
   Depends on (5). Run full `go test ./...`.
10. **Helm chart.** Update `values.yaml`, `configmap.yaml`, `deployment.yaml`
    (raft port + env), bump PVC default. Depends on (1).
11. **Docs.** Write ADR-015, update `index.md`, `docs/README.md`, helm
    `README.md`. Depends on all above.
12. **`go.mod` / `go.sum`.** `go mod tidy` after adding
    `github.com/hashicorp/raft` + `github.com/hashicorp/raft-boltdb/v2` and
    **removing `modernc.org/sqlite`** (and its now-unused transitive deps).
13. **Build + redeploy + verify** per `AGENTS.local.md` §4–§5 (build image →
    push → helm upgrade → rollout → agent checks → human UI verification).

## 12. Risks, trade-offs, open questions

### Risks

- **R1 — Cache-routing write latency on multi-node.** Every OCI push upserts a
  route via `raft.Apply` (a quorum round-trip). On single-node (current
  production) this is <1ms. On multi-node it adds replication latency to each
  push. *Mitigation:* acceptable for this app's push frequency; documented in
  ADR-015. Future: a local write-back cache in front of Raft (out of scope).
- **R2 — Stale reads on followers.** A follower may serve a read slightly
  behind the leader. *Mitigation:* bounded by replication latency; acceptable
  for RBAC/trace metadata. A `raft.ReadIndex` consistent-read option is
  documented as a future knob.
- **R3 — In-memory FSM memory bound.** The FSM holds all state in RAM.
  *Mitigation:* ADR-010 bounds the dataset to ~1 GiB; snapshots cap log growth.
  Documented.
- **R4 — Multi-node requires a StatefulSet + headless Service in Helm.** The
  supervisor code supports multi-node today (config-driven peers), but the
  chart's `Deployment` does not give stable pod addresses. *Mitigation:* v1
  ships single-node (the current production reality). A multi-node StatefulSet
  chart is a documented follow-up; the supervisor code is ready.
- **R5 — Loss of `node-id` file.** A node that loses `<dir>/node-id` becomes a
  new node and must be re-added via the peer list. *Mitigation:* the file is
  `0600` on a PVC; documented operational note.
- **R6 — No migration path for existing SQLite data.** This is a greenfield
  storage swap. Operators running a prior SQLite-backed release who need to
  preserve RBAC state must export/re-enter that state manually (e.g. re-create
  users, groups, tokens via the API/UI). *Mitigation:* documented as the
  intended behavior in ADR-015, `docs/README.md`, and the helm `README.md`.
  The bootstrap-admin flow provisions a fresh admin on first boot, so a new
  deployment is immediately usable.

### Trade-offs

- **JSON vs gob for FSM payloads.** Chose JSON for debuggability + stdlib +
  existing tags. Trade-off: larger payloads than gob. Acceptable for the
  dataset size.
- **bolt log vs inmem log.** Chose bolt (durable) for production; inmem for
  tests. Trade-off: bolt adds a file I/O path; justified by durability.
- **Stale reads vs `ReadIndex`.** Chose stale reads for simplicity + read
  performance (auth resolution reads users/groups on every request). Trade-off:
  followers can return slightly stale data. Acceptable; documented.
- **No migration vs explicit migration subcommand.** Chose to ship no
  migration path at all (per directive). Trade-off: existing deployments must
  re-provision state. Acceptable; documented as the intended behavior.

### Open questions (resolved with recommended answers)

- **OQ1 — Should reads on a follower be allowed, or must all reads go to the
  leader?** Recommended: **Allow stale reads on any node.** Matches
  single-node behavior; keeps the read path fast and simple; the staleness
  bound is tiny. A future `ReadIndex` knob can offer consistent reads.
- **OQ2 — Should `CacheRoutesRepo` get a `domain` interface (it currently is a
  concrete type used by `service.RegistryRouter`)?** Recommended: **No.** Keep
  the existing concrete-type coupling (ADR-009 D8 pattern). Adding an
  interface is out of scope for a storage swap and would expand the change
  surface for no behavioral gain.
- **OQ3 — Multi-node Helm: ship a StatefulSet in this change?** Recommended:
  **No.** Ship single-node (current production). The supervisor code supports
  multi-node; the StatefulSet chart is a documented follow-up. This keeps the
  change focused and the rollout low-risk.
- **OQ4 — TLS for the Raft transport in v1?** Recommended: **Off by
  default.** Plaintext transport on a dedicated port, intended to be
  cluster-internal. TLS is a documented hardening follow-up (the `raft.tls`
  config key is reserved).

### Go version / dependency notes

- `go 1.26.5` (from `go.mod`) is well above the minimums for
  `github.com/hashicorp/raft` (Go 1.21+) and
  `github.com/hashicorp/raft-boltdb/v2` (Go 1.21+).
- New direct deps: `github.com/hashicorp/raft`,
  `github.com/hashicorp/raft-boltdb/v2`. Transitive:
  `go.etcd.io/bbolt`, `github.com/hashicorp/go-msgpack/codec`,
  `github.com/hashicorp/go-hclog`, `github.com/hashicorp/go-immutable-radix`,
  `github.com/hashicorp/golang-lru/v2`. None require CGO (preserves the
  static-binary story).
- **`modernc.org/sqlite` is fully REMOVED** from `go.mod`/`go.sum` via
  `go mod tidy`. No file in the project imports it after the swap. This
  preserves the no-CGO static-binary story (bbolt is also pure-Go) while
  eliminating the SQLite codepath entirely.
