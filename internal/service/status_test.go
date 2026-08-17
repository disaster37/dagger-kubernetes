package service

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func openTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return "http://" + ln.Addr().String()
}

func closedTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func newStatusServiceWithRegistry(t *testing.T, cfg *domain.Config, cache *Cache, fleet *Manager) (*StatusService, *httptest.Server) {
	t.Helper()
	reg := newFakeRegistry()
	ts := httptest.NewServer(reg.handler())
	t.Cleanup(ts.Close)
	svc := NewStatusService(cfg, cache, repository.NewRegistryStatsClient(ts.Listener.Addr().String()), fleet, observ.NewTestLogger())
	return svc, ts
}

func emptyFleet() *Manager {
	return NewManager(&stubFleetProvider{}, NewStore(2*time.Minute), ManagerConfig{}, observ.NewTestLogger(), observ.NewMetrics(nil))
}

func TestStatusAllOK(t *testing.T) {
	cfg := &domain.Config{
		Telemetry: domain.TelemetryConfig{
			CollectorURL: openTCP(t),
			TempoURL:     openTCP(t),
			LokiURL:      openTCP(t),
			VictoriaURL:  openTCP(t),
		},
	}
	svc, _ := newStatusServiceWithRegistry(t, cfg, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, emptyFleet())

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != domain.ServiceOK {
		t.Fatalf("rollup = %q, want ok", status.State)
	}
	if len(status.Services) != 7 {
		t.Fatalf("services = %d, want 7", len(status.Services))
	}
	for _, svc := range status.Services {
		if svc.State != domain.ServiceOK {
			t.Errorf("service %s state = %q, want ok", svc.Name, svc.State)
		}
	}
}

func TestStatusTelemetryDown(t *testing.T) {
	cfg := &domain.Config{
		Telemetry: domain.TelemetryConfig{
			CollectorURL: closedTCP(t),
		},
	}
	svc, _ := newStatusServiceWithRegistry(t, cfg, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, emptyFleet())

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != domain.ServiceDown {
		t.Fatalf("rollup = %q, want down", status.State)
	}
	for _, svc := range status.Services {
		if svc.Name == "collector" && svc.State != domain.ServiceDown {
			t.Errorf("collector state = %q, want down", svc.State)
		}
	}
}

func TestStatusUnconfiguredNoRollupImpact(t *testing.T) {
	cfg := &domain.Config{} // no telemetry URLs
	svc, _ := newStatusServiceWithRegistry(t, cfg, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, emptyFleet())

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != domain.ServiceOK {
		t.Fatalf("rollup = %q, want ok", status.State)
	}
	for _, svc := range status.Services {
		if svc.Category == "telemetry" {
			if svc.State != domain.ServiceUnknown {
				t.Errorf("telemetry %s state = %q, want unknown", svc.Name, svc.State)
			}
			if svc.Configured {
				t.Errorf("telemetry %s configured = true, want false", svc.Name)
			}
		}
	}
}

func TestStatusFleetDegraded(t *testing.T) {
	cfg := &domain.Config{}
	fleet := NewManager(&stubFleetProvider{
		versions: []string{"v0.21.4"},
		replicas: map[string][]domain.Replica{
			"v0.21.4": {
				{Name: "p0", Version: "v0.21.4", Ready: false},
				{Name: "p1", Version: "v0.21.4", Ready: true},
			},
		},
	}, NewStore(2*time.Minute), ManagerConfig{}, observ.NewTestLogger(), observ.NewMetrics(nil))

	svc, _ := newStatusServiceWithRegistry(t, cfg, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, fleet)
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != domain.ServiceDegraded {
		t.Fatalf("rollup = %q, want degraded", status.State)
	}
	for _, svc := range status.Services {
		if svc.Name == "fleet" && svc.State != domain.ServiceDegraded {
			t.Errorf("fleet state = %q, want degraded", svc.State)
		}
	}
}

func TestStatusFleetDown(t *testing.T) {
	cfg := &domain.Config{}
	fleet := NewManager(&stubFleetProvider{allErr: errors.New("k8s down")}, NewStore(2*time.Minute), ManagerConfig{}, observ.NewTestLogger(), observ.NewMetrics(nil))

	svc, _ := newStatusServiceWithRegistry(t, cfg, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, fleet)
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != domain.ServiceDown {
		t.Fatalf("rollup = %q, want down", status.State)
	}
}

func TestStatusCacheS3(t *testing.T) {
	cfg := &domain.Config{}
	svc := NewStatusService(cfg, &Cache{Type: "s3", S3: domain.S3Ref{Bucket: "my-bucket"}}, nil, emptyFleet(), observ.NewTestLogger())

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, svc := range status.Services {
		if svc.Name == "cache" && svc.State != domain.ServiceOK {
			t.Errorf("cache state = %q, want ok", svc.State)
		}
	}
}

func TestStatusCacheDown(t *testing.T) {
	cfg := &domain.Config{}
	reg := newFakeRegistry()
	ts := httptest.NewServer(reg.handler())
	registry := repository.NewRegistryStatsClient(ts.Listener.Addr().String())
	ts.Close()

	svc := NewStatusService(cfg, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, registry, emptyFleet(), observ.NewTestLogger())
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != domain.ServiceDown {
		t.Fatalf("rollup = %q, want down", status.State)
	}
}

func TestStatusCacheS3Unconfigured(t *testing.T) {
	svc := NewStatusService(&domain.Config{}, &Cache{Type: "s3"}, nil, emptyFleet(), observ.NewTestLogger())
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, svc := range status.Services {
		if svc.Name == "cache" && svc.State != domain.ServiceDown {
			t.Errorf("cache state = %q, want down", svc.State)
		}
	}
}

func TestStatusCacheUnknownBackend(t *testing.T) {
	svc := NewStatusService(&domain.Config{}, &Cache{Type: "bogus"}, nil, emptyFleet(), observ.NewTestLogger())
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, svc := range status.Services {
		if svc.Name == "cache" && svc.State != domain.ServiceDown {
			t.Errorf("cache state = %q, want down", svc.State)
		}
	}
}

func TestStatusCacheRegistryNil(t *testing.T) {
	svc := NewStatusService(&domain.Config{}, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, nil, emptyFleet(), observ.NewTestLogger())
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, svc := range status.Services {
		if svc.Name == "cache" && svc.State != domain.ServiceDown {
			t.Errorf("cache state = %q, want down", svc.State)
		}
	}
}

func TestStatusFleetNilManager(t *testing.T) {
	svc := NewStatusService(&domain.Config{}, &Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}, nil, nil, observ.NewTestLogger())
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, svc := range status.Services {
		if svc.Name == "fleet" && svc.State != domain.ServiceOK {
			t.Errorf("fleet state = %q, want ok (nil manager)", svc.State)
		}
	}
}

func TestProbeTCP(t *testing.T) {
	ctx := context.Background()

	if err := probeTCP(ctx, openTCP(t)); err != nil {
		t.Fatalf("probeTCP(open) = %v", err)
	}
	if err := probeTCP(ctx, closedTCP(t)); err == nil {
		t.Fatal("probeTCP(closed) should fail")
	}
	if err := probeTCP(ctx, "://bad-url"); err == nil {
		t.Fatal("probeTCP(bad url) should fail")
	}
	// Missing port defaults to 80 (refused in this test environment).
	if err := probeTCP(ctx, "http://127.0.0.1"); err == nil {
		t.Fatal("probeTCP(missing port) should fail against port 80")
	}
}

func TestRollup(t *testing.T) {
	tests := []struct {
		name     string
		services []domain.ServiceStatus
		want     domain.ServiceState
	}{
		{"all-ok", []domain.ServiceStatus{
			{Name: "a", Configured: true, State: domain.ServiceOK},
			{Name: "b", Configured: true, State: domain.ServiceOK},
		}, domain.ServiceOK},
		{"one-down", []domain.ServiceStatus{
			{Name: "a", Configured: true, State: domain.ServiceOK},
			{Name: "b", Configured: true, State: domain.ServiceDown},
		}, domain.ServiceDown},
		{"one-degraded", []domain.ServiceStatus{
			{Name: "a", Configured: true, State: domain.ServiceOK},
			{Name: "b", Configured: true, State: domain.ServiceDegraded},
		}, domain.ServiceDegraded},
		{"down-wins-over-degraded", []domain.ServiceStatus{
			{Name: "a", Configured: true, State: domain.ServiceDegraded},
			{Name: "b", Configured: true, State: domain.ServiceDown},
		}, domain.ServiceDown},
		{"unknown-ignored", []domain.ServiceStatus{
			{Name: "a", Configured: true, State: domain.ServiceOK},
			{Name: "b", Configured: false, State: domain.ServiceUnknown},
		}, domain.ServiceOK},
		{"empty", []domain.ServiceStatus{}, domain.ServiceOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rollup(tc.services); got != tc.want {
				t.Fatalf("rollup = %q, want %q", got, tc.want)
			}
		})
	}
}
