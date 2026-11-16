package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// handleWorkflowRunGet returns a workflow run by id.
func (s *Server) handleWorkflowRunGet(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(ctx, c) {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	run, ok := s.workflows.Get(c.Param("runID"))
	if !ok {
		writeError(c, consts.StatusNotFound, "workflow run not found")
		return
	}
	writeJSON(c, run)
}

// handleWorkflowApprove approves a waiting workflow job.
func (s *Server) handleWorkflowApprove(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireAdmin(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	if err := s.workflows.Approve(ctx, c.Param("runID"), c.Param("job"), id.UserID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleArtifactsList lists artifacts for a run.
func (s *Server) handleArtifactsList(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(ctx, c) {
		return
	}
	if s.artifacts == nil {
		writeJSON(c, []string{})
		return
	}
	keys, err := s.artifacts.List(ctx, c.Query("org"), c.Query("project"), c.Query("run"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, keys)
}

// handleArtifactDownload streams an artifact by key.
func (s *Server) handleArtifactDownload(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(ctx, c) {
		return
	}
	if s.artifacts == nil {
		writeError(c, consts.StatusNotFound, "artifacts disabled")
		return
	}
	data, err := s.artifacts.Get(ctx, c.Query("key"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.Response.Header.Set("Content-Type", "application/octet-stream")
	c.SetStatusCode(consts.StatusOK)
	c.Response.SetBody(data)
}
