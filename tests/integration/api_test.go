package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// TestProvisionEngineWithAPIToken proves the real Dagger client contract:
// plain `Authorization: Bearer <token>` authenticates and provisions an engine.
func TestProvisionEngineWithAPIToken(t *testing.T) {
	logger := observ.NewTestLogger()
	dbPath := t.TempDir() + "/test.db"
	db, err := repository.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	defer db.Close()

	userRepo := repository.NewUserRepo(db)
	groupRepo := repository.NewGroupRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	traceMetaRepo := repository.NewTraceMetaRepo(db)

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

	authSvc := service.NewAuthService(service.AuthServiceConfig{}, usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(service.NewProjectService(repository.NewProjectRepo(db), groupRepo, logger), groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: ":18090",
		DataAddr:    ":18453",
		DataHost:    "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Tokens: tokensSvc, Quota: quotaSvc, Attribution: attributionSvc,
		TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Shutdown(context.Background())
	time.Sleep(500 * time.Millisecond)

	reqBody := map[string]string{"image": "registry.dagger.io/engine:v0.21.4", "trace_id": "test-trace-001"}
	b, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", "http://localhost:18090/v1/engines", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/engines: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var engResp handler.EngineSpecResponse
	if err := json.NewDecoder(resp.Body).Decode(&engResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if engResp.InstanceID == "" || engResp.UserID != "admin" {
		t.Fatalf("bad response: %+v", engResp)
	}
}

func TestHealthEndpoint(t *testing.T) {
	logger := observ.NewTestLogger()
	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}

	dbPath := t.TempDir() + "/test.db"
	db, err := repository.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	defer db.Close()

	userRepo := repository.NewUserRepo(db)
	groupRepo := repository.NewGroupRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	traceMetaRepo := repository.NewTraceMetaRepo(db)
	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("health-secret-32-bytes-ok!!!!!!!"), 15*time.Minute, 168*time.Hour)
	authSvc := service.NewAuthService(service.AuthServiceConfig{Disabled: true}, usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(service.NewProjectService(repository.NewProjectRepo(db), groupRepo, logger), groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	server := handler.NewServer(&handler.ServerConfig{
		ControlAddr: ":18091",
		DataAddr:    ":18454",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, AuthDisabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Start(ctx, serverTLS); err != nil {
		t.Fatal("Start:", err)
	}
	defer server.Shutdown(context.Background())
	time.Sleep(500 * time.Millisecond)

	resp, err := http.Get("http://localhost:18091/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
