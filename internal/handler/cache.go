package handler

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

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

// handleCachePurge purges every cache tag (POST /api/v1/cache/purge, admin-only).
func (s *Server) handleCachePurge(ctx context.Context, c *app.RequestContext) {
	if s.cachePurger == nil {
		writeError(c, consts.StatusInternalServerError, "cache purge unavailable")
		return
	}

	result, err := s.cachePurger.Purge(ctx)
	if err != nil {
		s.writePurgeError(c, err)
		return
	}
	writeJSON(c, result)
}

// writePurgeError maps purge sentinel errors to HTTP responses.
func (s *Server) writePurgeError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrRegistryCatalogDisabled):
		writeError(c, consts.StatusConflict, "registry catalog disabled; cannot purge")
	case errors.Is(err, domain.ErrRegistryDeleteDisabled):
		writeError(c, consts.StatusConflict, "registry delete not enabled")
	default:
		s.logger.WithError(err).Error("purge failed")
		writeError(c, consts.StatusInternalServerError, "purge failed")
	}
}
