package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// fleetMetricsWindow is the rolling window served for a fleet version: there
// is no trace window on the Runners page, so metrics are queried over the
// last 15 minutes.
const fleetMetricsWindow = 15 * time.Minute

// PromQL templates for the aggregate engine storage usage. Template vars:
// {ns} namespace, {pod} engine StatefulSet name. container_fs_usage_bytes is
// the mounted device's used bytes (summed across devices + pods by
// QueryRange); container_fs_limit_bytes is the device capacity cAdvisor
// reports.
var (
	fleetStorageUsageQuery = `container_fs_usage_bytes{namespace="{ns}",pod=~"{pod}-.*",container="engine"}`
	fleetStorageLimitQuery = `container_fs_limit_bytes{namespace="{ns}",pod=~"{pod}-.*",container="engine"}`
)

// FleetMetricsService builds per-version PromQL for the engine fleet's cAdvisor
// metrics over a recent rolling window and runs it against the metrics
// backend. It reuses the same series/escaping as EngineMetricsService.
type FleetMetricsService struct {
	queryer      domain.MetricsQueryer
	namespace    string
	step         time.Duration
	storageBytes int64 // per-pod fallback capacity (0 = unknown)
	logger       *logrus.Logger
}

// NewFleetMetricsService constructs the service. step is the query_range
// resolution; values below one second are clamped. storageBytes is the
// configured per-pod PVC size used when cAdvisor reports no capacity (0 =
// unknown).
func NewFleetMetricsService(queryer domain.MetricsQueryer, namespace string, step time.Duration, storageBytes int64, logger *logrus.Logger) *FleetMetricsService {
	if step < time.Second {
		step = time.Second
	}
	return &FleetMetricsService{
		queryer:      queryer,
		namespace:    namespace,
		step:         step,
		storageBytes: storageBytes,
		logger:       logger,
	}
}

// FleetMetrics builds the scoped PromQL for the version's fleet over
// [now-15m, now] and runs them. version must pass safeVersionRe (else an
// empty-but-valid payload with a WARN). replicas is the current replica count,
// used only for the storage-capacity fallback. Per-query failures are logged
// and skipped; the last error is returned only when every metric query failed.
func (s *FleetMetricsService) FleetMetrics(ctx context.Context, version string, replicas int) (*domain.FleetMetrics, error) {
	result := &domain.FleetMetrics{
		Version:     version,
		StepSeconds: int64(s.step.Seconds()),
		Series:      []domain.MetricSeries{},
	}
	// Reject a version that could break out of the PromQL label matcher before
	// it reaches the query builder (CWE-89/CWE-943). An invalid version yields
	// an empty-but-valid payload rather than an error.
	if !safeVersionRe.MatchString(version) {
		s.logger.WithField("version", version).Warn("fleet metrics: rejecting unsafe engine version")
		return result, nil
	}
	if s.queryer == nil {
		return result, nil
	}

	end := time.Now()
	start := end.Add(-fleetMetricsWindow)
	result.StartTime = start
	result.EndTime = end

	ns := escapePromQLLabelValue(s.namespace)
	pod := escapePromQLLabelValue(domain.StsName(version))
	var lastErr error
	for _, q := range defaultMetricQueries {
		promql := strings.ReplaceAll(q.promql, "{ns}", ns)
		promql = strings.ReplaceAll(promql, "{pod}", pod)
		points, err := s.queryer.QueryRange(ctx, promql, start, end, s.step)
		if err != nil {
			lastErr = err
			s.logger.WithError(err).WithFields(logrus.Fields{
				"version": version,
				"metric":  q.name,
			}).Warn("fleet metrics query failed")
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
		return result, fmt.Errorf("query fleet metrics: %w", lastErr)
	}

	result.Storage = s.fleetStorage(ctx, start, end, version, ns, pod, replicas)
	return result, nil
}

// fleetStorage queries the aggregate used/capacity bytes for the fleet's
// engine pods over the same window as the metric series. It returns nil when
// neither is known.
func (s *FleetMetricsService) fleetStorage(ctx context.Context, start, end time.Time, version, ns, pod string, replicas int) *domain.FleetStorage {
	used, usedErr := s.queryLastPoint(ctx, fleetStorageUsageQuery, ns, pod, start, end, version)
	capacity, capErr := s.queryLastPoint(ctx, fleetStorageLimitQuery, ns, pod, start, end, version)
	if usedErr != nil && capErr != nil {
		return nil
	}
	if capacity <= 0 && s.storageBytes > 0 && replicas > 0 {
		capacity = s.storageBytes * int64(replicas)
	}
	if used <= 0 && capacity <= 0 {
		return nil
	}
	percent := -1.0
	if capacity > 0 {
		percent = clampPct(float64(used) / float64(capacity) * 100)
	}
	return &domain.FleetStorage{
		UsedBytes:     used,
		CapacityBytes: capacity,
		Percent:       percent,
	}
}

// queryLastPoint runs one PromQL range query over [start, end] and returns its
// last sample as whole bytes (0 when the query failed or returned no points).
func (s *FleetMetricsService) queryLastPoint(ctx context.Context, tpl, ns, pod string, start, end time.Time, version string) (int64, error) {
	promql := strings.ReplaceAll(tpl, "{ns}", ns)
	promql = strings.ReplaceAll(promql, "{pod}", pod)
	points, err := s.queryer.QueryRange(ctx, promql, start, end, s.step)
	if err != nil {
		s.logger.WithError(err).WithField("version", version).Warn("fleet storage query failed")
		return 0, err
	}
	return int64(lastPoint(points)), nil
}

// lastPoint returns the value of the newest sample (0 for an empty series).
func lastPoint(points []domain.MetricPoint) float64 {
	if len(points) == 0 {
		return 0
	}
	best := points[0]
	for _, p := range points[1:] {
		if p.T >= best.T {
			best = p
		}
	}
	return best.V
}

// clampPct bounds a used/capacity percentage to [0, 100].
func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
