package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newFleetMetricsEngine builds a route engine with only the fleet metrics
// route registered (mirrors how trace_metrics_test.go isolates its handler).
func newFleetMetricsEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/fleet/:version/metrics", s.handleFleetMetrics)
	return e
}

func TestHandleFleetMetricsUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	e := newFleetMetricsEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet/v0.21.4/metrics", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleFleetMetricsInvalidVersion(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)
	env.server.fleetMetrics = service.NewFleetMetricsService(
		&stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}},
		"dagger-kubernetes", 15*time.Second, 0, env.server.logger)
	e := newFleetMetricsEngine(env.server)

	for _, version := range []string{"bad", "v0.19", "v0.19.0-rc1"} {
		resp := ut.PerformRequest(e, "GET", "/api/v1/fleet/"+version+"/metrics", nil,
			ut.Header{Key: "Authorization", Value: bearer})
		if resp.Result().StatusCode() != http.StatusBadRequest {
			t.Fatalf("version %q = %d, want 400", version, resp.Result().StatusCode())
		}
	}
}

func TestHandleFleetMetricsDisabled(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)
	e := newFleetMetricsEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet/v0.21.4/metrics", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.Result().StatusCode())
	}
}

func TestHandleFleetMetricsSuccess(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)
	env.server.fleetMetrics = service.NewFleetMetricsService(
		&stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}},
		"dagger-kubernetes", 15*time.Second, 50<<30, env.server.logger)
	e := newFleetMetricsEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet/v0.21.4/metrics", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}

	var body domain.FleetMetrics
	if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Version != "v0.21.4" {
		t.Fatalf("version = %q, want v0.21.4", body.Version)
	}
	// defaultMetricQueries has 6 entries (cpu, memory, disk read/write, net rx/tx).
	if len(body.Series) != 6 {
		t.Fatalf("series = %d, want 6", len(body.Series))
	}
	if body.StepSeconds != 15 {
		t.Fatalf("step_seconds = %d, want 15", body.StepSeconds)
	}
	// The stub returns a point for every query, so storage carries used +
	// capacity (the fleet has no replicas in the stub, so capacity comes from
	// cAdvisor's limit point too).
	if body.Storage == nil {
		t.Fatalf("storage = nil, want present")
	}
	if body.Storage.UsedBytes != 2 || body.Storage.CapacityBytes != 2 {
		t.Fatalf("storage = %+v, want used=2 capacity=2", body.Storage)
	}
}
