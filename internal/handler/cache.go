package handler

import (
	"context"
	"errors"
	"regexp"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var cacheTagRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// handleCacheInfo serves the rich cache payload (GET /api/v1/cache).
func (s *Server) handleCacheInfo(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}
	if s.cacheStats == nil {
		writeError(c, consts.StatusInternalServerError, "cache stats unavailable")
		return
	}
	stats, err := s.cacheStats.Stats(ctx)
	if err != nil {
		s.logger.WithError(err).Error("cache stats failed")
		writeError(c, consts.StatusInternalServerError, "cache stats failed")
		return
	}
	writeJSON(c, stats)
}

// handleCachePurge purges one version's cache ref (POST /api/v1/cache/purge,
// admin-only).
func (s *Server) handleCachePurge(ctx context.Context, c *app.RequestContext) {
	if s.cachePurger == nil {
		writeError(c, consts.StatusInternalServerError, "cache purge unavailable")
		return
	}

	var req domain.PurgeRequest
	if !decodeBody(c, &req) {
		return
	}

	parsed, err := domain.Parse(req.Version)
	if err != nil || !s.versionResolver.IsAllowed(parsed) {
		writeError(c, consts.StatusBadRequest, "invalid version")
		return
	}
	if req.Tag != "" && !cacheTagRe.MatchString(req.Tag) {
		writeError(c, consts.StatusBadRequest, "invalid tag")
		return
	}

	result, err := s.cachePurger.Purge(ctx, req)
	if err != nil {
		s.writePurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

// handleCachePurgeAll purges every cache tag (POST /api/v1/cache/purge-all,
// admin-only).
func (s *Server) handleCachePurgeAll(ctx context.Context, c *app.RequestContext) {
	if s.cachePurger == nil {
		writeError(c, consts.StatusInternalServerError, "cache purge unavailable")
		return
	}
	result, err := s.cachePurger.PurgeAll(ctx)
	if err != nil {
		s.writePurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

// writePurgeError maps purge sentinel errors to HTTP responses.
func (s *Server) writePurgeError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrRegistryDeleteDisabled):
		writeError(c, consts.StatusConflict, "registry delete not enabled")
	case errors.Is(err, domain.ErrValidation):
		writeError(c, consts.StatusBadRequest, err.Error())
	default:
		s.logger.WithError(err).Error("purge failed")
		writeError(c, consts.StatusInternalServerError, "purge failed")
	}
}
