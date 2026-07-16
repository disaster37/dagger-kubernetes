package observ

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus collectors used by the supervisor. It is
// constructed once via NewMetrics and injected into the components that
// observe metrics (Server, Manager), avoiding package-level global state.
type Metrics struct {
	EngineAcquireTotal    *prometheus.CounterVec
	EngineAcquireDuration *prometheus.HistogramVec
	ActiveLeases          prometheus.Gauge
	ActiveReplicas        *prometheus.GaugeVec
	OTelIngestTotal       *prometheus.CounterVec
}

// NewMetrics builds the Metrics collectors and registers them on reg when
// reg is non-nil. Pass prometheus.DefaultRegisterer in production; pass nil
// (or a fresh registry) in tests to avoid double-registration panics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		EngineAcquireTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_cache_engine_acquire_total",
			Help: "Total number of engine acquire requests",
		}, []string{"version", "status"}),

		EngineAcquireDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dagger_cache_engine_acquire_duration_seconds",
			Help:    "Duration of engine acquire requests",
			Buckets: prometheus.DefBuckets,
		}, []string{"version"}),

		ActiveLeases: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dagger_cache_active_leases",
			Help: "Number of active session leases",
		}),

		ActiveReplicas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dagger_cache_active_replicas",
			Help: "Number of active engine replicas per version",
		}, []string{"version"}),

		OTelIngestTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_cache_otel_ingest_total",
			Help: "Total OTLP ingest requests",
		}, []string{"signal", "status"}),
	}

	if reg != nil {
		reg.MustRegister(
			m.EngineAcquireTotal,
			m.EngineAcquireDuration,
			m.ActiveLeases,
			m.ActiveReplicas,
			m.OTelIngestTotal,
		)
	}

	return m
}
