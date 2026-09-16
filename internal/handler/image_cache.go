package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleImageCacheInfo lists the configured mirrors and their cached images
// (GET /api/v1/image-cache, admin-only). Per-mirror failures are carried in the
// response body (reachable:false + error) rather than failing the request.
func (s *Server) handleImageCacheInfo(ctx context.Context, c *app.RequestContext) {
	if s.imageCache == nil {
		writeJSON(c, &domain.ImageCacheInfo{
			Mirrors:     []domain.ImageCacheMirrorInfo{},
			CollectedAt: formatTime(time.Now()),
		})
		return
	}
	info, err := s.imageCache.List(ctx)
	if err != nil {
		s.writeImageCacheError(c, err)
		return
	}
	writeJSON(c, info)
}

// handleImageCachePrune deletes the requested refs from one mirror
// (POST /api/v1/image-cache/prune, admin-only). Per-item failures are carried
// in the body; only request-level failures (unknown mirror, malformed refs,
// unreachable/delete-disabled mirror) become HTTP errors.
func (s *Server) handleImageCachePrune(ctx context.Context, c *app.RequestContext) {
	if s.imageCache == nil {
		writeError(c, consts.StatusNotFound, "image cache mirror not found")
		return
	}
	var req domain.ImageCachePruneRequest
	if !decodeBody(c, &req) {
		return
	}
	result, err := s.imageCache.Prune(ctx, req.MirrorID, req.Refs)
	if err != nil {
		s.writeImageCacheError(c, err)
		return
	}
	writeJSON(c, result)
}

// handleImageCachePruneAll deletes every manifest reachable by tag on the
// selected mirror(s) (POST /api/v1/image-cache/prune-all, admin-only). An empty
// body (or empty mirror_id) means every configured mirror.
func (s *Server) handleImageCachePruneAll(ctx context.Context, c *app.RequestContext) {
	if s.imageCache == nil {
		writeJSON(c, &domain.ImageCachePruneAllResult{Mirrors: []domain.ImageCachePruneAllMirror{}})
		return
	}
	var req domain.ImageCachePruneAllRequest
	if !decodeOptionalBody(c, &req) {
		return
	}
	result, err := s.imageCache.PruneAll(ctx, req.MirrorID)
	if err != nil {
		s.writeImageCacheError(c, err)
		return
	}
	writeJSON(c, result)
}

// decodeOptionalBody behaves like decodeBody but treats an absent/empty body as
// an empty request rather than a decode error, so prune-all works with no body.
func decodeOptionalBody(c *app.RequestContext, v any) bool {
	body, ok := readControlBody(c)
	if !ok {
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(c, consts.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

// writeImageCacheError maps image-cache sentinel errors to HTTP responses.
func (s *Server) writeImageCacheError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, domain.ErrImageCacheInvalidRef):
		writeError(c, consts.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrImageCacheMirrorNotFound):
		writeError(c, consts.StatusNotFound, "image cache mirror not found")
	case errors.Is(err, domain.ErrRegistryDeleteDisabled):
		writeError(c, consts.StatusConflict, "image cache delete is disabled")
	case errors.Is(err, domain.ErrRegistryCatalogDisabled):
		writeError(c, consts.StatusConflict, "image cache catalog is disabled")
	case errors.Is(err, domain.ErrImageCacheUnreachable):
		writeError(c, consts.StatusBadGateway, "image cache mirror unreachable")
	default:
		s.logger.WithError(err).Error("image cache request failed")
		writeError(c, consts.StatusInternalServerError, "image cache request failed")
	}
}
