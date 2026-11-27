package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const identityKey = "auth_identity"

// resolveIdentity resolves the request identity via AuthService and stores it
// on the context. Credentials are read from the Authorization header first
// (bearer stays primary for CI), then the access cookie. A missing or
// unparseable credential results in an unauthenticated error (401).
func (s *Server) resolveIdentity(ctx context.Context, c *app.RequestContext) (*domain.Identity, bool) {
	bearer := s.bearerFromRequest(c)
	id, err := s.auth.Resolve(ctx, bearer)
	if err != nil {
		// Invalid/blocked authentication stays a 401; operational store
		// failures propagate (503 not-leader / 504 timeout / 500) rather than
		// masquerading as a credential problem.
		if errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrSessionRevoked) || errors.Is(err, domain.ErrInvalidCredential) {
			// A revoked session is a legitimate state, not a brute-force
			// signal; every other invalid non-empty credential counts toward
			// the bearer-token ban tracker.
			if !errors.Is(err, domain.ErrSessionRevoked) {
				s.recordTokenFailure(s.clientIP(c), bearer)
			}
			writeError(c, consts.StatusUnauthorized, "unauthorized")
			return nil, false
		}
		s.writeServiceError(c, err)
		return nil, false
	}
	c.Set(identityKey, id)
	return id, true
}

// bearerFromRequest returns the bearer credential from the Authorization header
// first (primary for CI), then the access cookie (SPA sessions).
func (s *Server) bearerFromRequest(c *app.RequestContext) string {
	bearer, _ := extractToken(c)
	if bearer == "" && s.cookieCfg.AccessName != "" {
		bearer = string(c.Cookie(s.cookieCfg.AccessName))
	}
	return bearer
}

// requireAuth resolves the identity and writes 401 on failure. Returns the
// identity for downstream use. Kept as a bool so existing call sites compile
// unchanged; handlers that need the identity use identityOf(c).
func (s *Server) requireAuth(ctx context.Context, c *app.RequestContext) bool {
	_, ok := s.resolveIdentity(ctx, c)
	return ok
}

// requireAuthWithQueryFallback resolves the identity from the Authorization
// header, then the access cookie, then the ?token= query param (D14). Only the
// SSE /live route uses this: EventSource clients cannot set headers, and tokens
// in URLs leak via logs/referrers, so query-param auth is limited to that one
// route.
func (s *Server) requireAuthWithQueryFallback(ctx context.Context, c *app.RequestContext) bool {
	bearer := s.bearerFromRequest(c)
	if bearer == "" {
		bearer = c.Query("token")
	}
	id, err := s.auth.Resolve(ctx, bearer)
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrSessionRevoked) || errors.Is(err, domain.ErrInvalidCredential) {
			writeError(c, consts.StatusUnauthorized, "unauthorized")
			return false
		}
		s.writeServiceError(c, err)
		return false
	}
	c.Set(identityKey, id)
	return true
}

// requireAdmin resolves the identity and enforces the admin role. Writes 401
// when unauthenticated, 403 when authenticated but not admin.
func (s *Server) requireAdmin(ctx context.Context, c *app.RequestContext) bool {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return false
	}
	if !id.IsAdmin() {
		writeError(c, consts.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// identityOf returns the identity stored on the context by resolveIdentity.
// Returns nil when no identity was stored (e.g. a route that did not call
// requireAuth).
func identityOf(c *app.RequestContext) *domain.Identity {
	v, ok := c.Get(identityKey)
	if !ok {
		return nil
	}
	id, _ := v.(*domain.Identity)
	return id
}

// authorizeTrace loads trace metadata and enforces visibility (D4):
//   - admin: always allowed (unknown meta -> allowed, fail-closed for others)
//   - owner (meta.UserID == id.UserID): allowed
//   - group member (meta.GroupID != "" && id.HasGroup): allowed
//   - otherwise: 404 "trace not found" (do not leak existence)
//
// Returns ok; the loaded meta is unused by callers (they re-fetch via the
// trace repository) so it is not returned.
func (s *Server) authorizeTrace(ctx context.Context, c *app.RequestContext, traceID string) bool {
	id := identityOf(c)
	if id == nil {
		// No identity resolved (should not happen on gated routes); deny.
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return false
	}

	meta, err := s.traceMeta.Get(ctx, traceID)
	if err != nil {
		if id.IsAdmin() {
			// Admins may view unknown traces (e.g. Tempo-only traces).
			return true
		}
		// Fail closed for non-admins: 404 (don't leak existence).
		writeError(c, consts.StatusNotFound, "trace not found")
		return false
	}

	if id.IsAdmin() {
		return true
	}
	if meta.UserID != "" && meta.UserID == id.UserID {
		return true
	}
	if meta.GroupID != "" && id.HasGroup(meta.GroupID) {
		return true
	}
	writeError(c, consts.StatusNotFound, "trace not found")
	return false
}

// canonicalOrgID maps an org ID or slug to the canonical org-<16hex> ID used
// for scoped storage. Unknown refs are returned unchanged so the caller's
// authorization check still fails closed.
func (s *Server) canonicalOrgID(ctx context.Context, orgRef string) string {
	if s.orgs == nil || orgRef == "" {
		return orgRef
	}
	o, err := s.orgs.Resolve(ctx, orgRef)
	if err != nil {
		return orgRef
	}
	return o.ID
}

// resolveRequestOrg resolves the request organization from :org, the
// X-Dagger-Org header, or ?org= and returns the canonical *domain.Org plus the
// caller identity. It writes the mapped error response and returns ok=false on
// any failure.
func (s *Server) resolveRequestOrg(ctx context.Context, c *app.RequestContext) (*domain.Org, *domain.Identity, bool) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return nil, nil, false
	}
	if s.orgs == nil {
		writeError(c, consts.StatusServiceUnavailable, "organization service unavailable")
		return nil, nil, false
	}
	explicit := c.Param("org")
	if explicit == "" {
		explicit = string(c.Request.Header.Peek(orgHeaderName))
	}
	if explicit == "" {
		explicit = c.Query("org")
	}
	org, err := s.orgs.ResolveForIdentity(ctx, id, explicit)
	if err != nil {
		s.writeServiceError(c, err)
		return nil, nil, false
	}
	return org, id, true
}

// requireOrgMember resolves the request org and requires membership (or global
// admin).
func (s *Server) requireOrgMember(ctx context.Context, c *app.RequestContext) (*domain.Org, *domain.Identity, bool) {
	org, id, ok := s.resolveRequestOrg(ctx, c)
	if !ok {
		return nil, nil, false
	}
	if !id.IsAdmin() {
		member, err := s.orgs.IsOrgMember(ctx, org.ID, id.UserID)
		if err != nil {
			s.writeServiceError(c, err)
			return nil, nil, false
		}
		if !member {
			writeError(c, consts.StatusForbidden, "forbidden")
			return nil, nil, false
		}
	}
	return org, id, true
}

// requireOrgAdminResolved resolves the request org and requires org_admin (or
// global admin).
func (s *Server) requireOrgAdminResolved(ctx context.Context, c *app.RequestContext) (*domain.Org, *domain.Identity, bool) {
	org, id, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return nil, nil, false
	}
	if !id.IsAdmin() {
		role, found, err := s.orgs.MemberRole(ctx, org.ID, id.UserID)
		if err != nil {
			s.writeServiceError(c, err)
			return nil, nil, false
		}
		if !found || role != domain.OrgAdmin {
			writeError(c, consts.StatusForbidden, "forbidden")
			return nil, nil, false
		}
	}
	return org, id, true
}

// attributionUserID returns the user id recorded in trace attribution, or ""
// for synthetic identities (legacy) that have no users-table row. Writing
// those ids would violate the trace_meta user_id foreign key.
func attributionUserID(id *domain.Identity) string {
	if id == nil || id.Method == domain.AuthLegacyTok {
		return ""
	}
	return id.UserID
}

// writeServiceError maps a domain sentinel error to the appropriate HTTP
// status and writes it. Unknown errors yield 500 (logged).
//
//nolint:gocyclo // exhaustive sentinel-to-status mapping
func (s *Server) writeServiceError(c *app.RequestContext, err error) {
	var mismatch *domain.OAuthStateUserMismatchError
	if errors.As(err, &mismatch) {
		c.Response.Header.Set("Cache-Control", "no-store")
		c.JSON(consts.StatusForbidden, map[string]string{
			"message":           "oauth state belongs to a different user",
			"code":              "state_user_mismatch",
			"expected_username": mismatch.ExpectedUsername,
			"current_username":  mismatch.CurrentUsername,
		})
		return
	}
	var rate *domain.GitHubRateLimitError
	if errors.As(err, &rate) {
		if rate.RetryAfter > 0 {
			c.Response.Header.Set("Retry-After", fmt.Sprintf("%d", int(rate.RetryAfter.Seconds())))
		}
		writeError(c, consts.StatusTooManyRequests, "github api rate limit exceeded, try again later")
		return
	}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(c, consts.StatusNotFound, "not found")
	case errors.Is(err, domain.ErrTokenExists):
		writeError(c, consts.StatusConflict, "token already exists")
	case errors.Is(err, domain.ErrConflict):
		writeError(c, consts.StatusConflict, "resource already exists")
	case errors.Is(err, domain.ErrStaleUser):
		writeError(c, consts.StatusConflict, "the account was modified concurrently; reload and try again")
	case errors.Is(err, domain.ErrLastAdmin):
		writeError(c, consts.StatusConflict, "cannot remove the last active administrator")
	case errors.Is(err, domain.ErrAccountBlocked):
		writeError(c, consts.StatusForbidden, "account access is disabled")
	case errors.Is(err, domain.ErrNotLeader):
		writeError(c, consts.StatusServiceUnavailable, "not the leader")
	case errors.Is(err, domain.ErrGitHubRefreshUnavailable):
		writeError(c, consts.StatusServiceUnavailable, "github token refresh is temporarily unavailable; retry shortly")
	case errors.Is(err, domain.ErrRaftTimeout):
		writeError(c, consts.StatusGatewayTimeout, "raft apply timeout")
	case errors.Is(err, domain.ErrInvalidCredential):
		writeError(c, consts.StatusUnauthorized, "invalid credentials")
	case errors.Is(err, domain.ErrForbidden):
		writeError(c, consts.StatusForbidden, "forbidden")
	case errors.Is(err, domain.ErrNoGroups):
		writeError(c, consts.StatusForbidden, "user is not a member of any group")
	case errors.Is(err, domain.ErrAgentUnavailable):
		writeError(c, consts.StatusForbidden, "engines not available for any of the user's groups")
	case errors.Is(err, domain.ErrQuotaExhausted):
		writeError(c, consts.StatusTooManyRequests, "group runner session quota exhausted")
	case errors.Is(err, domain.ErrOrgQuotaExceeded):
		c.Response.Header.Set("Retry-After", "30")
		writeError(c, consts.StatusTooManyRequests, err.Error())
	case errors.Is(err, domain.ErrAmbiguousOrg):
		writeError(c, consts.StatusBadRequest, "ambiguous organization: set X-Dagger-Org or use an org-scoped token")
	case errors.Is(err, domain.ErrNoOrgMembership):
		writeError(c, consts.StatusForbidden, "user is not a member of any organization")
	case errors.Is(err, domain.ErrOrgNotEmpty):
		writeError(c, consts.StatusConflict, "organization still contains groups or projects; delete them first or pass ?force=true")
	case errors.Is(err, domain.ErrSharedFleetMultiOrg):
		writeError(c, consts.StatusConflict, "cannot create additional organizations when fleet.isolation is \"shared\"")
	case errors.Is(err, domain.ErrBootstrapNotReady):
		writeError(c, consts.StatusServiceUnavailable, "organization bootstrap in progress")
	case errors.Is(err, domain.ErrUnauthenticated):
		writeError(c, consts.StatusUnauthorized, "unauthorized")
	case errors.Is(err, domain.ErrSessionRevoked):
		writeError(c, consts.StatusUnauthorized, "session revoked; please sign in again")
	case errors.Is(err, domain.ErrValidation):
		writeError(c, consts.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrGitHubOAuthDisabled), errors.Is(err, domain.ErrGitHubDeviceFlowDisabled):
		writeError(c, consts.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrInvalidRedirectURI), errors.Is(err, domain.ErrOAuthStateInvalid),
		errors.Is(err, domain.ErrRepoNotInInstallation), errors.Is(err, domain.ErrRepoInaccessible),
		errors.Is(err, domain.ErrGitHubDeviceAuthorizationDelay), errors.Is(err, domain.ErrGitHubDeviceSlowDown),
		errors.Is(err, domain.ErrGitHubDeviceFlowDenied):
		writeError(c, consts.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrGitHubTokenRevoked):
		writeError(c, consts.StatusConflict, "github connection token is expired or revoked; please reconnect")
	case errors.Is(err, domain.ErrGitHubNotConnected):
		writeError(c, consts.StatusConflict, "no GitHub account is connected; link a GitHub account first")
	case errors.Is(err, domain.ErrRepoClaimedByOtherOrg):
		writeError(c, consts.StatusConflict, "repository is already claimed by another organization")
	default:
		s.logger.WithError(err).Error("handler error")
		writeError(c, consts.StatusInternalServerError, "internal error")
	}
}
