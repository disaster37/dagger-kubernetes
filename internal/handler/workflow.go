package handler

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleOrgWorkflowsList lists the indexed workflow catalog.
func (s *Server) handleOrgWorkflowsList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeJSON(c, []*domain.WorkflowDef{})
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	defs, err := s.workflows.ListDefs(ctx, c.Param("org"), c.Query("project"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, defs)
}

type dispatchWorkflowRequest struct {
	ProjectID string         `json:"project_id"`
	Workflow  string         `json:"workflow"`
	Ref       string         `json:"ref"`
	Inputs    map[string]any `json:"inputs"`
}

// handleWorkflowDispatch manually triggers a workflow_dispatch run.
func (s *Server) handleWorkflowDispatch(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	var req dispatchWorkflowRequest
	if !decodeBody(c, &req) {
		return
	}
	run, err := s.workflows.Dispatch(ctx, c.Param("org"), req.ProjectID, req.Workflow, req.Ref, req.Inputs, id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, run)
}

// handleOrgWorkflowRunsList lists workflow runs for an org.
func (s *Server) handleOrgWorkflowRunsList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeJSON(c, []*domain.WorkflowRun{})
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	runs, err := s.workflows.ListRuns(ctx, &domain.WorkflowRunFilter{
		OrgID: c.Param("org"), ProjectID: c.Query("project"), Status: c.Query("status"), Limit: queryLimit(c),
	})
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, runs)
}

// handleWorkflowRunGet returns a workflow run by id, scoped to its org.
func (s *Server) handleWorkflowRunGet(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	run, err := s.workflows.GetRun(ctx, c.Param("runID"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	if !s.orgAllowed(ctx, id, run.OrgID) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	writeJSON(c, run)
}

// handleWorkflowApprove approves a waiting workflow job.
func (s *Server) handleWorkflowApprove(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	run, err := s.workflows.Approve(ctx, c.Param("runID"), c.Param("job"), id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, run)
}

type rejectWorkflowRequest struct {
	Reason string `json:"reason"`
}

// handleWorkflowReject rejects a waiting workflow job.
func (s *Server) handleWorkflowReject(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	var req rejectWorkflowRequest
	if !decodeBody(c, &req) {
		return
	}
	run, err := s.workflows.Reject(ctx, c.Param("runID"), c.Param("job"), id, req.Reason)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, run)
}

// handleArtifactsList lists artifacts for a run, scoped to the caller's org.
func (s *Server) handleArtifactsList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.artifacts == nil {
		writeJSON(c, []string{})
		return
	}
	org := c.Query("org")
	if !s.orgAllowed(ctx, id, org) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	keys, err := s.artifacts.List(ctx, org, c.Query("project"), c.Query("run"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, keys)
}

// handleArtifactDownload streams an artifact by key, scoped to the caller's
// org (CWE-862).
func (s *Server) handleArtifactDownload(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.artifacts == nil {
		writeError(c, consts.StatusNotFound, "artifacts disabled")
		return
	}
	key := c.Query("key")
	if !s.artifactKeyAllowed(ctx, id, key) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	data, err := s.artifacts.Get(ctx, key)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.Response.Header.Set("Content-Type", "application/octet-stream")
	c.SetStatusCode(consts.StatusOK)
	c.Response.SetBody(data)
}

// orgAllowed reports whether the caller may access resources scoped to org.
func (s *Server) orgAllowed(ctx context.Context, id *domain.Identity, org string) bool {
	if id == nil {
		return false
	}
	if id.Role == domain.RoleAdmin {
		return true
	}
	if org == "" {
		org = domain.DefaultOrgID
	}
	if id.OrgID != "" {
		return org == id.OrgID
	}
	if s.orgs != nil && id.UserID != "" {
		if orgs, err := s.orgs.OrgsForUser(ctx, id.UserID); err == nil {
			for _, o := range orgs {
				if o.Name == org || o.ID == org {
					return true
				}
			}
		}
	}
	return org == domain.DefaultOrgID
}

// artifactKeyAllowed reports whether the caller may read an artifact key.
// Keys are "artifacts/<org>/<project>/<run>/<name>"; the org segment must
// match the caller's org scope unless the caller is a global admin.
func (s *Server) artifactKeyAllowed(ctx context.Context, id *domain.Identity, key string) bool {
	if id == nil {
		return false
	}
	if id.Role == domain.RoleAdmin {
		return true
	}
	parts := strings.Split(key, "/")
	if len(parts) < 2 || parts[0] != "artifacts" {
		return false
	}
	return s.orgAllowed(ctx, id, parts[1])
}

type completeWorkflowJobRequest struct {
	Outputs   map[string]string `json:"outputs"`
	Artifacts []string          `json:"artifacts"`
	Error     string            `json:"error"`
}

// handleWorkflowJobComplete records a runner-reported job completion.
func (s *Server) handleWorkflowJobComplete(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.workflows == nil {
		writeError(c, consts.StatusNotFound, "workflows disabled")
		return
	}
	run, err := s.workflows.GetRun(ctx, c.Param("runID"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	if !s.orgAllowed(ctx, id, run.OrgID) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	var req completeWorkflowJobRequest
	if !decodeBody(c, &req) {
		return
	}
	updated, err := s.workflows.CompleteJob(ctx, run.ID, c.Param("job"), req.Outputs, req.Artifacts, req.Error)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, updated)
}

type artifactUploadRequest struct {
	Org       string `json:"org"`
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	Name      string `json:"name"`
	Data      []byte `json:"data"`
}

// handleArtifactUpload stores a workflow artifact (org member/admin).
func (s *Server) handleArtifactUpload(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.artifacts == nil {
		writeError(c, consts.StatusServiceUnavailable, "artifacts disabled")
		return
	}
	var req artifactUploadRequest
	if !decodeBody(c, &req) {
		return
	}
	if !s.orgAllowed(ctx, id, req.Org) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	key, err := s.artifacts.Put(ctx, req.Org, req.ProjectID, req.RunID, req.Name, req.Data)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	if s.workflows != nil {
		if err := s.workflows.OnArtifactPublished(ctx, req.Org, req.ProjectID, req.RunID, req.Name, key); err != nil {
			s.logger.WithError(err).Warn("artifact workflow trigger failed")
		}
	}
	c.JSON(consts.StatusCreated, map[string]string{"key": key})
}
