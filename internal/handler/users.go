package handler

import (
	"context"
	"mime"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type userRow struct {
	ID            string         `json:"id"`
	Username      string         `json:"username"`
	Role          domain.Role    `json:"role"`
	OAuthProvider string         `json:"oauth_provider,omitempty"`
	OAuthGroups   []string       `json:"oauth_groups,omitempty"`
	Groups        []groupSummary `json:"groups"`
	CreatedAt     string         `json:"created_at"`
	DisabledAt    *string        `json:"disabled_at,omitempty"`
	DeletedAt     *string        `json:"deleted_at,omitempty"`
	DeactivatedAt *string        `json:"deactivated_at,omitempty"`
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

type userStateRequest struct {
	Action domain.UserStateAction `json:"action"`
}

// All handlers below are registered through adminOnly, which enforces the
// admin role before they run.

// handleUsersList returns all users with their groups and masked token metadata.
func (s *Server) handleUsersList(ctx context.Context, c *app.RequestContext) {
	users, err := s.users.List(ctx)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, s.toUserRow(ctx, u))
	}
	c.JSON(consts.StatusOK, rows)
}

// handleUserCreate creates a new user.
func (s *Server) handleUserCreate(ctx context.Context, c *app.RequestContext) {
	var req createUserRequest
	if !decodeBody(c, &req) {
		return
	}
	u, err := s.users.Create(ctx, req.Username, req.Password, req.Role)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, s.toUserRow(ctx, u))
}

// handleUserGet returns a single user.
func (s *Server) handleUserGet(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	u, err := s.users.Get(ctx, id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toUserRow(ctx, u))
}

// handleUserUpdate updates a user's role.
func (s *Server) handleUserUpdate(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req updateUserRequest
	if !decodeBody(c, &req) {
		return
	}
	u, err := s.users.UpdateRole(ctx, id, req.Role)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toUserRow(ctx, u))
}

// handleUserDelete soft-deletes a user. Self-delete is rejected with 409.
func (s *Server) handleUserDelete(ctx context.Context, c *app.RequestContext) {
	targetID := c.Param("id")
	actorID := ""
	if id := identityOf(c); id != nil {
		if id.UserID == targetID {
			writeError(c, consts.StatusConflict, "cannot delete self")
			return
		}
		actorID = id.UserID
	}
	if err := s.users.Delete(ctx, actorID, targetID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleUserState applies an administrator disable/enable/restore action.
func (s *Server) handleUserState(ctx context.Context, c *app.RequestContext) {
	if !requireJSONContentType(c) {
		return
	}
	id := c.Param("id")
	var req userStateRequest
	if !decodeBody(c, &req) {
		return
	}
	actorID := ""
	if ident := identityOf(c); ident != nil {
		actorID = ident.UserID
	}
	u, err := s.users.SetState(ctx, actorID, id, req.Action)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, s.toUserRow(ctx, u))
}

// handleUserResetPassword sets a new password for a user (admin-set).
func (s *Server) handleUserResetPassword(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req resetPasswordRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Password == "" {
		writeError(c, consts.StatusBadRequest, "password is required")
		return
	}
	if err := s.users.ResetPassword(ctx, id, req.Password); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleUserGroups replaces a user's group memberships.
func (s *Server) handleUserGroups(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var req setUserGroupsRequest
	if !decodeBody(c, &req) {
		return
	}
	if err := s.groups.SetUserGroups(ctx, id, req.GroupIDs); err != nil {
		s.writeServiceError(c, err)
		return
	}
	// Return the new memberships as group summaries.
	gs, _ := s.groups.GroupsForUser(ctx, id)
	c.JSON(consts.StatusOK, toGroupSummaries(gs))
}

// handleUserTokenMeta returns masked metadata for a user's API token.
func (s *Server) handleUserTokenMeta(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	t, err := s.tokens.Meta(ctx, id)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, toTokenMeta(t))
}

// handleUserTokenRevoke deletes a user's API token.
func (s *Server) handleUserTokenRevoke(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if err := s.tokens.Revoke(ctx, id); err != nil {
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
		OAuthGroups:   u.OAuthGroups,
		CreatedAt:     formatTime(u.CreatedAt),
		DisabledAt:    formatTimePtr(u.DisabledAt),
		DeletedAt:     formatTimePtr(u.DeletedAt),
		DeactivatedAt: formatTimePtr(u.DeactivatedAt),
		Groups:        toGroupSummaries(gs),
	}
	if t, err := s.tokens.Meta(ctx, u.ID); err == nil {
		row.Token = toTokenMeta(t)
	}
	return row
}

// formatTimePtr renders an optional timestamp in the API's canonical form.
func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

// requireJSONContentType rejects a mutating JSON request without an
// application/json content type (a charset suffix is allowed). Returns false
// after writing 415 when the header is missing or wrong.
func requireJSONContentType(c *app.RequestContext) bool {
	ct := string(c.GetHeader("Content-Type"))
	if ct == "" {
		writeError(c, consts.StatusUnsupportedMediaType, "content-type must be application/json")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		writeError(c, consts.StatusUnsupportedMediaType, "content-type must be application/json")
		return false
	}
	return true
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
