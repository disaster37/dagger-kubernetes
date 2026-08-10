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

// All handlers below are registered through adminOnly, which enforces the
// admin role before they run.

// handleGroupsList returns all groups with member counts and live quota usage.
func (s *Server) handleGroupsList(_ context.Context, c *app.RequestContext) {
	gs, err := s.groups.List(context.Background())
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	rows := make([]groupRow, 0, len(gs))
	for _, g := range gs {
		rows = append(rows, s.toGroupRow(context.Background(), g))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleGroupCreate creates a new group.
func (s *Server) handleGroupCreate(_ context.Context, c *app.RequestContext) {
	var req createGroupRequest
	if !decodeBody(c, &req) {
		return
	}
	g, err := s.groups.Create(context.Background(), toGroupInput(req))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, toGroupRow(g, 0, 0))
}

// handleGroupGet returns a single group.
func (s *Server) handleGroupGet(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	g, err := s.groups.Get(context.Background(), id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toGroupRow(context.Background(), g))
}

// handleGroupUpdate modifies a group.
func (s *Server) handleGroupUpdate(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req updateGroupRequest
	if !decodeBody(c, &req) {
		return
	}
	g, err := s.groups.Update(context.Background(), id, toGroupInput(req))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toGroupRow(context.Background(), g))
}

// handleGroupDelete removes a group.
func (s *Server) handleGroupDelete(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if err := s.groups.Delete(context.Background(), id); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleGroupMembers returns the users in a group.
func (s *Server) handleGroupMembers(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	members, err := s.groups.Members(context.Background(), id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	rows := make([]userRow, 0, len(members))
	for _, u := range members {
		rows = append(rows, s.toUserRow(context.Background(), u))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleGroupSetMembers replaces a group's membership.
func (s *Server) handleGroupSetMembers(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req setGroupMembersRequest
	if !decodeBody(c, &req) {
		return
	}
	if err := s.groups.SetMembers(context.Background(), id, req.UserIDs); err != nil {
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
