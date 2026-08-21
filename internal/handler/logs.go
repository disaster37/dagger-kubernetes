package handler

import (
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// queryAndWriteTraceLogs queries Loki for a trace's logs and writes the JSON
// result. Shared by handleTracesLogs.
func (s *Server) queryAndWriteTraceLogs(traceID string, c *app.RequestContext) {
	end := time.Now()
	start := end.Add(-24 * time.Hour)

	entries, err := s.logs.QueryTraceLogs(traceID, start, end, 1000)
	if err != nil {
		writeError(c, consts.StatusNotFound, "logs not found")
		return
	}

	writeJSON(c, map[string]interface{}{
		"trace_id": traceID,
		"entries":  entries,
	})
}
