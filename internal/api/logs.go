package api

import (
	"context"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/telemetry"
)

// queryAndWriteTraceLogs queries Loki for a trace's logs and writes the JSON
// result. Shared by handleLogsRoutes and handleTracesLogs.
func (s *Server) queryAndWriteTraceLogs(traceID string, c *app.RequestContext) {
	logsClient := telemetry.NewLogsClient(s.cfg.LokiURL)
	end := time.Now()
	start := end.Add(-24 * time.Hour)

	entries, err := logsClient.QueryTraceLogs(traceID, start, end, 1000)
	if err != nil {
		writeError(c, consts.StatusNotFound, "logs not found")
		return
	}

	writeJSON(c, map[string]interface{}{
		"trace_id": traceID,
		"entries":  entries,
	})
}

func (s *Server) handleLogsRoutes(_ context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	traceID := strings.TrimSuffix(c.Param("traceID"), "/")
	if traceID == "" {
		writeError(c, consts.StatusBadRequest, "missing trace ID")
		return
	}

	s.queryAndWriteTraceLogs(traceID, c)
}
