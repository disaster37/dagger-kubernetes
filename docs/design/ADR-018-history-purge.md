# ADR-018: Pipeline history auto-purge + manual purge

- **Status:** accepted
- **Date:** 2026-08-18
- **Deciders:** dagger-kubernetes maintainers

## Context

Pipeline history accumulates without bound: every run writes a `trace_meta` row
into the Raft FSM, log streams into Loki, and metric series into
VictoriaMetrics. The existing trace listing (`GET /api/v1/traces`) only reads;
there is no way to delete pipeline history, so stale traces pile up forever and
operators cannot comply with retention policies (or right-to-erasure requests).

The cache layer already has the pattern we want: `cache.gc.*` config +
`CacheStatsService` + `POST /api/v1/cache/purge*`. This ADR mirrors that
pattern for pipeline history.

## Decision

### 1. Scope: trace_meta + Loki + VictoriaMetrics (not Tempo)

The supervisor purges **pipeline history** = trace metadata (Raft FSM) + Loki
logs + VictoriaMetrics metrics. **Tempo spans are not deleted** — Tempo has no
per-trace delete API, so spans age out via Tempo retention. The Helm chart and
docs recommend setting `tempo.retention` to match (or exceed)
`history.gc.max_age` so spans age out alongside the supervisor-side purge.

### 2. Running traces are protected

Traces whose `TraceMeta.Status` is `""` or `"running"` are skipped by both the
auto sweeper and `purge-all`. A manual per-trace-id `purge` is an admin override
and is **not** protected (the operator explicitly asked for that trace).

Traces with a zero-time sort key (`COALESCE(started_at, updated_at)` empty)
have unknown age and are **never purged** — conservative, matching cache GC's
unknown-age skip.

### 3. Manual purge scope

- `POST /api/v1/history/purge` — optional `{trace_id}` body, single-trace admin
  purge.
- `POST /api/v1/history/purge-all` — purge every trace older than
  `history.gc.max_age` (running traces protected).

Both are admin-only. `purge` is idempotent: a missing `trace_meta` row still
attempts the (idempotent) telemetry deletes and reports `already_purged`.

### 4. Config block

Top-level `history.gc.*` with `enabled`, `max_age`, `schedule` — the same shape
as `cache.gc.*`. Defaults: `enabled: false`, `max_age: "720h"` (30d),
`schedule: "1h"`. Env overrides are `DAGGER_KUBERNETES_HISTORY_GC_ENABLED`,
`DAGGER_KUBERNETES_HISTORY_GC_MAX_AGE`, `DAGGER_KUBERNETES_HISTORY_GC_SCHEDULE`.

### 5. Partial-failure semantics

For each candidate, telemetry deletion (Loki + VM) is attempted **first** and is
**best-effort** — a failure increments `telemetry_errors` (logged at warn level)
but does **not** abort the `trace_meta` deletion. The trace_meta row is then
deleted unconditionally (so the trace disappears from the UI). Rationale:

- Orphaned telemetry ages out via Loki/VM retention; orphaned trace_meta would
  cause the sweeper to retry forever.
- Telemetry deletes are idempotent (deleting already-deleted data is a no-op),
  so a retry after a transient backend outage is safe.

Ordering is enforced in `HistoryPurgeService` (new `internal/service/history_purge.go`).

### 6. Backend requirements (documented, not auto-configured)

- **Loki**: deletion is enabled **by default** in the chart
  (`limits_config.deletion_mode: filter-and-delete`,
  `compactor.retention_enabled: true`,
  `compactor.delete_request_store: filesystem`), so no operator action is
  needed for filesystem deployments; object-storage deployments must point
  `delete_request_store` at S3/GCS.
- **VictoriaMetrics**: `delete_series` is admin-only and deletes the **entire
  series** matching `match[]` (no time range); space is reclaimed lazily. If
  `-deleteAuthKey` is set on the VM deployment, the delete request must include
  that key (out of scope for v1; documented as a prerequisite). The
  OpenTelemetry collector's `transform/logs` processor promotes
  `trace_id`/`span_id` to **log** labels only; the metrics pipeline
  (`otlp → batch → prometheusremotewrite`) has no such transform, and the
  metrics currently emitted (BuildKit cache hit/miss counters, engine metrics)
  are aggregate with no trace association. `{trace_id="..."}` metric deletion
  is therefore a no-op today and activates automatically only if per-trace
  metrics carrying a `trace_id` label are introduced.

### 7. FSM + repository changes

`TraceMetaRepository` gains `ListBefore(cutoff, protectRunning)`, `Delete`
(idempotent) and `Stats`. These are backed by new FSM helpers
(`listTracesBefore`, `traceStats`) and a new `kindDeleteTrace` Raft command
(`delete(map, key)` is a no-op on a missing key). Deletion is captured by the
existing snapshot deep-copy, so no snapshot format change is needed.

`LogRepository` gains `DeleteTraceLogs`; `MetricsClient` gains `DeleteSeries` /
`DeleteTraceSeries`.

## Consequences

- Operators can enforce pipeline-history retention and satisfy erasure
  requests from the UI (`/history`) or the API.
- Running traces are never swept; only explicit per-trace admin purge can
  remove them.
- Tempo spans require a separately-configured retention policy; there is no
  supervisor-side span deletion.
- Purge is safe to retry: idempotent telemetry deletes + idempotent FSM delete.
- VictoriaMetrics deletion is a no-op for the metrics emitted today (aggregate,
  no `trace_id` label); it activates automatically once per-trace metrics
  carrying a `trace_id` label are introduced. Trace metadata and Loki log
  deletion are effective immediately.
- A single `purgeMu` serializes `Purge` / `PurgeAll` / `RunGC` so a manual
  purge waits for an in-flight sweep (and vice versa).
