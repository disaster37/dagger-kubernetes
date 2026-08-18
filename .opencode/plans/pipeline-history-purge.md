# Plan: Pipeline History Auto-Purge + Manual Purge UI

Mirror the existing cache pruning/GC pattern (`cache.gc.*` / `CacheStatsService` /
`POST /api/v1/cache/purge*`) to add duration-based auto-purge and admin manual
purge of **pipeline history** = trace metadata (Raft FSM) + Loki logs +
VictoriaMetrics metrics. Tempo spans are **not** deleted by this feature
(no per-trace delete API); they age out via Tempo retention, which the Helm
chart/docs will recommend configuring.

## Confirmed design decisions

1. **Tempo spans**: leave to Tempo retention; add a Tempo retention
   recommendation to the Helm chart + docs. No supervisor-side Tempo delete.
2. **Running traces are protected**: traces whose `TraceMeta.Status` is `""`
   or `"running"` are skipped by both the auto sweeper and `purge-all`.
   Manual per-trace-id `purge` is an admin override and is **not** protected.
3. **Manual purge scope**: both `POST /api/v1/history/purge` (optional
   `{trace_id}` body, single-trace admin purge) and
   `POST /api/v1/history/purge-all` (purge every trace older than
   `history.gc.max_age`).
4. **Config block**: top-level `history.gc.*` with `enabled`, `max_age`,
   `schedule` (mirrors `cache.gc.*` shape).

## Key assumptions / risks (verify before/while implementing)

- **Metrics `trace_id` label name**: the plan assumes Dagger CLI / OTel
  collector emit metrics with a `trace_id` label (matching the Loki stream
  label used in `log_store.go`). **VERIFY** by inspecting a real metric in
  VictoriaMetrics (`/api/v1/series?match[]={__name__=~".+"}`) on the home
  cluster. If the label is named differently (e.g. `traceId`), update the
  `match[]` selector in `MetricsClient.DeleteSeries` accordingly.
- **Loki deletion must be enabled**: `POST /loki/api/v1/delete` requires the
  Loki compactor running with `limits_config.deletion_mode` enabled and a
  `delete_request_store` configured. The Helm Loki subchart values must be
  updated (or documented) to enable this; otherwise delete requests return
  404/405 and the supervisor logs a warning (best-effort).
- **VictoriaMetrics `delete_series` is admin-only and full-series**: VM
  deletes the entire series matching `match[]`; it does **not** support time
  ranges, and space is reclaimed lazily during background merges. If
  `-deleteAuthKey` is set on the VM deployment, the supervisor's delete
  request must include that key (out of scope for v1; document as a
  prerequisite).
- **Tempo retention**: recommend setting `tempo.retention: 168h` (or
  matching `history.gc.max_age`) in the Helm chart `tempo` subchart values
  and `docs/README.md`.

## File-by-file changes

### 1. Config

**`internal/domain/config.go`** (modify)
- Add to `Config`:
  ```go
  History HistoryConfig `mapstructure:"history"`
  ```
- Add:
  ```go
  // HistoryConfig governs pipeline-history retention (trace_meta + logs +
  // metrics). Mirrors CacheConfig.GC.
  type HistoryConfig struct {
      GC HistoryGCConfig `mapstructure:"gc"`
  }

  // HistoryGCConfig governs the history auto-purge background sweeper.
  type HistoryGCConfig struct {
      Enabled  bool          `mapstructure:"enabled"`
      MaxAge   time.Duration `mapstructure:"max_age"`
      Schedule time.Duration `mapstructure:"schedule"`
  }
  ```

**`config/loader.go`** (modify) — add after the `cache.gc.*` defaults
(~line 99):
```go
v.SetDefault("history.gc.enabled", false)
v.SetDefault("history.gc.max_age", "720h") // 30d
v.SetDefault("history.gc.schedule", "1h")
```
Env override path: `DAGGER_CACHE_HISTORY_GC_ENABLED`,
`DAGGER_CACHE_HISTORY_GC_MAX_AGE`, `DAGGER_CACHE_HISTORY_GC_SCHEDULE`
(automatic via `v.AutomaticEnv()` + the `.`→`_` replacer).

**`config/config.app.yaml`** (modify) — add a `history:` block after
`cache:`:
```yaml
# --- Pipeline history retention ------------------------------------------------
history:
  gc:                                 # auto-purge of trace_meta + logs + metrics (see ADR-018).
    enabled: false                     # master switch; disabled by default.
    max_age: "720h"                    # purge traces whose last update is older than this (30d).
    schedule: "1h"                     # background sweeper ticker interval.
```

**`config/config.app.yaml.sample`** (modify) — add the same block with
full comments, plus a row in the config reference table in `docs/README.md`
(see §11).

### 2. Domain

**`internal/domain/history.go`** (create) — mirror of `cache.go` GC types:
```go
package domain

import (
    "context"
    "errors"
)

// ErrHistoryPurgeRunning is returned when a purge-all/sweeper attempt is
// already in flight (concurrency guard). Lives in domain so the handler can
// map it to 409.
var ErrHistoryPurgeRunning = errors.New("history purge already in progress")

// HistoryStats is the payload of GET /api/v1/history.
type HistoryStats struct {
    TraceCount     int             `json:"trace_count"`
    OldestUpdatedAt string         `json:"oldest_updated_at,omitempty"` // RFC3339; "" when no traces
    CollectedAt    string          `json:"collected_at"`                // RFC3339 UTC
    GC             HistoryGCRules  `json:"gc"`
}

// HistoryGCRules describes the history auto-purge config + last/next run.
type HistoryGCRules struct {
    Enabled       bool                 `json:"enabled"`
    MaxAge        string               `json:"max_age"`           // duration string e.g. "720h"
    Schedule      string               `json:"schedule"`         // duration string e.g. "1h"
    LastRunAt     string               `json:"last_run_at,omitempty"`      // RFC3339
    LastRunSummary *HistoryGCRunSummary `json:"last_run_summary,omitempty"`
    NextRunAt     string               `json:"next_run_at,omitempty"`      // RFC3339 (estimated)
}

// HistoryGCRunSummary is the result of one history GC sweep / purge-all.
type HistoryGCRunSummary struct {
    StartedAt       string `json:"started_at"`        // RFC3339
    FinishedAt      string `json:"finished_at"`       // RFC3339
    PurgedTraces    int    `json:"purged_traces"`
    SkippedRunning  int    `json:"skipped_running"`
    LogsDeleted      int    `json:"logs_deleted"`
    MetricsDeleted   int    `json:"metrics_deleted"`
    TelemetryErrors int    `json:"telemetry_errors"` // Loki+VM delete failures
    Errors          int    `json:"errors"`            // trace_meta delete failures
    Message        string `json:"message,omitempty"`
}

// HistoryPurgeRequest is the body of POST /api/v1/history/purge.
type HistoryPurgeRequest struct {
    TraceID string `json:"trace_id"` // optional; when set, purge a single trace (admin override, ignores running protection)
}

// HistoryPurgeResult is the response of the history purge endpoints.
type HistoryPurgeResult struct {
    PurgedTraces   int      `json:"purged_traces"`
    LogsDeleted    int      `json:"logs_deleted"`
    MetricsDeleted int      `json:"metrics_deleted"`
    TraceIDs       []string `json:"trace_ids"`        // affected trace IDs
    AlreadyPurged  int      `json:"already_purged"`   // trace_meta rows already absent
    Message        string   `json:"message,omitempty"`
}

// HistoryStatsProvider reports history stats + GC rules.
type HistoryStatsProvider interface {
    Stats(ctx context.Context) (*HistoryStats, error)
    GCRules() HistoryGCRules
}

// HistoryPurger purges pipeline history.
type HistoryPurger interface {
    Purge(ctx context.Context, req HistoryPurgeRequest) (*HistoryPurgeResult, error)
    PurgeAll(ctx context.Context) (*HistoryPurgeResult, error)
}
```

**`internal/domain/tracemeta.go`** (modify) — extend the repository
interface with deletion + listing helpers:
```go
type TraceMetaRepository interface {
    UpsertProvision(ctx context.Context, traceID, userID, version string) error
    UpsertIngest(ctx context.Context, m *TraceMeta) error
    Get(ctx context.Context, traceID string) (*TraceMeta, error)
    List(ctx context.Context, f TraceFilter) ([]*TraceListResult, error)

    // ListBefore returns trace_meta rows whose COALESCE(started_at, updated_at)
    // is older than cutoff. When protectRunning is true, rows with status ""
    // or "running" are excluded. Used by the history GC sweeper to enumerate
    // purge candidates without the 500-row List cap.
    ListBefore(ctx context.Context, cutoff time.Time, protectRunning bool) ([]*TraceMeta, error)

    // Delete removes a single trace_meta row. Idempotent: a missing row
    // returns nil.
    Delete(ctx context.Context, traceID string) error

    // Stats returns the total trace count and the oldest COALESCE(started_at,
    // updated_at) timestamp (zero time when no traces exist). Cheap FSM read.
    Stats(ctx context.Context) (count int, oldest time.Time, err error)
}
```

**`internal/domain/telemetry.go`** (modify) — extend `LogRepository`:
```go
type LogRepository interface {
    QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error)
    // DeleteTraceLogs requests deletion of all log streams for traceID from
    // Loki (POST /loki/api/v1/delete). Best-effort: returns nil on 204;
    // returns a wrapped error on non-2xx. Requires Loki compactor + deletion
    // enabled on the backend.
    DeleteTraceLogs(ctx context.Context, traceID string) error
}
```

### 3. Repository

**`internal/repository/fsm.go`** (modify)
- Add command kind constant after `kindReapUploads`:
  ```go
  kindDeleteTrace
  ```
- Add command payload struct in the `type (...)` block:
  ```go
  cmdDeleteTrace struct {
      TraceID string `json:"trace_id"`
  }
  ```
- Add apply-case in `applyCommand` switch (after `kindReapUploads`):
  ```go
  case kindDeleteTrace:
      return nil, applyPayload(cmd, "delete trace", func(p cmdDeleteTrace) error {
          delete(s.traces, p.TraceID)
          return nil
      })
  ```
- Add FSM read helpers near `readTrace` (~line 1048):
  ```go
  // listTracesBefore returns trace_meta rows older than cutoff (by
  // COALESCE(started_at, updated_at)), excluding running traces when
  // protectRunning is true. Sorted oldest-first. No limit (the sweeper must
  // see every candidate).
  func (f *FSM) listTracesBefore(cutoff time.Time, protectRunning bool) []*domain.TraceMeta {
      f.state.mu.RLock()
      defer f.state.mu.RUnlock()
      var out []*domain.TraceMeta
      for _, m := range f.state.traces {
          if traceSortKey(m).After(cutoff) {
              continue
          }
          if protectRunning && (m.Status == "" || m.Status == "running") {
              continue
          }
          cp := *m
          out = append(out, &cp)
      }
      sort.Slice(out, func(i, j int) bool {
          return traceSortKey(out[i]).Before(traceSortKey(out[j]))
      })
      return out
  }

  // traceStats returns the total trace count and the oldest trace sort key.
  func (f *FSM) traceStats() (int, time.Time) {
      f.state.mu.RLock()
      defer f.state.mu.RUnlock()
      var oldest time.Time
      for _, m := range f.state.traces {
          k := traceSortKey(m)
          if oldest.IsZero() || k.Before(oldest) {
              oldest = k
          }
      }
      return len(f.state.traces), oldest
  }
  ```

**`internal/repository/fsm_snapshot.go`** (no change required) — traces
are already copied in `Snapshot`/`Restore`; deletion is just `delete(map)`
which is captured by the existing deep-copy. Verify the snapshot round-trip
test still passes with deleted traces.

**`internal/repository/tracemeta_repo.go`** (modify) — implement the new
interface methods:
```go
func (r *TraceMetaRepo) ListBefore(ctx context.Context, cutoff time.Time, protectRunning bool) ([]*domain.TraceMeta, error) {
    return r.store.fsmRead().listTracesBefore(cutoff, protectRunning), nil
}

func (r *TraceMetaRepo) Delete(ctx context.Context, traceID string) error {
    return r.store.applyCtx(ctx, kindDeleteTrace, cmdDeleteTrace{TraceID: traceID})
}

func (r *TraceMetaRepo) Stats(ctx context.Context) (int, time.Time, error) {
    count, oldest := r.store.fsmRead().traceStats()
    return count, oldest, nil
}
```
Note: `Delete` is idempotent because the FSM apply-case `delete(map, key)`
on a missing key is a no-op (returns nil).

**`internal/repository/log_store.go`** (modify) — add `DeleteTraceLogs`:
```go
// DeleteTraceLogs requests deletion of all log streams for traceID from Loki.
// Endpoint: POST /loki/api/v1/delete with query={trace_id="<id>"}&start=<unix>&end=<unix>.
// Requires Loki compactor with deletion enabled. Returns nil on 204.
func (c *LogsClient) DeleteTraceLogs(ctx context.Context, traceID string) error {
    if !hexTraceID.MatchString(traceID) {
        return fmt.Errorf("invalid trace ID format")
    }
    if c.lokiURL == "" {
        return fmt.Errorf("loki URL not configured")
    }
    sanitized := sanitizeLogQLValue(traceID)
    // Delete all-time logs for this trace_id: start=0, end=now.
    params := url.Values{}
    params.Set("query", fmt.Sprintf(`{trace_id="%s"}`, sanitized))
    params.Set("start", "0")
    params.Set("end", fmt.Sprintf("%d", time.Now().UnixNano()))
    deleteURL := fmt.Sprintf("%s/loki/api/v1/delete?%s", c.lokiURL, params.Encode())
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, deleteURL, nil)
    if err != nil {
        return fmt.Errorf("loki delete request: %w", err)
    }
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("loki delete failed: %w", err)
    }
    defer func() { _ = resp.Body.Close() }()
    if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
        return nil
    }
    return fmt.Errorf("loki delete returned status %d", resp.StatusCode)
}
```

**`internal/repository/metrics_store.go`** (modify) — add `DeleteSeries`:
```go
// DeleteSeries deletes all time series matching the given PromQL match[]
// selectors from VictoriaMetrics. Endpoint:
// POST /api/v1/admin/tsdb/delete_series with match[] form params. Returns
// nil on 204/200. Note: VM deletes whole series (no time range); space is
// reclaimed lazily. Requires -deleteAuthKey unset or matching key.
func (c *MetricsClient) DeleteSeries(ctx context.Context, matchers []string) error {
    if c.victoriaURL == "" {
        return fmt.Errorf("victoria URL not configured")
    }
    if len(matchers) == 0 {
        return nil
    }
    params := url.Values{}
    for _, m := range matchers {
        params.Add("match[]", m)
    }
    deleteURL := fmt.Sprintf("%s/api/v1/admin/tsdb/delete_series?%s", c.victoriaURL, params.Encode())
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, deleteURL, nil)
    if err != nil {
        return fmt.Errorf("victoria delete request: %w", err)
    }
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("victoria delete failed: %w", err)
    }
    defer func() { _ = resp.Body.Close() }()
    if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
        return nil
    }
    return fmt.Errorf("victoria delete returned status %d", resp.StatusCode)
}

// DeleteTraceSeries deletes all metrics tagged with trace_id=<traceID>.
// Assumes the OTel collector promotes trace_id as a metric label named
// "trace_id" (verify on the live cluster).
func (c *MetricsClient) DeleteTraceSeries(ctx context.Context, traceID string) error {
    return c.DeleteSeries(ctx, []string{fmt.Sprintf(`{trace_id="%s"}`, traceID)})
}
```

### 4. Service

**`internal/service/history_purge.go`** (create) — mirror of
`cache_stats.go` GC structure. Key shape:
```go
type HistoryPurgeService struct {
    traceMeta  domain.TraceMetaRepository
    logs       domain.LogRepository        // Loki (may be nil)
    metrics    *repository.MetricsClient    // VictoriaMetrics (may be nil)
    gcCfg      domain.HistoryGCConfig
    logger     *logrus.Logger
    metricsObs *observ.Metrics              // may be nil

    mu       sync.Mutex
    cached   *domain.HistoryStats
    cachedAt time.Time

    purgeMu sync.Mutex // serializes Purge / PurgeAll / RunGC
    gcMu    sync.Mutex // guards lastGC / lastGCAt / nextGCAt
    lastGC  *domain.HistoryGCRunSummary
    lastGCAt time.Time
    nextGCAt time.Time
}

func NewHistoryPurgeService(
    traceMeta domain.TraceMetaRepository,
    logs domain.LogRepository,
    metrics *repository.MetricsClient,
    gcCfg domain.HistoryGCConfig,
    logger *logrus.Logger,
    obs *observ.Metrics,
) *HistoryPurgeService
```

Methods (mirror `CacheStatsService`):
- `Stats(ctx) (*domain.HistoryStats, error)` — TTL-cached 15s; reads
  `traceMeta.Stats()` + `GCRules()`.
- `GCRules() domain.HistoryGCRules` — same shape as `CacheStatsService.GCRules`.
- `Purge(ctx, req domain.HistoryPurgeRequest) (*domain.HistoryPurgeResult, error)`:
  1. Validate `req.TraceID` against `hexTraceID` regex (reuse
     `repository.hexTraceID` — expose it or re-declare a domain-level regex;
     simplest: re-declare `var hexTraceID = regexp.MustCompile(...)` in the
     service package, or add a `domain.ValidTraceID` helper). **Recommended**:
     add `domain.ValidTraceID(id string) bool` in `internal/domain/tracemeta.go`
     and use it in both handler and service.
  2. `purgeMu.Lock()`.
  3. Best-effort: `logs.DeleteTraceLogs(ctx, traceID)` (count `logs_deleted`
     on success, `telemetry_errors` on failure). Skip if `logs == nil`.
  4. Best-effort: `metrics.DeleteTraceSeries(ctx, traceID)` (count
     `metrics_deleted` / `telemetry_errors`). Skip if `metrics == nil`.
  5. `traceMeta.Delete(ctx, traceID)` — idempotent; if the row was already
     absent, count `already_purged` (check via `traceMeta.Get` first, or
     treat nil-error as purged). Simplest: `Get` first; if `ErrNotFound` →
     `already_purged++` and still attempt telemetry delete (idempotent).
     Actually: do telemetry delete first (idempotent), then `Get` to decide
     `purged` vs `already_purged`, then `Delete`.
  6. Increment `metricsObs.HistoryPurgeTotal`.
  7. Return `HistoryPurgeResult`.
- `PurgeAll(ctx) (*domain.HistoryPurgeResult, error)`:
  1. `purgeMu.Lock()`.
  2. `cutoff := time.Now().Add(-s.gcCfg.MaxAge)`.
  3. `candidates := traceMeta.ListBefore(ctx, cutoff, true)` (protect running).
  4. For each candidate: same per-trace steps as `Purge` (telemetry best-effort
     → `traceMeta.Delete`). Aggregate counts.
  5. `invalidateCache()`.
  6. Return result.
- `RunGC(ctx) (*domain.HistoryGCRunSummary, error)`:
  1. `purgeMu.Lock()`.
  2. `summary := &domain.HistoryGCRunSummary{StartedAt: rfc3339(now)}`.
  3. `cutoff := time.Now().Add(-s.gcCfg.MaxAge)`.
  4. `candidates := traceMeta.ListBefore(ctx, cutoff, true)`.
  5. For each candidate: telemetry delete (best-effort, count
     `logs_deleted`/`metrics_deleted`/`telemetry_errors`), then
     `traceMeta.Delete` (count `purged_traces` / `errors`). `skipped_running`
     is already excluded by `ListBefore(..., true)`; to report it, call
     `ListBefore(ctx, cutoff, false)` and subtract — **simpler**: just
     report `purged_traces` and `telemetry_errors`; set `skipped_running` to
     the count of running traces older than cutoff via a second
     `ListBefore(ctx, cutoff, false)` minus the protected list length. Keep
     it simple: do one `ListBefore(cutoff, true)` and report
     `skipped_running = 0` (running traces are silently skipped, matching
     cache GC's `Skipped` semantics only when explicitly enumerated). Decision:
     call `ListBefore(cutoff, false)` once, then in the loop skip running
     ones (counting `skipped_running`), so the summary is informative.
  6. `finish(msg, err)` → `recordGC(summary, err)`, `invalidateCache()`.
- `recordGC(summary, err)` — same as cache: stores last/next, bumps
  `metricsObs.HistoryGCRunTotal.WithLabelValues(status)`.
- `invalidateCache()` — clears `cached`.
- `StartGCSweeper(ctx) (stop func())` — identical structure to
  `CacheStatsService.StartGCSweeper`: no-op when `!gcCfg.Enabled`;
  `time.NewTicker(gcCfg.Schedule)`; on tick call `RunGC` and log errors.

**Ordering / partial-failure semantics** (documented in the service doc
comment): for each candidate, telemetry deletion (Loki + VM) is attempted
first and is **best-effort** — a failure increments `telemetry_errors` and
is logged but does **not** abort the trace_meta deletion. The trace_meta
row is then deleted unconditionally (so the trace disappears from the UI).
Rationale: orphaned telemetry ages out via Loki/VM retention; orphaned
trace_meta would cause the sweeper to retry forever. Telemetry deletes are
idempotent (deleting already-deleted data is a no-op), so a retry after a
transient backend outage is safe.

**Context / cancellation**: each backend call uses the passed `ctx`; on
`ctx.Err()` the loop breaks and the summary is finished with the partial
counts + a "cancelled" message.

**Concurrency**: `purgeMu` serializes `Purge`/`PurgeAll`/`RunGC` (a manual
purge while the sweeper is running waits). `gcMu` guards only the last/next
run fields (short critical section). The sweeper goroutine respects
`ctx.Done()` and the `done` channel (same as cache).

**Empty trace list**: `ListBefore` returning an empty slice yields a
summary with all-zero counts and `Message: "no traces older than max_age"`;
this is a success, not an error.

### 5. Handler + routes

**`internal/handler/history.go`** (create) — mirror of `cache.go`:
```go
package handler

import (
    "context"
    "errors"

    "github.com/cloudwego/hertz/pkg/app"
    "github.com/cloudwego/hertz/pkg/protocol/consts"

    "github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleHistoryInfo serves the history stats + GC rules (GET /api/v1/history).
func (s *Server) handleHistoryInfo(ctx context.Context, c *app.RequestContext) {
    if !s.requireAuth(c) {
        return
    }
    if s.historyStats == nil {
        writeError(c, consts.StatusInternalServerError, "history stats unavailable")
        return
    }
    stats, err := s.historyStats.Stats(ctx)
    if err != nil {
        s.logger.WithError(err).Error("history stats failed")
        writeError(c, consts.StatusInternalServerError, "history stats failed")
        return
    }
    writeJSON(c, stats)
}

// handleHistoryPurge purges a single trace (POST /api/v1/history/purge, admin-only).
func (s *Server) handleHistoryPurge(ctx context.Context, c *app.RequestContext) {
    if s.historyPurger == nil {
        writeError(c, consts.StatusInternalServerError, "history purge unavailable")
        return
    }
    var req domain.HistoryPurgeRequest
    if !decodeBody(c, &req) {
        return
    }
    if req.TraceID == "" || !domain.ValidTraceID(req.TraceID) {
        writeError(c, consts.StatusBadRequest, "invalid trace_id")
        return
    }
    result, err := s.historyPurger.Purge(ctx, req)
    if err != nil {
        s.writeHistoryPurgeError(c, err)
        return
    }
    writeJSON(c, result)
}

// handleHistoryPurgeAll purges every trace older than history.gc.max_age
// (POST /api/v1/history/purge-all, admin-only).
func (s *Server) handleHistoryPurgeAll(ctx context.Context, c *app.RequestContext) {
    if s.historyPurger == nil {
        writeError(c, consts.StatusInternalServerError, "history purge unavailable")
        return
    }
    result, err := s.historyPurger.PurgeAll(ctx)
    if err != nil {
        s.writeHistoryPurgeError(c, err)
        return
    }
    writeJSON(c, result)
}

func (s *Server) writeHistoryPurgeError(c *app.RequestContext, err error) {
    switch {
    case errors.Is(err, domain.ErrValidation):
        writeError(c, consts.StatusBadRequest, err.Error())
    case errors.Is(err, domain.ErrHistoryPurgeRunning):
        writeError(c, consts.StatusConflict, "history purge already in progress")
    default:
        s.logger.WithError(err).Error("history purge failed")
        writeError(c, consts.StatusInternalServerError, "history purge failed")
    }
}
```

**`internal/handler/server.go`** (modify)
- Add to `Deps` struct (after `CachePurger`):
  ```go
  HistoryStatsProvider domain.HistoryStatsProvider
  HistoryPurger         domain.HistoryPurger
  ```
- Add to `Server` struct (after `cachePurger`):
  ```go
  historyStats  domain.HistoryStatsProvider
  historyPurger domain.HistoryPurger
  ```
- Wire in `NewServer`:
  ```go
  historyStats:  deps.HistoryStatsProvider,
  historyPurger: deps.HistoryPurger,
  ```
- Register routes in `configure()` after the cache routes (~line 440):
  ```go
  h.GET("/api/v1/history", s.handleHistoryInfo)
  h.POST("/api/v1/history/purge", s.adminOnly(s.handleHistoryPurge))
  h.POST("/api/v1/history/purge-all", s.adminOnly(s.handleHistoryPurgeAll))
  ```

**`internal/handler/test_helper_test.go`** (modify)
- Add stubs:
  ```go
  type stubHistoryStatsProvider struct {
      stats *domain.HistoryStats
      err   error
  }
  func (s *stubHistoryStatsProvider) Stats(context.Context) (*domain.HistoryStats, error) {
      return s.stats, s.err
  }
  func (s *stubHistoryStatsProvider) GCRules() domain.HistoryGCRules { return domain.HistoryGCRules{} }

  type stubHistoryPurger struct {
      result *domain.HistoryPurgeResult
      err    error
  }
  func (p *stubHistoryPurger) Purge(context.Context, domain.HistoryPurgeRequest) (*domain.HistoryPurgeResult, error) {
      return p.result, p.err
  }
  func (p *stubHistoryPurger) PurgeAll(context.Context) (*domain.HistoryPurgeResult, error) {
      return p.result, p.err
  }
  ```
- Wire them into `newTestEnv`'s `Deps`:
  ```go
  HistoryStatsProvider: &stubHistoryStatsProvider{
      stats: &domain.HistoryStats{TraceCount: 0, GC: domain.HistoryGCRules{}},
  },
  HistoryPurger: &stubHistoryPurger{result: &domain.HistoryPurgeResult{}},
  ```

### 6. Observability

**`internal/observ/metrics.go`** (modify)
- Add fields to `Metrics`:
  ```go
  HistoryPurgeTotal prometheus.Counter
  HistoryGCRunTotal *prometheus.CounterVec
  ```
- Construct in `NewMetrics`:
  ```go
  HistoryPurgeTotal: prometheus.NewCounter(prometheus.CounterOpts{
      Name: "dagger_cache_history_purge_total",
      Help: "Total number of traces purged from history (manual + GC)",
  }),
  HistoryGCRunTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
      Name: "dagger_cache_history_gc_run_total",
      Help: "Total number of history GC sweeper runs",
  }, []string{"status"}),
  ```
- Register both in the `reg.MustRegister(...)` call.

### 7. UI

**`ui/src/api/types.ts`** (modify) — add:
```ts
export interface HistoryGCRunSummary {
  started_at: string
  finished_at: string
  purged_traces: number
  skipped_running: number
  logs_deleted: number
  metrics_deleted: number
  telemetry_errors: number
  errors: number
  message?: string
}
export interface HistoryGCRules {
  enabled: boolean
  max_age: string
  schedule: string
  last_run_at?: string
  last_run_summary?: HistoryGCRunSummary
  next_run_at?: string
}
export interface HistoryInfo {
  trace_count: number
  oldest_updated_at?: string
  collected_at: string
  gc: HistoryGCRules
}
export interface HistoryPurgeRequest {
  trace_id?: string
}
export interface HistoryPurgeResult {
  purged_traces: number
  logs_deleted: number
  metrics_deleted: number
  trace_ids: string[]
  already_purged: number
  message?: string
}
```

**`ui/src/api/client.ts`** (modify) — add:
```ts
export async function fetchHistoryInfo(): Promise<HistoryInfo> {
  const { data } = await api.get('/api/v1/history')
  return data as HistoryInfo
}
export async function purgeHistory(payload: HistoryPurgeRequest): Promise<HistoryPurgeResult> {
  const { data } = await api.post('/api/v1/history/purge', payload)
  return data as HistoryPurgeResult
}
export async function purgeAllHistory(): Promise<HistoryPurgeResult> {
  const { data } = await api.post('/api/v1/history/purge-all')
  return data as HistoryPurgeResult
}
```
(Add `HistoryInfo`, `HistoryPurgeRequest`, `HistoryPurgeResult` to the
import list at the top.)

**`ui/src/history/History.vue`** (create) — mirror `MagicCache.vue`:
- `onMounted` → `fetchHistoryInfo()`.
- Show a card with `trace_count`, `oldest_updated_at`, `collected_at`.
- Show a "History auto-purge (GC)" card mirroring the cache GC card:
  enabled badge, `max_age`, `schedule`, last/next run, last run summary
  (`purged_traces`, `logs_deleted`, `metrics_deleted`, `telemetry_errors`).
- Admin card (`v-if="auth.isAdmin"`):
  - A text input + "Purge trace" button (calls `purgeHistory({trace_id})`
    with a `confirm()` dialog).
  - A "Purge all history older than max_age" button (calls `purgeAllHistory()`
    with a `confirm()` dialog).
  - A `purgeMessage` ref rendered after the buttons.
- Reuse `formatTime` (copy from MagicCache or extract to a shared util; for
  minimal churn, duplicate the two helpers).

**`ui/src/router/index.ts`** (modify) — add route:
```ts
{ path: '/history', name: 'history', component: () => import('@/src/history/History.vue') },
```
(Check the actual alias `@` → `ui/src`; use `@/history/History.vue`.)

**`ui/src/App.vue`** (modify) — add a nav link after MagicCache:
```html
<router-link to="/history">History</router-link>
```

**UI build / embed** — the Vue build output must be regenerated and copied
into the embedded dir:
```bash
cd ui && npm run build
# Vite emits ui/dist/; the Dockerfile copies ui/dist -> internal/handler/ui-dist/
cp -r ui/dist/* internal/handler/ui-dist/
```
Both `ui/dist/` and `internal/handler/ui-dist/` are committed (the
`//go:embed all:ui-dist` in `internal/handler/ui.go` reads the latter). The
Dockerfile rebuilds from `ui/` so the committed `internal/handler/ui-dist/`
is only needed for non-Docker `go build`. **Both must be regenerated and
committed** so a local `go build ./cmd/api` works without a Node toolchain.

### 8. Wiring

**`cmd/api/main.go`** (modify)
- After `cacheStatsSvc := ...` (~line 252), add:
  ```go
  historyPurgeSvc := service.NewHistoryPurgeService(
      traceMetaRepo, logsClient, metricsClient, cfg.History.GC, logger, metrics,
  )
  ```
- Add to `&handler.Deps{...}`:
  ```go
  HistoryStatsProvider: historyPurgeSvc,
  HistoryPurger:        historyPurgeSvc,
  ```
- After `stopGC := cacheStatsSvc.StartGCSweeper(ctx)` (~line 301), add:
  ```go
  stopHistoryGC := historyPurgeSvc.StartGCSweeper(ctx)
  defer stopHistoryGC()
  ```

### 9. Tests (stdlib-only, table-driven)

**`internal/repository/fsm_test.go`** (modify) — add:
- `TestFSMDeleteTrace`: insert a trace via `kindUpsertTraceIngest`, apply
  `kindDeleteTrace`, assert `readTrace` returns `ErrNotFound`; re-apply
  `kindDeleteTrace` (idempotent, no error).
- `TestFSMListTracesBefore`: insert traces with varied `StartedAt`/
  `UpdatedAt`/`Status`; assert `listTracesBefore(cutoff, true)` excludes
  running + future, returns oldest-first; assert
  `listTracesBefore(cutoff, false)` includes running.
- `TestFSMTraceStats`: assert `traceStats()` returns correct count + oldest.
- Extend the snapshot round-trip test (`TestFSMSnapshotRoundTrip` or
  similar) to include a deleted-then-snapshotted trace.

**`internal/repository/tracemeta_repo_test.go`** (modify) — add:
- `TestTraceMetaRepoDelete`: via `newTestStore`, upsert + delete + verify
  `Get` returns `ErrNotFound`; delete again (idempotent).
- `TestTraceMetaRepoListBefore` + `TestTraceMetaRepoStats`.

**`internal/repository/log_store_test.go`** / `telemetry_test.go` (modify)
— add `TestDeleteTraceLogs`: use `httptest.Server` returning 204 → nil;
return 404 → wrapped error; verify the request URL contains
`query={trace_id="..."}` + `start=0` + `end=<unix>`.

**`internal/repository/metrics_store_test.go`** (create or modify) — add
`TestDeleteSeries`: `httptest.Server` returning 204 → nil; verify the
request URL contains `match[]={trace_id="..."}`; multiple matchers →
multiple `match[]` params.

**`internal/service/history_purge_test.go`** (create) — mirror
`cache_stats_test.go`:
- Fake `TraceMetaRepository` (in-memory) + fake Loki/VM `httptest.Server`.
- `TestPurgeSingleTrace`, `TestPurgeInvalidTraceID`,
  `TestPurgeAlreadyPurged`, `TestPurgeAll`, `TestRunGCPurgesOldTraces`,
  `TestRunGCProtectsRunning`, `TestRunGCTelemetryErrorsContinue` (Loki
  500 → `telemetry_errors++` but trace_meta still deleted),
  `TestRunGCEmptyCandidates`, `TestGCRulesReflectConfigAndLastRun`,
  `TestStartGCSweeperDisabled` (no-op stop), `TestStartGCSweeperEnabled`
  (ticker fires `RunGC`), `TestPurgeConcurrency` (`purgeMu` serializes).
- `TestStatsCached` (15s TTL).

**`internal/handler/history_test.go`** (create) — mirror `cache_test.go`:
- `TestHandleHistoryInfoAuthGating` (401 without token, 200 with admin).
- `TestHandleHistoryInfoShape`.
- `TestHandleHistoryPurgeAdminOnly` (401 / 403 user / 200 admin).
- `TestHandleHistoryPurgeInvalidTraceID` (400).
- `TestHandleHistoryPurgeAllAdminOnly`.
- `TestHandleHistoryPurgeErrorMapping` (stub returns
  `ErrHistoryPurgeRunning` → 409).

**`config/loader_test.go`** (modify) — add to `TestLoadDefaults`:
```go
if cfg.History.GC.Enabled {
    t.Fatal("history.gc.enabled default should be false")
}
if cfg.History.GC.MaxAge != 720*time.Hour {
    t.Fatalf("history.gc.max_age default = %v, want 720h", cfg.History.GC.MaxAge)
}
if cfg.History.GC.Schedule != time.Hour {
    t.Fatalf("history.gc.schedule default = %v, want 1h", cfg.History.GC.Schedule)
}
```
- Add an env-override test for `DAGGER_CACHE_HISTORY_GC_ENABLED=true`.

### 10. Docs

**`docs/design/ADR-018-history-purge.md`** (create) — context, decision,
the four confirmed design decisions, the Tempo/Loki/VM assumptions, the
partial-failure semantics, and the config reference.

**`docs/README.md`** (modify)
- Add a "Pipeline history retention" subsection under "Remote shared cache"
  (or a new top-level section) describing `history.gc.*`, the manual purge
  endpoints, the Tempo/Loki/VM prerequisites, and the running-trace
  protection.
- Add rows to the config reference table:
  ```
  | `history`   | `gc.enabled`                | `false`                          | Master switch for the history auto-purge sweeper. |
  |             | `gc.max_age`                | `720h`                           | Purge traces whose last update is older than this (30d). |
  |             | `gc.schedule`               | `1h`                             | History sweeper ticker interval. |
  ```
- Add a Tempo retention recommendation to the "Telemetry stack" section:
  set `tempo.retention` to match `history.gc.max_age` (or longer) so spans
  age out alongside the supervisor-side purge.
- Add a Loki deletion prerequisite note: enable the compactor +
  `limits_config.deletion_mode` in the Loki subchart values.
- Add a VictoriaMetrics note: `delete_series` is admin-only; ensure
  `-deleteAuthKey` is unset or document the key requirement.

**`config/config.app.yaml.sample`** (modify) — add the `history:` block
with full comments (see §1).

**`deploy/helm/dagger-kubernetes/values.yaml`** (modify, recommended) —
add under `supervisor.config`:
```yaml
## @param supervisor.config.history.gc.enabled Master switch for the history auto-purge sweeper.
## @param supervisor.config.history.gc.maxAge Purge traces older than this (30d default).
## @param supervisor.config.history.gc.schedule History sweeper ticker interval.
history:
  gc:
    enabled: false
    maxAge: "720h"
    schedule: "1h"
```
**`deploy/helm/dagger-kubernetes/templates/configmap.yaml`** (modify,
recommended) — render the `history:` block (mirror the `cache:` block;
note the existing chart does **not** render `cache.gc.*`, falling back to
compiled defaults — for `history.gc.*` we add explicit rendering so
operators can override via Helm).

**`deploy/helm/dagger-kubernetes/values.yaml`** (modify, recommended) —
add a Tempo retention recommendation under the `tempo` subchart values
comment, and a Loki deletion-mode note under the `loki` subchart values
comment. (Actual subchart value changes are out of scope if the subchart
defaults are acceptable; document them.)

### 11. Validation / redeploy (per AGENTS.local.md §6)

After implementation:
1. `go test ./...` green (all new + existing tests).
2. `cd ui && npm run build && cp -r dist/* ../internal/handler/ui-dist/`.
3. `docker build -t docker.io/disaster/dagger-kubernetes:dev .`
4. `docker push docker.io/disaster/dagger-kubernetes:dev`.
5. `helm --kubeconfig /home/user/.kube/home get values dagger-cache-test -n dagger-cache-test -o yaml > /tmp/dagger-cache-test.values.yaml`
6. `helm --kubeconfig /home/user/.kube/home upgrade --install dagger-cache-test ./deploy/helm/dagger-kubernetes -n dagger-cache-test -f /tmp/dagger-cache-test.values.yaml --set supervisor.image.tag=dev --set supervisor.image.pullPolicy=Always --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes`
7. `kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test rollout restart deploy/dagger-cache-test-dagger-kubernetes && kubectl ... rollout status ... --timeout=300s`
8. Agent verification (§5.1): pods ready, `/healthz`+`/readyz` 200,
   `GET /api/v1/history` 200 with admin token, supervisor logs clean.
9. Human verification (§5.2): the new `/history` UI page renders, the GC
   card shows the configured rules, and the admin purge buttons work
   against real cluster data.
10. Update `AGENTS.local.md` §3/§7 if any deployed values change.

## Edge cases (handled)

- **Empty trace list**: `ListBefore` returns empty → summary all-zero,
  success.
- **Unknown age**: a trace with zero `StartedAt` and `UpdatedAt` is treated
  as epoch (1970) → always older than cutoff → purged. **Decision**: treat
  zero-time as "unknown age, skip" (conservative, mirrors cache GC's
  unknown-age skip). Implement in `listTracesBefore`: skip when
  `traceSortKey(m).IsZero()`.
- **Tempo retention**: spans not deleted; documented.
- **Loki/VM unreachable**: `telemetry_errors++`, trace_meta still deleted;
  logged at warn level.
- **Partial deletion failures**: per-trace; loop continues; summary
  aggregates.
- **Already-deleted rows**: `traceMeta.Delete` is idempotent (FSM
  `delete(map, missing)` is a no-op); `Purge` uses `Get` first to count
  `already_purged`.
- **Concurrency**: `purgeMu` serializes all purge entry points; the
  sweeper's tick waits for any in-flight manual purge.
- **Trace still running**: excluded by `ListBefore(..., protectRunning=true)`
  for `PurgeAll`/`RunGC`; `Purge` (single-trace admin override) does NOT
  protect running traces.
- **Context cancellation**: loop breaks on `ctx.Err()`; summary finished
  with partial counts + "cancelled" message.
- **Non-leader Raft node**: `traceMeta.Delete` returns
  `domain.ErrNotLeader` → counted as `errors` in the summary; the sweeper
  logs it. (Single-node home cluster is always leader.)

## Ordered implementation checklist

1. `internal/domain/config.go` — add `HistoryConfig` + `HistoryGCConfig` +
  `Config.History`.
2. `config/loader.go` — add `history.gc.*` defaults.
3. `internal/domain/history.go` (new) — `HistoryStats`, `HistoryGCRules`,
   `HistoryGCRunSummary`, `HistoryPurgeRequest`, `HistoryPurgeResult`,
   `HistoryStatsProvider`, `HistoryPurger`, `ErrHistoryPurgeRunning`.
4. `internal/domain/tracemeta.go` — extend `TraceMetaRepository`
   (`ListBefore`, `Delete`, `Stats`) + add `ValidTraceID`.
5. `internal/domain/telemetry.go` — extend `LogRepository` with
   `DeleteTraceLogs`.
6. `internal/repository/fsm.go` — `kindDeleteTrace` + `cmdDeleteTrace` +
   apply-case + `listTracesBefore` + `traceStats` (skip zero-time sort key).
7. `internal/repository/fsm_snapshot.go` — verify (no change expected).
8. `internal/repository/tracemeta_repo.go` — implement `ListBefore`,
   `Delete`, `Stats`.
9. `internal/repository/log_store.go` — `DeleteTraceLogs`.
10. `internal/repository/metrics_store.go` — `DeleteSeries` +
    `DeleteTraceSeries`.
11. `internal/observ/metrics.go` — `HistoryPurgeTotal`, `HistoryGCRunTotal`.
12. `internal/service/history_purge.go` (new) — full service mirroring
    `CacheStatsService`.
13. `internal/handler/history.go` (new) — three handlers + error mapper.
14. `internal/handler/server.go` — `Deps` + `Server` fields + `NewServer`
    wiring + route registration.
15. `internal/handler/test_helper_test.go` — stubs + `newTestEnv` wiring.
16. `cmd/api/main.go` — construct `HistoryPurgeService`, wire into `Deps`,
    start sweeper.
17. Tests: `fsm_test.go`, `tracemeta_repo_test.go`, `log_store_test.go`/
    `telemetry_test.go`, `metrics_store_test.go`, `history_purge_test.go`
    (new), `history_test.go` (new), `loader_test.go`.
18. UI: `types.ts`, `client.ts`, `history/History.vue` (new),
    `router/index.ts`, `App.vue`; rebuild `ui/dist` + copy to
    `internal/handler/ui-dist/`.
19. Docs: `ADR-018-history-purge.md` (new), `docs/README.md`,
    `config/config.app.yaml`, `config/config.app.yaml.sample`.
20. Helm (recommended): `values.yaml` + `configmap.yaml` render
    `history.gc.*`; add Tempo retention + Loki deletion-mode notes.
21. Build, push, helm upgrade, rollout, agent + human verification per
    `AGENTS.local.md` §4–§6.
