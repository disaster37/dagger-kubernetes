package domain

import "context"

// CacheMetricsClient is the slice of the VictoriaMetrics client the service
// layer needs (cache hit-rate + per-trace series deletion). Implemented by
// repository.MetricsClient.
type CacheMetricsClient interface {
	CacheHitRate(ctx context.Context) (hit, miss float64, err error)
	DeleteTraceSeries(ctx context.Context, traceID string) error
}
