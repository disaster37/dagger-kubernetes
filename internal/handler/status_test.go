package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestHandlePlatformStatusAuthGating(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/status", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Result().StatusCode())
	}

	auth := env.loginAsAdmin(t)
	resp = ut.PerformRequest(e, "GET", "/api/v1/status", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandlePlatformStatusShape(t *testing.T) {
	env := newTestEnv(t)
	env.server.status = &stubStatusProvider{status: &domain.PlatformStatus{
		State: domain.ServiceOK,
		Services: []domain.ServiceStatus{
			{Name: "supervisor", Category: "control", State: domain.ServiceOK, Configured: true},
		},
	}}
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/status", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}

	var status domain.PlatformStatus
	if err := json.Unmarshal(resp.Result().Body(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.State != domain.ServiceOK {
		t.Fatalf("state = %q", status.State)
	}
	if len(status.Services) != 1 || status.Services[0].Name != "supervisor" {
		t.Fatalf("services = %+v", status.Services)
	}
}

func TestHandleHealthzDegraded(t *testing.T) {
	env := newTestEnv(t)
	env.server.status = &stubStatusProvider{status: &domain.PlatformStatus{
		State:    domain.ServiceDown,
		Services: []domain.ServiceStatus{},
	}}
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/healthz", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 (liveness never 5xx), got %d", resp.Result().StatusCode())
	}
	var body map[string]domain.ServiceState
	if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != domain.ServiceDegraded {
		t.Fatalf("state = %q, want degraded", body["state"])
	}
}

func TestHandleReadyzCacheDownStillReady(t *testing.T) {
	env := newTestEnv(t)
	env.server.status = &stubStatusProvider{status: &domain.PlatformStatus{
		State: domain.ServiceDown,
		Services: []domain.ServiceStatus{
			{Name: "supervisor", Category: "control", State: domain.ServiceOK, Configured: true},
			{Name: "cache", Category: "cache", State: domain.ServiceDown, Configured: true},
		},
	}}
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/readyz", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 (down cache must not gate readiness), got %d", resp.Result().StatusCode())
	}
	var body map[string]domain.ServiceState
	if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != domain.ServiceOK {
		t.Fatalf("state = %q, want ok", body["state"])
	}
}

func TestHandleReadyzSupervisorDown(t *testing.T) {
	env := newTestEnv(t)
	env.server.status = &stubStatusProvider{status: &domain.PlatformStatus{
		State: domain.ServiceDown,
		Services: []domain.ServiceStatus{
			{Name: "supervisor", Category: "control", State: domain.ServiceDown, Configured: true},
		},
	}}
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/readyz", nil)
	if resp.Result().StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.Result().StatusCode())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["state"] != "down" {
		t.Fatalf("state = %v, want down", body["state"])
	}
}

func TestReadinessState(t *testing.T) {
	tests := []struct {
		name     string
		services []domain.ServiceStatus
		want     domain.ServiceState
	}{
		{
			name: "supervisor ok with down cache",
			services: []domain.ServiceStatus{
				{Name: "supervisor", State: domain.ServiceOK, Configured: true},
				{Name: "cache", State: domain.ServiceDown, Configured: true},
			},
			want: domain.ServiceOK,
		},
		{
			name: "supervisor ok with down telemetry",
			services: []domain.ServiceStatus{
				{Name: "supervisor", State: domain.ServiceOK, Configured: true},
				{Name: "victoria", State: domain.ServiceDown, Configured: true},
			},
			want: domain.ServiceOK,
		},
		{
			name:     "supervisor down",
			services: []domain.ServiceStatus{{Name: "supervisor", State: domain.ServiceDown, Configured: true}},
			want:     domain.ServiceDown,
		},
		{
			name:     "no supervisor row",
			services: []domain.ServiceStatus{{Name: "cache", State: domain.ServiceDown, Configured: true}},
			want:     domain.ServiceOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readinessState(&domain.PlatformStatus{Services: tc.services}); got != tc.want {
				t.Errorf("readinessState() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHealthzState(t *testing.T) {
	tests := []struct {
		in   domain.ServiceState
		want domain.ServiceState
	}{
		{domain.ServiceOK, domain.ServiceOK},
		{domain.ServiceDegraded, domain.ServiceDegraded},
		{domain.ServiceDown, domain.ServiceDegraded},
		{domain.ServiceUnknown, domain.ServiceOK},
	}
	for _, tc := range tests {
		if got := healthzState(tc.in); got != tc.want {
			t.Errorf("healthzState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
