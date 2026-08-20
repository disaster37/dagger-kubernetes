package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const ciWrapperTraceID = "abcdef0123456789abcdef0123456789"

// startPipelineURLServer boots a full supervisor wired like the real
// control plane (Raft store, stub fleet, admin + API token) with the
// pipeline-view base set to https://supv.example.com. It returns the
// control-plane URL and an admin API token.
func startPipelineURLServer(t *testing.T, controlAddr, dataAddr string) (string, string) {
	t.Helper()
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("integration-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)

	admin, err := usersSvc.Create(context.Background(), "admin", "password123", domain.RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	adminToken, _, err := tokensSvc.Generate(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(
		service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger),
		groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: controlAddr,
		DataAddr:    dataAddr,
		DataHost:    "localhost",
		PipelineURL: "https://supv.example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	time.Sleep(500 * time.Millisecond)

	return fmt.Sprintf("http://localhost%s", controlAddr), adminToken
}

// TestPipelineViewURLEndpoint proves the real handler wiring: provisioning an
// engine records a trace, and GET /api/v1/traces/:traceID/url returns the
// self-hosted pipeline-view URL for it.
func TestPipelineViewURLEndpoint(t *testing.T) {
	serverURL, adminToken := startPipelineURLServer(t, ":18097", ":18458")

	reqBody := map[string]string{"image": "registry.dagger.io/engine:v0.21.4", "trace_id": "trace-url-int"}
	b, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/v1/engines", serverURL), bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/engines: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/engines: status %d, want 201", resp.StatusCode)
	}

	urlReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/traces/trace-url-int/url", serverURL), nil)
	urlReq.Header.Set("Authorization", "Bearer "+adminToken)
	urlResp, err := http.DefaultClient.Do(urlReq)
	if err != nil {
		t.Fatalf("GET /api/v1/traces/trace-url-int/url: %v", err)
	}
	defer func() { _ = urlResp.Body.Close() }()
	if urlResp.StatusCode != http.StatusOK {
		t.Fatalf("GET url: status %d, want 200", urlResp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(urlResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["trace_id"] != "trace-url-int" {
		t.Fatalf("trace_id = %v, want trace-url-int", body["trace_id"])
	}
	if body["url"] != "https://supv.example.com/pipelines/trace-url-int" {
		t.Fatalf("url = %v, want https://supv.example.com/pipelines/trace-url-int", body["url"])
	}
}

// TestCIWrapperPrintsSelfHostedURL proves the client-facing behavior
// end-to-end: the compiled dagger-cache-ci wrapper, pointed at a running
// integration server with a fake dagger on PATH, prints the self-hosted
// pipeline-view link using the /pipelines/<id> path.
func TestCIWrapperPrintsSelfHostedURL(t *testing.T) {
	serverURL, adminToken := startPipelineURLServer(t, ":18098", ":18459")

	binDir := t.TempDir()
	bin := filepath.Join(binDir, "dagger-cache-ci")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/ci")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dagger-cache-ci: %v\n%s", err, out)
	}

	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "dagger")
	script := fmt.Sprintf("#!/bin/sh\necho '%s' >&2\nexit 0\n", ciWrapperTraceID)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dagger: %v", err)
	}

	cmd := exec.Command(bin,
		"--server", serverURL,
		"--token", adminToken,
		"--ui-url", "https://supv.example.com",
		"--config", filepath.Join(t.TempDir(), "missing.yaml"),
		"call", "foo",
	)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s%c%s", fakeDir, os.PathListSeparator, os.Getenv("PATH")))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dagger-cache-ci: %v\n%s", err, stderr.String())
	}

	want := fmt.Sprintf("Pipeline View: https://supv.example.com/pipelines/%s", ciWrapperTraceID)
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want containing %q", stderr.String(), want)
	}
}
