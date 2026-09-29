package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type projectRow struct {
	ID        string `json:"id"`
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

// All handlers below are registered through adminOnly, which enforces the
// admin role before they run.

// handleProjectsList returns all projects with their group names joined.
func (s *Server) handleProjectsList(ctx context.Context, c *app.RequestContext) {
	ps, err := s.projects.List(ctx)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	groups, _ := s.groups.List(ctx)
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

// handleProjectCreate creates a new project.
func (s *Server) handleProjectCreate(ctx context.Context, c *app.RequestContext) {
	var req createProjectRequest
	if !decodeBody(c, &req) {
		return
	}
	p, err := s.projects.Create(ctx, req.Name, req.GroupID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, toProjectRow(p, ""))
}

// handleProjectUpdate assigns (or unassigns when group_id is empty) a project.
func (s *Server) handleProjectUpdate(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req updateProjectRequest
	if !decodeBody(c, &req) {
		return
	}
	p, err := s.projects.Assign(ctx, id, req.GroupID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, toProjectRow(p, ""))
}

// handleProjectDelete removes a project.
func (s *Server) handleProjectDelete(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if err := s.projects.Delete(ctx, id); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

func toProjectRow(p *domain.Project, groupName string) projectRow {
	return projectRow{
		ID:        p.ID,
		Name:      p.Name,
		GroupID:   p.GroupID,
		GroupName: groupName,
		CreatedAt: formatTime(p.CreatedAt),
	}
}
