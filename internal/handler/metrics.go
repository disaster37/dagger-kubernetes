package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleMetricsProxy reverse-proxies PromQL queries to VictoriaMetrics (when
// configured). With no VictoriaURL it returns a small help document describing
// the available query endpoints.
func (s *Server) handleMetricsProxy(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}

	if s.victoriaProxy == nil {
		writeJSON(c, map[string]interface{}{
			"fleet":     "/api/v1/fleet",
			"query":     "/api/v1/metrics/query?query=<promql>",
			"range":     "/api/v1/metrics/query_range?query=<promql>&start=<unix>&end=<unix>&step=<seconds>",
			"endpoints": []string{"/api/v1/metrics/query", "/api/v1/metrics/query_range"},
		})
		return
	}

	s.victoriaProxy.ServeHTTP(ctx, c)
}

// handleTraceMetrics serves engine resource metrics for a trace (auth-gated by
// the same visibility rules as the trace detail endpoint). The supervisor
// builds the scoped PromQL server-side; the UI never writes PromQL. A missing
// trace_meta yields an empty-but-valid payload (HTTP 200), never a 404.
func (s *Server) handleTraceMetrics(ctx context.Context, c *app.RequestContext) {
	traceID, ok := s.authorizeTraceRequest(c)
	if !ok {
		return
	}
	if s.engineMetrics == nil {
		writeError(c, consts.StatusNotImplemented, "metrics unavailable")
		return
	}

	var meta *domain.TraceMeta
	if s.traceMeta != nil {
		if m, err := s.traceMeta.Get(ctx, traceID); err == nil {
			meta = m
		}
	}

	tm, err := s.engineMetrics.TraceMetrics(ctx, meta)
	if err != nil {
		s.logger.WithError(err).WithField("trace_id", traceID).Warn("trace metrics unavailable")
		tm = &domain.TraceMetrics{TraceID: traceID, Series: []domain.MetricSeries{}}
	}
	if tm.TraceID == "" {
		tm.TraceID = traceID
	}
	writeJSON(c, tm)
}
