package domain

import "context"

// TraceSeriesDeleter deletes per-trace metric series from VictoriaMetrics.
// Implemented by repository.MetricsClient.
type TraceSeriesDeleter interface {
	DeleteTraceSeries(ctx context.Context, traceID string) error
}
