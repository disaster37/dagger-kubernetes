package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// rbacEnv holds a fully wired server on fixed ports for black-box HTTP tests.
type rbacEnv struct {
	baseURL    string
	sessions   *service.Store
	traceMeta  domain.TraceMetaRepository
	users      *service.UserService
	groups     *service.GroupService
	tokens     *service.TokenService
	projects   *service.ProjectService
	adminToken string // plaintext admin API token
}

func newRBACEnv(t *testing.T) *rbacEnv {
	t.Helper()
	logger := observ.NewTestLogger()
	dbPath := t.TempDir() + "/rbac.db"
	db, err := repository.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	userRepo := repository.NewUserRepo(db)
	groupRepo := repository.NewGroupRepo(db)
	projectRepo := repository.NewProjectRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	traceMetaRepo := repository.NewTraceMetaRepo(db)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger)
	jwtSvc := service.NewJWTService([]byte("rbac-integration-secret-32-bytes!"), 15*time.Minute, 168*time.Hour)

	// Bootstrap admin + API token.
	admin, _ := usersSvc.Create(context.Background(), "admin", "password123", domain.RoleAdmin)
	adminToken, _, _ := tokensSvc.Generate(context.Background(), admin.ID)

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
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: ":18095",
		DataAddr:    ":18456",
		DataHost:    "localhost",
	}, handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc,
		Groups: groupsSvc, Projects: projectsSvc, Tokens: tokensSvc,
		Quota: quotaSvc, Attribution: attributionSvc, TraceMeta: traceMetaRepo,
		Traces: traces, Logs: logsClient, JWT: jwtSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	time.Sleep(500 * time.Millisecond)

	return &rbacEnv{
		baseURL:    "http://localhost:18095",
		sessions:   sessions,
		traceMeta:  traceMetaRepo,
		users:      usersSvc,
		groups:     groupsSvc,
		tokens:     tokensSvc,
		projects:   projectsSvc,
		adminToken: adminToken,
	}
}

func (e *rbacEnv) adminBearer() string {
	return "Bearer " + e.adminToken
}

// TestRBACQuotaAndVisibility is the end-to-end RBAC scenario from the plan:
// bootstrap admin -> create group (max=1) + user + membership -> user token ->
// POST /v1/engines 201 -> second provision 429 -> second group relaxes ->
// OTLP ingest attributes trace via regex -> user sees trace, non-member 404 ->
// admin sees all -> regenerate invalidates old token.
func TestRBACQuotaAndVisibility(t *testing.T) {
	env := newRBACEnv(t)
	ctx := context.Background()

	// Admin creates a group with max_runner_sessions=1, agent_available=true.
	g1, err := env.groups.Create(ctx, service.GroupInput{
		Name: "G1", AgentAvailable: true, MaxRunnerSessions: 1,
	})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}

	// Create a user + membership.
	u, err := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := env.groups.SetUserGroups(ctx, u.ID, []string{g1.ID}); err != nil {
		t.Fatalf("set user groups: %v", err)
	}

	// User API token.
	userToken, _, err := env.tokens.Generate(ctx, u.ID)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}
	bearer := "Bearer " + userToken

	// First provision: 201.
	if code := postEngines(t, env.baseURL, bearer, "trace-rbac-1"); code != http.StatusCreated {
		t.Fatalf("first provision: %d, want 201", code)
	}

	// Second provision: 429 (quota exhausted; max=1, 1 active lease).
	if code := postEngines(t, env.baseURL, bearer, "trace-rbac-2"); code != http.StatusTooManyRequests {
		t.Fatalf("second provision: %d, want 429", code)
	}

	// Add a second group with capacity; admission should succeed via the new group.
	g2, err := env.groups.Create(ctx, service.GroupInput{
		Name: "G2", AgentAvailable: true, MaxRunnerSessions: 8,
	})
	if err != nil {
		t.Fatalf("create g2: %v", err)
	}
	if err := env.groups.SetUserGroups(ctx, u.ID, []string{g1.ID, g2.ID}); err != nil {
		t.Fatalf("add g2 membership: %v", err)
	}
	if code := postEngines(t, env.baseURL, bearer, "trace-rbac-3"); code != http.StatusCreated {
		t.Fatalf("third provision after relax: %d, want 201", code)
	}

	// Attribute a trace to the user (simulating OTLP ingest).
	env.traceMeta.UpsertProvision(ctx, "trace-attr", u.ID)
	env.traceMeta.UpsertIngest(ctx, &domain.TraceMeta{
		TraceID: "trace-attr", UserID: u.ID, CIRepo: "github.com/acme/api",
	})

	// User sees the trace in their scoped list.
	traces := listTraces(t, env.baseURL, bearer)
	if !traceInList(traces, "trace-attr") {
		t.Fatalf("user should see trace-attr, got %v", traces)
	}

	// Non-member gets 404 on detail.
	other, _ := env.users.Create(ctx, "bob", "password123", domain.RoleUser)
	otherToken, _, _ := env.tokens.Generate(ctx, other.ID)
	otherBearer := "Bearer " + otherToken
	if code := getTrace(t, env.baseURL, otherBearer, "trace-attr"); code != http.StatusNotFound {
		t.Fatalf("non-member trace detail: %d, want 404", code)
	}

	// Admin passes authorizeTrace (404 from Tempo since no real trace).
	if code := getTrace(t, env.baseURL, env.adminBearer(), "trace-attr"); code != http.StatusNotFound {
		t.Fatalf("admin trace detail: %d, want 404 (from Tempo)", code)
	}

	// Regenerate the user's token; old token is invalid immediately.
	newToken, _, _ := env.tokens.Regenerate(ctx, u.ID)
	if code := postEngines(t, env.baseURL, bearer, "trace-rbac-4"); code != http.StatusUnauthorized {
		t.Fatalf("old token after regenerate: %d, want 401", code)
	}
	newBearer := "Bearer " + newToken
	if code := postEngines(t, env.baseURL, newBearer, "trace-rbac-5"); code != http.StatusCreated {
		t.Fatalf("new token after regenerate: %d, want 201", code)
	}
}

// TestRBACNoGroupsForbidden verifies a user with no groups gets 403 on engines.
func TestRBACNoGroupsForbidden(t *testing.T) {
	env := newRBACEnv(t)
	ctx := context.Background()

	u, _ := env.users.Create(ctx, "alice", "password123", domain.RoleUser)
	token, _, _ := env.tokens.Generate(ctx, u.ID)
	bearer := "Bearer " + token

	if code := postEngines(t, env.baseURL, bearer, "trace-no-groups"); code != http.StatusForbidden {
		t.Fatalf("no groups: %d, want 403", code)
	}
}

// TestRBACAdminBypassesQuota verifies admins bypass quota entirely.
func TestRBACAdminBypassesQuota(t *testing.T) {
	env := newRBACEnv(t)
	bearer := env.adminBearer()
	for i := 0; i < 5; i++ {
		if code := postEngines(t, env.baseURL, bearer, "trace-admin"); code != http.StatusCreated {
			t.Fatalf("admin provision %d: %d, want 201", i, code)
		}
	}
}

// TestRBACLegacyTokenCompat verifies a flat-file legacy token still works when
// configured, running as the legacy admin identity (full access).
func TestRBACLegacyTokenCompat(t *testing.T) {
	logger := observ.NewTestLogger()
	dbPath := t.TempDir() + "/legacy.db"
	db, err := repository.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	defer db.Close()

	tokensPath := t.TempDir() + "/tokens"
	if err := os.WriteFile(tokensPath, []byte("legacy-token-123\n"), 0o600); err != nil {
		t.Fatalf("write tokens: %v", err)
	}

	userRepo := repository.NewUserRepo(db)
	groupRepo := repository.NewGroupRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	traceMetaRepo := repository.NewTraceMetaRepo(db)
	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger)
	jwtSvc := service.NewJWTService([]byte("legacy-secret-32-bytes-ok!!!!!!!"), 15*time.Minute, 168*time.Hour)
	legacyValidator := service.NewTokenValidator(tokensPath, true, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(repository.NewProjectRepo(db), groupRepo, logger)
	authSvc := service.NewAuthService(service.AuthServiceConfig{}, usersSvc, groupRepo, tokensSvc, jwtSvc, legacyValidator, logger)

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: ":18096",
		DataAddr:    ":18457",
		DataHost:    "localhost",
	}, handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Projects: projectsSvc, Tokens: tokensSvc, Quota: quotaSvc, Attribution: attributionSvc,
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

	// Legacy token authenticates as legacy admin (full access, quota bypass).
	if code := postEngines(t, "http://localhost:18096", "Bearer legacy-token-123", "trace-legacy"); code != http.StatusCreated {
		t.Fatalf("legacy token: %d, want 201", code)
	}
}

// --- helpers ---

func postEngines(t *testing.T, baseURL, bearer, traceID string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"image":    "registry.dagger.io/engine:v0.21.4",
		"trace_id": traceID,
	})
	req, _ := http.NewRequest("POST", baseURL+"/v1/engines", bytes.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/engines: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func listTraces(t *testing.T, baseURL, bearer string) []string {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/api/v1/traces", nil)
	req.Header.Set("Authorization", bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/traces: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list traces: %d", resp.StatusCode)
	}
	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id, ok := r["trace_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func getTrace(t *testing.T, baseURL, bearer, traceID string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/api/v1/traces/"+traceID, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET trace: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func traceInList(list []string, id string) bool {
	for _, l := range list {
		if l == id {
			return true
		}
	}
	return false
}
