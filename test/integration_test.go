package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/disaster/dagger-cache/internal/api"
	"github.com/disaster/dagger-cache/internal/ca"
	"github.com/disaster/dagger-cache/internal/cache"
	"github.com/disaster/dagger-cache/internal/fleet"
	"github.com/disaster/dagger-cache/internal/session"
	"github.com/disaster/dagger-cache/internal/version"
	"go.uber.org/zap"
)

func TestProvisionEngineFlow(t *testing.T) {
	logger := zap.NewNop()
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
	}, logger)

	cacheBackend := &cache.Backend{
		Type:     "registry",
		Registry: "cache.reg/dagger-cache",
	}

	server := api.NewServer(&api.ServerConfig{
		ControlAddr:  ":18080",
		DataAddr:     ":18443",
		DataHost:     "localhost",
		PublicURL:    "http://localhost:18080",
		UIURL:        "http://localhost:5173",
		CollectorURL: "http://localhost:4318",
		TempoURL:     "http://localhost:3200",
	}, logger, mintingCA, fleetManager, sessions, cacheBackend, versionResolver)

	serverTLS, err := mintingCA.TLSCertificate()
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(500 * time.Millisecond)

	reqBody := map[string]string{
		"image":    "registry.dagger.io/engine:v0.21.4",
		"trace_id": "test-trace-001",
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	req, err := http.NewRequest("POST", "http://localhost:18080/v1/engines", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.SetBasicAuth("test-token", "")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/engines: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var engResp api.EngineSpecResponse
	if err := json.NewDecoder(resp.Body).Decode(&engResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if engResp.InstanceID == "" {
		t.Fatal("empty instance_id")
	}
	if engResp.Cert == nil || len(engResp.Cert.CertificateChain) == 0 {
		t.Fatal("empty cert in response")
	}
	if engResp.Location != "k8s" {
		t.Fatalf("expected location k8s, got %s", engResp.Location)
	}
}

func TestHealthEndpoint(t *testing.T) {
	logger := zap.NewNop()
	mintingCA, _ := ca.NewMintingCA(2 * time.Hour)
	versionResolver, _ := version.NewResolver("v0.19.0", nil, nil)
	sessions := session.NewStore(2 * time.Minute)
	provider := fleet.NewStubProvider()
	fleetManager := fleet.NewManager(provider, sessions, fleet.ManagerConfig{}, logger)
	cacheBackend := &cache.Backend{Type: "registry", Registry: "cache.reg/dagger-cache"}

	server := api.NewServer(&api.ServerConfig{
		ControlAddr: ":18081",
		DataAddr:    ":18444",
	}, logger, mintingCA, fleetManager, sessions, cacheBackend, versionResolver)

	serverTLS, _ := mintingCA.TLSCertificate()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Start(ctx, serverTLS); err != nil {
		t.Fatal("Start:", err)
	}
	defer server.Shutdown(context.Background())

	time.Sleep(500 * time.Millisecond)

	resp, err := http.Get("http://localhost:18081/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
