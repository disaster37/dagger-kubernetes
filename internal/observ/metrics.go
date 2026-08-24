package observ

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus collectors used by the supervisor. It is
// constructed once via NewMetrics and injected into the components that
// observe metrics (Server, Manager), avoiding package-level global state.
type Metrics struct {
	EngineAcquireTotal            *prometheus.CounterVec
	EngineAcquireDuration         *prometheus.HistogramVec
	ActiveLeases                  prometheus.Gauge
	ActiveReplicas                *prometheus.GaugeVec
	OTelIngestTotal               *prometheus.CounterVec
	CacheSizeBytes                prometheus.Gauge
	CacheObjectCount              prometheus.Gauge
	CachePurgeTotal               prometheus.Counter
	GCRunTotal                    *prometheus.CounterVec
	HistoryPurgeTotal             prometheus.Counter
	HistoryGCRunTotal             *prometheus.CounterVec
	PipelineDisconnectFailedTotal *prometheus.CounterVec
	CLICacheTotal                 *prometheus.CounterVec
	CLIUpstreamFetchTotal         *prometheus.CounterVec
}

// NewMetrics builds the Metrics collectors and registers them on reg when
// reg is non-nil. Pass prometheus.DefaultRegisterer in production; pass nil
// (or a fresh registry) in tests to avoid double-registration panics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		EngineAcquireTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_engine_acquire_total",
			Help: "Total number of engine acquire requests",
		}, []string{"version", "status"}),

		EngineAcquireDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dagger_kubernetes_engine_acquire_duration_seconds",
			Help:    "Duration of engine acquire requests",
			Buckets: prometheus.DefBuckets,
		}, []string{"version"}),

		ActiveLeases: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dagger_kubernetes_active_leases",
			Help: "Number of active session leases",
		}),

		ActiveReplicas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dagger_kubernetes_active_replicas",
			Help: "Number of active engine replicas per version",
		}, []string{"version"}),

		OTelIngestTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_otel_ingest_total",
			Help: "Total OTLP ingest requests",
		}, []string{"signal", "status"}),

		CacheSizeBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dagger_kubernetes_cache_size_bytes",
			Help: "Total size of cache blobs observed in the OCI registry",
		}),

		CacheObjectCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dagger_kubernetes_cache_object_count",
			Help: "Total number of cache layers/blobs observed in the OCI registry",
		}),

		CachePurgeTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dagger_kubernetes_cache_purge_total",
			Help: "Total number of cache tags purged (manual + GC)",
		}),

		GCRunTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_gc_run_total",
			Help: "Total number of cache GC sweeper runs",
		}, []string{"status"}),

		HistoryPurgeTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dagger_kubernetes_history_purge_total",
			Help: "Total number of traces purged from history (manual + GC)",
		}),

		HistoryGCRunTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_history_gc_run_total",
			Help: "Total number of history GC sweeper runs",
		}, []string{"status"}),

		PipelineDisconnectFailedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_pipeline_disconnect_failed_total",
			Help: "Total number of pipelines marked failed by disconnect detection",
		}, []string{"source"}),

		CLICacheTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_cli_cache_total",
			Help: "Total number of Dagger CLI tarball cache resolutions by result",
		}, []string{"result"}),

		CLIUpstreamFetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dagger_kubernetes_cli_upstream_fetch_total",
			Help: "Total number of Dagger CLI upstream fetches by status",
		}, []string{"status"}),
	}

	if reg != nil {
		reg.MustRegister(
			m.EngineAcquireTotal,
			m.EngineAcquireDuration,
			m.ActiveLeases,
			m.ActiveReplicas,
			m.OTelIngestTotal,
			m.CacheSizeBytes,
			m.CacheObjectCount,
			m.CachePurgeTotal,
			m.GCRunTotal,
			m.HistoryPurgeTotal,
			m.HistoryGCRunTotal,
			m.PipelineDisconnectFailedTotal,
			m.CLICacheTotal,
			m.CLIUpstreamFetchTotal,
		)
	}

	return m
}
