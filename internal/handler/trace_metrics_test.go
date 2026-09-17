package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const traceMetricsTraceID = "abcdef0123456789abcdef0123456789"

// stubMetricsQueryer returns canned points for every query.
type stubMetricsQueryer struct {
	points []domain.MetricPoint
}

func (s *stubMetricsQueryer) QueryRange(_ context.Context, _ string, _, _ time.Time, _ time.Duration) ([]domain.MetricPoint, error) {
	return s.points, nil
}

func TestHandleTraceMetrics(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)

	traceMetaRepo := repository.NewTraceMetaRepo(env.store)
	if err := traceMetaRepo.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID:    traceMetricsTraceID,
		Version:    "v0.21.4",
		StartedAt:  time.Now().Add(-time.Minute),
		DurationMS: 30000,
	}); err != nil {
		t.Fatalf("seed trace meta: %v", err)
	}

	env.server.engineMetrics = service.NewEngineMetricsService(
		&stubMetricsQueryer{points: []domain.MetricPoint{{T: 1, V: 2}}},
		"dagger-kubernetes", 15*time.Second, env.server.logger)

	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/traces/:traceID/metrics", env.server.handleTraceMetrics)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/"+traceMetricsTraceID+"/metrics", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}

	var body domain.TraceMetrics
	if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TraceID != traceMetricsTraceID {
		t.Fatalf("trace_id = %q, want %q", body.TraceID, traceMetricsTraceID)
	}
	if body.StepSeconds != 15 {
		t.Fatalf("step_seconds = %d, want 15", body.StepSeconds)
	}
	// defaultMetricQueries has 6 entries (cpu, memory, disk read/write, net rx/tx).
	if len(body.Series) != 6 {
		t.Fatalf("series = %d, want 6", len(body.Series))
	}
}

func TestHandleTraceMetricsUnknownTrace(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)

	env.server.engineMetrics = service.NewEngineMetricsService(
		&stubMetricsQueryer{}, "dagger-kubernetes", 15*time.Second, env.server.logger)

	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/traces/:traceID/metrics", env.server.handleTraceMetrics)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/"+traceMetricsTraceID+"/metrics", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}

	var body domain.TraceMetrics
	if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TraceID != traceMetricsTraceID {
		t.Fatalf("trace_id = %q, want %q", body.TraceID, traceMetricsTraceID)
	}
	if len(body.Series) != 0 {
		t.Fatalf("series = %v, want empty", body.Series)
	}
}

func TestHandleTraceMetricsDisabled(t *testing.T) {
	env := newTestEnv(t)
	bearer := env.loginAsAdmin(t)

	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/traces/:traceID/metrics", env.server.handleTraceMetrics)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/"+traceMetricsTraceID+"/metrics", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.Result().StatusCode())
	}
}
