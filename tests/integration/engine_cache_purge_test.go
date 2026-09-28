package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fakeIntegrationFleetProvider implements the methods the purge service uses
// (replicas + PVCs); the embedded nil interface satisfies the rest of the
// contract.
type fakeIntegrationFleetProvider struct {
	domain.FleetProvider
	versions  []string
	replicas  map[string][]domain.Replica
	pvcs      map[string][]domain.PVCInfo
	deleteErr map[string]error
}

func (p *fakeIntegrationFleetProvider) GetReplicas(version string) ([]domain.Replica, error) {
	return p.replicas[version], nil
}

func (p *fakeIntegrationFleetProvider) AllVersions() ([]string, error) {
	return p.versions, nil
}

func (p *fakeIntegrationFleetProvider) ListPVCs(version string) ([]domain.PVCInfo, error) {
	return p.pvcs[version], nil
}

func (p *fakeIntegrationFleetProvider) DeletePVC(name string) error {
	if err, ok := p.deleteErr[name]; ok {
		return err
	}
	return nil
}

var _ domain.FleetProvider = (*fakeIntegrationFleetProvider)(nil)

// purgePVCName is the StatefulSet PVC naming convention (test version).
func purgePVCName(ordinal int) string {
	return fmt.Sprintf("dagger-kubernetes-%s-%d", domain.StsName("v0.19.0"), ordinal)
}

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
// provider (2 replicas) and a fake prune collaborator. The mutators run before
// the server starts, letting a test reshape the provider (replicas, PVCs).
// The real HTTP-session transport is validated on the live cluster, not
// in-process.
func newEngineCachePurgeTestEnv(t *testing.T, mutate ...func(*fakeIntegrationFleetProvider)) *engineCachePurgeTestEnv {
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
	for _, opt := range mutate {
		opt(provider)
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

// postPurge runs the purge endpoint for version and returns the HTTP status
// plus the decoded result.
func postPurge(t *testing.T, env *engineCachePurgeTestEnv, version string) (int, domain.EngineCachePurgeResult) {
	t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/fleet/%s/purge-cache", env.base, version), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.adminToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST purge: %v", err)
	}
	defer resp.Body.Close()
	var result domain.EngineCachePurgeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode purge: %v", err)
	}
	return resp.StatusCode, result
}

func TestEngineCachePurgeEndToEnd(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t)

	status, result := postPurge(t, env, "v0.19.0")
	if status != http.StatusOK {
		t.Fatalf("purge status = %d, want 200", status)
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
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/fleet/v0.19.0/purge-cache", env.base), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", env.adminToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	var got domain.EngineCachePurgeResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || got.State != "completed" {
		t.Fatalf("status = %d/%s, want 200/completed", resp.StatusCode, got.State)
	}
}

func TestEngineCachePurgeFleetNotFound(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t)

	status, _ := postPurge(t, env, "v0.20.0")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if len(env.pruner.calls) != 0 {
		t.Fatal("no pruner calls expected")
	}
}

func TestEngineCachePurgeDeletesOrphanedPVCs(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t, func(p *fakeIntegrationFleetProvider) {
		p.pvcs = map[string][]domain.PVCInfo{
			"v0.19.0": {
				{Name: purgePVCName(0), Ordinal: 0},
				{Name: purgePVCName(1), Ordinal: 1},
				{Name: purgePVCName(2), Ordinal: 2},
			},
		}
	})

	status, result := postPurge(t, env, "v0.19.0")
	if status != http.StatusOK {
		t.Fatalf("purge status = %d, want 200", status)
	}
	if result.State != "completed" || result.Replicas != 2 {
		t.Fatalf("result = %+v, want completed/2", result)
	}
	// Only the orphaned ordinal 2 is deleted; the running runners keep theirs.
	if len(result.PVCs) != 1 {
		t.Fatalf("pvcs = %+v, want only the orphaned ordinal 2", result.PVCs)
	}
	got := result.PVCs[0]
	if got.Ordinal != 2 || !got.Deleted || got.Error != "" {
		t.Fatalf("pvc = %+v, want ordinal 2 deleted", got)
	}
	if want := purgePVCName(2); got.PVCName != want {
		t.Fatalf("pvc name = %q, want %q", got.PVCName, want)
	}
}

func TestEngineCachePurgeZeroReplicasDeletesAllPVCs(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t, func(p *fakeIntegrationFleetProvider) {
		p.replicas = nil
		p.pvcs = map[string][]domain.PVCInfo{
			"v0.19.0": {
				{Name: purgePVCName(0), Ordinal: 0},
				{Name: purgePVCName(1), Ordinal: 1},
				{Name: purgePVCName(2), Ordinal: 2},
			},
		}
	})

	status, result := postPurge(t, env, "v0.19.0")
	if status != http.StatusOK {
		t.Fatalf("purge status = %d, want 200", status)
	}
	if result.State != "completed" || result.Replicas != 0 {
		t.Fatalf("result = %+v, want completed/0", result)
	}
	if len(result.PVCs) != 3 {
		t.Fatalf("pvcs = %+v, want all 3 deleted", result.PVCs)
	}
	for i, pvc := range result.PVCs {
		if pvc.Ordinal != i || !pvc.Deleted {
			t.Fatalf("pvcs[%d] = %+v, want ordinal %d deleted", i, pvc, i)
		}
	}
	if len(env.pruner.calls) != 0 {
		t.Fatal("no pruner calls expected")
	}
}

func TestEngineCachePurgePVCDeleteError(t *testing.T) {
	env := newEngineCachePurgeTestEnv(t, func(p *fakeIntegrationFleetProvider) {
		p.pvcs = map[string][]domain.PVCInfo{
			"v0.19.0": {{Name: purgePVCName(2), Ordinal: 2}},
		}
		p.deleteErr = map[string]error{purgePVCName(2): errors.New("delete boom")}
	})

	status, result := postPurge(t, env, "v0.19.0")
	if status != http.StatusOK {
		t.Fatalf("purge status = %d, want 200", status)
	}
	if result.State != "completed" {
		t.Fatalf("state = %q, want completed", result.State)
	}
	if len(result.PVCs) != 1 {
		t.Fatalf("pvcs = %+v, want 1 entry", result.PVCs)
	}
	got := result.PVCs[0]
	if got.Deleted || !strings.Contains(got.Error, "delete boom") {
		t.Fatalf("pvc = %+v, want per-PVC error", got)
	}
}
