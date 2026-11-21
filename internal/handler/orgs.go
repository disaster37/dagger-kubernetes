package handler

import (
	"context"
	"errors"
	"sort"
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
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type orgMemberGroupsRequest struct {
	GroupIDs []string `json:"group_ids"`
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
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	o, err := s.orgs.Get(ctx, s.canonicalOrgID(ctx, c.Param("org")))
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

// handleOrgUpdate updates display name/features (org admin or global admin).
func (s *Server) handleOrgUpdate(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req orgRequest
	if !decodeBody(c, &req) {
		return
	}
	display := req.DisplayName
	o, err := s.orgs.Update(ctx, org.ID, service.OrgUpdateInput{Name: req.Name, DisplayName: &display, Features: req.Features})
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
	force := c.Query("force") == "true"
	onCascade := func(ctx context.Context, orgID string) error {
		if s.fleetManager != nil {
			if err := s.fleetManager.DeleteOrgFleets(ctx, orgID); err != nil {
				s.logger.WithError(err).WithField("org", orgID).Warn("delete org fleets failed")
			}
		}
		if s.remoteCache != nil {
			if err := s.remoteCache.Purge(ctx, s.remoteCacheNamespaceFor(orgID)); err != nil {
				s.logger.WithError(err).WithField("org", orgID).Warn("purge org remote cache failed")
			}
		}
		if s.metrics != nil {
			s.metrics.DeleteOrgSeries(orgID)
		}
		return nil
	}
	if err := s.orgs.Delete(ctx, s.canonicalOrgID(ctx, c.Param("org")), force, onCascade); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// orgMemberRow is the enriched view of a direct org membership.
type orgMemberRow struct {
	OrgID     string         `json:"org_id"`
	UserID    string         `json:"user_id"`
	Username  string         `json:"username"`
	Role      domain.OrgRole `json:"role"`
	Groups    []groupSummary `json:"groups"`
	CreatedAt time.Time      `json:"created_at"`
}

// orgMemberRow enriches one direct membership with the user's name and the
// groups it holds inside orgID. Lookups are best-effort: a missing user or a
// failed group read leaves the field empty instead of failing the listing.
func (s *Server) orgMemberRow(ctx context.Context, orgID, userID string, role domain.OrgRole, createdAt time.Time) orgMemberRow {
	row := orgMemberRow{
		OrgID:     orgID,
		UserID:    userID,
		Role:      role,
		Groups:    []groupSummary{},
		CreatedAt: createdAt,
	}
	if s.users != nil {
		if u, err := s.users.Get(ctx, userID); err != nil {
			s.logger.WithError(err).WithField("user_id", userID).Debug("org member username lookup failed")
		} else if u != nil {
			row.Username = u.Username
		}
	}
	if s.groups != nil {
		if gs, err := s.groups.GroupsForUserInOrg(ctx, orgID, userID); err != nil {
			s.logger.WithError(err).WithField("user_id", userID).WithField("org_id", orgID).Debug("org member groups lookup failed")
		} else {
			row.Groups = toGroupSummaries(gs)
		}
	}
	sort.SliceStable(row.Groups, func(i, j int) bool {
		if row.Groups[i].Name != row.Groups[j].Name {
			return row.Groups[i].Name < row.Groups[j].Name
		}
		return row.Groups[i].ID < row.Groups[j].ID
	})
	return row
}

// orgMemberRows enriches every direct membership of an org.
func (s *Server) orgMemberRows(ctx context.Context, orgID string, members []*domain.OrgMembership) []orgMemberRow {
	rows := make([]orgMemberRow, 0, len(members))
	for _, m := range members {
		if m == nil {
			continue
		}
		rows = append(rows, s.orgMemberRow(ctx, orgID, m.UserID, m.Role, m.CreatedAt))
	}
	return rows
}

// handleOrgMembers lists direct org memberships (member/admin only), enriched
// with usernames and org-scoped groups.
func (s *Server) handleOrgMembers(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	orgID := s.canonicalOrgID(ctx, c.Param("org"))
	if s.orgs == nil {
		writeJSON(c, []orgMemberRow{})
		return
	}
	if !s.orgAllowed(ctx, id, orgID) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	members, err := s.orgs.Members(ctx, orgID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, s.orgMemberRows(ctx, orgID, members))
}

// handleOrgSetMember adds/updates a direct org membership (admin only) and
// returns the resulting row.
func (s *Server) handleOrgSetMember(ctx context.Context, c *app.RequestContext) {
	orgID := s.canonicalOrgID(ctx, c.Param("org"))
	if !s.requireOrgAdmin(ctx, c, orgID) {
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
	userID := req.UserID
	if userID == "" && req.Username != "" {
		u, uErr := s.users.GetByUsername(ctx, req.Username)
		if uErr != nil {
			s.writeServiceError(c, uErr)
			return
		}
		userID = u.ID
	}
	if userID == "" {
		writeError(c, consts.StatusBadRequest, "user_id or username is required")
		return
	}
	role := domain.OrgRole(req.Role)
	if err := s.orgs.SetMember(ctx, orgID, userID, role); err != nil {
		s.writeServiceError(c, err)
		return
	}
	// Read the committed membership back so the response carries the recorded
	// creation time (which a role change preserves).
	var createdAt time.Time
	if members, err := s.orgs.Members(ctx, orgID); err == nil {
		for _, m := range members {
			if m != nil && m.UserID == userID {
				createdAt = m.CreatedAt
				break
			}
		}
	}
	writeJSON(c, s.orgMemberRow(ctx, orgID, userID, role, createdAt))
}

// handleOrgMemberGroups replaces a user's groups inside the org.
func (s *Server) handleOrgMemberGroups(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req orgMemberGroupsRequest
	if !decodeBody(c, &req) {
		return
	}
	if err := s.groups.SetUserGroupsInOrg(ctx, org.ID, c.Param("userID"), req.GroupIDs); err != nil {
		s.writeServiceError(c, err)
		return
	}
	gs, _ := s.groups.GroupsForUserInOrg(ctx, org.ID, c.Param("userID"))
	c.JSON(consts.StatusOK, toGroupSummaries(gs))
}

// handleOrgQuotasGet returns live usage against effective org quotas.
func (s *Server) handleOrgQuotasGet(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	usage, err := s.quota.UsageSnapshot(ctx, org.ID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, usage)
}

// handleOrgQuotasPut replaces an org's soft/hard quota limits (admin only),
// emitting an audit log entry.
func (s *Server) handleOrgQuotasPut(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	var q domain.OrgQuotas
	if !decodeBody(c, &q) {
		return
	}
	org, err := s.orgs.UpdateQuotas(ctx, id, s.canonicalOrgID(ctx, c.Param("org")), q)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	usage, err := s.quota.UsageSnapshot(ctx, org.ID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, usage)
}

// handleOrgRemoveMember removes a direct org membership (admin only).
func (s *Server) handleOrgRemoveMember(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	if err := s.orgs.RemoveMember(ctx, s.canonicalOrgID(ctx, c.Param("org")), c.Param("userID")); err != nil {
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
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	t, err := s.orgs.TokenMeta(ctx, s.canonicalOrgID(ctx, c.Param("org")))
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
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	pt, t, err := s.orgs.GenerateToken(ctx, s.canonicalOrgID(ctx, c.Param("org")))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, map[string]any{"token": pt, "meta": t})
}

// handleOrgTokenRevoke revokes the org token.
func (s *Server) handleOrgTokenRevoke(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
		return
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "org service unavailable")
		return
	}
	if err := s.orgs.RevokeToken(ctx, s.canonicalOrgID(ctx, c.Param("org"))); err != nil {
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
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	envs, err := s.environments.List(ctx, s.canonicalOrgID(ctx, c.Param("org")))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, envs)
}

// handleEnvironmentPut creates/updates a protected environment (org admin only).
func (s *Server) handleEnvironmentPut(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
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
	env, err := s.environments.Upsert(ctx, s.canonicalOrgID(ctx, c.Param("org")), c.Param("env"), service.EnvironmentInput{
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
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
		return
	}
	if s.environments == nil {
		writeError(c, consts.StatusServiceUnavailable, "environment service unavailable")
		return
	}
	if err := s.environments.Delete(ctx, s.canonicalOrgID(ctx, c.Param("org")), c.Param("env")); err != nil {
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
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	secrets, err := s.secrets.ListEnvironment(ctx, s.canonicalOrgID(ctx, c.Param("org")), c.Param("env"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, secrets)
}

// handleEnvSecretPut stores an environment secret (org admin only).
func (s *Server) handleEnvSecretPut(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
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
	sec, err := s.secrets.PutEnvironment(ctx, s.canonicalOrgID(ctx, c.Param("org")), c.Param("env"), req.Name, req.Value)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, sec)
}

// handleEnvSecretDelete deletes an environment secret (org admin only).
func (s *Server) handleEnvSecretDelete(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
		return
	}
	if s.secrets == nil {
		writeError(c, consts.StatusServiceUnavailable, "secret service unavailable")
		return
	}
	if err := s.secrets.DeleteEnvironment(ctx, s.canonicalOrgID(ctx, c.Param("org")), c.Param("env"), c.Param("name")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
