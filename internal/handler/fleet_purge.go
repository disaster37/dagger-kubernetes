package handler

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleFleetPurgeCache prunes the local cache of every engine pod in the
// version's StatefulSet. Admin-only; blocks until the purge completes (or
// per-pod timeouts fire).
func (s *Server) handleFleetPurgeCache(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	version := c.Param("version")
	if !domain.IsFullVersion(version) {
		writeError(c, consts.StatusBadRequest, "invalid version")
		return
	}
	if s.engineCachePurger == nil {
		writeError(c, consts.StatusServiceUnavailable, "engine cache purge unavailable")
		return
	}
	result, err := s.engineCachePurger.PurgeForOrg(ctx, org.ID, version)
	if err != nil {
		s.writeFleetPurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

// handleFleetPurgeStatus returns the last recorded purge result for the
// version, or 404 when no purge has been recorded. Admin-only.
func (s *Server) handleFleetPurgeStatus(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	version := c.Param("version")
	if !domain.IsFullVersion(version) {
		writeError(c, consts.StatusBadRequest, "invalid version")
		return
	}
	if s.engineCachePurger == nil {
		writeError(c, consts.StatusServiceUnavailable, "engine cache purge unavailable")
		return
	}
	result, ok := s.engineCachePurger.StatusForOrg(org.ID, version)
	if !ok {
		writeError(c, consts.StatusNotFound, "no purge recorded")
		return
	}
	writeJSON(c, result)
}

// writeFleetPurgeError maps purge sentinel errors to HTTP responses.
func (s *Server) writeFleetPurgeError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrEngineFleetNotFound):
		writeError(c, consts.StatusNotFound, "engine fleet not found")
	case errors.Is(err, domain.ErrPurgeInProgress):
		writeError(c, consts.StatusConflict, "purge already in progress")
	default:
		s.logger.WithError(err).Error("fleet purge failed")
		writeError(c, consts.StatusInternalServerError, "purge failed")
	}
}
