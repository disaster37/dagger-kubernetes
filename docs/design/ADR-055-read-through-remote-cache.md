# ADR-055: Read-through remote cache service (beta.28 protocol, `engine.json` config channel)

**Status:** Accepted — rewritten 2026-11-28 after the first live enablement failed  
**Date:** 2026-10-20 (original), 2026-11-28 (rewrite)

## Context

Dagger Cloud provides a shared remote cache over `GET /v1/engine` so engines
reuse values and layers across pods, restarts and nodes. This platform
implements a self-hosted equivalent.

The first implementation (October 2026) was wired against a home-grown,
one-directional `{"type":…,"data":…}` snake_case protocol and a
`/etc/dagger/engine.toml` config file. A live enablement attempt on
2026-11-28 showed both assumptions were wrong:

1. **Config channel.** Dagger engines read their own settings from
   `/etc/dagger/engine.json` (`engine/config.LoadDefault()` →
   `DefaultConfigPath()`), decoded with `DisallowUnknownFields` and fatal on
   error. `engine.toml` is BuildKit's config (`--config`), never read for
   `remoteCache`/`registries`/`logLevel`. The repo's `[remoteCache]` URL and
   registry mirrors were therefore silently ignored.
2. **Protocol.** Dagger **v1.0.0-beta.28** `engine/remotecache/protocol` frames
   every message as `{"id":N,"re":M,"type":"…","body":{…}}` with camelCase
   bodies, and both sides send requests. `export`, `upload`, `merge` and
   `offer` are service→engine requests; their `exported`/`uploaded`/`merged`/
   `offered` replies are correlated by `re`. The repo's codec matched no
   Dagger release: the engine greeted, received an uncorrelated reply and timed
   out (`hello: context deadline exceeded`).
3. **Availability.** `engine/remotecache` does not exist at any `0.19`/`0.20`/
   `0.21` release (including `0.21.12`), nor at `v1.0.0-beta.2/5/10/15`; it
   first appears at `beta.19`, and its beta.28 file set (`lookup.go`, `pack/`)
   is the one this service targets. Serving one codec to every 1.0 beta is not
   safe.

## Decision

### D1 — `engine.json` is the config channel, rendered per engine version

The supervisor renders the subset of `engine/config.Config` it manages:
`logLevel` (`"debug"` for `fleet.engine_debug`), `registries`
(`mirrors` + `http: true`), and `remoteCache.url`. It is stored in the
fleet-wide ConfigMap `dagger-engine-config` (key `engine.json`) and projected
into `/etc/dagger/engine.json` next to the image-pull auth secret.

Because the engine's decode is fatal on unknown fields, `remoteCache` is
rendered **only** when `domain.Parse(version).Compare(Parse("v1.0.0-beta.28")) >= 0`.
The gate is a capability threshold, never `Major >= 1`: `beta.10`/`beta.15`
have no `RemoteCache` field, so a major-only gate would crash those pods at
config load. Unparseable/custom tags fail closed (no `remoteCache`).

`fleet.engine_log_format` is supervisor-only: the engine hardcodes its text
formatter, so it is not rendered.

### D2 — Implement the beta.28 protocol exactly

`internal/domain/remotecache.go` defines the envelope and every body with the
upstream JSON tags. Bundles are stored and replayed as opaque dagql
`ValueBundle` JSON; digests stay strings because `domain` is stdlib-only.

The connection is a per-connection state machine
(`internal/service/remotecache_conn.go`):

- two independent ID spaces, each starting at 1; `re` names the request a
  message answers (`re == 0` = not a reply), and `pending` is keyed by `re`;
- engine requests: `hello`, `open`, `settle`, `claims`, `lookup`, `renew`;
- service requests: `export`, `upload`, `merge`, `offer`;
- `hello` must be welcomed before any service request; a duplicate `hello` is
  answered with `error`; a `settle` for an older period with
  `error{"stale settle"}`;
- the service never selects outputs, so blobs are presigned as standalone
  objects; packs are accepted on the wire but not produced.

Read path: a `lookup` miss that the index knows is answered `lookedUp` with
`status: "merging"`, a reserved `merge` id and `parts: "complete"`, then a
`merge` request carrying the stored bundle and `lookup` = the engine lookup id.
`renew` answers with a presigned GET address (the pack object's address when the
member lives in a pack, so the engine ranges inside it).

Write path: after `settle` (and on a reconnect that arrives `settled`), the
leader exports the cache entries it learned from the engine's OTLP cache
evidence (`dagger.io/engine.cache` + `dagger.io/cache.result.id` +
`dagger.io/dag.digest`), stores each bundle in S3 and the index, then sends
`upload` with presigned PUTs for the bundle's layer blobs. An engine reconnected
already settled is idle, so it can be asked immediately.

### D3 — Transport and auth

- `/v1/engine` is served on the control listener and shares the route table
  with the internal listener (port + 2).
- The engine dials with `Authorization: Basic base64(orgEngineToken+":")`.
  The handler resolves the identity through `AuthService` and derives the
  namespace from `Identity.OrgID`; it never uses user membership or
  `X-Dagger-Org`.
- A follower relays the upgrade to the leader's **internal service hostname**
  (`hostOnly(server.internal_control_url)`, internal port), forwarding the
  Authorization header, so the minting-CA leaf's SAN matches. The minting-CA
  internal leaf already carries the internal control host's bare, `.svc` and
  `.svc.<clusterDomain>` forms.
- The engine ingress names the control Service in
  `nginx.org/websocket-services` (F5 NGINX Ingress Controller) alongside the
  community controller's long read/send timeouts. Remote cache stays disabled
  by default.
- Keepalive matches upstream: 64 MiB read limit, 15 s service ping, pong
  deadline 3× the interval, close code `4001` when the per-heartbeat
  `auth.Resolve` finds the token revoked.

### D4 — Isolation, quota and index

- Namespace = org ID (`per_org`) or `_shared` (deprecated `shared` mode); the
  index and S3 prefix are per namespace.
- The `bbolt` index remains rebuildable and is checkpointed to S3; GC runs on
  the leader every `cache.remote.gc_interval`.
- `cache.remote.workers` bounds concurrent export→upload chains through a
  semaphore; each service request has a 30 s timeout.
- A `read-only` connection (`X-Dagger-Cache-Mode: read-only`, kept for future
  clients) welcomes, answers `lookedUp` with `notStored`, and never exports,
  uploads or merges.
- Presigned URLs are signed for `cache.remote.s3_engine_endpoint` and never
  against an engine-supplied host. Tokens and signed URLs are never logged;
  logs carry digests and counts only.

## Consequences

- Registry mirrors and `debug` work on every engine release again; the remote
  cache works on `>= v1.0.0-beta.28` and is inert (no dial) below it.
- **Pinned protocol.** Any engine bump requires re-verifying the new tag's
  `engine/remotecache` file set and `engine/config.Config` fields before
  moving `engineRemoteCacheMinVersion`.
- **Telemetry dependency.** The write path learns roots from OTLP cache
  evidence; without it the service can still serve read hits but its own
  exports stay empty. The engine sets `Telemetry.EngineEvents` when the remote
  cache is on and exports session spans with the cache attributes.
- **Presign host.** If `s3_engine_endpoint` differs from the host engines dial,
  uploads/downloads fail at the engine while the service reports healthy;
  documented as an operator caveat.
- **Log format is not configurable** on the engine; `engine_log_format` remains
  a supervisor-side value only.

## Alternatives considered

- **Render `engine.toml` for mirrors only** — rejected: proven unread by the
  engine for `registries`; the live symptom was silently ignored mirrors.
- **Gate remote cache on `Major >= 1`** — rejected: `beta.10`/`beta.15` have
  no `RemoteCache` field and the engine exits on the unknown key.
- **Serve beta.19/20 codecs too** — rejected: different, smaller file set and
  the service targets beta.28's `lookup`/`pack` flow.
- **Keep the legacy snake_case codec** — rejected: it matches no Dagger
  release; 0.21 has no remote cache to be compatible with.
