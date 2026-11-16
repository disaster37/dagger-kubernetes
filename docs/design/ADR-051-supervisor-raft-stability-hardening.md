# ADR-051: Supervisor & Raft stability hardening

## Status
Accepted

## Context
At 200+ concurrent developers the Raft leader must not be OOM-killed or
stalled by telemetry ingest, and the Raft PVC must not compete with
rebuildable cache state.

## Decision
- Decode each OTLP request body exactly once (`service.ExtractOTLPBatch`) and
  reuse the result for attribution, live fan-out, and cache-event hooks.
- Gate buffered OTLP bodies with a weighted byte semaphore
  (`otel.max_inflight_bytes`, default 256 MiB) and return `503` on saturation.
- Deduplicate non-terminal `AttributionService.Ingest` Raft writes within 15s.
- Make the data-plane connection cap configurable (`server.max_data_conns`,
  default 2048).
- Mount a dedicated `emptyDir` at `/var/cache/dagger-kubernetes`
  (`supervisor.cache_scratch.size_limit`, default `8Gi`) for the rebuildable
  remote-cache index; the Raft PVC only holds `raft.db` and snapshots.
- Support optional `GOMEMLIMIT` injection (`supervisor.gomemlimit`).

## Consequences
Telemetry ingest degrades gracefully under memory pressure, Raft write load is
bounded, and cache churn cannot fill the Raft volume.
