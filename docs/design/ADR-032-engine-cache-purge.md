# ADR-032: Engine local-cache purge (per version, live dagql prune)

**Status:** Accepted  
**Date:** 2026-09-14

> **Partially superseded (2026-09-15):** the post-prune `push-once` sync step
> was removed together with the worker-cache sync; the purge is now purely
> local. The live-dagql-prune decision below still stands.

## Context

Each engine pod keeps a BuildKit **local cache** on its per-pod PVC at
`/var/lib/dagger/worker` (`metadata_v2.db` + `containerdmeta.db` + `workerid` +
a content-addressed `content/blobs/sha256/...` store). Operators need to reclaim
it per engine version (e.g. `dagger-engine-v0-19-0`) without taking the fleet
down.

The operator explicitly rejected a `scale-to-zero → delete PVCs → scale back up`
design: during scale-to-zero the version has no engine available. The revised
design must invoke the **real engine prune API** on every **running** pod and
keep the engines available. The shared worker snapshot no longer exists
(worker-cache sync removed, 2026-09); the retained per-pod PVC is the only
warm-start source.

> **Scope.** This is NOT `POST /api/v1/cache/purge`, which deletes the remote
> BuildKit `cache/` prefix via `S3CacheGC.Purge`. This feature operates on the
> engine's **local cache** (per-pod PVC). The two purges are orthogonal.

## Decision

Per-version purge = a live dagql
`{ engine { localCache { prune(useDefaultPolicy: false) } } }` against **every
pod** of the version's StatefulSet, executed **in-process** by the supervisor.
Purge is local-only: it has no sync/push follow-up (the removed worker-cache
sync used to exec `supervisor cache-sync push-once` in each pod's
`cache-sync` sidecar via the K8s `pods/exec` subresource; that sidecar, the
RBAC rule, and the result field are gone).

### The wire call (route B)

The supervisor speaks the engine's session HTTP protocol directly over
plaintext TCP — the same call the Dagger CLI makes — instead of shelling out to
the CLI or importing the Dagger Go SDK:

```text
POST http://<podIP>:9999/query            (HTTP/1.1, plaintext TCP)
Content-Type: application/json
X-Dagger-Client-Metadata: base64({
    "client_id":            "<16-byte random hex>",
    "client_secret_token":  "<16-byte random hex>",
    "session_id":           "<16-byte random hex>",
    "client_hostname":      "supervisor",
    "client_stable_id":     "<16-byte random hex>",
    "client_version":       "<pod's engine version, e.g. v0.19.0>",
    "labels": {}
})

{"query": "{ engine { localCache { prune(useDefaultPolicy: false) } } }"}
```

Success is HTTP 200 with an empty GraphQL `errors` array; any non-200 status or
non-empty `errors` is a per-pod failure.

### Evidence (Dagger source @ v0.19.0)

1. The engine serves a single h2c handler on `:9999` that routes gRPC and
   session-HTTP by content-type (`cmd/engine/main.go`). `h2c.NewHandler` accepts
   both h2c and plain HTTP/1.1, so a stdlib `net/http` client suffices.
2. Session/client metadata is one header, `X-Dagger-Client-Metadata`
   (`engine/opts.go`), base64-encoded JSON.
3. The session is created lazily and is not pre-registered: `getOrInitClient`
   requires only non-empty `SessionID`, `ClientID`, `ClientSecretToken`; the
   first client of a session becomes the *main* client. The engine never
   consults the supervisor's `domain.SessionStore`/`SessionRegistry` (those are
   keyed by client-cert fingerprint for L4 data-plane routing, ADR-026).
4. The query endpoint is `POST /query` (`engine/consts.go`); a single POST
   self-initialises the session.
5. `engine.localCache.prune` requires the main client
   (`query.RequireMainClient`), guaranteed by a fresh synthetic session per
   prune. `useDefaultPolicy:false` prunes the whole cache of any releasable
   entries.
6. The engine enforces only a *minimum* client version
   (`MinimumClientVersion = "v0.19.0"`); setting `client_version` to the pod's
   exact StatefulSet version satisfies the check and selects the engine's own
   schema view.
7. The engine port is plaintext in this repo: `--addr=tcp://0.0.0.0:9999`, no
   `--tlscert`/`--tlskey`. The supervisor's mTLS boundary is the data plane; the
   engine hop is plaintext, so the pruner needs no client certificate.

### Local-only purge (post-removal)

An earlier revision followed each successful prune with an immediate
`supervisor cache-sync push-once` exec in that pod's `cache-sync` sidecar so the
shared S3 snapshot reflected the pruned state. Both the sidecar and the
subcommand were removed with the worker-cache sync (2026-09); the purge now
only prunes the local cache, and `EnginePodPurgeResult` no longer carries a
`synced` field. Warm start after a purge therefore comes solely from the
retained per-pod PVC (empty after the prune until the engine re-fills it) —
there is no shared snapshot to reconcile.

### Session / pinned-session semantics

The pruner creates a fresh synthetic Dagger session per pod (new `session_id` +
`client_id` + `client_secret_token` every call) and never touches the
supervisor's `SessionStore`/`SessionRegistry`. Because
`engine.localCache.prune(useDefaultPolicy:false)` prunes only **releasable**
entries, a live prune does not corrupt running pipelines — it is the same
operation Dagger itself performs under load. The purge is therefore **allowed
while the version has pinned (running) sessions**; there is no pinned-session
refusal.

### API surface

| Item | Value |
|---|---|
| Purge route | `POST /api/v1/fleet/:version/purge-cache` |
| Status route | `GET /api/v1/fleet/:version/purge-cache` (last recorded status) |
| Auth | admin-only on both |
| Body | none (version in path) |
| Success | `200` with `EngineCachePurgeResult` JSON |

`EngineCachePurgeResult` carries `version`, `state`
(`running`/`completed`/`failed`), `started_at`, `finished_at`, `replicas`,
`pods[]` (`pod_name`, `ordinal`, `pruned`, `error`), and `message`.
`freed_bytes` is intentionally omitted: the dagql `prune` mutation reports no
byte count.

Error mapping: `ErrEngineFleetNotFound` → 404, `ErrPurgeInProgress` → 409,
other → 500. `:version` must satisfy `domain.IsFullVersion` → else 400.

### Concurrency & state

Purges are serialized **per version** via an in-memory `active` set + `status`
map under one mutex; a concurrent purge of the same version returns 409.
Different versions can purge in parallel. Status is in-memory only (a supervisor
restart loses it; re-running is idempotent). Per-pod prune jobs are bounded by
`pruneConcurrency = 8`; each prune is bounded by `prunePodTimeout = 10m`.

### Edge cases

- **StatefulSet missing** → `AllVersions()` does not contain the version →
  `ErrEngineFleetNotFound` → 404 (`GetReplicas` alone cannot distinguish
  "missing" from "present but 0 replicas").
- **Zero replicas** → no-op: `replicas=0`, `state:"completed"`, message
  "no running engine pods; nothing to prune".
- **Pod unreachable / NotReady / IP changed** → per-pod error, `pruned:false`;
  other pods proceed; aggregate is `completed` with per-pod detail.
- **Version mismatch** → the engine returns 500 "incompatible client version …";
  surfaced per pod. Should not happen because `client_version` is the pod's own
  version.
- **Partial failure** → each pod is an independent atomic prune. Pods that
  failed to prune keep their cache; the operator re-runs the purge (idempotent)
  to finish failed pods.

## Alternatives considered

- **Scale-to-zero + delete PVCs** — rejected by the operator: the version has no
  engine available during the operation.
- **Dagger Go SDK (`dagger.io/dagger`)** — rejected. At v0.19.0 the public API is
  `dagger.Connect(ctx, dagger.WithConn(conn engineconn.EngineConn))`;
  `EngineConn` is an HTTP `Doer` (`Do(*http.Request) (*http.Response, error)`),
  not a raw `net.Conn`. Route A would require exactly the same HTTP/session work
  as route B plus a large dependency tree (genqlient, gqlparser, OTel,
  querybuilder, engineconn) for a single static query. The SDK's curated query
  builder does not expose `Engine.localCache` (dagql-internal), so it would still
  need a raw query string.
- **Version-matched CLI exec** — rejected by the operator (no shelling out to
  the `dagger` CLI, no CLI download/materialization).
- **exec + buildctl** — rejected: buildctl does not expose the dagql
  `engine.localCache.prune` semantics and would add a second tool to the image.
- **SIGUSR1 handler for the immediate push** — rejected at the time (a one-shot
  subcommand was simpler, idempotent, and testable); moot now that the push
  path was removed with the worker-cache sync.

## Consequences

- New domain interfaces `EnginePruner`, `EngineCachePurger` and sentinel errors
  `ErrEngineFleetNotFound`, `ErrPurgeInProgress`.
- New repository implementation `SessionEnginePruner` (stdlib-only HTTP session
  client).
- New service `EngineCachePurgeService`; new handler routes; new UI per-version
  "Purge cache" button on the Runners page.
- `cmd/api` wires `NewEngineCachePurgeService(provider, pruner, logger)`.
- The sync-specific `SidecarSyncPusher`/`ExecSyncPusher`, the
  `supervisor cache-sync push-once` subcommand, and the `pods/exec` RBAC rule
  were removed with the worker-cache sync (2026-09).
- The purge endpoint no longer requires `cli.enabled=true`; it requires only a
  Kubernetes clientset + FleetProvider (always true in the live deployment) and
  reachable target pods.

## Compatibility surface to re-check

The single header (`X-Dagger-Client-Metadata`) and endpoint (`POST /query`) are
the compatibility surface. The wire protocol is version-stable across the fleet
(0.19–0.21); each engine enforces only a minimum client version, so one HTTP
client with a per-pod `client_version` is sufficient. A future engine (≥ v0.22)
that changes the header name/encoding or the `/query` path is a new-version
migration, not a runtime branch.
