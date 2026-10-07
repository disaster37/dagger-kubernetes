package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TraceMetricsRecorder samples engine metrics for running traces and writes them into
// VictoriaMetrics tagged with trace_id, so the pipeline view can show the run's metrics
// even after the engine pod has scaled down or the run has finished. Leader-only to
// avoid duplicate samples across replicas.
type TraceMetricsRecorder struct {
	sampler    domain.TraceMetricsRecorderStore
	traceMeta  domain.TraceMetaRepository
	namespace  string
	interval   time.Duration
	rateWindow time.Duration
	maxWindow  time.Duration
	isLeader   func() bool
	logger     *logrus.Logger
}

// NewTraceMetricsRecorder constructs the recorder. interval is the sampling
// tick, rateWindow the rate() lookback, and maxWindow the per-trace recording
// cap. A nil isLeader is treated as "never leader".
func NewTraceMetricsRecorder(sampler domain.TraceMetricsRecorderStore, traceMeta domain.TraceMetaRepository,
	namespace string, interval, rateWindow, maxWindow time.Duration, isLeader func() bool, logger *logrus.Logger) *TraceMetricsRecorder {
	if isLeader == nil {
		isLeader = func() bool { return false }
	}
	return &TraceMetricsRecorder{
		sampler:    sampler,
		traceMeta:  traceMeta,
		namespace:  namespace,
		interval:   interval,
		rateWindow: rateWindow,
		maxWindow:  maxWindow,
		isLeader:   isLeader,
		logger:     logger,
	}
}

// rateString renders the rate window as a PromQL duration, e.g. "60s".
func (r *TraceMetricsRecorder) rateString() string {
	return fmt.Sprintf("%ds", int64(r.rateWindow.Seconds()))
}

// Run samples all running traces once. No-op on non-leaders.
func (r *TraceMetricsRecorder) Run(ctx context.Context) {
	if !r.isLeader() {
		return
	}
	traces, err := r.traceMeta.ListRunning(ctx)
	if err != nil {
		r.logger.WithError(err).Warn("trace metrics recorder: list running traces failed")
		return
	}
	now := time.Now()
	ns := escapePromQLLabelValue(r.namespace)
	for _, m := range traces {
		// A non-hex trace id cannot be cleaned by DeleteTraceSeries; an empty or
		// unsafe version cannot be interpolated into the pod selector; a zero
		// start time cannot be bounded by maxWindow.
		if !domain.ValidTraceID(m.TraceID) || m.Version == "" || !safeVersionRe.MatchString(m.Version) || m.StartedAt.IsZero() {
			continue
		}
		if now.Sub(m.StartedAt) > r.maxWindow {
			continue
		}
		pod := escapePromQLLabelValue(domain.StsName(m.Version))
		for _, q := range defaultMetricQueries {
			promql := buildPromQL(q.promql, ns, pod, r.rateString())
			val, ok, err := r.sampler.QueryInstant(ctx, promql, now)
			if err != nil {
				r.logger.WithError(err).WithFields(logrus.Fields{
					"trace_id": m.TraceID,
					"metric":   q.name,
				}).Warn("trace metrics recorder: query failed")
				continue
			}
			if !ok {
				continue
			}
			if err := r.sampler.WriteSamples(ctx, recordedMetricName(q.name), map[string]string{
				"trace_id": m.TraceID,
				"version":  m.Version,
			}, []domain.MetricPoint{{T: now.Unix(), V: val}}); err != nil {
				r.logger.WithError(err).WithFields(logrus.Fields{
					"trace_id": m.TraceID,
					"metric":   q.name,
				}).Warn("trace metrics recorder: write failed")
			}
		}
	}
}

// Start launches the background ticker; returns a stop func (safe to call multiple times).
func (r *TraceMetricsRecorder) Start(ctx context.Context) (stop func()) {
	if r.interval <= 0 {
		return func() {}
	}
	ticker := time.NewTicker(r.interval)
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				r.Run(ctx)
			}
		}
	}()
	return func() {
		stopOnce.Do(func() { close(done) })
	}
}
