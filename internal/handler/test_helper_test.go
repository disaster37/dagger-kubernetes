package handler

import (
	"context"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// testEnv bundles a Server with its backing services + repos for handler tests.
type testEnv struct {
	server   *Server
	users    *service.UserService
	groups   *service.GroupService
	projects *service.ProjectService
	tokens   *service.TokenService
	auth     *service.AuthService
	jwt      *service.JWTService
	sessions *service.Store
	store    *repository.RaftStore
}

// newTestStore builds a single-node in-memory Raft store for handler tests.
func newTestStore(t *testing.T) *repository.RaftStore {
	t.Helper()
	store, err := repository.NewInmemRaftStore("handler-test", observ.NewTestLogger(), 5*time.Second)
	if err != nil {
		t.Fatalf("NewInmemRaftStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestEnv builds a Server wired to a temp in-memory Raft store + stub fleet,
// with auth enabled by default. Pass authDisabled=true for the no-auth mode.
func newTestEnv(t *testing.T, authDisabled bool) *testEnv {
	t.Helper()
	logger := observ.NewTestLogger()

	store := newTestStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, testTokenEncKey())
	jwtSvc := service.NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 15*time.Minute, 168*time.Hour)

	// Bootstrap an admin user so admin routes work in tests.
	admin, err := usersSvc.Create(context.Background(), "admin", "password123", domain.RoleAdmin)
	if err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	_ = admin

	var legacy domain.TokenValidator
	authSvc := service.NewAuthService(service.AuthServiceConfig{Disabled: authDisabled}, usersSvc, groupRepo, tokensSvc, jwtSvc, legacy, logger)

	sessions := service.NewStore(2 * time.Minute)
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)

	mintingCA, err := repository.NewMintingCA(2 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	versionResolver, err := service.NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	connectSvc := service.NewConnectService(&domain.Config{
		Server:  domain.ServerConfig{PublicURL: "https://supv.example.com", DataHost: "data.example.com"},
		Cache:   domain.CacheConfig{Backend: "registry"},
		Version: domain.VersionConfig{Floor: "v0.19.0"},
	}, cacheBackend, versionResolver, tokensSvc, logger)

	srv := NewServer(&ServerConfig{
		ControlAddr: ":0",
		DataAddr:    ":0",
		DataHost:    "localhost",
	}, &Deps{
		Logger:          logger,
		Metrics:         observ.NewMetrics(nil),
		MintingCA:       mintingCA,
		FleetManager:    fleetManager,
		Sessions:        sessions,
		CacheBackend:    cacheBackend,
		VersionResolver: versionResolver,
		Auth:            authSvc,
		AuthDisabled:    authDisabled,
		Users:           usersSvc,
		Groups:          groupsSvc,
		Projects:        projectsSvc,
		Tokens:          tokensSvc,
		Quota:           quotaSvc,
		Attribution:     attributionSvc,
		TraceMeta:       traceMetaRepo,
		Traces:          traces,
		Logs:            logsClient,
		JWT:             jwtSvc,
		CacheStatsProvider: &stubCacheStatsProvider{
			stats: &domain.CacheStats{Backend: "registry", Registry: "cache.reg/dagger-cache", Running: true, Reachable: true},
		},
		CachePurger:    &stubCachePurger{result: &domain.PurgeResult{}},
		StatusProvider: &stubStatusProvider{status: &domain.PlatformStatus{State: domain.ServiceOK, Services: []domain.ServiceStatus{}}},
		Connect:        connectSvc,
	})

	return &testEnv{
		server:   srv,
		users:    usersSvc,
		groups:   groupsSvc,
		projects: projectsSvc,
		tokens:   tokensSvc,
		auth:     authSvc,
		jwt:      jwtSvc,
		sessions: sessions,
		store:    store,
	}
}

// loginAsAdmin returns an Authorization header value for the bootstrap admin.
func (e *testEnv) loginAsAdmin(t *testing.T) string {
	t.Helper()
	access, _, _, err := e.auth.Login(context.Background(), "admin", "password123")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	return "Bearer " + access
}

// createUserAndToken creates a user named "alice" (RoleUser) + API token and
// returns the bearer header.
func (e *testEnv) createUserAndToken(t *testing.T) (string, *domain.User) {
	t.Helper()
	u, err := e.users.Create(context.Background(), "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	plaintext, _, err := e.tokens.Generate(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return "Bearer " + plaintext, u
}

// testTokenEncKey returns a fixed 32-byte AES key for handler tests so tokens
// are recoverable via the Connect-env flow.
func testTokenEncKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

// --- stub collaborators for cache stats / purge / status handlers ---

type stubCacheStatsProvider struct {
	stats *domain.CacheStats
	err   error
}

func (s *stubCacheStatsProvider) Stats(context.Context) (*domain.CacheStats, error) {
	return s.stats, s.err
}
func (s *stubCacheStatsProvider) GCRules() domain.GCRules { return domain.GCRules{} }

type stubCachePurger struct {
	result *domain.PurgeResult
	err    error
}

func (p *stubCachePurger) Purge(context.Context, domain.PurgeRequest) (*domain.PurgeResult, error) {
	return p.result, p.err
}
func (p *stubCachePurger) PurgeAll(context.Context) (*domain.PurgeResult, error) {
	return p.result, p.err
}

type stubStatusProvider struct {
	status *domain.PlatformStatus
	err    error
}

func (p *stubStatusProvider) Status(context.Context) (*domain.PlatformStatus, error) {
	return p.status, p.err
}
