package handler

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	raftconfig "github.com/disaster37/go-raft/config"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

type orgRequest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Features    []string `json:"features"`
}

type orgMemberRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

type environmentRequest struct {
	ApproverGroupIDs    []string `json:"approver_group_ids"`
	PreventSelfApproval bool     `json:"prevent_self_approval"`
	ApprovalTimeout     string   `json:"approval_timeout"`
}

// requireOrgAdmin allows a global admin or a direct org_admin of org.
func (s *Server) requireOrgAdmin(ctx context.Context, c *app.RequestContext, org string) bool {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return false
	}
	if id.IsAdmin() {
		return true
	}
	if s.orgs != nil && id.UserID != "" && s.orgs.IsOrgAdmin(ctx, org, id.UserID) {
		return true
	}
	writeError(c, consts.StatusForbidden, "forbidden")
	return false
}

// handleOrgsList lists organizations the caller can access (all for admins).
func (s *Server) handleOrgsList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.orgs == nil {
		writeJSON(c, []*domain.Org{})
		return
	}
	if id.Role == domain.RoleAdmin {
		orgs, err := s.orgs.List(ctx)
		if err != nil {
			s.writeServiceError(c, err)
			return
		}
		writeJSON(c, orgs)
		return
	}
	orgs, err := s.orgs.OrgsForUser(ctx, id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, orgs)
}

// handleOrgGet returns one organization by id or slug (member/admin only).
func (s *Server) handleOrgGet(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusNotFound, "org not found")
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	o, err := s.orgs.Get(ctx, c.Param("org"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, o)
}

// handleOrgCreate creates an organization (admin only).
func (s *Server) handleOrgCreate(ctx context.Context, c *app.RequestContext) {
	if !s.requireAdmin(ctx, c) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	var req orgRequest
	if !decodeBody(c, &req) {
		return
	}
	o, err := s.orgs.Create(ctx, service.OrgInput{Name: req.Name, DisplayName: req.DisplayName, Features: req.Features})
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, o)
}

// handleOrgUpdate updates display name/features (admin only).
func (s *Server) handleOrgUpdate(ctx context.Context, c *app.RequestContext) {
	if !s.requireAdmin(ctx, c) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	var req orgRequest
	if !decodeBody(c, &req) {
		return
	}
	o, err := s.orgs.Update(ctx, c.Param("org"), service.OrgInput{DisplayName: req.DisplayName, Features: req.Features})
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, o)
}

// handleOrgDelete deletes an organization (admin only).
func (s *Server) handleOrgDelete(ctx context.Context, c *app.RequestContext) {
	if !s.requireAdmin(ctx, c) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	if err := s.orgs.Delete(ctx, c.Param("org")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleOrgMembers lists direct org memberships (member/admin only).
func (s *Server) handleOrgMembers(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.orgs == nil {
		writeJSON(c, []*domain.OrgMembership{})
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	members, err := s.orgs.Members(ctx, c.Param("org"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, members)
}

// handleOrgSetMember adds/updates a direct org membership (admin only).
func (s *Server) handleOrgSetMember(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	var req orgMemberRequest
	if !decodeBody(c, &req) {
		return
	}
	if err := s.orgs.SetMember(ctx, c.Param("org"), req.UserID, domain.OrgRole(req.Role)); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleOrgRemoveMember removes a direct org membership (admin only).
func (s *Server) handleOrgRemoveMember(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	if err := s.orgs.RemoveMember(ctx, c.Param("org"), c.Param("userID")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleOrgTokenMeta returns the masked org token metadata (member/admin only).
func (s *Server) handleOrgTokenMeta(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusNotFound, "org not found")
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	t, err := s.orgs.TokenMeta(ctx, c.Param("org"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeJSON(c, domain.ConnectTokenMeta{Exists: false})
			return
		}
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, t)
}

// handleOrgTokenGenerate issues/rotates the org-scoped dag_<org>_ token.
func (s *Server) handleOrgTokenGenerate(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	pt, t, err := s.orgs.GenerateToken(ctx, c.Param("org"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, map[string]any{"token": pt, "meta": t})
}

// handleOrgTokenRevoke revokes the org token.
func (s *Server) handleOrgTokenRevoke(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	if err := s.orgs.RevokeToken(ctx, c.Param("org")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleEnvironmentsList lists protected environments (member/admin only).
func (s *Server) handleEnvironmentsList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.environments == nil {
		writeJSON(c, []*domain.Environment{})
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	envs, err := s.environments.List(ctx, c.Param("org"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, envs)
}

// handleEnvironmentPut creates/updates a protected environment (org admin only).
func (s *Server) handleEnvironmentPut(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.environments == nil {
		writeError(c, consts.StatusServiceUnavailable, "environment service unavailable")
		return
	}
	var req environmentRequest
	if !decodeBody(c, &req) {
		return
	}
	var timeout time.Duration
	if req.ApprovalTimeout != "" {
		d, err := raftconfig.ParseExtendedDuration(req.ApprovalTimeout)
		if err != nil {
			writeError(c, consts.StatusBadRequest, "invalid approval_timeout")
			return
		}
		timeout = d
	}
	env, err := s.environments.Upsert(ctx, c.Param("org"), c.Param("env"), service.EnvironmentInput{
		ApproverGroupIDs:    req.ApproverGroupIDs,
		PreventSelfApproval: req.PreventSelfApproval,
		ApprovalTimeout:     timeout,
	})
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, env)
}

// handleEnvironmentDelete deletes a protected environment (org admin only).
func (s *Server) handleEnvironmentDelete(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.environments == nil {
		writeError(c, consts.StatusServiceUnavailable, "environment service unavailable")
		return
	}
	if err := s.environments.Delete(ctx, c.Param("org"), c.Param("env")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleEnvSecretsList lists environment secret metadata (member/admin only).
func (s *Server) handleEnvSecretsList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.secrets == nil {
		writeJSON(c, []*domain.Secret{})
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	secrets, err := s.secrets.ListEnvironment(ctx, c.Param("org"), c.Param("env"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, secrets)
}

// handleEnvSecretPut stores an environment secret (org admin only).
func (s *Server) handleEnvSecretPut(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.secrets == nil {
		writeError(c, consts.StatusServiceUnavailable, "secret service unavailable")
		return
	}
	var req secretRequest
	if !decodeBody(c, &req) {
		return
	}
	sec, err := s.secrets.PutEnvironment(ctx, c.Param("org"), c.Param("env"), req.Name, req.Value)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, sec)
}

// handleEnvSecretDelete deletes an environment secret (org admin only).
func (s *Server) handleEnvSecretDelete(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, c.Param("org")) {
		return
	}
	if s.secrets == nil {
		writeError(c, consts.StatusServiceUnavailable, "secret service unavailable")
		return
	}
	if err := s.secrets.DeleteEnvironment(ctx, c.Param("org"), c.Param("env"), c.Param("name")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
