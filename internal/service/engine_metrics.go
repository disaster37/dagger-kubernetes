package service

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// maxMetricsWindow bounds the query_range window derived from trace metadata
// so a malformed/absurd duration cannot trigger an unbounded VictoriaMetrics
// query (CWE-400).
const maxMetricsWindow = 24 * time.Hour

// safeVersionRe bounds the engine version interpolated into the PromQL pod
// selector. trace_meta.version is client-supplied via OTLP, so a hostile value
// must not be able to break out of the {pod=~"..."} matcher and inject
// arbitrary PromQL (CWE-89/CWE-943). Legitimate Dagger versions are
// semver-ish (v0.21.4, 0.21, v0.21.4-rc1).
var safeVersionRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// promQLLabelReplacer escapes the two characters that terminate a PromQL
// double-quoted label value. Applied to the namespace and StatefulSet name as
// defense-in-depth even though both are normally trusted/validated.
var promQLLabelReplacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// escapePromQLLabelValue escapes a value interpolated inside a PromQL
// double-quoted label matcher.
func escapePromQLLabelValue(v string) string {
	return promQLLabelReplacer.Replace(v)
}

// defaultMetricQueries are the cAdvisor container_* series surfaced for an
// engine pod. Template vars: {ns} namespace, {pod} engine StatefulSet name.
// Isolated here so the metric names/labels can be tuned after a live-cluster
// inspection without touching the query logic.
var defaultMetricQueries = []struct{ name, label, unit, promql string }{
	{"cpu", "CPU", "cores", `rate(container_cpu_usage_seconds_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[5m])`},
	{"memory", "Memory", "bytes", `container_memory_working_set_bytes{namespace="{ns}",pod=~"{pod}-.*",container="engine"}`},
	{"disk_read", "Disk read", "bytes/s", `rate(container_fs_reads_bytes_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[5m])`},
	{"disk_write", "Disk write", "bytes/s", `rate(container_fs_writes_bytes_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[5m])`},
	{"net_rx", "Network rx", "bytes/s", `rate(container_network_receive_bytes_total{namespace="{ns}",pod=~"{pod}-.*"}[5m])`},
	{"net_tx", "Network tx", "bytes/s", `rate(container_network_transmit_bytes_total{namespace="{ns}",pod=~"{pod}-.*"}[5m])`},
}

// EngineMetricsService builds trace-scoped PromQL for the engine pod's
// cAdvisor metrics and runs it against the metrics backend. The UI never
// writes PromQL; it consumes the curated series this service returns.
type EngineMetricsService struct {
	queryer   domain.MetricsQueryer
	namespace string
	step      time.Duration
	logger    *logrus.Logger
}

// NewEngineMetricsService constructs the service. step is the query_range
// resolution; values below one second are clamped.
func NewEngineMetricsService(queryer domain.MetricsQueryer, namespace string, step time.Duration, logger *logrus.Logger) *EngineMetricsService {
	if step < time.Second {
		step = time.Second
	}
	return &EngineMetricsService{
		queryer:   queryer,
		namespace: namespace,
		step:      step,
		logger:    logger,
	}
}

// TraceMetrics builds the scoped PromQL for the trace's engine + time window
// and runs them. meta may be nil (unknown trace) — then it returns an
// empty-but-valid TraceMetrics rather than erroring. Per-query failures are
// logged and skipped so a single missing metric does not blank the card; when
// every query fails the last error is returned so the handler can log it.
func (s *EngineMetricsService) TraceMetrics(ctx context.Context, meta *domain.TraceMeta) (*domain.TraceMetrics, error) {
	result := &domain.TraceMetrics{
		StepSeconds: int64(s.step.Seconds()),
		Series:      []domain.MetricSeries{},
	}
	if meta == nil {
		return result, nil
	}
	result.TraceID = meta.TraceID

	if s.queryer == nil || meta.Version == "" {
		return result, nil
	}
	// Reject a version that could break out of the PromQL label matcher before
	// it reaches the query builder (CWE-89/CWE-943). An invalid version yields
	// an empty-but-valid payload rather than an error.
	if !safeVersionRe.MatchString(meta.Version) {
		s.logger.WithFields(logrus.Fields{
			"trace_id": meta.TraceID,
			"version":  meta.Version,
		}).Warn("engine metrics: rejecting unsafe engine version")
		return result, nil
	}

	start, end := metricsWindow(meta, time.Now())
	result.StartTime = start
	result.EndTime = end

	stsName := domain.StsName(meta.Version)
	ns := escapePromQLLabelValue(s.namespace)
	pod := escapePromQLLabelValue(stsName)
	var lastErr error
	for _, q := range defaultMetricQueries {
		promql := strings.ReplaceAll(q.promql, "{ns}", ns)
		promql = strings.ReplaceAll(promql, "{pod}", pod)
		points, err := s.queryer.QueryRange(ctx, promql, start, end, s.step)
		if err != nil {
			lastErr = err
			s.logger.WithError(err).WithFields(logrus.Fields{
				"trace_id": meta.TraceID,
				"metric":   q.name,
			}).Warn("engine metrics query failed")
			continue
		}
		result.Series = append(result.Series, domain.MetricSeries{
			Name:   q.name,
			Label:  q.label,
			Unit:   q.unit,
			Points: points,
		})
	}
	if len(result.Series) == 0 && lastErr != nil {
		return result, fmt.Errorf("query engine metrics: %w", lastErr)
	}
	return result, nil
}

// metricsWindow derives the query window from trace metadata: [started,
// started+duration] for a finished trace, [started, now] while running, and
// the last 24h when the start time is unknown. The window is always bounded to
// maxMetricsWindow.
func metricsWindow(meta *domain.TraceMeta, now time.Time) (start, end time.Time) {
	start = meta.StartedAt
	if start.IsZero() {
		start = now.Add(-maxMetricsWindow)
	}
	end = start.Add(time.Duration(meta.DurationMS) * time.Millisecond)
	if meta.DurationMS <= 0 || end.After(now) {
		end = now
	}
	if maxEnd := start.Add(maxMetricsWindow); end.After(maxEnd) {
		end = maxEnd
	}
	if !end.After(start) {
		end = start.Add(time.Minute)
	}
	return start, end
}
