package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type projectRow struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	Name      string `json:"name"`
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name,omitempty"`
	CreatedAt string `json:"created_at"`
}

type createProjectRequest struct {
	Name    string `json:"name"`
	GroupID string `json:"group_id"`
}

type updateProjectRequest struct {
	GroupID string `json:"group_id"`
}

// handleProjectsList returns the request org's projects.
func (s *Server) handleProjectsList(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	ps, err := s.projects.ListByOrg(ctx, org.ID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	groups, _ := s.groups.ListByOrg(ctx, org.ID)
	groupName := make(map[string]string, len(groups))
	for _, g := range groups {
		groupName[g.ID] = g.Name
	}
	rows := make([]projectRow, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, toProjectRow(p, groupName[p.GroupID]))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleProjectCreate creates a project inside the request org.
func (s *Server) handleProjectCreate(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req createProjectRequest
	if !decodeBody(c, &req) {
		return
	}
	if warning, err := s.quota.CheckProjectCreate(ctx, id, org.ID); err != nil {
		s.writeServiceError(c, err)
		return
	} else {
		setQuotaWarning(c, warning)
	}
	p, err := s.projects.Create(ctx, org.ID, req.Name, req.GroupID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, toProjectRow(p, ""))
}

// handleProjectUpdate assigns (or unassigns when group_id is empty) a project.
func (s *Server) handleProjectUpdate(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req updateProjectRequest
	if !decodeBody(c, &req) {
		return
	}
	p, err := s.projects.Assign(ctx, org.ID, c.Param("id"), req.GroupID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, toProjectRow(p, ""))
}

// handleProjectDelete removes a project.
func (s *Server) handleProjectDelete(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if err := s.projects.Delete(ctx, org.ID, c.Param("id")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

func toProjectRow(p *domain.Project, groupName string) projectRow {
	return projectRow{
		ID:        p.ID,
		OrgID:     p.OrgID,
		Name:      p.Name,
		GroupID:   p.GroupID,
		GroupName: groupName,
		CreatedAt: formatTime(p.CreatedAt),
	}
}
