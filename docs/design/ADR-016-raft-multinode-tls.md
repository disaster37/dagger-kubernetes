# ADR-016: Multi-node Raft with TLS (goca-issued internal PKI) + StatefulSet discovery

- **Status:** Accepted
- **Date:** 2026-08-17
- **Builds on:** ADR-015 (Raft replaces SQLite)
- **Related:** ADR-005 (embedded minting CA, goca), ADR-009 (clean architecture layering), ADR-015 (Raft replaces SQLite)

## Context

ADR-015 replaced SQLite with a single-node Hashicorp Raft store. Its review
called out four gaps blocking horizontal scaling of the supervisor control
plane:

1. The Raft transport was plaintext TCP; `raft.tls.enabled` was inert.
2. `raft.bind_addr: ":8081"` advertised `127.0.0.1`, which is unreachable
   from sibling pods — breaking multi-node.
3. The Helm chart shipped a `Deployment`, not a StatefulSet + headless
   Service, so pods had no stable identity or DNS name for peers to dial.
4. `cmd/api/main.go` treated `WaitForLeader` as a hard startup barrier: a
   non-leader exited instead of serving, contradicting ADR-015 D5 (stale
   reads on followers).

This ADR closes all four: the Raft transport is mTLS-secured with an internal
goca CA, peers are discovered from StatefulSet pod DNS names, each node
advertises its pod FQDN, and followers serve stale reads while returning
`ErrNotLeader` (503) on writes.

## Decision

### Components

- **Transport TLS (mTLS)**: the Raft `StreamLayer` is wrapped in
  `crypto/tls` (TLS 1.2+, `RequireAndVerifyClientCert`). Peers present a
  per-node leaf certificate (server+client auth) and verify each other
  against a shared internal CA. Plaintext remains available for single-node
  dev/test (`raft.tls.enabled=false`); the Helm chart enables TLS by default.
- **Internal PKI (goca)**: a self-signed internal CA is generated with
  `github.com/disaster37/goca` (the exact API already used in
  `ca_providers.go`) and shared across pods via a Kubernetes Secret. Each pod
  issues itself a leaf via the existing `MintingCA.IssuePeerCertificate`
  (stdlib `crypto/x509`), with SANs covering its stable pod DNS names +
  `127.0.0.1`. CA + leaf persist under `<database.dir>/tls/` and are reused
  across restarts; leaves are re-issued when within a 7-day expiry margin.
- **Minting CA sharing (CWE-295)**: the StatefulSet conversion gives each pod
  its own PVC, so the embedded engine-client minting CA (ADR-005) can no
  longer be persisted per-pod under `tls.ca_path` — each pod would mint a
  distinct CA and reject other pods' engine client certs. The
  `EmbeddedProvider` therefore also shares the minting CA via the
  `<release>-minting-ca` Secret (`ca.minting_ca_secret`), mirroring the raft
  CA Secret pattern (bootstrap on ordinal 0, poll otherwise). The local
  `tls.ca_path` files remain as a cache. Both Secrets hold CA **private keys**
  and must be RBAC-restricted to the supervisor ServiceAccount.
- **StatefulSet discovery**: peers are derived from the StatefulSet's stable
  pod DNS names (`<sts>-<i>.<headless>.<ns>.svc.cluster.local:8081` for
  `i=0..replicas-1`) — pure DNS arithmetic, no K8s API calls and no new RBAC.
  An explicit `raft.peers` override is kept for non-K8s/testing.
- **Advertise address**: each node advertises its pod FQDN (derived from
  `os.Hostname()` + headless service + namespace + port), not `127.0.0.1`.
- **Follower startup**: the hard `WaitForLeader` barrier is replaced with
  `WaitForLeader` (wait until *a* leader exists, not necessarily self). The
  leader provisions the JWT secret + token-encryption key (idempotent);
  followers poll the local FSM for those meta keys (replicated), then serve
  stale reads and return `ErrNotLeader` on writes. `WaitForSelfLeadership`
  is used by `migrate-tokens` (which must write).
- **Membership reconciliation**: a leader-only `joinLoop` periodically
  compares the resolver's voter list to the running raft configuration and
  calls `raft.AddVoter` for missing voters and `raft.RemoveServer` for
  removed voters (scale-up/scale-down, idempotent).
- **Helm chart**: `Deployment` → `StatefulSet` with `volumeClaimTemplates`
  (per-pod PVC), a headless Service, `podManagementPolicy: Parallel`, and TLS
  env wiring. The existing control/data Services stay (ClusterIP, select all
  Ready pods).
- **Single source of truth for the cluster size (2026-08-21 revision):** the
  chart's `supervisor.replicaCount` is the only size knob — the Raft voter
  count is derived from it (one supervisor pod = one voter), injected as
  `DAGGER_KUBERNETES_RAFT_REPLICAS`. The separate
  `supervisor.config.raft.replicas` value was removed because a drift between
  the two knobs produced a cluster that could never form quorum (e.g. a
  1-pod StatefulSet configured for 3 voters). Outside the chart (bare
  binary/Docker) `raft.replicas` remains a valid config key.
- **HPA removed:** the chart's `supervisor.autoscaling` HPA was removed in the
  same revision. A quorum-based Raft store cannot follow HPA-driven scaling:
  the voter count must equal the pod count exactly, membership changes are
  explicit (`raft.AddVoter`/`raft.RemoveServer` via the joinLoop), quorum
  needs an odd voter count, and an HPA scale-out of a 1-voter cluster would
  start N independent single-voter clusters (split brain). Dynamic voter
  counts are therefore not possible without a membership-operator that
  performs quorum-safe joins/leaves — out of scope. Scaling is a manual
  `supervisor.replicaCount` change + rolling restart.

### TLS PKI modes

| Mode | Trigger | CA distribution |
|---|---|---|
| Auto (K8s) | `raft.tls.enabled=true`, no manual paths, K8s clientset + `raft.tls.ca_secret` set | Pod-0 (`os.Hostname()` ends in `-0`, or `raft.tls.ca_bootstrap=true`) generates the CA via goca and writes a K8s Secret; other pods poll it (bounded by `leader_wait_timeout`) before issuing their leaf. |
| Manual | `raft.tls.ca_cert` + `raft.tls.cert` + `raft.tls.key` all set | Operator pre-provisions CA + leaf (e.g. cert-manager). No auto-generation, no Secret. |
| Local-only | `raft.tls.enabled=true`, no manual paths, no K8s clientset | The node generates a CA + its own leaf locally. Multi-node is rejected in this mode. |

### Resolved decisions

| # | Decision |
|---|---|
| D1 | Raft transport mTLS (TLS 1.2+, `RequireAndVerifyClientCert`), plaintext opt-out for single-node dev/test. |
| D2 | Internal raft CA generated with goca (`goca.New` + `GetCertificate`/`GetPrivateKey`). |
| D3 | CA shared via a Kubernetes Secret; pod-0 (auto-detected by ordinal) writes it, others poll. |
| D4 | Peers discovered by StatefulSet DNS arithmetic — no K8s API, no new RBAC. `raft.peers` is an explicit override. |
| D5 | Advertise address = pod FQDN (derived from hostname + headless service + namespace + port). |
| D6 | Followers serve stale local reads and return `ErrNotLeader` (503) on writes; `WaitForLeader` means "any leader exists". |
| D7 | Leader-only `joinLoop` reconciles membership via `raft.AddVoter`/`raft.RemoveServer`. |
| D8 | Single-node remains a degenerate one-voter cluster (identical behavior plus TLS). |
| D9 | The engine-client minting CA (ADR-005) is shared across pods via the `<release>-minting-ca` Secret when the embedded TLS provider is used in multi-node (CWE-295). |
| D10 | `supervisor.replicaCount` is the single source of truth for the Raft voter count; the separate `raft.replicas` chart value and the supervisor HPA were removed (quorum-safe dynamic scaling is not possible without a membership operator). |

## Consequences

- **Positive**: the supervisor control plane scales horizontally behind a
  quorum-based Raft cluster with encrypted replication; pod identities and
  PVCs are stable across restarts; scale-up/down is automatic via the
  joinLoop; followers add read capacity.
- **Negative**: writes behind a load-balanced Service may hit a follower and
  return 503 (client retries); scale-up requires bumping `replicaCount` and a
  rolling restart (the voter count follows `replicaCount`); CA rotation is out
  of scope (10-year goca CA); `migrate-tokens` still assumes single-node (it
  must write); no supervisor HPA (see D10).
- **Operational notes**: shrinking the cluster requires deleting the removed
  pod after the joinLoop calls `RemoveServer`; a pod that loses its PVC
  (`<dir>/node-id`) becomes a new node that must re-join.

## Risks

- **R1 — CA Secret write race / pod-0 failure**: pods 1..N-1 poll the secret
  (bounded) and exit if it never appears; K8s restarts pod-0 and the retry
  succeeds. Mitigated by `podManagementPolicy: Parallel` + bounded poll.
- **R2 — AddVoter/RemoveServer joinLoop complexity**: documented runbook in
  the Helm README; the loop is leader-only and idempotent.
- **R3 — stale reads + 503 on writes behind a load-balanced Service**:
  acceptable at RBAC write frequency; a leader-only Service is a future
  enhancement (OQ1).
- **R4 — 2-node cluster has no failure tolerance**: chart defaults
  `replicaCount: 3`.
- **R5 — CA rotation out of scope**: the CA has a 10-year life (goca default).
- **R6 — `migrate-tokens` with multi-node**: pre-existing limitation; run it
  against a running leader via the API or scale to 1 first.
- **R7 — TLS hostname verification on dial**: `podSANs` covers all FQDN forms
  + `127.0.0.1`; tested by `newThreeNodeTLSRaftCluster`.

## Testing

- `raft_discovery_test.go`: both resolvers, own-peer matching, advertise-addr
  derivation, SAN list computation, validation errors.
- `raft_tls_test.go`: goca CA creation, manual/local/Secret CA modes, leaf
  issue/reuse/expiry, `tls.Config` build, stream-layer mTLS accept/dial and
  untrusted-peer rejection, file permissions.
- `ca_test.go`: `TestMintingCAIssuePeerCertificate` (DNS+IP SANs, server+client
  auth, round-trip).
- `raft_store_test.go`: `TestNewRaftStoreTLS`, `WaitForLeader` (any leader) vs
  `WaitForSelfLeadership`, membership reconcile AddVoter/RemoveServer.
- `raft_test_helpers_test.go`: 3-node inmem and 3-node real-goca-mTLS
  clusters — replication, not-leader-on-follower, leader failover, AddVoter
  join.
- `cmd/api/main_test.go`: follower meta-wait path, `validateRaftConfig`,
  `raftCABootstrap` ordinal-0 detection.
