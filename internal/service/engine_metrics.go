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
// engine pod. Template vars: {ns} namespace, {pod} engine StatefulSet name,
// {rate} rate() lookback. Isolated here so the metric names/labels can be
// tuned after a live-cluster inspection without touching the query logic.
var defaultMetricQueries = []struct{ name, label, unit, promql string }{
	{"cpu", "CPU", "cores", `rate(container_cpu_usage_seconds_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[{rate}])`},
	{"memory", "Memory", "bytes", `container_memory_working_set_bytes{namespace="{ns}",pod=~"{pod}-.*",container="engine"}`},
	{"disk_read", "Disk read", "bytes/s", `rate(container_fs_reads_bytes_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[{rate}])`},
	{"disk_write", "Disk write", "bytes/s", `rate(container_fs_writes_bytes_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[{rate}])`},
	{"net_rx", "Network rx", "bytes/s", `rate(container_network_receive_bytes_total{namespace="{ns}",pod=~"{pod}-.*"}[{rate}])`},
	{"net_tx", "Network tx", "bytes/s", `rate(container_network_transmit_bytes_total{namespace="{ns}",pod=~"{pod}-.*"}[{rate}])`},
}

// buildPromQL substitutes {ns}/{pod}/{rate} into a metric query template.
func buildPromQL(tpl, ns, pod, rate string) string {
	promql := strings.ReplaceAll(tpl, "{ns}", ns)
	promql = strings.ReplaceAll(promql, "{pod}", pod)
	return strings.ReplaceAll(promql, "{rate}", rate)
}

// recordedMetricPrefix names the samples the recorder writes, distinct from
// container_* so fleet queries never double-count them.
const recordedMetricPrefix = "dagger_engine_"

// recordedMetricName maps a curated series name to its recorded metric name.
func recordedMetricName(name string) string { return recordedMetricPrefix + name }

// EngineMetricsService builds trace-scoped PromQL for the engine pod's
// cAdvisor metrics and runs it against the metrics backend. The UI never
// writes PromQL; it consumes the curated series this service returns.
type EngineMetricsService struct {
	queryer    domain.MetricsQueryer
	namespace  string
	step       time.Duration
	rateWindow time.Duration
	logger     *logrus.Logger
}

// NewEngineMetricsService constructs the service. step is the query_range
// resolution and rateWindow the rate() lookback; values below one second are
// clamped.
func NewEngineMetricsService(queryer domain.MetricsQueryer, namespace string, step, rateWindow time.Duration, logger *logrus.Logger) *EngineMetricsService {
	if step < time.Second {
		step = time.Second
	}
	if rateWindow < time.Second {
		rateWindow = time.Second
	}
	return &EngineMetricsService{
		queryer:    queryer,
		namespace:  namespace,
		step:       step,
		rateWindow: rateWindow,
		logger:     logger,
	}
}

// rateString renders the rate window as a PromQL duration, e.g. "60s".
func (s *EngineMetricsService) rateString() string {
	return fmt.Sprintf("%ds", int64(s.rateWindow.Seconds()))
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

	// Prefer samples the recorder persisted during the run: they survive engine
	// scale-down and short trace windows. Fall back to the live cAdvisor query
	// when nothing was recorded.
	if series, ok := s.recordedMetrics(ctx, meta.TraceID, start, end); ok {
		result.Series = series
		return result, nil
	}

	stsName := domain.StsName(meta.Version)
	ns := escapePromQLLabelValue(s.namespace)
	pod := escapePromQLLabelValue(stsName)
	var lastErr error
	for _, q := range defaultMetricQueries {
		promql := buildPromQL(q.promql, ns, pod, s.rateString())
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

// recordedMetrics reads dagger_engine_*{trace_id="..."} over [start,end]. Returns
// the curated series and ok=true when at least one recorded series has a point.
func (s *EngineMetricsService) recordedMetrics(ctx context.Context, traceID string, start, end time.Time) ([]domain.MetricSeries, bool) {
	if !domain.ValidTraceID(traceID) {
		return nil, false
	}
	label := escapePromQLLabelValue(traceID)
	series := make([]domain.MetricSeries, 0, len(defaultMetricQueries))
	found := false
	for _, q := range defaultMetricQueries {
		promql := fmt.Sprintf(`%s{trace_id="%s"}`, recordedMetricName(q.name), label)
		points, err := s.queryer.QueryRange(ctx, promql, start, end, s.step)
		if err != nil {
			s.logger.WithError(err).WithFields(logrus.Fields{
				"trace_id": traceID,
				"metric":   q.name,
			}).Warn("recorded engine metrics query failed")
			continue
		}
		if len(points) > 0 {
			found = true
		}
		series = append(series, domain.MetricSeries{
			Name:   q.name,
			Label:  q.label,
			Unit:   q.unit,
			Points: points,
		})
	}
	if !found {
		return nil, false
	}
	return series, true
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
