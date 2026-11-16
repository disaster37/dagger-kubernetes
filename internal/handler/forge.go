package handler

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

type forgeSourceRequest struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	Repo          string `json:"repo"`
	Token         string `json:"token"`
	WebhookSecret string `json:"webhook_secret"`
}

// handleForgeSourcesList lists an org's forge integrations (member/admin only).
func (s *Server) handleForgeSourcesList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.forges == nil {
		writeJSON(c, []*domain.ForgeSource{})
		return
	}
	if !s.orgAllowed(ctx, id, c.Param("org")) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	sources, err := s.forges.List(ctx, c.Param("org"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, sources)
}

// handleForgeSourceCreate creates a forge integration (admin only).
func (s *Server) handleForgeSourceCreate(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
		return
	}
	if s.forges == nil {
		writeError(c, consts.StatusServiceUnavailable, "forge service unavailable")
		return
	}
	var req forgeSourceRequest
	if !decodeBody(c, &req) {
		return
	}
	src, err := s.forges.CreateSource(ctx, &service.ForgeSourceInput{
		ID: req.ID, OrgID: c.Param("org"), Provider: domain.ForgeKind(req.Provider),
		BaseURL: req.BaseURL, Repo: req.Repo, Token: req.Token, WebhookSecret: req.WebhookSecret,
	})
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, src)
}

// handleForgeSourceDelete deletes a forge integration (admin only).
func (s *Server) handleForgeSourceDelete(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
		return
	}
	if s.forges == nil {
		writeError(c, consts.StatusServiceUnavailable, "forge service unavailable")
		return
	}
	if err := s.forges.Delete(ctx, c.Param("sourceID")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleForgeWebhook receives POST /v1/webhooks/:provider/:sourceID. Signature
// failures are counted but never recorded as token failures (shared NAT IPs).
func (s *Server) handleForgeWebhook(ctx context.Context, c *app.RequestContext) {
	if s.checks == nil {
		writeError(c, consts.StatusNotFound, "checks disabled")
		return
	}
	body, err := readBoundedBody(c, maxControlBody)
	if err != nil {
		writeError(c, consts.StatusBadRequest, "invalid body")
		return
	}
	headers := map[string]string{
		"X-Hub-Signature-256": string(c.Request.Header.Peek("X-Hub-Signature-256")),
		"X-Gitlab-Token":      string(c.Request.Header.Peek("X-Gitlab-Token")),
		"X-Gitea-Signature":   string(c.Request.Header.Peek("X-Gitea-Signature")),
		"X-GitHub-Event":      string(c.Request.Header.Peek("X-GitHub-Event")),
		"X-Gitlab-Event":      string(c.Request.Header.Peek("X-Gitlab-Event")),
		"X-Gitea-Event":       string(c.Request.Header.Peek("X-Gitea-Event")),
	}
	provider := domain.ForgeKind(c.Param("provider"))
	ev, err := s.checks.HandleWebhook(ctx, provider, c.Param("sourceID"), headers, body)
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			s.metrics.WebhookSignatureInvalid.WithLabelValues(string(provider)).Inc()
			writeError(c, consts.StatusUnauthorized, "invalid webhook signature")
			return
		}
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, ev)
}

type secretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// handleOrgSecretsList lists org secret metadata (member/admin only).
func (s *Server) handleOrgSecretsList(ctx context.Context, c *app.RequestContext) {
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
	secrets, err := s.secrets.List(ctx, domain.SecretScopeOrg, c.Param("org"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, secrets)
}

// handleOrgSecretPut stores an org secret (admin only).
func (s *Server) handleOrgSecretPut(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
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
	sec, err := s.secrets.Put(ctx, domain.SecretScopeOrg, c.Param("org"), req.Name, req.Value)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, sec)
}

// handleOrgSecretDelete deletes an org secret (admin only).
func (s *Server) handleOrgSecretDelete(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
		return
	}
	if s.secrets == nil {
		writeError(c, consts.StatusServiceUnavailable, "secret service unavailable")
		return
	}
	if err := s.secrets.Delete(ctx, domain.SecretScopeOrg, c.Param("org"), c.Param("name")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleProjectSecretsList lists project secret metadata (admin only, matching
// the PUT/DELETE endpoints).
func (s *Server) handleProjectSecretsList(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
		return
	}
	if s.secrets == nil {
		writeJSON(c, []*domain.Secret{})
		return
	}
	secrets, err := s.secrets.List(ctx, domain.SecretScopeProject, c.Param("id"))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, secrets)
}

// handleProjectSecretPut stores a project secret (admin only).
func (s *Server) handleProjectSecretPut(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
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
	sec, err := s.secrets.Put(ctx, domain.SecretScopeProject, c.Param("id"), req.Name, req.Value)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, sec)
}

// handleProjectSecretDelete deletes a project secret (admin only).
func (s *Server) handleProjectSecretDelete(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
		return
	}
	if s.secrets == nil {
		writeError(c, consts.StatusServiceUnavailable, "secret service unavailable")
		return
	}
	if err := s.secrets.Delete(ctx, domain.SecretScopeProject, c.Param("id"), c.Param("name")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
