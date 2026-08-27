# ADR-026: Replicated session leases + leader-routed Services

- **Status:** accepted
- **Date:** 2026-08-27
- **Deciders:** dagger-kubernetes maintainers

## Context

The data plane routes Dagger CLI connections to engine pods by the client
certificate fingerprint: `POST /v1/engines` mints a per-session client cert and
registers a lease (`certFP -> version, engine pod, trace, user`), and
`serveDataTunnel` looks the fingerprint up and proxies to the engine pod.

Before this ADR the lease registry was an **in-memory map local to each
supervisor pod** (`service.Store`). On a 3-replica deployment this broke in two
ways:

1. **Provision/tunnel split.** The provision request lands on a random pod
   (ingress → `-control` Service round-robin); the data-plane tunnel lands on
   another random pod (`-data` Service round-robin). The serving pod had no
   lease for the fingerprint → `lease not found` → the session-attachables
   connection is dropped (`read response: unexpected EOF`) and the pipeline
   fails. With 3 pods roughly 2 of 3 runs failed.
2. **No follower write path.** Raft writes (`applyCtx`) return `ErrNotLeader`
   on follower pods. The tunnel heartbeat refreshes the lease's `LastActivity`
   so the reaper (2m `lease_ttl`) and the fleet sweeper do not kill the engine
   mid-pipeline. A follower pod serving a tunnel could never refresh a lease
   shared with the leader.

## Decision

### 1. Session leases are Raft-replicated

Two new FSM command kinds, `kindUpsertSession` and `kindTouchSession`, carry
the lease (plus a leader-clock timestamp) into the Raft log. The FSM does not
store the state itself: it forwards every applied session command to a
**`domain.SessionStateSink`** — the pod-local `service.Store` — so every pod's
existing in-memory registry (used by the data plane, quota, the fleet sweeper,
and the reaper) mirrors the replicated state deterministically. The sink is
wired into `NewRaftStore` **before** the node starts so log replay restores
leases on every pod at boot.

- `repository.SessionRepo` implements the new `domain.SessionRegistry`
  (`Register`, `Touch`) on top of `applyCtx`.
- `handleEngines` registers through the registry (local fallback on
  `ErrNotLeader` so a provision is never lost during a leader failover window).
- The tunnel heartbeat touches through the registry (local fallback likewise).
- Reads (`Get`, `PinnedSessionsOnReplica`, `CountByUser`, `List`) stay local;
  `IncInFlight`/`DecInFlight` stay local to the serving pod (only it observes
  the tunnel lifecycle).

### 2. Control and data Services are leader-routed

Follower pods cannot apply Raft writes, so anything that must write (session
registration, tunnel heartbeat touches) must run on the leader. The Helm
chart's `-control` and `-data` Services therefore select on the pod label
`dagger-kubernetes.io/raft-leader: "true"`, maintained at runtime by
`observeLeadership` (each pod patches its own label on leadership changes).

Consequences:

- All ingress traffic — API reads AND writes, data-plane tunnels — terminates
  on the leader. Follower pods are hot standbys (they keep serving probes and
  stay Raft-synced). This also removes the previous flaky behaviour where
  write endpoints returned 503 "not the leader" for ~2/3 of requests.
- On leadership change the endpoints converge within seconds (label patch +
  endpoint propagation); the Dagger CLI's gRPC reconnect absorbs the window.
- The Raft headless Service is untouched (it must reach every voter).
- Until a leader is elected on a fresh cluster the Services have no endpoints
  (a short control-plane outage — acceptable; the platform requires a leader
  anyway).

## Alternatives considered

- **Cert-CN-derived routing** (parse the engine pod name from the client cert
  and route without the lease). Rejected: the lease registry also drives quota,
  reaping, and the fleet sweeper; bypassing it splits the state and the touch
  path remains unsolved.
- **Pod-to-pod forwarding / internal HTTP APIs for touches.** Rejected: adds a
  new authenticated inter-pod surface for a problem the leader-routed Services
  solve with standard Kubernetes primitives.
- **Single-replica deployment.** Rejected: loses Raft HA; this project's
  deployment model is 3 voters.

## Consequences

- Multi-pod deployments can serve pipelines reliably; leases survive pod
  restarts and leader changes (replay/apply restores them).
- Two Raft commits per active session per 30s heartbeat — negligible log
  churn.
- The data plane and control plane have a brief dependency on leader-label
  convergence after elections.
