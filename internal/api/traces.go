package api

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/telemetry"
)

func (s *Server) handleTracesList(_ context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	traces := []map[string]string{
		{"trace_id": "example-1", "status": "success", "version": "v0.21.4"},
	}
	writeJSON(c, traces)
}

func (s *Server) handleTracesDetail(_ context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	traceID := c.Param("traceID")
	if traceID == "" {
		writeError(c, consts.StatusBadRequest, "missing trace ID")
		return
	}

	reconstructor := telemetry.NewSpanTreeReconstructor(s.cfg.TempoURL)
	trace, err := reconstructor.GetTrace(traceID)
	if err != nil {
		writeError(c, consts.StatusNotFound, "trace not found")
		return
	}

	writeJSON(c, trace)
}

func (s *Server) handleTracesLogs(_ context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	traceID := c.Param("traceID")
	if traceID == "" {
		writeError(c, consts.StatusBadRequest, "missing trace ID")
		return
	}

	s.queryAndWriteTraceLogs(traceID, c)
}

// handleTracesLive streams live span updates for a trace over Server-Sent
// Events using Hertz's native SSE writer.
func (s *Server) handleTracesLive(ctx context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	traceID := c.Param("traceID")
	if traceID == "" {
		writeError(c, consts.StatusBadRequest, "missing trace ID")
		return
	}

	c.SetStatusCode(consts.StatusOK)
	c.Response.Header.Set("Content-Type", "text/event-stream")
	c.Response.Header.Set("Cache-Control", "no-cache")
	c.Response.Header.Set("Connection", "keep-alive")

	client := telemetry.NewLiveClient(c, traceID)

	s.liveHub.Subscribe(traceID, client)

	<-ctx.Done()
	s.liveHub.Unsubscribe(traceID, client)
	<-client.Done()
}
