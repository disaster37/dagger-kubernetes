# ADR-014: Registry proxy — token control + multi-registry load balancing

**Status:** Accepted · **Date:** 2026-08-17 · **Author:** dagger-cache team

## Context

The cache-ref emission previously exposed the raw registry address to the
Dagger CLI and engine:

```
type=registry,ref=dagger-cache-test-registry:5000/dagger-cache:v0-19-0,mode=max
```

This violates two requirements: the Dagger client/engine must never speak
directly with the Docker registry, and the Supervisor must control the
registry credentials. It must also be able to route cache traffic across
multiple registry deployments to distribute cache "charge", while preserving
OCI correctness (cross-run hits and upload-session affinity).

## Decision

1. **Dedicated cache vhost.** The emitted cache ref points at a dedicated,
   explicitly configured vhost (`cache.public_host`), defaulting to
   `cache.<host-of-server.public_url>` and validated at startup to differ
   from the control-plane host. The engine reaches it over the existing TLS
   listener; cache traffic is selected by `Host` header equality.
2. **Supervisor terminates engine auth.** The engine presents
   `DAGGER_CACHE_TOKEN` (from the `engine-registry-auth` secret) as a bearer
   token to the Supervisor proxy. The Supervisor validates it (constant-time)
   and injects per-backend `username`/`password`; credentials never reach the
   engine.
3. **Least-charged push + routing-table pull with self-healing probe.** New
   objects (manifest PUT, blob upload start) go to the least-charged healthy
   backend. Pulls consult a persisted SQLite routing table
   (`cache_object_routes`, `cache_blob_routes`) and, on a miss, probe healthy
   backends (least-charged first) and self-heal the table on a hit.
4. **Charge via catalog-walk manifest-size sum.** The Supervisor's own
   periodic catalog walks compute per-backend `stored_bytes`; no sidecar.
5. **Upload-session affinity.** In-flight blob uploads are pinned to one
   backend via a `cache_upload_sessions` table (POST → PATCH → PUT).
6. **SQLite v3 migration** adds the routing tables idempotently.

## Alternatives considered

- **Consistent hashing** — rejected: splits the cache on backend failure,
  but the plan chose least-charged (hashing deferred as a follow-up).
- **In-memory routing table** — rejected: lost on restart; pulls would
  re-probe every time.
- **Sidecar stats** — rejected: operational burden; catalog walks suffice.
- **Fan-out on miss** — rejected: N× load and non-deterministic routing.
- **Path-based `/v2/` interception** — rejected: the OCI protocol hardcodes
  `/v2/`; a dedicated vhost avoids control-plane collision.
- **Separate listener** — rejected for simplicity; the cache vhost shares the
  control-plane listener.

## Consequences

- New config keys: `cache.public_host` (effective default), `cache.auth_token`,
  `cache.registries[]` (per-backend `id`, `internal_addr`, `username`,
  `password`).
- SQLite v3 migration adds `cache_object_routes`, `cache_blob_routes`, and
  `cache_upload_sessions`.
- `BuildEngineJSON` (and its `EngineJSON`/`RegistryAuthEntry` types) removed —
  dead code with no callers.
- The global Hertz `WithReadTimeout` is disabled (`0`) so multi-GB blob
  uploads are not killed; control-API bodies remain capped per-handler
  (`POST /v1/engines` 1 MiB).
- The control-plane TLS certificate must include `cache.public_host` as a SAN.
- Helm chart values for `cache.registries[]` and the cache-vhost ingress/TLS
  SAN are a follow-up (not part of this changeset).
