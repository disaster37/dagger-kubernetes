# ADR-015: Raft replaces SQLite

- **Status:** Accepted
- **Date:** 2026-08-17
- **Supersedes:** ADR-010 (persistence engine; the multi-user RBAC model is unchanged)
- **Related:** ADR-009 (clean architecture layering), ADR-010 (SQLite-backed multi-user RBAC), ADR-014 (registry proxy routing table), ADR-016 (multi-node TLS + StatefulSet)

## Context

ADR-010 introduced a multi-user RBAC store backed by a single SQLite file
(`modernc.org/sqlite`, pure-Go). SQLite served the supervisor well for a
single-node deployment, but it pins all state to one replica and blocks
horizontal scaling of the supervisor control plane: there is no safe way for
two supervisor replicas to share one SQLite file, and sharding the RBAC state
across replicas would break quota accounting and trace visibility.

The data set is small and bounded (users/groups/projects/tokens + trace
metadata + the cache routing tables, ~1 GiB per ADR-010), which makes a
replicated in-memory state machine viable. The supervisor must keep its
single-binary, no-CGO story.

## Decision

Replace SQLite entirely with a **Hashicorp Raft** replicated state machine.
**Raft always runs**, even for a single-node deployment — there is no
non-Raft code path, so a one-node cluster and an N-node cluster are served by
the same binary, config shape, and code.

### Components

- **FSM** (`internal/repository/fsm.go`): an in-memory Go struct of maps
  protected by a `sync.RWMutex`, holding every row SQLite held. Reads are
  served directly from the FSM (O(1) map lookups). Writes are deterministic
  mutations applied from the Raft log.
- **LogStore + StableStore**: `github.com/hashicorp/raft-boltdb/v2`
  (bbolt), a single `raft.db` file. Pure-Go, no CGO.
- **SnapshotStore**: `raft.NewFileSnapshotStore` under `<data_dir>/snapshots/`
  (durable, survives restart, supports log compaction).
- **Transport**: `raft.NewTCPTransport` on a dedicated `raft.bind_addr`
  (`:8081`), separate from the control (`:8080`) and data (`:8443`) listeners.
  Multi-node transport TLS (mTLS via an internal goca-issued CA) and the
  StatefulSet DNS-arithmetic peer discovery that make multi-node deployable
  are implemented in ADR-016 (which supersedes R4 below).
- **Leadership**: writes go through `raft.Apply` on the leader. A follower
  returns `domain.ErrNotLeader` (503). Reads are served from the **local** FSM
  on any node (stale reads, bounded by replication latency).
- **Bootstrap**: on first boot a node calls `raft.BootstrapCluster` with the
  full voter configuration (`raft.peers`; empty = self only). `ErrCantBootstrap`
  (existing state) is ignored. A stable `<data_dir>/node-id` UUID is generated
  on first boot and reused.
- **Startup barrier**: the supervisor waits on `raft.LeaderCh()` for
  `raft.leader_wait_timeout` (30s) so the first `MetaStore.Set` (JWT secret /
  token-encryption key) can succeed.

### Resolved decisions

| # | Decision |
|---|---|
| D1 | Raft always runs (single-node = one-voter cluster). No non-Raft path. |
| D2 | bolt (raft-boltdb/v2) for log + stable storage. |
| D3 | File snapshots for compaction and restart recovery. |
| D4 | JSON serialization for commands and snapshots (debuggable, stdlib, existing tags). |
| D5 | Stale local reads on any node; writes leader-only via `raft.Apply`. |
| D6 | SQLite is fully removed: no migration path, no `migrate-sqlite`, no embedded schema. The store starts fresh (empty FSM); the bootstrap-admin flow provisions the first admin. |
| D7 | The session/lease store (`service.Store`) stays in-memory and local (ADR-010). |

## Consequences

- **Positive**: the supervisor can scale horizontally behind Raft; the
  persistence layer is a single replicated, debuggable state machine with a
  compact log (snapshots cap `raft.db` growth); no CGO.
- **Negative**: writes now pay a quorum round-trip (single-node: <1ms);
  followers can serve slightly stale reads; the FSM holds all state in RAM.
- **Risk**: losing a node's `node-id` file makes it a new node that must be
  re-added (the file is `0600` on the PVC). Multi-node requires a StatefulSet
  + headless Service with mTLS — implemented in ADR-016 (this ADR originally
  deferred it).
- **Migration**: there is intentionally **no migration path** for existing
  SQLite data. Operators running a prior SQLite-backed release re-provision
  RBAC state via the API/UI; the bootstrap-admin flow makes a fresh deployment
  immediately usable.

## Risks

- **R1 — cache-routing write latency on multi-node**: every OCI push upserts a
  route via `raft.Apply`. Acceptable at this app's push frequency; a local
  write-back cache is a future optimization.
- **R2 — stale reads on followers**: bounded by replication latency; acceptable
  for RBAC/trace metadata. A `raft.ReadIndex` consistent-read option is a
  future knob.
- **R3 — in-memory FSM memory bound**: bounded by ADR-010's ~1 GiB dataset;
  snapshots cap log growth.
- **R4 — multi-node Helm**: superseded by ADR-016 (StatefulSet + headless
  Service + mTLS + DNS discovery are implemented).
- **R5 — node-id loss**: documented operational note; file is `0600` on a PVC.
- **R6 — no migration path**: intended (D6); documented in `docs/README.md`,
  `config/config.app.yaml.sample`, and the Helm README.

## Testing

- FSM: `applyCommand` exercised directly (no Raft) for every command kind —
  case-insensitive uniqueness, FK cascades, `SetMembers` replace, one-token-
  per-user, trace `COALESCE` matrices, reap cutoff, and `Snapshot`/`Restore`
  round-trip (including password/token hashes surviving the JSON round-trip).
- `RaftStore`: single-node inmem apply, not-leader (two-node inmem cluster),
  apply-error mapping (`ErrNotLeader`/`ErrRaftTimeout`), close idempotency,
  node-id generation/reuse, and a real `NewRaftStore` (TCP + bolt + file
  snapshot) single-node smoke test.
- Repository/service/handler/integration suites run unchanged against an
  in-memory Raft store, proving the storage-engine swap preserves behavior.
