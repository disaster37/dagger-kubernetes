package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

type groupRow struct {
	ID                string `json:"id"`
	OrgID             string `json:"org_id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	MaxRunnerSessions int    `json:"max_runner_sessions"`
	AgentAvailable    bool   `json:"agent_available"`
	AutoAssignPattern string `json:"auto_assign_pattern"`
	MemberCount       int    `json:"member_count"`
	ActiveSessions    int    `json:"active_sessions"`
	CreatedAt         string `json:"created_at"`
}

type createGroupRequest struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	MaxRunnerSessions int    `json:"max_runner_sessions"`
	AgentAvailable    bool   `json:"agent_available"`
	AutoAssignPattern string `json:"auto_assign_pattern"`
}

type updateGroupRequest = createGroupRequest

type setGroupMembersRequest struct {
	UserIDs []string `json:"user_ids"`
}

// handleGroupsList returns the request org's groups.
func (s *Server) handleGroupsList(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	gs, err := s.groups.ListByOrg(ctx, org.ID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	rows := make([]groupRow, 0, len(gs))
	for _, g := range gs {
		rows = append(rows, s.toGroupRow(ctx, g))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleGroupCreate creates a group inside the request org.
func (s *Server) handleGroupCreate(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req createGroupRequest
	if !decodeBody(c, &req) {
		return
	}
	if warning, err := s.quota.CheckGroupCreate(ctx, id, org.ID); err != nil {
		s.writeServiceError(c, err)
		return
	} else {
		setQuotaWarning(c, warning)
	}
	g, err := s.groups.Create(ctx, org.ID, toGroupInput(req))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, s.toGroupRow(ctx, g))
}

// handleGroupGet returns a single group.
func (s *Server) handleGroupGet(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	g, err := s.groups.GetInOrg(ctx, org.ID, c.Param("id"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toGroupRow(ctx, g))
}

// handleGroupUpdate modifies a group.
func (s *Server) handleGroupUpdate(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req updateGroupRequest
	if !decodeBody(c, &req) {
		return
	}
	g, err := s.groups.Update(ctx, org.ID, c.Param("id"), toGroupInput(req))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toGroupRow(ctx, g))
}

// handleGroupDelete removes a group.
func (s *Server) handleGroupDelete(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if err := s.groups.Delete(ctx, org.ID, c.Param("id")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleGroupMembers returns the users in a group.
func (s *Server) handleGroupMembers(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	if _, err := s.groups.GetInOrg(ctx, org.ID, c.Param("id")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	members, err := s.groups.Members(ctx, c.Param("id"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	rows := make([]userRow, 0, len(members))
	for _, u := range members {
		rows = append(rows, s.toUserRow(ctx, u))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleGroupSetMembers replaces a group's membership.
func (s *Server) handleGroupSetMembers(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req setGroupMembersRequest
	if !decodeBody(c, &req) {
		return
	}
	if warning, err := s.quota.CheckMemberAdd(ctx, id, org.ID, int64(len(req.UserIDs))); err != nil {
		s.writeServiceError(c, err)
		return
	} else {
		setQuotaWarning(c, warning)
	}
	if err := s.groups.SetMembers(ctx, org.ID, c.Param("id"), req.UserIDs); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// toGroupRow enriches a group with its live member count and quota usage.
func (s *Server) toGroupRow(ctx context.Context, g *domain.Group) groupRow {
	members, _ := s.groups.Members(ctx, g.ID)
	usage, _ := s.quota.UsageByGroup(ctx)
	return toGroupRow(g, len(members), usage[g.ID])
}

func toGroupInput(req createGroupRequest) service.GroupInput {
	return service.GroupInput{
		Name:              req.Name,
		Description:       req.Description,
		MaxRunnerSessions: req.MaxRunnerSessions,
		AgentAvailable:    req.AgentAvailable,
		AutoAssignPattern: req.AutoAssignPattern,
	}
}

func toGroupRow(g *domain.Group, memberCount, activeSessions int) groupRow {
	return groupRow{
		ID:                g.ID,
		OrgID:             g.OrgID,
		Name:              g.Name,
		Description:       g.Description,
		MaxRunnerSessions: g.MaxRunnerSessions,
		AgentAvailable:    g.AgentAvailable,
		AutoAssignPattern: g.AutoAssignPattern,
		MemberCount:       memberCount,
		ActiveSessions:    activeSessions,
		CreatedAt:         formatTime(g.CreatedAt),
	}
}

// setQuotaWarning emits the soft-quota warning header.
func setQuotaWarning(c *app.RequestContext, warning string) {
	if warning != "" {
		c.Response.Header.Set("X-Dagger-Quota-Warning", warning)
	}
}
