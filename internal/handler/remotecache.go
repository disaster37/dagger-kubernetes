package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleRemoteCacheStats returns index statistics for a namespace, scoped to
// the caller's org (CWE-862).
func (s *Server) handleRemoteCacheStats(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.remoteCache == nil {
		writeJSON(c, map[string]any{"enabled": false})
		return
	}
	namespace := c.Query("namespace")
	if namespace == "" {
		namespace = domain.DefaultOrgID
	}
	if !s.orgAllowed(ctx, id, namespace) {
		writeError(c, consts.StatusForbidden, "forbidden")
		return
	}
	count, bytes, err := s.remoteCache.Stats(namespace)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, map[string]any{
		"enabled":   true,
		"namespace": namespace,
		"entries":   count,
		"bytes":     bytes,
	})
}

// handleRemoteCachePurge purges a namespace (admin only).
func (s *Server) handleRemoteCachePurge(ctx context.Context, c *app.RequestContext) {
	if _, ok := s.requireAdmin(ctx, c); !ok {
		return
	}
	if s.remoteCache == nil {
		writeError(c, consts.StatusNotFound, "remote cache disabled")
		return
	}
	namespace := c.Query("namespace")
	if namespace == "" {
		namespace = domain.DefaultOrgID
	}
	if err := s.remoteCache.Purge(ctx, namespace); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
