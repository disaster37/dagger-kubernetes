package handler

import (
	"context"
	"errors"
	"strconv"

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
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	sources, err := s.forges.List(ctx, s.canonicalOrgID(ctx, c.Param("org")))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, sources)
}

// handleForgeSourceCreate creates a forge integration (admin only).
func (s *Server) handleForgeSourceCreate(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
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
		ID: req.ID, OrgID: s.canonicalOrgID(ctx, c.Param("org")), Provider: domain.ForgeKind(req.Provider),
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
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
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
	res, err := s.checks.HandleWebhook(ctx, provider, c.Param("sourceID"), headers, body)
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			s.metrics.WebhookSignatureInvalid.WithLabelValues(string(provider)).Inc()
			writeError(c, consts.StatusUnauthorized, "invalid webhook signature")
			return
		}
		s.writeServiceError(c, err)
		return
	}
	if res.Ignored {
		c.JSON(consts.StatusOK, map[string]string{
			"status": "ignored", "reason": res.Reason, "event": res.RawEvent,
		})
		return
	}
	writeJSON(c, res.Event)
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
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	secrets, err := s.secrets.List(ctx, domain.SecretScopeOrg, s.canonicalOrgID(ctx, c.Param("org")))
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, secrets)
}

// handleOrgSecretPut stores an org secret (admin only).
func (s *Server) handleOrgSecretPut(ctx context.Context, c *app.RequestContext) {
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
	sec, err := s.secrets.Put(ctx, domain.SecretScopeOrg, s.canonicalOrgID(ctx, c.Param("org")), req.Name, req.Value)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusCreated, sec)
}

// handleOrgSecretDelete deletes an org secret (admin only).
func (s *Server) handleOrgSecretDelete(ctx context.Context, c *app.RequestContext) {
	if !s.requireOrgAdmin(ctx, c, s.canonicalOrgID(ctx, c.Param("org"))) {
		return
	}
	if s.secrets == nil {
		writeError(c, consts.StatusServiceUnavailable, "secret service unavailable")
		return
	}
	if err := s.secrets.Delete(ctx, domain.SecretScopeOrg, s.canonicalOrgID(ctx, c.Param("org")), c.Param("name")); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleProjectSecretsList lists project secret metadata (admin only, matching
// the PUT/DELETE endpoints).
func (s *Server) handleProjectSecretsList(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if _, err := s.projects.GetInOrg(ctx, org.ID, c.Param("id")); err != nil {
		s.writeServiceError(c, err)
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
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if _, err := s.projects.GetInOrg(ctx, org.ID, c.Param("id")); err != nil {
		s.writeServiceError(c, err)
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
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if _, err := s.projects.GetInOrg(ctx, org.ID, c.Param("id")); err != nil {
		s.writeServiceError(c, err)
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

// queryLimit parses the ?limit= query parameter with a 100 default.
func queryLimit(c *app.RequestContext) int {
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	return limit
}

// loadCheckRunForCaller loads a check run and enforces org visibility.
func (s *Server) loadCheckRunForCaller(ctx context.Context, c *app.RequestContext) (*domain.CheckRunRecord, bool) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return nil, false
	}
	if s.checks == nil {
		writeError(c, consts.StatusNotFound, "checks disabled")
		return nil, false
	}
	record, err := s.checks.Get(ctx, c.Param("id"))
	if err != nil {
		s.writeServiceError(c, err)
		return nil, false
	}
	if !s.orgAllowed(ctx, id, record.OrgID) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return nil, false
	}
	return record, true
}

// handleOrgChecksList lists Cloud Checks runs for an org (member/admin only).
func (s *Server) handleOrgChecksList(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.checks == nil {
		writeJSON(c, []*domain.CheckRunRecord{})
		return
	}
	if !s.orgAllowed(ctx, id, s.canonicalOrgID(ctx, c.Param("org"))) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	records, err := s.checks.List(ctx, &domain.CheckRunFilter{
		OrgID: s.canonicalOrgID(ctx, c.Param("org")), Status: c.Query("status"), Repo: c.Query("repo"), Limit: queryLimit(c),
	})
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	for _, record := range records {
		s.enrichCheckTests(record)
	}
	writeJSON(c, records)
}

// handleCheckRunGet returns one check run (org member/admin only).
func (s *Server) handleCheckRunGet(ctx context.Context, c *app.RequestContext) {
	record, ok := s.loadCheckRunForCaller(ctx, c)
	if !ok {
		return
	}
	s.enrichCheckTests(record)
	writeJSON(c, record)
}

// enrichCheckTests fills each CheckItem.Tests from its trace at read time. The
// per-test view is never persisted (it would bloat the replicated Raft state);
// a missing trace simply yields no tests.
func (s *Server) enrichCheckTests(records ...*domain.CheckRunRecord) {
	if s.traces == nil {
		return
	}
	for _, record := range records {
		if record == nil {
			continue
		}
		for i := range record.Checks {
			traceID := record.Checks[i].TraceID
			if traceID == "" {
				continue
			}
			trace, err := s.traces.GetTrace(traceID)
			if err != nil {
				s.logger.WithError(err).WithField("trace_id", traceID).Debug("check test derivation failed")
				continue
			}
			record.Checks[i].Tests = service.DeriveTests(trace.RootSpan)
		}
	}
}

type approveCheckRequest struct {
	IncludeSecrets bool `json:"include_secrets"`
}

// handleCheckRunApprove releases a fork-PR hold (org member/admin).
func (s *Server) handleCheckRunApprove(ctx context.Context, c *app.RequestContext) {
	if s.checks != nil && !s.checks.Enabled() {
		writeError(c, consts.StatusBadRequest, "cloud checks are disabled (checks.enabled is false)")
		return
	}
	record, ok := s.loadCheckRunForCaller(ctx, c)
	if !ok {
		return
	}
	id := identityOf(c)
	var req approveCheckRequest
	if !decodeBody(c, &req) {
		return
	}
	isOrgAdmin := id.IsAdmin() || (s.orgs != nil && s.orgs.IsOrgAdmin(ctx, record.OrgID, id.UserID))
	updated, err := s.checks.ApproveForkRun(ctx, record.ID, id.UserID, req.IncludeSecrets, isOrgAdmin)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, updated)
}

// handleCheckRunRerun re-dispatches a check run (org member/admin).
func (s *Server) handleCheckRunRerun(ctx context.Context, c *app.RequestContext) {
	if s.checks != nil && !s.checks.Enabled() {
		writeError(c, consts.StatusBadRequest, "cloud checks are disabled (checks.enabled is false)")
		return
	}
	record, ok := s.loadCheckRunForCaller(ctx, c)
	if !ok {
		return
	}
	updated, err := s.checks.Rerun(ctx, record.ID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, updated)
}

// handleCheckRunDelete removes a check run (org admin or global admin).
// Idempotent: deleting an already-absent run returns 204.
func (s *Server) handleCheckRunDelete(ctx context.Context, c *app.RequestContext) {
	record, ok := s.loadCheckRunForCaller(ctx, c)
	if !ok {
		return
	}
	id := identityOf(c)
	if !id.IsAdmin() && (s.orgs == nil || !s.orgs.IsOrgAdmin(ctx, record.OrgID, id.UserID)) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	if err := s.checks.Delete(ctx, record.ID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// runTokenHeader carries the per-run callback credential.
const runTokenHeader = "X-Dagger-Run-Token" //nolint:gosec // header name, not a credential value

// runTokenAllowed verifies the per-run callback token against any of the
// stored hashes (the current hash and, for workflow runs, the previous hash
// retained across a leader-restart re-mint).
func runTokenAllowed(c *app.RequestContext, hashes ...string) bool {
	token := string(c.Request.Header.Peek(runTokenHeader))
	for _, hash := range hashes {
		if service.VerifyRunToken(hash, token) {
			return true
		}
	}
	return false
}

// handleCheckRunLoad records the check-runner load phase.
func (s *Server) handleCheckRunLoad(ctx context.Context, c *app.RequestContext) {
	record, ok := s.loadCheckRunForCaller(ctx, c)
	if !ok {
		return
	}
	if !runTokenAllowed(c, record.RunTokenHash) {
		writeError(c, consts.StatusForbidden, "invalid run token")
		return
	}
	var in service.LoadReportInput
	if !decodeBody(c, &in) {
		return
	}
	updated, err := s.checks.RecordLoad(ctx, record.OrgID, record.ID, in)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, updated)
}

// handleCheckRunComplete records the check-runner completion phase.
func (s *Server) handleCheckRunComplete(ctx context.Context, c *app.RequestContext) {
	record, ok := s.loadCheckRunForCaller(ctx, c)
	if !ok {
		return
	}
	if !runTokenAllowed(c, record.RunTokenHash) {
		writeError(c, consts.StatusForbidden, "invalid run token")
		return
	}
	var in service.CompleteReportInput
	if !decodeBody(c, &in) {
		return
	}
	updated, err := s.checks.RecordComplete(ctx, record.OrgID, record.ID, in)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, updated)
}
