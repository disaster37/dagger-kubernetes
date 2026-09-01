package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// registryStub serves a minimal OCI Distribution v2 API with one repository,
// one tag, and delete enabled.
func registryStub(t *testing.T) *httptest.Server {
	t.Helper()
	manifest := `{"config":{"digest":"sha256:cfg","size":10},"layers":[{"digest":"sha256:l1","size":20},{"digest":"sha256:l2","size":30}]}`
	// Valid sha256:<64 hex> digest used as the manifest digest so the client's
	// digest-shape validation accepts it and the DELETE path matches.
	dgst := fmt.Sprintf("sha256:%s", strings.Repeat("a", 64))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/_catalog":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":["dagger-cache"]}`))
		case r.URL.Path == "/v2/dagger-cache/tags/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"dagger-cache","tags":["cache"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/dagger-cache/manifests/cache":
			w.Header().Set("Docker-Content-Digest", dgst)
			_, _ = w.Write([]byte(manifest))
		case r.Method == http.MethodHead && r.URL.Path == "/v2/dagger-cache/manifests/cache":
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == fmt.Sprintf("/v2/dagger-cache/manifests/%s", dgst):
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestCacheStatusAndPurgeIntegration(t *testing.T) {
	env := newCacheStatusTestEnv(t, nil)
	base := env.base
	adminToken := env.adminToken

	// --- GET /api/v1/cache ---
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/cache", base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", adminToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/cache: %v", err)
	}
	var stats domain.CacheStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache status = %d, want 200", resp.StatusCode)
	}
	if !stats.Running || !stats.Reachable {
		t.Fatalf("running=%v reachable=%v, want true", stats.Running, stats.Reachable)
	}
	if stats.TotalSize != 60 {
		t.Fatalf("total_size = %d, want 60", stats.TotalSize)
	}
	if stats.ObjectCount != 2 {
		t.Fatalf("object_count = %d, want 2", stats.ObjectCount)
	}
	if stats.Ref == nil || stats.Ref.Tag != "cache" {
		t.Fatalf("ref = %+v", stats.Ref)
	}

	// --- GET /api/v1/status ---
	req, _ = http.NewRequest("GET", fmt.Sprintf("%s/api/v1/status", base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", adminToken))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/status: %v", err)
	}
	var status domain.PlatformStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || status.State != domain.ServiceOK {
		t.Fatalf("status = %d/%s, want 200/ok", resp.StatusCode, status.State)
	}
	foundCache := false
	for _, svc := range status.Services {
		if svc.Name == "cache" {
			foundCache = true
			if svc.State != domain.ServiceOK {
				t.Fatalf("cache service state = %q, want ok", svc.State)
			}
		}
	}
	if !foundCache {
		t.Fatal("cache service missing from status")
	}

	// --- POST /api/v1/cache/purge (admin) ---
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/api/v1/cache/purge", base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", adminToken))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/cache/purge: %v", err)
	}
	var purge domain.PurgeResult
	if err := json.NewDecoder(resp.Body).Decode(&purge); err != nil {
		t.Fatalf("decode purge: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("purge status = %d, want 200", resp.StatusCode)
	}
	if purge.Purged != 1 {
		t.Fatalf("purge = %+v, want purged=1", purge)
	}
}

// stubRaftCleanState stubs domain.RaftCleanState for status tests.
type stubRaftCleanState struct {
	clean bool
}

func (s *stubRaftCleanState) IsCleanState() bool { return s.clean }

// cacheStatusTestEnv is a running control-plane server with auth wired and an
// admin API token, backed by a stub registry.
type cacheStatusTestEnv struct {
	base       string
	adminToken string
}

// newCacheStatusTestEnv builds the shared server fixture. raftCleanState is
// the domain.RaftCleanState wired into the status service (nil = raft clean,
// as in single-node/dev setups).
func newCacheStatusTestEnv(t *testing.T, raftCleanState domain.RaftCleanState) *cacheStatusTestEnv {
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
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger), groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	// Stub registry + status/stats services.
	ts := registryStub(t)
	router := service.NewRegistryRouter(
		[]domain.RegistryBackend{{ID: "default", InternalAddr: ts.Listener.Addr().String()}},
		repository.NewCacheRoutesRepo(store),
		func(b domain.RegistryBackend) domain.RegistryClient {
			return repository.NewRegistryStatsClientWithAuth(b.InternalAddr, b.Username, b.Password)
		},
		logger,
	)
	cacheStatsSvc := service.NewCacheStatsService(cacheBackend, router, nil, domain.GCConfig{
		Enabled: false, MaxAge: 168 * time.Hour, Schedule: time.Hour,
	}, logger, observ.NewMetrics(nil))
	statusSvc := service.NewStatusService(&domain.Config{}, cacheBackend, router, fleetManager, logger, raftCleanState)

	controlAddr := freeAddr(t)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: controlAddr,
		DataAddr:    freeAddr(t),
		DataHost:    "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store), CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Tokens: tokensSvc, Quota: quotaSvc, Attribution: attributionSvc,
		TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
		CacheStatsProvider: cacheStatsSvc,
		CachePurger:        cacheStatsSvc,
		StatusProvider:     statusSvc,
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

	return &cacheStatusTestEnv{
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
	env := newCacheStatusTestEnv(t, &stubRaftCleanState{clean: false})

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
