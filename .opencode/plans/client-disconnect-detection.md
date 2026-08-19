# Plan: Client disconnect detection — trap stuck pipelines when the owning client tunnel closes

## 1. Problem statement & root-cause hypothesis

**Symptom:** Pipeline `4459dd78862098d8a3cc398fb9d57549` (a Dagger trace ID) stays
"running" forever in the live pipeline UI after the Dagger CLI on the dev
machine was killed abruptly.

**Root cause:** The Dagger CLI holds a single long-lived L4 mTLS tunnel to the
supervisor's data plane (`Server.handleDataConn` in `internal/handler/server.go`,
lines ~851–944). The tunnel is keyed by a client-cert fingerprint (`certFP`);
the session lease registered at `POST /v1/engines` time carries the `TraceID`.
The pipeline's terminal status is normally set by an OTLP root-span finish
record arriving at `POST /v1/traces` → `service.ExtractTraceSummaries` →
`AttributionService.Ingest` → `TraceMetaRepository.UpsertIngest` (FSM
`upsertTraceIngest`), which flips `trace_meta.status` from `""`/`"running"` to
`"success"`/`"failed"`.

When the CLI is killed:
- The L4 tunnel drops; `io.Copy` returns, the `errc` channel fires, and
  `handleDataConn` returns. Its only side effects are `DecInFlight(fp)` (via
  `defer`) and closing the conns. **It never touches `trace_meta`.**
- The OTLP root-span finish record never arrives (the CLI process that emits
  OTLP is gone), so `trace_meta.status` is never promoted.
- The in-memory lease is reaped by `ReapOrphans` after `lease_ttl` (2m), but
  lease reaping does not touch `trace_meta` either.

Result: `trace_meta.status` stays `""`/`"running"` forever; the UI renders it
as running indefinitely. The history GC sweeper even *protects* it from purge
(`protectRunning`).

The SSE live endpoint `GET /api/v1/traces/:traceID/live` (`handleTracesLive` +
`repository.LiveHub`) is for **passive UI viewers**, not the executing client —
its disconnect must NOT fail the trace.

**Fix:** Detect disconnect of the *owning* client tunnel in `handleDataConn`
and transition the associated `trace_meta` to `FAILED` with a reason, idempotently.
Add a background staleness sweeper to catch the supervisor-restart / crash case
(where the disconnect handler cannot run because leases are in-memory and lost
on restart).

## 2. Design overview

- **Disconnect detection point:** `handleDataConn` (the L4 tunnel). On tunnel
  close, after the lease's in-flight count drops to zero, mark the lease's
  `TraceID` failed. SSE viewer disconnects are NOT a trigger.
- **State transition:** A new idempotent Raft command `kindMarkTraceFailed`
  sets `trace_meta.status = "failed"` + `trace_meta.failure_reason` **only when
  the current status is non-terminal** (`""` or `"running"`). Already-terminal
  traces (`success`/`failed`) are untouched. The transition is replicated →
  survives supervisor restart.
- **Grace period:** Configurable `pipeline.disconnect_grace` (default `0s` =
  immediate). With `0s` the failure is applied synchronously on tunnel close.
  With `>0`, a per-trace timer is started; a new tunnel for the same `TraceID`
  cancels it. Default `0s` because the tunnel's 30s heartbeat + 10m deadline
  mean it only closes on a real disconnect, and the CLI generates a new trace
  ID per run (no same-trace reconnect).
- **Staleness sweeper (supervisor-restart / crash fallback):** A background
  job marks `running` traces with **no active lease** (`InFlight == 0`) older
  than `pipeline.stale_sweep.stale_after` as failed. On restart the in-memory
  store is empty, so all orphaned running traces are cleaned within one sweep.
  Active long-running pipelines are protected by their `InFlight > 0` lease.
- **Live UI update:** On a successful transition, broadcast a `trace_update`
  SSE event so connected viewers re-fetch immediately (the 5s poll fallback
  remains).
- **Reason field:** Add `FailureReason string` to `domain.TraceMeta` so the
  UI/API can show *why* (`"client connection lost"` / `"client session expired"`).

## 3. File-by-file changes

### 3.1 `internal/domain/tracemeta.go`
- Add field to `TraceMeta`:
  ```go
  FailureReason string `json:"failure_reason,omitempty"`
  ```
- Add to `TraceMetaRepository` interface:
  ```go
  // MarkFailed transitions a non-terminal trace (status "" or "running") to
  // "failed" with the given reason. Idempotent: a trace already in a terminal
  // state ("success"/"failed") is not modified. Returns transitioned=true
  // when the status actually changed.
  MarkFailed(ctx context.Context, traceID, reason string) (transitioned bool, err error)
  ```

### 3.2 `internal/domain/session.go`
- Add to `SessionStore` interface (atomic decrement + read, race-free):
  ```go
  // DecInFlightAndGet decrements the in-flight count for certFP and returns
  // the resulting count. Returns 0 and a non-nil error when the lease is gone.
  DecInFlightAndGet(certFP string) (int, error)
  ```

### 3.3 `internal/domain/config.go`
- Add new config section:
  ```go
  type Config struct {
      ...
      Pipeline PipelineConfig `mapstructure:"pipeline"`
  }

  type PipelineConfig struct {
      DisconnectGrace time.Duration      `mapstructure:"disconnect_grace"`
      StaleSweep      PipelineStaleSweep `mapstructure:"stale_sweep"`
  }

  type PipelineStaleSweep struct {
      Enabled   bool          `mapstructure:"enabled"`
      Schedule  time.Duration `mapstructure:"schedule"`
      StaleAfter time.Duration `mapstructure:"stale_after"`
  }
  ```

### 3.4 `config/loader.go`
- Add defaults (env prefix `DAGGER_CACHE_`):
  ```go
  v.SetDefault("pipeline.disconnect_grace", 0)            // 0 = immediate
  v.SetDefault("pipeline.stale_sweep.enabled", true)
  v.SetDefault("pipeline.stale_sweep.schedule", time.Minute)
  v.SetDefault("pipeline.stale_sweep.stale_after", 5*time.Minute)
  ```

### 3.5 `config/config.app.yaml.sample` and `config/config.app.yaml`
- Add a `pipeline:` section (2-space indent) documenting `disconnect_grace`
  and `stale_sweep.*`, in sync with `loader.go`. Place after the `lease_ttl`
  block.

### 3.6 `internal/repository/fsm.go`
- Add command kind:
  ```go
  kindMarkTraceFailed
  ```
- Add payload:
  ```go
  cmdMarkTraceFailed struct {
      TraceID   string    `json:"trace_id"`
      Reason    string    `json:"reason"`
      UpdatedAt time.Time `json:"updated_at"`
  }
  ```
- Add `applyCommand` case:
  ```go
  case kindMarkTraceFailed:
      return applyPayload(cmd, "mark trace failed", func(p cmdMarkTraceFailed) (bool, error) {
          return s.markTraceFailed(p), nil
      })
  ```
  (Note: `applyPayload` returns `error`; the bool return needs a small variant
  or inline decode. Implement inline like `kindReapUploads`: decode then call
  `s.markTraceFailed(p)` returning the bool.)
- Add FSM method:
  ```go
  // markTraceFailed transitions a non-terminal trace to "failed". Returns
  // true iff the status changed. Deterministic; caller supplies UpdatedAt.
  func (s *fsmState) markTraceFailed(p cmdMarkTraceFailed) bool {
      m, ok := s.traces[p.TraceID]
      if !ok {
          return false
      }
      if m.Status == "success" || m.Status == "failed" {
          return false
      }
      m.Status = "failed"
      m.FailureReason = p.Reason
      m.UpdatedAt = p.UpdatedAt
      s.traces[p.TraceID] = m
      return true
  }
  ```
- In `upsertTraceIngest`: when a non-empty `m.Status` is promoted, clear
  `FailureReason` (a real OTLP finish is authoritative and supersedes a
  disconnect reason — handles the rare late-arriving success span):
  ```go
  if m.Status != "" {
      existing.Status = m.Status
      existing.FailureReason = "" // OTLP finish supersedes disconnect reason
  }
  ```

### 3.7 `internal/repository/fsm_snapshot.go`
- No change required. `Traces []*domain.TraceMeta` is JSON-encoded; the new
  `FailureReason` field decodes to `""` from old snapshots (backward-compatible).

### 3.8 `internal/repository/tracemeta_repo.go`
- Implement `MarkFailed`:
  ```go
  func (r *TraceMetaRepo) MarkFailed(ctx context.Context, traceID, reason string) (bool, error) {
      if !domain.ValidTraceIDKey(traceID) {
          return false, fmt.Errorf("invalid trace ID: %w", domain.ErrValidation)
      }
      if len(reason) > maxReasonLen { reason = reason[:maxReasonLen] } // bound, CWE-770
      resp, err := r.store.applyCtxResponse(ctx, kindMarkTraceFailed, cmdMarkTraceFailed{
          TraceID: traceID, Reason: reason, UpdatedAt: time.Now().UTC(),
      })
      if err != nil {
          return false, err
      }
      transitioned, _ := resp.(bool)
      return transitioned, nil
  }
  ```
  (`maxReasonLen = 256`, mirroring `attribution.maxIngestFieldLen`.)

### 3.9 `internal/service/attribution_service.go`
- Add:
  ```go
  // MarkFailed transitions a non-terminal trace to "failed" with reason.
  // Best-effort like Provision/Ingest: errors are logged, never returned to
  // break the caller. Returns transitioned=true when the status changed.
  func (a *AttributionService) MarkFailed(ctx context.Context, traceID, reason string) bool {
      if traceID == "" || len(traceID) > maxIngestFieldLen {
          return false
      }
      if len(reason) > maxIngestFieldLen {
          reason = reason[:maxIngestFieldLen]
      }
      transitioned, err := a.traceMeta.MarkFailed(ctx, traceID, reason)
      if err != nil {
          a.logger.WithError(err).WithField("trace_id", traceID).Warn("attribution mark failed")
          return false
      }
      return transitioned
  }
  ```

### 3.10 `internal/service/pipeline_lifecycle.go` (NEW)
- New service coordinating disconnect tracking + staleness sweep. Depends only
  on `domain` + `service` (layering-compliant):
  ```go
  package service

  // TraceEventBroadcaster fans out a live SSE event (implemented by
  // repository.LiveHub in the handler wiring). Defined in service to avoid
  // importing repository.
  type TraceEventBroadcaster interface {
      Broadcast(traceID string, event any)
  }

  type PipelineLifecycle struct {
      attribution *AttributionService
      traceMeta   domain.TraceMetaRepository
      sessions    domain.SessionStore
      broadcaster TraceEventBroadcaster // may be nil
      grace       time.Duration
      staleCfg    domain.PipelineStaleSweep
      logger      *logrus.Logger
      metrics     *observ.Metrics // may be nil

      mu      sync.Mutex
      pending map[string]*time.Timer // traceID -> grace timer (grace > 0 only)
  }

  func NewPipelineLifecycle(attribution *AttributionService, traceMeta domain.TraceMetaRepository,
      sessions domain.SessionStore, broadcaster TraceEventBroadcaster,
      cfg domain.PipelineConfig, logger *logrus.Logger, metrics *observ.Metrics) *PipelineLifecycle
  ```
- Methods:
  - `OnTunnelOpen(traceID string)`: cancel any pending grace timer for
    `traceID` (no-op when `grace == 0`). Called by `handleDataConn` after
    `IncInFlight`.
  - `OnTunnelClosed(traceID string, remainingInFlight int)`: if `traceID == ""`
    or `remainingInFlight > 0` → return (another tunnel still active). If
    `grace == 0` → `markFailed(ctx, traceID, "client connection lost")`
    synchronously. Else start/replace a `pending[traceID]` timer that fires
    `markFailed` after `grace`.
  - `markFailed(traceID, reason)`: call `attribution.MarkFailed`; if
    `transitioned` and `broadcaster != nil`, broadcast
    `map[string]string{"type": "trace_update"}`; if `metrics != nil`, inc
    `PipelineDisconnectFailedTotal`.
  - `RunStaleSweep(ctx)`: enumerate
    `traceMeta.ListBefore(ctx, now - staleCfg.StaleAfter, false)`; build
    `active` set of `TraceID`s whose lease has `InFlight > 0` via
    `sessions.List()`; for each candidate with `status == "" || "running"` and
    `!active[TraceID]`, call `markFailed(ctx, TraceID, "client session
    expired")`. Returns a summary `{scanned, marked, skippedActive}`.
  - `StartStaleSweep(ctx) (stop func())`: no-op when
    `!staleCfg.Enabled || staleCfg.Schedule <= 0`; else a ticker goroutine
    mirroring `HistoryPurgeService.StartGCSweeper`.
- Use `context.Background()` for the synchronous `markFailed` in
  `OnTunnelClosed` (the tunnel's request ctx is already cancelled by
  disconnect); the Raft apply uses `applyCtx` with `ApplyTimeout`.

### 3.11 `internal/observ/metrics.go`
- Add to `Metrics`:
  ```go
  PipelineDisconnectFailedTotal *prometheus.CounterVec
  ```
  Labels: `[]string{"source"}` (`"tunnel_close"` | `"stale_sweep"`). Register
  in `NewMetrics`.

### 3.12 `internal/handler/server.go`
- Add `domain.TraceEventBroadcaster` wiring: add `LiveHub` to `Deps` as
  `domain.TraceEventBroadcaster` (concrete `*repository.LiveHub` satisfies it).
  In `NewServer`, use `deps.LiveHub` when non-nil, else create
  `repository.NewLiveHub()` (preserves current test behavior). Export nothing
  new otherwise.
- Add `Lifecycle *service.PipelineLifecycle` to `Deps`; store as `s.lifecycle`.
- Restructure `handleDataConn`:
  ```go
  lease, err := s.sessions.Get(fp)
  if err != nil { ...; return }                       // no lease, no trace
  _ = s.sessions.IncInFlight(fp)
  if s.lifecycle != nil {
      s.lifecycle.OnTunnelOpen(lease.TraceID)         // cancel pending grace timer
  }
  defer func() {
      remaining, _ := s.sessions.DecInFlightAndGet(fp)
      if s.lifecycle != nil {
          s.lifecycle.OnTunnelClosed(lease.TraceID, remaining)
      }
  }()
  // ... existing dial / io.Copy / heartbeat loop unchanged ...
  ```
  Remove the old `defer func() { _ = s.sessions.DecInFlight(fp) }()`. The new
  defer replaces it (decrement + lifecycle notification on every exit path after
  `IncInFlight`, including dial-failure and `errc`).
- `handleTracesLive` is unchanged (viewer disconnect must not fail the trace).

### 3.13 `internal/service/session.go`
- Implement `DecInFlightAndGet`:
  ```go
  func (s *Store) DecInFlightAndGet(certFP string) (int, error) {
      s.mu.Lock(); defer s.mu.Unlock()
      l, err := s.lease(certFP)
      if err != nil { return 0, err }
      if l.InFlight > 0 { l.InFlight-- }
      return l.InFlight, nil
  }
  ```

### 3.14 `internal/service/stubs_test.go`
- Add `DecInFlightAndGet` to `stubSessionStore` (track an `inFlight` map or
  counter per certFP; return the decremented value).

### 3.15 `internal/handler/test_helper_test.go`
- In `newTestEnv`: construct a `*service.PipelineLifecycle` (with the env's
  `attributionSvc`, `sessions`, a `nil` broadcaster or a real `LiveHub`, a
  default `domain.PipelineConfig{}`, the test logger, `observ.NewMetrics(nil)`)
  and pass it + the live hub through `Deps`.

### 3.16 `cmd/api/main.go`
- Create `liveHub := repository.NewLiveHub()` once; pass it to `Deps.LiveHub`
  (so the server and the lifecycle share one hub).
- Construct `pipelineLifecycle := service.NewPipelineLifecycle(attributionSvc,
  traceMetaRepo, sessions, liveHub, cfg.Pipeline, logger, metrics)`.
- Pass `pipelineLifecycle` via `Deps.Lifecycle`.
- After `server.Start`, start the sweeper:
  ```go
  stopStaleSweep := pipelineLifecycle.StartStaleSweep(ctx)
  defer stopStaleSweep()
  ```

### 3.17 `docs/README.md`
- Add a "Pipeline disconnect detection" subsection under "Pipeline history
  retention" (or "Pipeline UI") describing: the L4 tunnel is the owning client
  connection; on close the trace is marked `failed` with reason `client
  connection lost`; the staleness sweeper handles supervisor restart; config
  keys `pipeline.disconnect_grace` and `pipeline.stale_sweep.*`; the
  `failure_reason` field on trace detail.

### 3.18 `docs/design/ADR-019-client-disconnect-detection.md` (NEW) + `docs/design/index.md`
- New ADR documenting: decision to detect on the L4 tunnel (not SSE), idempotent
  FSM transition, grace period, staleness sweeper, `FailureReason` field, and
  the supervisor-restart limitation/rationale. Add a row to `index.md`.

## 4. Exact behavior specification

### State machine
- Terminal states: `success`, `failed`. Non-terminal: `""`, `"running"`.
- `MarkFailed(traceID, reason)`:
  - trace_meta missing → no-op, `transitioned=false`, no error.
  - status terminal → no-op, `transitioned=false`.
  - status non-terminal → set `status="failed"`, `failure_reason=reason`,
    `updated_at=now`; `transitioned=true`.
- `UpsertIngest` with non-empty `status` → set `status`, clear
  `failure_reason` (OTLP finish is authoritative; supersedes a disconnect
  reason in the rare late-success race).

### Ordering / idempotency
- `OnTunnelClosed` only acts when `remainingInFlight == 0` (last tunnel for
  the lease). Multiple tunnels sharing a certFP (rare) do not fail the trace
  while one is still open.
- `MarkFailed` is idempotent and replicated; the disconnect handler and the
  stale sweeper may both target the same trace — only the first transition
  wins; the second is a no-op.
- The `OnTunnelOpen`/`OnTunnelClosed` pair is ordered: open is called before
  the tunnel loop, closed in the defer (after the loop). With `grace > 0`, a
  new `OnTunnelOpen` for the same `TraceID` cancels a pending close timer.

### Timing
- `disconnect_grace = 0` (default): `MarkFailed` is applied synchronously in
  the `handleDataConn` defer (Raft `ApplyTimeout` bounds the wait).
- `disconnect_grace > 0`: a `time.Timer` fires `MarkFailed` after `grace`;
  `OnTunnelOpen` cancels it. The timer goroutine uses `context.Background()`.
- Stale sweeper: runs every `pipeline.stale_sweep.schedule` (default `1m`);
  only considers traces whose sort key (`COALESCE(started_at, updated_at)`) is
  older than `stale_after` (default `5m`); skips any trace with an active
  lease (`InFlight > 0`).

### Reason strings (bounded to 256 bytes)
- Tunnel close: `"client connection lost"`.
- Stale sweeper: `"client session expired"`.

## 5. Edge cases & intended behavior

| Edge case | Behavior |
|---|---|
| Multiple UI viewers on same trace (SSE) | Their disconnect does NOT fail the trace — only the L4 owning tunnel does. |
| Multiple tunnels, same certFP (rare) | `OnTunnelClosed` no-ops while `remainingInFlight > 0`; fails only when the last closes. |
| Brief network blip on the L4 tunnel | The 30s heartbeat + 10m deadline keep the tunnel alive through idle periods; `io.Copy` only returns on a real disconnect. With `grace=0` the trace fails immediately (correct — the CLI run is dead). Operators wanting a linger window set `disconnect_grace > 0`. |
| Late OTLP success arrives after tunnel close | `UpsertIngest` sets `status=success` and clears `failure_reason` (OTLP finish is authoritative). Narrow window; acceptable. |
| Normal completion then tunnel close | OTLP finish set `success` first → `MarkFailed` is a no-op (terminal). |
| Supervisor restart with open tunnels | Disconnect handler can't run (process dying, leases in-memory). The stale sweeper marks orphaned running traces failed within `stale_after` + one schedule tick. |
| Supervisor crash (no clean shutdown) | Same as restart — stale sweeper recovers on next boot. |
| Freshly provisioned trace, tunnel not yet open | Stale sweeper skips it: age < `stale_after`. The disconnect handler hasn't run (no tunnel closed yet). |
| Trace with empty `TraceID` (legacy/anonymous lease) | `OnTunnelClosed` no-ops; `AttributionService.MarkFailed` guards `traceID == ""`. |
| `trace_meta` row missing (never provisioned) | `MarkFailed` no-ops (`transitioned=false`). |
| Lease reaped by TTL mid-tunnel | Cannot happen: heartbeat `Touch` every 30s keeps `LastActivity` fresh; `lease_ttl` default 2m. |
| Stale sweeper races with a tunnel opening | A trace older than `stale_after` with `InFlight==0` at sweep time may be marked failed just as a tunnel opens. Self-corrects: the now-open run's OTLP finish overwrites to `success`/`failed` via `UpsertIngest`. Extremely narrow. |
| Raft follower (not leader) | `applyCtx` returns `ErrNotLeader`; `AttributionService.MarkFailed` logs and returns `false`. The leader will apply it; followers' FSMs replicate. For the stale sweeper on a follower, `ListBefore` is a local read (stale but acceptable); writes no-op until leader. |

## 6. Error handling & validation rules (per AGENTS.md)

- Strings: `fmt.Sprintf` only, never `+` concatenation.
- Errors: wrap with `%w` (`fmt.Errorf("mark trace failed: %w", err)`). Client
  errors via `writeError(c, consts.Status..., "message")`.
- Logging: logrus with fields:
  ```go
  s.logger.WithFields(logrus.Fields{
      "trace_id": traceID, "cert_fp": fp, "reason": reason, "grace": grace,
  }).Info("client tunnel closed; marking trace failed")
  ```
  and `s.logger.WithError(err).WithField("trace_id", traceID).Warn("mark failed")`.
- Bound `reason` to 256 bytes (CWE-770) in both `TraceMetaRepo.MarkFailed` and
  `AttributionService.MarkFailed`.
- Validate `traceID` with `domain.ValidTraceIDKey` in `TraceMetaRepo.MarkFailed`
  (returns `domain.ErrValidation`).
- `MarkFailed` is best-effort in the disconnect path: a Raft error is logged,
  never returned to the caller (the tunnel is already closed; nothing to
  surface to a client).

## 7. Test plan (standard `testing`, table-driven, target 100%)

### `internal/repository/fsm_test.go` (extend)
- Table-driven `TestMarkTraceFailed`:
  - missing trace → `transitioned=false`, no row.
  - status `""` → `failed`, reason set, `transitioned=true`.
  - status `"running"` → `failed`, `transitioned=true`.
  - status `"success"` → unchanged, `transitioned=false`.
  - status `"failed"` (existing reason) → unchanged, `transitioned=false`.
  - `UpdatedAt` advanced on transition.
- `TestUpsertTraceIngestClearsFailureReason`: a trace marked failed then
  re-ingested with `status="success"` → `failure_reason=""`.

### `internal/repository/tracemeta_repo_test.go` (extend)
- `TestMarkFailedInvalidTraceID` → `ErrValidation`.
- `TestMarkFailedBoundsReason` → reason > 256 truncated.
- `TestMarkFailedTransitioned` via a real in-memory Raft store
  (`NewInmemRaftStore`).

### `internal/service/attribution_service_test.go` (extend)
- `TestMarkFailed`: empty traceID → false; valid → transitions; Raft error
  → logged + false (inject a stub `TraceMetaRepository` returning error).

### `internal/service/pipeline_lifecycle_test.go` (NEW)
- Table-driven `TestOnTunnelClosed`:
  - `grace=0`, `remaining=0` → `MarkFailed` called with `"client connection lost"`.
  - `grace=0`, `remaining=1` → no `MarkFailed`.
  - `traceID=""` → no-op.
  - `grace=5s`, `remaining=0` → timer fires after 5s → `MarkFailed` (use a
    stub clock or short duration).
  - `grace=5s`, `OnTunnelOpen` before fire → timer cancelled, no `MarkFailed`.
- `TestRunStaleSweep`:
  - one running trace older than `stale_after`, no active lease → marked
    `"client session expired"`.
  - one running trace with active lease (`InFlight>0`) → skipped.
  - one `success` trace older than cutoff → skipped (terminal).
  - one running trace younger than `stale_after` → skipped.
  - broadcaster receives `trace_update` only on transition.
  - metric incremented only on transition.
- Use a stub `TraceMetaRepository` (in-memory: `Get`/`ListBefore`/`MarkFailed`),
  a stub `SessionStore` (with `List` + `DecInFlightAndGet`), and a capturing
  `TraceEventBroadcaster`. Logger: `logrus.New()` → `io.Discard`.

### `internal/service/session_test.go` (extend)
- `TestDecInFlightAndGet`: inc twice, dec+get returns 1, dec+get returns 0,
  dec+get on missing lease returns error + 0.

### `internal/handler/server_test.go` (extend) + new `internal/handler/data_conn_test.go`
- `TestHandleDataConnMarksTraceFailedOnClose`: drive `handleDataConn` with a
  stub `net.Conn` pair (pipe) + a stub backend; close the client side; assert
  `traceMeta.Get` shows `status="failed"`, `failure_reason="client connection
  lost"`, and that the live hub broadcast a `trace_update`. Use the existing
  `newTestEnv` wiring (with `PipelineLifecycle` injected).
- `TestHandleDataConnNoFailWhenInFlight`: simulate two concurrent tunnels on
  the same certFP; closing one does not fail the trace.
- `TestHandleDataConnLeaseNotFound`: no lease → no panic, no `MarkFailed`.
- (These are integration-style handler tests; the L4 listener is not started —
  `handleDataConn` is invoked directly with a `*tls.Conn`-shaped stub or the
  function is refactored to accept a `net.Conn` for testability. If a `*tls.Conn`
  is required for the cert extraction, add a thin test-only seam: extract the
  cert→fp logic into a helper taking `*tls.ConnectionState` so tests can call
  the tunnel body without a real TLS handshake.)

### `config/loader_test.go` (extend)
- `TestPipelineDefaults`: `disconnect_grace=0`, `stale_sweep.enabled=true`,
  `schedule=1m`, `stale_after=5m`.
- Env override `DAGGER_CACHE_PIPELINE_DISCONNECT_GRACE=10s` parsed.

### `internal/observ/observ_test.go` (extend)
- `PipelineDisconnectFailedTotal` registered and labeled.

## 8. Documentation / config updates (per AGENTS.md)

- `config/config.app.yaml.sample` + `config/config.app.yaml`: add `pipeline:`
  section (keys, types, defaults, comments) — kept in sync with `loader.go`.
- `docs/README.md`: new "Pipeline disconnect detection" subsection.
- `docs/design/ADR-019-client-disconnect-detection.md`: new ADR.
- `docs/design/index.md`: add ADR-019 row.

## 9. Verification checklist

- [ ] `go build ./...` passes.
- [ ] `go test ./...` passes (incl. `-race`).
- [ ] `gofmt` + `goimports` (local prefix `github.com/disaster/dagger-kubernetes`).
- [ ] New unit tests green: FSM `markTraceFailed`, `TraceMetaRepo.MarkFailed`,
  `AttributionService.MarkFailed`, `PipelineLifecycle` (grace + stale sweep),
  `Store.DecInFlightAndGet`, handler `handleDataConn` close→failed.
- [ ] `config.app.yaml.sample` and `loader.go` in sync; `config/loader_test.go`
  covers defaults + env override.
- [ ] ADR-019 + `docs/README.md` updated; `index.md` row added.
- [ ] Integration note: a `tests/integration/` case (or manual) that provisions
  an engine, opens the data-plane tunnel, kills the client side, and asserts
  the trace becomes `failed` with `failure_reason="client connection lost"`
  via `GET /api/v1/traces/:id`.
- [ ] Per `AGENTS.local.md` §6: rebuild image → push → `helm upgrade` (with
  `--set supervisor.config.raft.replicas=1`) → rollout restart → §5.1 agent
  checks → request §5.2 human verification on the live UI (the stuck trace
  `4459dd78...` should now show `failed`).

## 10. Risks / unknowns / assumptions

- **Assumption:** the Dagger CLI generates a new trace ID per run and does not
  reconnect a killed run with the same trace ID. If it did, `grace=0` would
  mark the first attempt failed before the reconnect; `grace > 0` +
  `OnTunnelOpen` cancellation handles that. Default `0` is chosen because the
  CLI behavior makes same-trace reconnect implausible.
- **Assumption:** the L4 tunnel is the only owning-client connection. OTLP
  `POST /v1/traces` are short-lived HTTP requests and are not a liveness
  signal. Confirmed by reading `handleOTel` (stateless proxy + best-effort
  attribution).
- **Known limitation:** the stale sweeper uses `trace_meta.updated_at` (set
  only on OTLP ingest) as the age signal, NOT a heartbeat. A long-running
  pipeline that sends no OTLP for > `stale_after` but still has an open tunnel
  is protected by its `InFlight > 0` lease. If a future change makes leases
  non-in-memory, revisit. The supervisor-restart recovery window is up to
  `stale_after + schedule` (default ~6m).
- **Race:** stale sweeper vs a tunnel opening in the same instant —
  self-corrects via `UpsertIngest` overwriting on the real finish. Documented.
- **Forward race:** late OTLP success after disconnect-fail — `UpsertIngest`
  clears `failure_reason` and sets `success`. Acceptable (the engine did
  finish); documented.
- **UI rendering of `failure_reason`** is a frontend change; this plan adds
  the field to the API response (`GET /api/v1/traces/:id` already returns the
  merged `TraceMeta`). A follow-up UI task can render the reason badge; out of
  scope here but noted.
- **`handleDataConn` testability:** the function extracts the client cert from
  a `*tls.Conn`. If direct invocation in tests is awkward, add a small seam
  (helper taking `tls.ConnectionState`) — noted in §7.
