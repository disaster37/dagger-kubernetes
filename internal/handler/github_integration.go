package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type githubConnectionResponse struct {
	Connected         bool                         `json:"connected"`
	OAuthEnabled      bool                         `json:"oauth_enabled"`
	DeviceFlowEnabled bool                         `json:"device_flow_enabled"`
	Connection        *domain.UserGitHubConnection `json:"connection,omitempty"`
}

type githubOAuthCompleteRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

type githubPATRequest struct {
	Token   string `json:"token"`
	BaseURL string `json:"base_url"`
}

type githubDeviceCodeRequest struct {
	Org string `json:"org"`
}

type githubDevicePollRequest struct {
	SessionID string `json:"session_id"`
}

type githubOAuthStartRequest struct {
	RedirectURI string `json:"redirect_uri"`
}

type githubAccountCreateRequest struct {
	Owner   string `json:"owner"`
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

type githubRepoSettingPayload struct {
	Repo                     string `json:"repo"`
	RunChecksOnDefaultBranch *bool  `json:"run_checks_on_default_branch"`
	RunChecksOnPullRequest   *bool  `json:"run_checks_on_pull_request"`
	RunChecksOnBranchPush    *bool  `json:"run_checks_on_branch_push"`
	IsPublic                 *bool  `json:"is_public"`
}

type githubSourcesRequest struct {
	AccountID string                     `json:"account_id"`
	Repos     []githubRepoSettingPayload `json:"repos"`
}

type githubRepoSettingsRequest struct {
	RunChecksOnDefaultBranch *bool `json:"run_checks_on_default_branch"`
	RunChecksOnPullRequest   *bool `json:"run_checks_on_pull_request"`
	RunChecksOnBranchPush    *bool `json:"run_checks_on_branch_push"`
	IsPublic                 *bool `json:"is_public"`
}

func (s *Server) requireGitHubHuman(ctx context.Context, c *app.RequestContext) (*domain.Identity, bool) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return nil, false
	}
	if id.UserID == "" {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	return id, true
}

func (s *Server) handleGitHubConnectionGet(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	resp := githubConnectionResponse{
		OAuthEnabled:      s.githubIntegration.OAuthEnabled(),
		DeviceFlowEnabled: s.githubIntegration.DeviceFlowEnabled(),
	}
	conn, err := s.githubIntegration.GetUserConnection(ctx, id.UserID)
	if err == nil && conn.Status == domain.GitHubConnectionActive {
		resp.Connected = true
		resp.Connection = conn
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, resp)
}

func (s *Server) handleGitHubConnectionDelete(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	if err := s.githubIntegration.DisconnectUser(ctx, id.UserID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

func (s *Server) handleGitHubOAuthComplete(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	var req githubOAuthCompleteRequest
	if !decodeBody(c, &req) {
		return
	}
	result, err := s.githubIntegration.CompleteOAuth(ctx, id.UserID, id.IsAdmin(), req.Code, req.State)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, result)
}

func (s *Server) handleGitHubPAT(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	var req githubPATRequest
	if !decodeBody(c, &req) {
		return
	}
	conn, err := s.githubIntegration.ConnectUserPAT(ctx, id.UserID, req.BaseURL, req.Token)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	owners, err := s.githubIntegration.ListUserOwners(ctx, id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, map[string]any{"connection": conn, "available_owners": owners})
}

func (s *Server) handleGitHubDeviceCode(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	var req githubDeviceCodeRequest
	if !decodeBody(c, &req) {
		return
	}
	orgID := ""
	if req.Org != "" {
		org, err := s.orgs.Resolve(ctx, req.Org)
		if err != nil {
			s.writeServiceError(c, err)
			return
		}
		orgID = org.ID
	}
	result, err := s.githubIntegration.StartDeviceFlow(ctx, id.UserID, orgID)
	if err != nil {
		if errors.Is(err, domain.ErrGitHubDeviceFlowDisabled) || errors.Is(err, domain.ErrGitHubOAuthDisabled) {
			writeError(c, consts.StatusNotFound, "GitHub device flow is not configured on this deployment")
			return
		}
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, result)
}

func (s *Server) handleGitHubDevicePoll(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	var req githubDevicePollRequest
	if !decodeBody(c, &req) {
		return
	}
	result, err := s.githubIntegration.PollDeviceFlow(ctx, id.UserID, id.IsAdmin(), req.SessionID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrGitHubDeviceAuthorizationDelay):
			writeJSON(c, map[string]any{"status": "pending"})
			return
		case errors.Is(err, domain.ErrGitHubDeviceSlowDown):
			writeJSON(c, map[string]any{
				"status":   "slow_down",
				"interval": s.githubIntegration.DeviceSessionInterval(id.UserID, req.SessionID),
			})
			return
		default:
			s.writeServiceError(c, err)
			return
		}
	}
	writeJSON(c, map[string]any{"status": "complete", "result": result})
}

func (s *Server) handleGitHubOwners(ctx context.Context, c *app.RequestContext) {
	id, ok := s.requireGitHubHuman(ctx, c)
	if !ok {
		return
	}
	owners, err := s.githubIntegration.ListUserOwners(ctx, id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, map[string]any{"owners": owners})
}

func (s *Server) handleGitHubOrgOAuthStart(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req githubOAuthStartRequest
	if !decodeBody(c, &req) {
		return
	}
	url, err := s.githubIntegration.BuildOAuthURL(ctx, id.UserID, org.ID, req.RedirectURI)
	if err != nil {
		if errors.Is(err, domain.ErrGitHubOAuthDisabled) {
			writeError(c, consts.StatusNotFound, "GitHub OAuth app integration is not configured on this deployment")
			return
		}
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, map[string]string{"authorize_url": url})
}

func (s *Server) handleGitHubOrgAccountsList(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	accounts, err := s.githubIntegration.ListOrgAccounts(ctx, org.ID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, accounts)
}

func (s *Server) handleGitHubOrgAccountCreate(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req githubAccountCreateRequest
	if !decodeBody(c, &req) {
		return
	}
	acct, err := s.githubIntegration.ConnectOrgAccount(ctx, id.UserID, id.IsAdmin(), org.ID, req.Owner, req.BaseURL, req.Token)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, acct)
}

func (s *Server) handleGitHubOrgAccountDelete(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	cascade := strings.ToLower(c.Query("cascade")) != "false"
	if err := s.githubIntegration.DisconnectOrgAccount(ctx, id.UserID, id.IsAdmin(), org.ID, c.Param("accountID"), cascade); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

func (s *Server) handleGitHubOrgRepositories(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	repos, err := s.githubIntegration.ListOrgGitHubRepositories(ctx, id.UserID, id.IsAdmin(), org, c.Query("account_id"), c.Query("owner"), c.Query("q"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, map[string]any{"repositories": repos})
}

func (s *Server) handleGitHubOrgSources(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req githubSourcesRequest
	if !decodeBody(c, &req) {
		return
	}
	settings := make([]domain.RepoSettingMutation, 0, len(req.Repos))
	for _, repo := range req.Repos {
		mutation := domain.RepoSettingMutation{
			Repo: repo.Repo, IsPublic: repo.IsPublic,
			RunChecksOnDefaultBranch: repo.RunChecksOnDefaultBranch,
			RunChecksOnPullRequest:   repo.RunChecksOnPullRequest,
			RunChecksOnBranchPush:    repo.RunChecksOnBranchPush,
		}
		if req.AccountID != "" {
			accountID := req.AccountID
			mutation.SourceID = &accountID
		}
		settings = append(settings, mutation)
	}
	if err := s.githubIntegration.UpdateOrgRepoSettings(ctx, id.UserID, id.IsAdmin(), org, settings); err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, map[string]int{"updated": len(settings)})
}

func (s *Server) handleGitHubOrgRepoSettingsPut(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	var req githubRepoSettingsRequest
	if !decodeBody(c, &req) {
		return
	}
	repo := githubRepoFromPath(c)
	mutation := domain.RepoSettingMutation{
		Repo: repo, IsPublic: req.IsPublic,
		RunChecksOnDefaultBranch: req.RunChecksOnDefaultBranch,
		RunChecksOnPullRequest:   req.RunChecksOnPullRequest,
		RunChecksOnBranchPush:    req.RunChecksOnBranchPush,
	}
	if err := s.githubIntegration.UpdateOrgRepoSettings(ctx, id.UserID, id.IsAdmin(), org, []domain.RepoSettingMutation{mutation}); err != nil {
		s.writeServiceError(c, err)
		return
	}
	setting, err := s.githubIntegration.GetRepoSetting(ctx, org.ID, repo)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, setting)
}

func (s *Server) handleGitHubOrgRepoClaimDelete(ctx context.Context, c *app.RequestContext) {
	org, id, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if err := s.githubIntegration.DeleteOrgRepoSetting(ctx, id.UserID, id.IsAdmin(), org, githubRepoFromPath(c)); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

func githubRepoFromPath(c *app.RequestContext) string {
	owner := c.Param("owner")
	repo := c.Param("repo")
	return strings.Trim(fmt.Sprintf("%s/%s", owner, repo), "/")
}
