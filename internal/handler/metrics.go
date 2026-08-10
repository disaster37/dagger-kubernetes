package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
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
			"cache":     "/api/v1/cache",
			"query":     "/api/v1/metrics/query?query=<promql>",
			"range":     "/api/v1/metrics/query_range?query=<promql>&start=<unix>&end=<unix>&step=<seconds>",
			"endpoints": []string{"/api/v1/metrics/query", "/api/v1/metrics/query_range"},
		})
		return
	}

	s.victoriaProxy.ServeHTTP(ctx, c)
}
