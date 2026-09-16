package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fakeIntegrationFleetProvider implements only the two methods the purge
// service uses; the embedded nil interface satisfies the rest of the contract.
type fakeIntegrationFleetProvider struct {
	domain.FleetProvider
	versions []string
	replicas map[string][]domain.Replica
}

func (p *fakeIntegrationFleetProvider) GetReplicas(version string) ([]domain.Replica, error) {
	return p.replicas[version], nil
}

func (p *fakeIntegrationFleetProvider) AllVersions() ([]string, error) {
	return p.versions, nil
}

var _ domain.FleetProvider = (*fakeIntegrationFleetProvider)(nil)

// fakeIntegrationPruner records the pod IPs it was asked to prune.
type fakeIntegrationPruner struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeIntegrationPruner) PruneLocalCache(_ context.Context, podIP, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, podIP)
	return nil
}

// engineCachePurgeTestEnv is a running control-plane server with auth wired, an
// admin API token, and a fake prune collaborator.
type engineCachePurgeTestEnv struct {
	base       string
	adminToken string
	pruner     *fakeIntegrationPruner
}

// newEngineCachePurgeTestEnv starts a real Hertz supervisor with a fake fleet
// provider (2 replicas) and a fake prune collaborator. The real HTTP-session
// transport is validated on the live cluster, not in-process.
func newEngineCachePurgeTestEnv(t *testing.T) *engineCachePurgeTestEnv {
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

	provider := &fakeIntegrationFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {
				{Name: "dagger-engine-v0-19-0-0", Ordinal: 0, PodIP: "10.0.0.1"},
				{Name: "dagger-engine-v0-19-0-1", Ordinal: 1, PodIP: "10.0.0.2"},
			},
		},
	}
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))

	pruner := &fakeIntegrationPruner{}
	purgeSvc := service.NewEngineCachePurgeService(provider, pruner, logger)

	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(
		service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger),
		groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

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
		VersionResolver: versionResolver, Auth: authSvc,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces,
		Logs: logsClient, JWT: jwtSvc, EngineCachePurger: purgeSvc,
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

	return &engineCachePurgeTestEnv{
		base:       fmt.Sprintf("http://localhost%s", controlAddr),
		adminToken: adminToken,
		pruner:     pruner,
	}
}

func TestEngineCachePurgeEndToEnd(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t)

	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/fleet/v0.19.0/purge-cache", env.base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.adminToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST purge: %v", err)
	}
	var result domain.EngineCachePurgeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode purge: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("purge status = %d, want 200", resp.StatusCode)
	}
	if result.State != "completed" || result.Replicas != 2 || len(result.Pods) != 2 {
		t.Fatalf("result = %+v, want completed/2 pods", result)
	}
	for _, p := range result.Pods {
		if !p.Pruned {
			t.Fatalf("pod = %+v, want pruned", p)
		}
	}
	if len(env.pruner.calls) != 2 {
		t.Fatalf("pruner calls = %v, want 2", env.pruner.calls)
	}
	// Per-pod isolation: the pruner must have seen exactly the two pod IPs.
	sort.Strings(env.pruner.calls)
	if !slices.Equal(env.pruner.calls, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("pruner calls = %v, want both pod IPs", env.pruner.calls)
	}

	// GET returns the recorded status.
	req, _ = http.NewRequest("GET", fmt.Sprintf("%s/api/v1/fleet/v0.19.0/purge-cache", env.base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.adminToken))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	var status domain.EngineCachePurgeResult
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || status.State != "completed" {
		t.Fatalf("status = %d/%s, want 200/completed", resp.StatusCode, status.State)
	}
}

func TestEngineCachePurgeFleetNotFound(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t)

	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/fleet/v0.20.0/purge-cache", env.base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.adminToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST purge: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if len(env.pruner.calls) != 0 {
		t.Fatal("no pruner calls expected")
	}
}
