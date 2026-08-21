package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

const historyStatsTTL = 15 * time.Second

// HistoryPurgeService implements domain.HistoryStatsProvider + domain.HistoryPurger.
// It owns pipeline-history retention: trace metadata (Raft FSM) + Loki logs +
// VictoriaMetrics metrics. Tempo spans are NOT deleted here (no per-trace
// delete API) — they age out via Tempo retention.
//
// Ordering / partial-failure semantics: for each candidate, telemetry deletion
// (Loki + VM) is attempted first and is best-effort — a failure increments the
// telemetry error count and is logged but does NOT abort the trace_meta
// deletion. The trace_meta row is then deleted unconditionally (so the trace
// disappears from the UI). Rationale: orphaned telemetry ages out via Loki/VM
// retention; orphaned trace_meta would cause the sweeper to retry forever.
// Telemetry deletes are idempotent (deleting already-deleted data is a no-op),
// so a retry after a transient backend outage is safe.
type HistoryPurgeService struct {
	traceMeta  domain.TraceMetaRepository
	logs       domain.LogRepository      // Loki (may be nil)
	metrics    domain.CacheMetricsClient // VictoriaMetrics (may be nil)
	gcCfg      domain.HistoryGCConfig
	logger     *logrus.Logger
	metricsObs *observ.Metrics // may be nil

	mu       sync.Mutex
	cached   *domain.HistoryStats
	cachedAt time.Time

	purgeMu  sync.Mutex // serializes Purge / PurgeAll / RunGC
	gcMu     sync.Mutex // guards lastGC / lastGCAt / nextGCAt (short critical section)
	lastGC   *domain.HistoryGCRunSummary
	lastGCAt time.Time
	nextGCAt time.Time
}

func NewHistoryPurgeService(
	traceMeta domain.TraceMetaRepository,
	logs domain.LogRepository,
	metrics domain.CacheMetricsClient,
	gcCfg domain.HistoryGCConfig,
	logger *logrus.Logger,
	obs *observ.Metrics,
) *HistoryPurgeService {
	return &HistoryPurgeService{
		traceMeta:  traceMeta,
		logs:       logs,
		metrics:    metrics,
		gcCfg:      gcCfg,
		logger:     logger,
		metricsObs: obs,
	}
}

// telemetryDeleteCounts aggregates best-effort Loki + VictoriaMetrics delete
// outcomes for one trace.
type telemetryDeleteCounts struct {
	logsDeleted    int
	metricsDeleted int
	errors         int
}

// deleteTraceTelemetry best-effort deletes Loki logs + VictoriaMetrics series
// for traceID, returning the per-backend outcome counts. Failures are logged at
// warn level but never returned (callers must always proceed to the trace_meta
// delete).
func (s *HistoryPurgeService) deleteTraceTelemetry(ctx context.Context, traceID string) (tc telemetryDeleteCounts) {
	if s.logs != nil {
		if err := s.logs.DeleteTraceLogs(ctx, traceID); err != nil {
			s.logger.WithField("trace_id", traceID).WithError(err).Warn("loki log delete failed")
			tc.errors++
		} else {
			tc.logsDeleted++
		}
	}
	if s.metrics != nil {
		if err := s.metrics.DeleteTraceSeries(ctx, traceID); err != nil {
			s.logger.WithField("trace_id", traceID).WithError(err).Warn("victoria metrics delete failed")
			tc.errors++
		} else {
			tc.metricsDeleted++
		}
	}
	return tc
}

// purgeTrace deletes a trace's telemetry (best-effort) then its trace_meta row,
// bumping the purge counter on success. It returns the per-trace telemetry
// outcome and any trace_meta delete error.
func (s *HistoryPurgeService) purgeTrace(ctx context.Context, traceID string) (telemetryDeleteCounts, error) {
	tc := s.deleteTraceTelemetry(ctx, traceID)
	if err := s.traceMeta.Delete(ctx, traceID); err != nil {
		return tc, err
	}
	if s.metricsObs != nil {
		s.metricsObs.HistoryPurgeTotal.Inc()
	}
	return tc, nil
}

// Stats implements domain.HistoryStatsProvider. TTL-cached 15s.
func (s *HistoryPurgeService) Stats(ctx context.Context) (*domain.HistoryStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && time.Since(s.cachedAt) < historyStatsTTL {
		return s.cached, nil
	}

	count, oldest, err := s.traceMeta.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("trace stats: %w", err)
	}

	stats := &domain.HistoryStats{
		TraceCount:  count,
		CollectedAt: rfc3339(time.Now()),
		GC:          s.GCRules(),
	}
	if !oldest.IsZero() {
		stats.OldestUpdatedAt = rfc3339(oldest)
	}
	s.cached = stats
	s.cachedAt = time.Now()
	return stats, nil
}

// GCRules implements domain.HistoryStatsProvider.
func (s *HistoryPurgeService) GCRules() domain.HistoryGCRules {
	rules := domain.HistoryGCRules{
		Enabled:  s.gcCfg.Enabled,
		MaxAge:   s.gcCfg.MaxAge.String(),
		Schedule: s.gcCfg.Schedule.String(),
	}

	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if !s.lastGCAt.IsZero() {
		rules.LastRunAt = rfc3339(s.lastGCAt)
		rules.LastRunSummary = s.lastGC
	}
	if s.gcCfg.Enabled && !s.nextGCAt.IsZero() {
		rules.NextRunAt = rfc3339(s.nextGCAt)
	}
	return rules
}

// Purge implements domain.HistoryPurger. Single-trace admin override: it does
// NOT protect running traces. Telemetry is deleted first (idempotent), then
// the trace_meta row.
func (s *HistoryPurgeService) Purge(ctx context.Context, req domain.HistoryPurgeRequest) (*domain.HistoryPurgeResult, error) {
	if !domain.ValidTraceID(req.TraceID) {
		return nil, fmt.Errorf("%w: invalid trace_id", domain.ErrValidation)
	}

	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	result := &domain.HistoryPurgeResult{TraceIDs: []string{req.TraceID}}
	tc := s.deleteTraceTelemetry(ctx, req.TraceID)
	result.LogsDeleted += tc.logsDeleted
	result.MetricsDeleted += tc.metricsDeleted

	// Decide purged vs already_purged, then delete the row (idempotent).
	_, err := s.traceMeta.Get(ctx, req.TraceID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		result.AlreadyPurged = 1
	case err != nil:
		return nil, fmt.Errorf("trace get: %w", err)
	default:
		if err := s.traceMeta.Delete(ctx, req.TraceID); err != nil {
			return nil, fmt.Errorf("trace delete: %w", err)
		}
		result.PurgedTraces = 1
		if s.metricsObs != nil {
			s.metricsObs.HistoryPurgeTotal.Inc()
		}
	}

	s.invalidateCache()
	return result, nil
}

// PurgeAll implements domain.HistoryPurger. It purges every trace older than
// history.gc.max_age, protecting running traces.
func (s *HistoryPurgeService) PurgeAll(ctx context.Context) (*domain.HistoryPurgeResult, error) {
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	if s.gcCfg.MaxAge <= 0 {
		return &domain.HistoryPurgeResult{Message: "max_age must be positive; history purge is disabled"}, nil
	}

	cutoff := time.Now().Add(-s.gcCfg.MaxAge)
	candidates, err := s.traceMeta.ListBefore(ctx, cutoff, true)
	if err != nil {
		return nil, fmt.Errorf("list purge candidates: %w", err)
	}

	result := &domain.HistoryPurgeResult{}
	for _, m := range candidates {
		if ctx.Err() != nil {
			result.Message = "cancelled"
			break
		}
		tc, err := s.purgeTrace(ctx, m.TraceID)
		result.LogsDeleted += tc.logsDeleted
		result.MetricsDeleted += tc.metricsDeleted
		if err != nil {
			s.logger.WithField("trace_id", m.TraceID).WithError(err).Warn("trace_meta delete failed")
			continue
		}
		result.PurgedTraces++
		result.TraceIDs = append(result.TraceIDs, m.TraceID)
	}

	s.invalidateCache()
	return result, nil
}

// RunGC is the history sweeper entry point. It purges traces older than MaxAge
// that are not running.
func (s *HistoryPurgeService) RunGC(ctx context.Context) (*domain.HistoryGCRunSummary, error) {
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	summary := &domain.HistoryGCRunSummary{StartedAt: rfc3339(time.Now())}
	finish := func(msg string, runErr error) (*domain.HistoryGCRunSummary, error) {
		summary.FinishedAt = rfc3339(time.Now())
		summary.Message = msg
		s.recordGC(summary, runErr)
		return summary, runErr
	}

	if s.gcCfg.MaxAge <= 0 {
		return finish("max_age must be positive; history purge is disabled", nil)
	}

	cutoff := time.Now().Add(-s.gcCfg.MaxAge)
	candidates, err := s.traceMeta.ListBefore(ctx, cutoff, false)
	if err != nil {
		return finish("", fmt.Errorf("list purge candidates: %w", err))
	}

	cancelled := false
	for _, m := range candidates {
		if ctx.Err() != nil {
			cancelled = true
			break
		}
		if m.Status == "" || m.Status == "running" {
			summary.SkippedRunning++
			continue
		}
		tc, err := s.purgeTrace(ctx, m.TraceID)
		summary.LogsDeleted += tc.logsDeleted
		summary.MetricsDeleted += tc.metricsDeleted
		summary.TelemetryErrors += tc.errors
		if err != nil {
			summary.Errors++
			s.logger.WithField("trace_id", m.TraceID).WithError(err).Warn("trace_meta delete failed")
			continue
		}
		summary.PurgedTraces++
	}

	s.invalidateCache()

	msg := ""
	switch {
	case cancelled:
		msg = "cancelled"
	case len(candidates) == 0:
		msg = "no traces older than max_age"
	}
	return finish(msg, nil)
}

// recordGC stores the last run summary and bumps the GC run counter.
func (s *HistoryPurgeService) recordGC(summary *domain.HistoryGCRunSummary, runErr error) {
	s.gcMu.Lock()
	s.lastGC = summary
	s.lastGCAt = time.Now()
	s.nextGCAt = s.lastGCAt.Add(s.gcCfg.Schedule)
	s.gcMu.Unlock()

	if s.metricsObs != nil {
		status := "success"
		if runErr != nil {
			status = "error"
		}
		s.metricsObs.HistoryGCRunTotal.WithLabelValues(status).Inc()
	}
}

// invalidateCache clears the stats cache so the next Stats() re-probes.
func (s *HistoryPurgeService) invalidateCache() {
	s.mu.Lock()
	s.cached = nil
	s.cachedAt = time.Time{}
	s.mu.Unlock()
}

// StartGCSweeper launches the background ticker goroutine and returns a stop
// func. No-op when gcCfg.Enabled is false.
func (s *HistoryPurgeService) StartGCSweeper(ctx context.Context) (stop func()) {
	if !s.gcCfg.Enabled {
		return func() {}
	}
	if s.gcCfg.Schedule <= 0 {
		s.logger.Warn("history gc schedule is non-positive; sweeper disabled")
		return func() {}
	}
	ticker := time.NewTicker(s.gcCfg.Schedule)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if _, err := s.RunGC(ctx); err != nil {
					s.logger.WithError(err).Error("history gc run failed")
				}
			}
		}
	}()
	return func() { close(done) }
}
