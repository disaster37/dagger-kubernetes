package handler

import (
	"context"
	"fmt"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleRemoteCacheStats returns index statistics for a namespace, scoped to
// the caller's org (CWE-862).
func (s *Server) handleRemoteCacheStats(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgMember(ctx, c)
	if !ok {
		return
	}
	if s.remoteCache == nil {
		writeJSON(c, map[string]any{"enabled": false})
		return
	}
	namespace := s.remoteCacheNamespaceFor(org.ID)
	count, bytes, err := s.remoteCache.Stats(namespace)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	var soft, hard int64
	if s.quota != nil {
		if eff, qErr := s.quota.EffectiveQuotas(ctx, org.ID); qErr == nil {
			soft = eff.MaxCacheBytes.Soft
			hard = eff.MaxCacheBytes.Hard
		}
	}
	if soft > 0 && bytes > soft {
		setQuotaWarning(c, fmt.Sprintf("%s soft limit exceeded (%d/%d)", domain.QuotaCacheBytes, bytes, soft))
	}
	writeJSON(c, map[string]any{
		"enabled":          true,
		"namespace":        namespace,
		"org_slug":         org.Name,
		"entries":          count,
		"bytes":            bytes,
		"soft_limit_bytes": soft,
		"hard_limit_bytes": hard,
	})
}

// handleRemoteCachePurge purges a namespace (admin only).
func (s *Server) handleRemoteCachePurge(ctx context.Context, c *app.RequestContext) {
	org, _, ok := s.requireOrgAdminResolved(ctx, c)
	if !ok {
		return
	}
	if s.remoteCache == nil {
		writeError(c, consts.StatusNotFound, "remote cache disabled")
		return
	}
	namespace := s.remoteCacheNamespaceFor(org.ID)
	if err := s.remoteCache.Purge(ctx, namespace); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}
