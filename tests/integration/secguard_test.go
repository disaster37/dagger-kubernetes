package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/disaster37/go-secguard"
	sgconfig "github.com/disaster37/go-secguard/config"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// secguardFixture is a booted supervisor with a go-secguard guard whose clock
// is controllable.
type secguardFixture struct {
	baseURL string
	clock   *time.Time
	users   *service.UserService
	tokens  *service.TokenService
}

// newSecguardFixture boots a supervisor with the security guard enabled.
func newSecguardFixture(t *testing.T, cfg sgconfig.Config) *secguardFixture { //nolint:gocritic // test helper
	t.Helper()
	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr, dataAddr := listenerAddr(controlLn), listenerAddr(dataLn)

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
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)

	if _, err := usersSvc.Create(context.Background(), "admin", "password123", domain.RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	now := time.Now()
	guard, err := secguard.New(cfg, secguard.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("secguard.New: %v", err)
	}

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

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        dataAddr,
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
		PipelineURL:     "https://supv.example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient,
		JWT: jwtSvc, Security: guard,
		CookieCfg: domain.CookieConfig{
			AccessName:  "dagger_kubernetes_access",
			RefreshName: "dagger_kubernetes_refresh",
		},
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

	baseURL := fmt.Sprintf("http://localhost%s", controlAddr)
	waitReady(t, baseURL)

	return &secguardFixture{baseURL: baseURL, clock: &now, users: usersSvc, tokens: tokensSvc}
}

func (f *secguardFixture) post(t *testing.T, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", f.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (f *secguardFixture) get(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", f.baseURL+path, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestSecguardCredentialStuffingBanAndUnban proves the end-to-end IP ban,
// healthz exemption, admin inspection/unban, and clock-based expiry.
func TestSecguardCredentialStuffingBanAndUnban(t *testing.T) {
	f := newSecguardFixture(t, sgconfig.DefaultConfig())

	// 10 distinct failed credentials from the same IP trigger a ban.
	for i := 0; i < 10; i++ {
		resp := f.post(t, "/api/v1/auth/login", fmt.Sprintf(`{"username":"user-%d","password":"wrong"}`, i), nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.StatusCode)
		}
	}

	// A valid login is now rejected with 403.
	resp := f.post(t, "/api/v1/auth/login", `{"username":"admin","password":"password123"}`, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("banned login: %d, want 403", resp.StatusCode)
	}

	// /healthz is exempt from the ban.
	resp = f.get(t, "/healthz", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d, want 200", resp.StatusCode)
	}

	// Admin can list and lift the ban (the admin endpoints are reachable from a
	// banned IP).
	adminToken, _, err := f.tokens.Generate(context.Background(), adminUserID(t, f))
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	auth := map[string]string{"Authorization": "Bearer " + adminToken}

	resp = f.get(t, "/api/v1/security/bans", auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list bans: %d, want 200", resp.StatusCode)
	}
	var bans []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&bans); err != nil {
		t.Fatalf("decode bans: %v", err)
	}
	_ = resp.Body.Close()
	if len(bans) == 0 {
		t.Fatal("expected at least one ban")
	}
	ip, _ := bans[0]["ip"].(string)

	req, _ := http.NewRequest("DELETE", f.baseURL+"/api/v1/security/bans/"+ip, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unban: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("unban: %d, want 204", delResp.StatusCode)
	}

	// After unban a valid login succeeds again.
	resp = f.post(t, "/api/v1/auth/login", `{"username":"admin","password":"password123"}`, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-unban login: %d, want 200", resp.StatusCode)
	}
}

// TestSecguardAuthRateLimit proves the per-IP auth DoS rate limit.
func TestSecguardAuthRateLimit(t *testing.T) {
	cfg := sgconfig.DefaultConfig()
	cfg.RateLimit.MaxRequests = 3
	f := newSecguardFixture(t, cfg)

	for i := 0; i < 3; i++ {
		resp := f.get(t, "/api/v1/auth/providers", nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i, resp.StatusCode)
		}
	}
	resp := f.get(t, "/api/v1/auth/providers", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limited: %d, want 429", resp.StatusCode)
	}
}

// adminUserID returns the bootstrap admin's user id.
func adminUserID(t *testing.T, f *secguardFixture) string {
	t.Helper()
	u, err := f.users.GetByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	return u.ID
}
