package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// stubRaftCleanState stubs domain.RaftCleanState for status tests.
type stubRaftCleanState struct {
	clean bool
}

func (s *stubRaftCleanState) IsCleanState() bool { return s.clean }

// statusTestEnv is a running control-plane server with auth wired and an admin
// API token.
type statusTestEnv struct {
	base       string
	adminToken string
}

// newStatusTestEnv builds the shared server fixture. raftCleanState is the
// domain.RaftCleanState wired into the status service (nil = raft clean, as in
// single-node/dev setups).
func newStatusTestEnv(t *testing.T, raftCleanState domain.RaftCleanState) *statusTestEnv {
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
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger), groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	statusSvc := service.NewStatusService(&domain.Config{}, fleetManager, logger, raftCleanState)

	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr := listenerAddr(controlLn)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        listenerAddr(dataLn),
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Tokens: tokensSvc, Quota: quotaSvc, Attribution: attributionSvc,
		TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
		StatusProvider: statusSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(500 * time.Millisecond)

	return &statusTestEnv{
		base:       fmt.Sprintf("http://localhost%s", controlAddr),
		adminToken: adminToken,
	}
}

// TestStatusRaftNotCleanSupervisorDownIntegration proves the HTTP status API
// contract that the UI's services view renders from: when the Raft consensus
// layer is not in a clean state, the supervisor row must be "down" (never
// green/ok) with the "raft consensus not clean" message, and the rollup must
// be "down" so the header indicator turns red.
func TestStatusRaftNotCleanSupervisorDownIntegration(t *testing.T) {
	env := newStatusTestEnv(t, &stubRaftCleanState{clean: false})

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/status", env.base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.adminToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	var status domain.PlatformStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || status.State != domain.ServiceDown {
		t.Fatalf("status = %d/%s, want 200/down", resp.StatusCode, status.State)
	}
	foundSupervisor := false
	for _, svc := range status.Services {
		if svc.Name == "supervisor" {
			foundSupervisor = true
			if svc.State != domain.ServiceDown {
				t.Fatalf("supervisor state = %q, want down (never ok/green when raft is not clean)", svc.State)
			}
			if svc.Message != "raft consensus not clean" {
				t.Fatalf("supervisor message = %q, want 'raft consensus not clean'", svc.Message)
			}
		}
	}
	if !foundSupervisor {
		t.Fatal("supervisor service missing from status")
	}
}
