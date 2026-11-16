package handler

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
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
