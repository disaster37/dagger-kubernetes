# ADR-004: Per-Version StatefulSet Autoscaler

**Status:** Accepted · **Date:** 2025-06-01 · **Author:** dagger-cache team

## Context

Each Dagger engine version requires isolated execution environments. A
shared fleet pool would create cross-version cache poisoning and version
conflicts. The autoscaler must manage engine replicas per version.

## Decision

Use a **per-version StatefulSet** model:

- Each engine version (`v0.21.4`, `v0.22.0`, etc.) gets its own StatefulSet
  named `dagger-engine-<slug>`.
- The autoscaler (in `internal/fleet.Manager`) maintains per-version
  replica counts independently.
- Scale-up: new replicas are created when all existing replicas for a
  version are at capacity (`max_sessions_per_replica`).
- Scale-down: idle replicas (no in-flight sessions for
  `replica_idle_ttl`) are removed one at a time per sweep cycle.
- Garbage collection: versions with zero replicas for `version_retention`
  have their StatefulSet deleted.

## Rationale

- Prevents cross-version resource contention.
- Allows per-version resource limits and scale policies.
- Clean separation: removing a version's StatefulSet is the garbage
  collection signal.
- Simpler than a shared pool with version-aware scheduling.

## Consequences

- The fleet provider (currently a stub; K8s is a future milestone) must
  implement `EnsureStatefulSet`, `GetReplicas`, `ScaleUp`, `ScaleDown`,
  and `WaitForReady` per version.
- The session store tracks per-replica pinning to enable least-pinned
  replica selection.
