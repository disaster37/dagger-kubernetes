package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type userRow struct {
	ID            string         `json:"id"`
	Username      string         `json:"username"`
	Role          domain.Role    `json:"role"`
	OAuthProvider string         `json:"oauth_provider,omitempty"`
	Groups        []groupSummary `json:"groups"`
	CreatedAt     string         `json:"created_at"`
	Token         *tokenMeta     `json:"token,omitempty"`
}

type tokenMeta struct {
	ID         string  `json:"id"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
}

type createUserRequest struct {
	Username string      `json:"username"`
	Password string      `json:"password"`
	Role     domain.Role `json:"role"`
}

type updateUserRequest struct {
	Role domain.Role `json:"role"`
}

type resetPasswordRequest struct {
	Password string `json:"password"`
}

type setUserGroupsRequest struct {
	GroupIDs []string `json:"group_ids"`
}

// All handlers below are registered through adminOnly, which enforces the
// admin role before they run.

// handleUsersList returns all users with their groups and masked token metadata.
func (s *Server) handleUsersList(_ context.Context, c *app.RequestContext) {
	users, err := s.users.List(context.Background())
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, s.toUserRow(context.Background(), u))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleUserCreate creates a new user.
func (s *Server) handleUserCreate(_ context.Context, c *app.RequestContext) {
	var req createUserRequest
	if !decodeBody(c, &req) {
		return
	}
	u, err := s.users.Create(context.Background(), req.Username, req.Password, req.Role)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, s.toUserRow(context.Background(), u))
}

// handleUserGet returns a single user.
func (s *Server) handleUserGet(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	u, err := s.users.Get(context.Background(), id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toUserRow(context.Background(), u))
}

// handleUserUpdate updates a user's role.
func (s *Server) handleUserUpdate(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req updateUserRequest
	if !decodeBody(c, &req) {
		return
	}
	u, err := s.users.UpdateRole(context.Background(), id, req.Role)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toUserRow(context.Background(), u))
}

// handleUserDelete removes a user. Self-delete is rejected with 409.
func (s *Server) handleUserDelete(_ context.Context, c *app.RequestContext) {
	targetID := c.Param("id")
	if id := identityOf(c); id != nil && id.UserID == targetID {
		writeError(c, consts.StatusConflict, "cannot delete self")
		return
	}
	if err := s.users.Delete(context.Background(), targetID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleUserResetPassword sets a new password for a user (admin-set).
func (s *Server) handleUserResetPassword(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req resetPasswordRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Password == "" {
		writeError(c, consts.StatusBadRequest, "password is required")
		return
	}
	if err := s.users.ResetPassword(context.Background(), id, req.Password); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleUserGroups replaces a user's group memberships.
func (s *Server) handleUserGroups(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req setUserGroupsRequest
	if !decodeBody(c, &req) {
		return
	}
	if err := s.groups.SetUserGroups(context.Background(), id, req.GroupIDs); err != nil {
		s.writeServiceError(c, err)
		return
	}
	// Return the new memberships as group summaries.
	gs, _ := s.groups.GroupsForUser(context.Background(), id)
	c.JSON(consts.StatusOK, toGroupSummaries(gs))
}

// handleUserTokenMeta returns masked metadata for a user's API token.
func (s *Server) handleUserTokenMeta(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	t, err := s.tokens.Meta(context.Background(), id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, toTokenMeta(t))
}

// handleUserTokenRevoke deletes a user's API token.
func (s *Server) handleUserTokenRevoke(_ context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if err := s.tokens.Revoke(context.Background(), id); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// toUserRow builds a userRow with joined groups + masked token metadata.
func (s *Server) toUserRow(ctx context.Context, u *domain.User) userRow {
	gs, _ := s.groups.GroupsForUser(ctx, u.ID)
	row := userRow{
		ID:            u.ID,
		Username:      u.Username,
		Role:          u.Role,
		OAuthProvider: u.OAuthProvider,
		CreatedAt:     formatTime(u.CreatedAt),
		Groups:        toGroupSummaries(gs),
	}
	if t, err := s.tokens.Meta(ctx, u.ID); err == nil {
		row.Token = toTokenMeta(t)
	}
	return row
}

// toGroupSummaries maps groups to their id/name summary form.
func toGroupSummaries(gs []*domain.Group) []groupSummary {
	out := make([]groupSummary, 0, len(gs))
	for _, g := range gs {
		out = append(out, groupSummary{ID: g.ID, Name: g.Name})
	}
	return out
}

func toTokenMeta(t *domain.APIToken) *tokenMeta {
	out := &tokenMeta{
		ID:        t.ID,
		Prefix:    t.Prefix,
		CreatedAt: formatTime(t.CreatedAt),
	}
	if t.LastUsedAt != nil {
		s := formatTime(*t.LastUsedAt)
		out.LastUsedAt = &s
	}
	return out
}
