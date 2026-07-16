package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/auth"
	"github.com/disaster/dagger-kubernetes/internal/ca"
	"github.com/disaster/dagger-kubernetes/internal/cache"
	"github.com/disaster/dagger-kubernetes/internal/fleet"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/session"
	"github.com/disaster/dagger-kubernetes/internal/version"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	logger := observ.NewTestLogger()
	mintingCA, err := ca.NewMintingCA(2 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}

	versionResolver, err := version.NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	sessions := session.NewStore(2 * time.Minute)
	provider := fleet.NewStubProvider()

	fleetManager := fleet.NewManager(provider, sessions, fleet.ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
		VersionRetention:      24 * time.Hour,
		MinReplicasPerVersion: 0,
	}, logger, observ.NewMetrics(nil))

	cacheBackend := &cache.Backend{
		Type:     "registry",
		Registry: "cache.reg/dagger-cache",
	}

	tokenValidator := auth.NewTokenValidator("", false, logger)

	srv := NewServer(&ServerConfig{
		ControlAddr:  ":0",
		DataAddr:     ":0",
		DataHost:     "localhost",
		PublicURL:    "http://localhost:8080",
		CollectorURL: "",
		TempoURL:     "",
		LokiURL:      "",
		VictoriaURL:  "",
	}, logger, observ.NewMetrics(nil), mintingCA, fleetManager, sessions, cacheBackend, versionResolver, tokenValidator)

	return srv
}

// newTestEngine creates a route.Engine and registers all handlers for testing.
func newTestEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/healthz", s.handleHealthz)
	e.GET("/readyz", s.handleReadyz)
	e.POST("/v1/engines", s.handleEngines)
	e.GET("/api/v1/traces/:traceID", s.handleTracesDetail)
	e.GET("/api/v1/traces/:traceID/logs", s.handleTracesLogs)
	e.GET("/api/v1/logs/:traceID", s.handleLogsRoutes)
	e.GET("/api/v1/fleet", s.handleFleetInfo)
	e.GET("/api/v1/cache", s.handleCacheInfo)
	e.GET("/api/v1/versions", s.handleAdminVersions)
	return e
}

func TestHandleHealthz(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/healthz", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleReadyz(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/readyz", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleEnginesSuccess(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	body := `{"image":"registry.dagger.io/engine:v0.21.4","trace_id":"test-001"}`
	resp := ut.PerformRequest(e, "POST", "/v1/engines", &ut.Body{
		Body: strings.NewReader(body),
		Len:  len(body),
	}, ut.Header{Key: "Content-Type", Value: "application/json"})

	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.Result().StatusCode())
	}

	var engResp EngineSpecResponse
	if err := json.Unmarshal(resp.Result().Body(), &engResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if engResp.InstanceID == "" {
		t.Fatal("empty instance_id")
	}
	if engResp.Cert == nil || len(engResp.Cert.CertificateChain) == 0 {
		t.Fatal("empty cert in response")
	}
}

func TestHandleEnginesInvalidJSON(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	body := `not-json`
	resp := ut.PerformRequest(e, "POST", "/v1/engines", &ut.Body{
		Body: strings.NewReader(body),
		Len:  len(body),
	}, ut.Header{Key: "Content-Type", Value: "application/json"})

	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}

func TestHandleEnginesBadVersion(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	body := `{"image":"registry.dagger.io/engine:invalid","trace_id":"test-001"}`
	resp := ut.PerformRequest(e, "POST", "/v1/engines", &ut.Body{
		Body: strings.NewReader(body),
		Len:  len(body),
	}, ut.Header{Key: "Content-Type", Value: "application/json"})

	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}

func TestHandleFleetInfo(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCacheInfo(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/api/v1/cache", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}

	var info map[string]string
	if err := json.Unmarshal(resp.Result().Body(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info["backend"] != "registry" {
		t.Fatalf("backend = %q", info["backend"])
	}
}

func TestHandleAdminVersions(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/api/v1/versions", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleMetricsProxyNoVictora(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)
	e.GET("/api/v1/metrics", s.handleMetricsProxy)

	resp := ut.PerformRequest(e, "GET", "/api/v1/metrics", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}

	var help map[string]interface{}
	if err := json.Unmarshal(resp.Result().Body(), &help); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if help["fleet"] == nil {
		t.Fatal("expected fleet endpoint in help response")
	}
}

func TestTracesList(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)
	e.GET("/api/v1/traces", s.handleTracesList)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestWriteError(t *testing.T) {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/test-error", func(ctx context.Context, c *app.RequestContext) {
		writeError(c, http.StatusNotFound, "test error message")
	})

	resp := ut.PerformRequest(e, "GET", "/test-error", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Result().StatusCode())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(resp.Result().Body(), &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp.Message != "test error message" {
		t.Fatalf("message = %q", errResp.Message)
	}
}

func TestWriteJSON(t *testing.T) {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/test-json", func(ctx context.Context, c *app.RequestContext) {
		writeJSON(c, map[string]string{"key": "value"})
	})

	resp := ut.PerformRequest(e, "GET", "/test-json", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}

	var data map[string]string
	if err := json.Unmarshal(resp.Result().Body(), &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if data["key"] != "value" {
		t.Fatalf("key = %q", data["key"])
	}
}

func TestHandleTracesDetailRoute(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/test-trace-001", nil)
	// Without tempo configured, this will return 404 (trace not found)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Result().StatusCode())
	}
}

func TestHandleTracesLogsRoute(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	resp := ut.PerformRequest(e, "GET", "/api/v1/traces/test-trace-001/logs", nil)
	// Without loki configured, this will return 404
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Result().StatusCode())
	}
}

func TestHandleEnginesBodyTooLarge(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	// Create a body larger than 1 MB
	largeBody := strings.Repeat("x", 2*1024*1024)
	body := fmt.Sprintf(`{"image":"%s","trace_id":"test-001"}`, largeBody)
	resp := ut.PerformRequest(e, "POST", "/v1/engines", &ut.Body{
		Body: strings.NewReader(body),
		Len:  len(body),
	}, ut.Header{Key: "Content-Type", Value: "application/json"})

	if resp.Result().StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.Result().StatusCode())
	}
}
