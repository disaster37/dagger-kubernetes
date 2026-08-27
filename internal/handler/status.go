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

// handleReadyz is the kube readiness probe: 200 when the control plane is up
// (ok/degraded/unknown all count), 503 only when the control plane itself is
// down. Downstream cache/telemetry/fleet backends deliberately do NOT gate
// readiness: gating on the aggregate rollup caused a first-boot deadlock — the
// pod never became Ready while the registry or any telemetry subchart was
// still starting (or when a subchart was disabled but its URL still
// auto-derived), which blocked Service routing and StatefulSet rollout.
func (s *Server) handleReadyz(ctx context.Context, c *app.RequestContext) {
	state := domain.ServiceOK
	if st := s.currentStatus(ctx); st != nil {
		state = readinessState(st)
	}
	if state == domain.ServiceDown {
		c.JSON(consts.StatusServiceUnavailable, map[string]any{"state": state})
		return
	}
	c.JSON(consts.StatusOK, map[string]any{"state": state})
}

// handleStartup is the kube startup probe: 200 when the Raft node has joined
// the cluster and DNS is resolved. Used by Kubernetes startupProbe.
func (s *Server) handleStartup(ctx context.Context, c *app.RequestContext) {
	if s.startupProvider == nil {
		c.JSON(consts.StatusOK, map[string]string{"status": "no_provider"})
		return
	}
	if s.startupProvider.IsStarted() {
		c.JSON(consts.StatusOK, map[string]string{"status": "started"})
		return
	}
	c.JSON(consts.StatusServiceUnavailable, map[string]string{"status": "starting"})
}

// readinessState derives pod readiness from the control-plane ("supervisor")
// service only. A down cache/telemetry/fleet backend degrades the platform but
// must not make the supervisor pod unready.
func readinessState(st *domain.PlatformStatus) domain.ServiceState {
	for _, svc := range st.Services {
		if svc.Name == "supervisor" {
			if svc.State == domain.ServiceDown {
				return domain.ServiceDown
			}
			return domain.ServiceOK
		}
	}
	// No supervisor row (status provider misconfigured): report ready rather
	// than deadlock the pod.
	return domain.ServiceOK
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
