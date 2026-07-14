package observ

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	EngineAcquireTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dagger_cache_engine_acquire_total",
		Help: "Total number of engine acquire requests",
	}, []string{"version", "status"})

	EngineAcquireDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "dagger_cache_engine_acquire_duration_seconds",
		Help:    "Duration of engine acquire requests",
		Buckets: prometheus.DefBuckets,
	}, []string{"version"})

	ActiveLeases = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "dagger_cache_active_leases",
		Help: "Number of active session leases",
	})

	ActiveReplicas = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dagger_cache_active_replicas",
		Help: "Number of active engine replicas per version",
	}, []string{"version"})

	OTelIngestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dagger_cache_otel_ingest_total",
		Help: "Total OTLP ingest requests",
	}, []string{"signal", "status"})
)
