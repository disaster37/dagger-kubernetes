package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// TestOAuthGroupMappingAutoCreateFlow is a black-box OIDC flow test: it starts a
// supervisor pointed at a loopback OIDC issuer with a group_mappings rule whose
// target group does not exist, drives the login → callback flow, and asserts the
// login succeeds and the mapped group was auto-created with the configured
// default engine limit and joined by the user.
func TestOAuthGroupMappingAutoCreateFlow(t *testing.T) {
	const clientID = "integration-autocreate-client"
	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr, dataAddr := listenerAddr(controlLn), listenerAddr(dataLn)
	issuer := newOIDCIssuer(t, clientID, []any{"devs"})

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

	oauthCfg := &domain.OAuthConfig{
		Enabled:       true,
		Provider:      "oidc",
		ClientID:      clientID,
		ClientSecret:  "csec",
		RedirectURL:   fmt.Sprintf("http://localhost%s/api/v1/auth/oauth/oidc/callback", controlAddr),
		IssuerURL:     issuer.srv.URL,
		Scopes:        []string{"openid", "profile", "email"},
		UsernameClaim: "preferred_username",
		GroupsClaim:   "groups",
		GroupMappings: []domain.GroupMappingRule{
			{Pattern: "^devs$", Replacement: "auto-created-dev"},
		},
		MappedGroupMaxRunnerSessions: 4,
	}
	mapper, err := service.NewGroupMapper(oauthCfg.GroupMappings)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	oauthSvc := service.NewOIDCOAuthService(oauthCfg, mapper, usersSvc, groupRepo, jwtSvc, logger, nil, nil)

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
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient,
		JWT: jwtSvc, OAuth: oauthSvc, OAuthProvider: "oidc",
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

	baseURL := fmt.Sprintf("http://localhost%s", controlAddr)
	noRedirect := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 1. Login: must 302 to the OIDC authorize URL and set the nonce cookie.
	loginResp, err := noRedirect.Get(baseURL + "/api/v1/auth/oauth/oidc/login?redirect=/pipelines")
	if err != nil {
		t.Fatalf("GET oidc login: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", loginResp.StatusCode)
	}
	location := loginResp.Header.Get("Location")
	if !strings.HasPrefix(location, issuer.srv.URL+"/auth?") {
		t.Fatalf("login Location = %q, want authorize endpoint", location)
	}
	nonce := cookieValue(loginResp.Header.Get("Set-Cookie"), "oauth_state")
	if nonce == "" {
		t.Fatal("login must set the oauth_state nonce cookie")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL must carry the state token")
	}

	// 2. Callback: allowed user with a mapped (missing) group → SPA redirect.
	callbackURL := baseURL + "/api/v1/auth/oauth/oidc/callback?code=code&state=" + url.QueryEscape(state)
	req, err := http.NewRequest("GET", callbackURL, http.NoBody)
	if err != nil {
		t.Fatalf("new callback request: %v", err)
	}
	req.Header.Set("Cookie", "oauth_state="+nonce)
	cbResp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET oidc callback: %v", err)
	}
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", cbResp.StatusCode)
	}
	if loc := cbResp.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/callback?") {
		t.Fatalf("callback Location = %q, want /auth/callback redirect", loc)
	}

	// 3. The mapped group must have been auto-created with the default limit.
	autoCtx := context.Background()
	created, err := groupRepo.GetByName(autoCtx, "auto-created-dev")
	if err != nil {
		t.Fatalf("auto-created group not persisted: %v", err)
	}
	if created.MaxRunnerSessions != 4 {
		t.Fatalf("auto-created group quota = %d, want 4", created.MaxRunnerSessions)
	}
	if !created.AgentAvailable {
		t.Fatal("auto-created group must have AgentAvailable = true")
	}

	// 4. The user must be a member of the auto-created group.
	u, err := userRepo.GetByOAuth(autoCtx, "oidc", "alice-sub")
	if err != nil {
		t.Fatalf("resolve oauth user: %v", err)
	}
	member, err := groupRepo.GroupsForUser(autoCtx, u.ID)
	if err != nil {
		t.Fatalf("GroupsForUser: %v", err)
	}
	if len(member) != 1 || member[0].ID != created.ID {
		t.Fatalf("memberships = %v, want only the auto-created group", member)
	}
}
