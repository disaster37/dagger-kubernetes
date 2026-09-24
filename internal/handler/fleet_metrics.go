package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleFleetMetrics serves per-version engine resource + storage metrics
// (auth-gated, like GET /api/v1/fleet). A malformed version is a 400; a
// disabled metrics service is a 501; an unknown-but-well-formed version (or a
// metrics-backend failure) yields an empty-but-valid payload (HTTP 200),
// matching the tolerant trace-metrics endpoint.
func (s *Server) handleFleetMetrics(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}
	version := c.Param("version")
	if !domain.IsFullVersion(version) {
		writeError(c, consts.StatusBadRequest, "invalid version")
		return
	}
	if s.fleetMetrics == nil {
		writeError(c, consts.StatusNotImplemented, "metrics unavailable")
		return
	}
	replicas := 0
	if fleet, err := s.fleetManager.GetVersionFleet(version); err == nil {
		replicas = len(fleet.Ordinals)
	}
	fm, err := s.fleetMetrics.FleetMetrics(ctx, version, replicas)
	if err != nil {
		s.logger.WithError(err).WithField("version", version).Warn("fleet metrics unavailable")
		fm = &domain.FleetMetrics{Version: version, Series: []domain.MetricSeries{}}
	}
	writeJSON(c, fm)
}
