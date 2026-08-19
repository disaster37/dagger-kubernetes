# ADR-019: Client disconnect detection — trap stuck pipelines when the owning client tunnel closes

- **Status:** accepted
- **Date:** 2026-08-19
- **Deciders:** dagger-cache maintainers

## Context

A Dagger CLI run holds a single long-lived L4 mTLS tunnel to the supervisor's
data plane (`Server.handleDataConn`). The tunnel is keyed by a client-cert
fingerprint (`certFP`); the session lease registered at `POST /v1/engines` time
carries the run's `TraceID`.

The pipeline's terminal status is normally set by an OTLP root-span finish
record arriving at `POST /v1/traces` → `AttributionService.Ingest` →
`TraceMetaRepository.UpsertIngest`, which flips `trace_meta.status` from
`""`/`"running"` to `"success"`/`"failed"`.

When the CLI is killed abruptly:

- The L4 tunnel drops; `io.Copy` returns and `handleDataConn` returns. Its only
  side effects are decrementing the in-flight count and closing connections —
  it never touches `trace_meta`.
- The OTLP finish record never arrives (the process that emits OTLP is gone).
- The in-memory lease is reaped after `lease_ttl`, but lease reaping does not
  touch `trace_meta` either.

Result: `trace_meta.status` stays `""`/`"running"` forever; the UI renders it as
running indefinitely, and the history GC sweeper even protects it from purge.

## Decision

### 1. Detect on the L4 tunnel, not the SSE viewer stream

The owning-client signal is the L4 data-plane tunnel, not the passive SSE live
viewer stream (`GET /api/v1/traces/:id/live`). A UI viewer's disconnect must
**never** fail a trace. `handleDataConn` is the single detection point: on
tunnel exit, once the lease's in-flight count drops to zero, the lease's
`TraceID` is marked failed.

### 2. Idempotent, replicated FSM transition

A new Raft command `kindMarkTraceFailed` transitions `trace_meta.status` to
`"failed"` and sets `trace_meta.failure_reason`, but **only** when the current
status is non-terminal (`""` or `"running"`). Terminal traces
(`success`/`failed`) are untouched, and a missing row is a no-op. Because the
transition is replicated, it survives supervisor restart.

A real OTLP finish is authoritative: when `UpsertIngest` promotes a non-empty
status it clears `failure_reason` (handles the rare late-arriving success span
after a disconnect-fail).

### 3. Grace period

`pipeline.disconnect_grace` (default `0s`) controls how quickly a closed tunnel
fails the trace. With `0s` the failure is applied synchronously in the tunnel's
deferred teardown (bounded by the Raft `ApplyTimeout`). With `>0` a per-trace
timer is started; a new tunnel for the same `TraceID` cancels it. The default
is `0s` because the tunnel's 30s heartbeat + 10m deadline mean it only closes on
a real disconnect, and the CLI generates a new trace ID per run (no same-trace
reconnect).

### 4. Staleness sweeper (supervisor-restart / crash fallback)

Leases are in-memory and lost on restart, so the disconnect handler cannot run
for tunnels that were open when the supervisor died. A background sweeper
(`pipeline.stale_sweep.*`) marks `running` traces with **no active lease**
(`InFlight == 0`) older than `stale_after` as failed. Active long-running
pipelines are protected by their `InFlight > 0` lease. On restart the in-memory
store is empty, so all orphaned running traces are cleaned within one sweep
(default recovery window ~6m).

### 5. Failure reason field

`domain.TraceMeta` gains `FailureReason string` (`failure_reason` in JSON), so
the API/UI can show *why* a pipeline failed. Reason strings are bounded to 256
bytes (CWE-770). Two reasons are emitted:

- `"client connection lost"` — tunnel close.
- `"client session expired"` — staleness sweeper.

### 6. Config block

```yaml
pipeline:
  disconnect_grace: "0s"   # 0 = immediate on tunnel close
  stale_sweep:
    enabled: true
    schedule: "1m"
    stale_after: "5m"
```

Env overrides: `DAGGER_CACHE_PIPELINE_DISCONNECT_GRACE`,
`DAGGER_CACHE_PIPELINE_STALE_SWEEP_ENABLED`,
`DAGGER_CACHE_PIPELINE_STALE_SWEEP_SCHEDULE`,
`DAGGER_CACHE_PIPELINE_STALE_SWEEP_STALE_AFTER`.

### 7. Live UI update + metric

On a successful transition, a `trace_update` SSE event is broadcast so connected
viewers re-fetch immediately (the 5s poll fallback remains). A new
`dagger_cache_pipeline_disconnect_failed_total` counter (label `source`:
`tunnel_close` | `stale_sweep`) counts transitions.

## Consequences

- Stuck pipelines are no longer stuck: a killed CLI run becomes `failed` with a
  recorded reason, either immediately (tunnel close) or within
  `stale_after + schedule` (restart/crash).
- The transition is replicated and idempotent; the disconnect handler and the
  stale sweeper may both target the same trace — only the first transition wins.
- Multiple tunnels sharing a certFP (rare) only fail the trace when the last one
  closes (`remainingInFlight == 0`).
- A late OTLP success after a disconnect-fail self-corrects: `UpsertIngest`
  overwrites `status=success` and clears `failure_reason`. Extremely narrow
  window; acceptable.
- **Known limitation:** the stale sweeper uses `trace_meta` sort key
  (`COALESCE(started_at, updated_at)`) as the age signal, not a heartbeat. A
  long-running pipeline that sends no OTLP for > `stale_after` but still has an
  open tunnel is protected by its `InFlight > 0` lease. If a future change makes
  leases non-in-memory, revisit.
- **Rendering** `failure_reason` in the UI is a frontend change; the field is
  already returned by the trace API (the merged `TraceMeta`). A follow-up UI
  task can render the reason badge.
