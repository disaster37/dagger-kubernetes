package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handlePlatformStatus serves the aggregated platform status (GET /api/v1/status).
func (s *Server) handlePlatformStatus(ctx context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}
	if s.status == nil {
		writeError(c, consts.StatusInternalServerError, "status unavailable")
		return
	}
	status, err := s.status.Status(ctx)
	if err != nil {
		writeError(c, consts.StatusInternalServerError, "status unavailable")
		return
	}
	writeJSON(c, status)
}

// currentStatus returns the latest platform status, or nil when the status
// provider is absent or the probe fails (probes are best-effort).
func (s *Server) currentStatus(ctx context.Context) *domain.PlatformStatus {
	if s.status == nil {
		return nil
	}
	st, err := s.status.Status(ctx)
	if err != nil {
		return nil
	}
	return st
}

// handleHealthz is the kube liveness probe: 200 always. A down rollup is
// reported as "degraded" so kube never restarts the process over a down
// telemetry sidecar or cache backend.
func (s *Server) handleHealthz(ctx context.Context, c *app.RequestContext) {
	state := domain.ServiceOK
	if st := s.currentStatus(ctx); st != nil {
		state = healthzState(st.State)
	}
	c.JSON(consts.StatusOK, map[string]domain.ServiceState{"state": state})
}

// handleReadyz is the kube readiness probe: 200 when ok/degraded, 503 when the
// rollup is down (so kube stops routing traffic when the cache is unreachable).
func (s *Server) handleReadyz(ctx context.Context, c *app.RequestContext) {
	state := domain.ServiceOK
	var services []domain.ServiceStatus
	if st := s.currentStatus(ctx); st != nil {
		state = st.State
		services = st.Services
	}
	if state == domain.ServiceDown {
		c.JSON(consts.StatusServiceUnavailable, map[string]any{"state": state, "services": services})
		return
	}
	c.JSON(consts.StatusOK, map[string]domain.ServiceState{"state": state})
}

// healthzState maps a rollup to a liveness state: degraded/down both report as
// "degraded"; everything else (ok/unknown) reports as "ok".
func healthzState(state domain.ServiceState) domain.ServiceState {
	switch state {
	case domain.ServiceDegraded, domain.ServiceDown:
		return domain.ServiceDegraded
	default:
		return domain.ServiceOK
	}
}
