# ADR-055: Read-through remote cache service

## Status
Accepted

## Context
Dagger Cloud provides a shared remote cache over `GET /v1/engine` so engines
can reuse layers across pods and restarts.

## Decision
- Implement the `engine/remotecache/protocol` envelope and message set
  (`Hello/Welcome`, `Open/Opened`, `Claims/Claimed`, `Settle/Settled`,
  `Export/Exported`, `Upload/Uploaded`, `Merge/Merged`, `Offer/Offered`,
  `Renew/Renewed`, `Lookup/LookedUp`, `Error`).
- Store `dagql.ValueBundle` JSON and blobs in S3; issue presigned PUT/GET URLs
  signed with `cache.remote.s3_engine_endpoint` so the Host matches what engine
  pods dial.
- Keep a rebuildable `bbolt` lookup index at
  `/var/cache/dagger-kubernetes/remotecache.db` (`NoSync: true`), partitioned by
  org id (`per_org`) or `_shared`.
- Serve `/v1/engine` with `hertz-contrib/websocket`; followers relay upgrades to
  the leader through a `repository` dialer using the dynamic mTLS provider.
- Render `[remoteCache] url` in `engine.toml` and inject `DAGGER_CLOUD_TOKEN`.
- Expose `/api/v1/remote-cache` stats/purge and Prometheus metrics.

## Consequences
Cache hits survive engine restarts and leader failover; the index is
disposable and never written to Raft.

## Update — residual-risk remediation (PR #70 review)
- Small blobs are assembled into `dagger-pack v1` objects. The header records
  absolute member offsets, and `PresignGetRange` signs
  `Range: bytes=<offset>-<offset+size-1>` via `minio.Client.PresignHeader`, so
  engines fetch one member with HTTP `206 Partial Content`.
- `cache.remote.workers` bounds concurrent exports with
  `golang.org/x/sync/semaphore`; `cache.remote.pack_max_member_bytes` (4 MiB)
  controls packing.
- `Lookup` reports `stored` (committed), `merging` (in-flight on another
  engine), or `notStored`; `Merge`/`Renew` return `Locations` with signed
  `Range` headers plus `BundleURLs`.
- `X-Dagger-Cache-Mode: read-only` makes mutating messages no-op
  acknowledgements (fork-PR guard).
- The bbolt index is checkpointed to S3 and GC runs every
  `cache.remote.gc_interval` (5m) on the leader.
- Revoked tokens close the WebSocket with code `4001`.
