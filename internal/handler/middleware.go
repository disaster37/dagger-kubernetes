package handler

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const identityKey = "auth_identity"

// resolveIdentity resolves the request identity via AuthService and stores it
// on the context. Credentials are read from the Authorization header first
// (bearer stays primary for CI), then the access cookie. A missing or
// unparseable credential results in an unauthenticated error (401).
func (s *Server) resolveIdentity(c *app.RequestContext) (*domain.Identity, bool) {
	id, err := s.auth.Resolve(context.Background(), s.bearerFromRequest(c))
	if err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
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
func (s *Server) requireAuth(c *app.RequestContext) bool {
	_, ok := s.resolveIdentity(c)
	return ok
}

// requireAuthWithQueryFallback resolves the identity from the Authorization
// header, then the access cookie, then the ?token= query param (D14). Only the
// SSE /live route uses this: EventSource clients cannot set headers, and tokens
// in URLs leak via logs/referrers, so query-param auth is limited to that one
// route.
func (s *Server) requireAuthWithQueryFallback(c *app.RequestContext) bool {
	bearer := s.bearerFromRequest(c)
	if bearer == "" {
		bearer = c.Query("token")
	}
	id, err := s.auth.Resolve(context.Background(), bearer)
	if err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return false
	}
	c.Set(identityKey, id)
	return true
}

// requireAdmin resolves the identity and enforces the admin role. Writes 401
// when unauthenticated, 403 when authenticated but not admin.
func (s *Server) requireAdmin(c *app.RequestContext) (*domain.Identity, bool) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return nil, false
	}
	if !id.IsAdmin() {
		writeError(c, consts.StatusForbidden, "forbidden")
		return nil, false
	}
	return id, true
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
func (s *Server) authorizeTrace(c *app.RequestContext, traceID string) bool {
	id := identityOf(c)
	if id == nil {
		// No identity resolved (should not happen on gated routes); deny.
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return false
	}

	meta, err := s.traceMeta.Get(context.Background(), traceID)
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
func (s *Server) writeServiceError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(c, consts.StatusNotFound, "not found")
	case errors.Is(err, domain.ErrTokenExists):
		writeError(c, consts.StatusConflict, "token already exists")
	case errors.Is(err, domain.ErrConflict):
		writeError(c, consts.StatusConflict, "resource already exists")
	case errors.Is(err, domain.ErrNotLeader):
		writeError(c, consts.StatusServiceUnavailable, "not the leader")
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
	case errors.Is(err, domain.ErrUnauthenticated):
		writeError(c, consts.StatusUnauthorized, "unauthorized")
	case errors.Is(err, domain.ErrSessionRevoked):
		writeError(c, consts.StatusUnauthorized, "session revoked; please sign in again")
	case errors.Is(err, domain.ErrValidation):
		writeError(c, consts.StatusBadRequest, err.Error())
	default:
		s.logger.WithError(err).Error("handler error")
		writeError(c, consts.StatusInternalServerError, "internal error")
	}
}
