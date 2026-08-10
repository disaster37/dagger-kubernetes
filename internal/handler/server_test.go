package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newTestServer builds a Server with the legacy (no-auth) mode for the
// pre-existing engine/fleet/cache tests that don't care about RBAC.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestEnv(t, true).server
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

func TestHandleEnginesBodyTooLarge(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

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

// TestClampLimit verifies the limit query-param helper.
func TestClampLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 100},
		{"0", 100},
		{"-1", 100},
		{"abc", 100},
		{"50", 50},
		{"500", 500},
		{"1000", 500},
		{"100", 100},
	}
	for _, tc := range cases {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestValidTraceID verifies the client-supplied trace ID bounds.
func TestValidTraceID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true}, // optional field
		{"abc123", true},
		{"trace-rbac-1", true},
		{"DEADBEEF0123456789abcdef", true},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false}, // too long
		{"bad space", false},
		{"bad/slash", false},
		{"bad\nline", false},
		{"-leading-dash", false},
	}
	for _, tc := range cases {
		if got := validTraceID(tc.in); got != tc.want {
			t.Errorf("validTraceID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestHandleEnginesInvalidTraceID verifies oversized/malformed trace IDs are
// rejected before persistence (CWE-770/CWE-20).
func TestHandleEnginesInvalidTraceID(t *testing.T) {
	s := newTestServer(t)
	e := newTestEngine(s)

	body := fmt.Sprintf(`{"image":"registry.dagger.io/engine:v0.21.4","trace_id":"%s"}`, strings.Repeat("a", 200))
	resp := ut.PerformRequest(e, "POST", "/v1/engines", &ut.Body{
		Body: strings.NewReader(body),
		Len:  len(body),
	}, ut.Header{Key: "Content-Type", Value: "application/json"})

	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}

// TestSecurityHeaders verifies the hardening response headers (CWE-1021,
// CWE-200) are set on every response.
func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t)
	e := route.NewEngine(config.NewOptions(nil))
	e.Use(s.securityHeaders())
	e.GET("/healthz", s.handleHealthz)

	resp := ut.PerformRequest(e, "GET", "/healthz", nil)
	h := &resp.Result().Header
	for key, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := string(h.Peek(key)); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// Compile-time assertions that the test helpers wire the expected types.
var (
	_ *service.Manager    = (*service.Manager)(nil)
	_ *repository.LiveHub = (*repository.LiveHub)(nil)
	_ domain.SessionStore = (*service.Store)(nil)
	_ *observ.Metrics     = (*observ.Metrics)(nil)
)
