package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fakeGitHubTokenServer emulates the GitHub OAuth token endpoint for an app
// with "Expire user access tokens": the device grant mints a short-lived
// access token plus a refresh token; every refresh rotates both and mints a
// long-lived pair. Only the newest access token is accepted by /user.
type fakeGitHubTokenServer struct {
	*httptest.Server
	mu        sync.Mutex
	access    string
	refresh   string
	refreshes int
	grants    []url.Values
}

func newFakeGitHubTokenServer(t *testing.T) *fakeGitHubTokenServer {
	t.Helper()
	f := &fakeGitHubTokenServer{access: "gho_v1", refresh: "ghr_v1"}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHubTokenServer) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/login/device/code":
		_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	case "/login/oauth/access_token":
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		f.mu.Lock()
		defer f.mu.Unlock()
		if form.Get("grant_type") == "refresh_token" {
			f.refreshes++
			f.grants = append(f.grants, form)
			f.access = fmt.Sprintf("gho_v%d", f.refreshes+1)
			f.refresh = fmt.Sprintf("ghr_v%d", f.refreshes+1)
			// The rotated pair lives 8 h, so it is no longer inside the 5 m
			// proactive window and a later request must not rotate again.
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer","scope":"repo","refresh_token":%q,"expires_in":28800,"refresh_token_expires_in":15897600}`, f.access, f.refresh)
			return
		}
		// Device-code grant: 60 s access token, inside the 5 m refresh skew.
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer","scope":"repo","refresh_token":%q,"expires_in":60,"refresh_token_expires_in":15897600}`, f.access, f.refresh)
	case "/user":
		if !f.accepts(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":7,"login":"alice","type":"User"}`)
	case "/user/orgs":
		if !f.accepts(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHubTokenServer) accepts(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return r.Header.Get("Authorization") == fmt.Sprintf("Bearer %s", f.access)
}

func (f *fakeGitHubTokenServer) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

func (f *fakeGitHubTokenServer) refreshGrants() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.grants...)
}

type githubRefreshEnv struct {
	base   string
	fake   *fakeGitHubTokenServer
	cookie *http.Client
}

// newGitHubRefreshEnv builds a real Hertz control plane (internal auth only)
// whose GitHub OAuth client points at fakeGitHubTokenServer.
func newGitHubRefreshEnv(t *testing.T) *githubRefreshEnv {
	t.Helper()
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	ctx := context.Background()
	fake := newFakeGitHubTokenServer(t)
	encKey := []byte("0123456789abcdef0123456789abcdef")

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	orgRepo := repository.NewOrgRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)
	forgeRepo := repository.NewForgeSourceRepo(store)
	secretRepo := repository.NewSecretRepo(store)
	githubConnRepo := repository.NewGitHubConnectionRepo(store)
	githubAccountRepo := repository.NewGitHubAccountSourceRepo(store)
	repoSettingRepo := repository.NewRepoSettingRepo(store)
	oauthStateRepo := repository.NewGitHubOAuthStateRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, encKey)
	jwtSvc := service.NewJWTService([]byte("github-refresh-integration-secret"), 15*time.Minute, 168*time.Hour)
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	authSvc.SetOrgRepository(orgRepo)
	usersSvc.SetOrgRepository(orgRepo)
	orgsSvc := service.NewOrgService(orgRepo, groupRepo, tokensSvc, logger)
	deviceSvc := service.NewDeviceAuthService(service.NewMemoryDeviceAuthStore(), usersSvc, groupRepo, jwtSvc, 10*time.Minute, 5*time.Second, logger)
	secretSvc := service.NewSecretService(secretRepo, encKey, logger)

	githubClient, err := repository.NewGitHubClient(repository.GitHubClientConfig{
		ClientID: "client-id", OAuthBaseURL: fake.URL, DefaultAPIURL: fake.URL, HTTPTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	integration := service.NewGitHubIntegrationService(service.GitHubIntegrationConfig{
		PublicURL: "https://example.com", DefaultAPIURL: fake.URL,
		OAuthEnabled: true, ClientID: "client-id",
		Scopes: []string{"repo"}, StateTTL: 10 * time.Minute,
		DeviceFlowEnabled: true, RefreshEnabled: true, RefreshSkew: 5 * time.Minute,
	}, service.GitHubIntegrationDeps{
		Connections: githubConnRepo, AccountSources: githubAccountRepo, RepoSettings: repoSettingRepo,
		ForgeSources: forgeRepo, OAuthStates: oauthStateRepo,
		DeviceSessions: service.NewMemoryGitHubDeviceSessionStore(), Users: userRepo,
		Orgs: orgsSvc, Projects: projectsSvc, Sealer: secretSvc, GitHub: githubClient,
		ForgeProvider: repository.NewGitHubForge(fake.URL), Logger: logger,
		IsLeader: func() bool { return true },
	})

	user, err := usersSvc.Create(ctx, "refresh-dev", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orgsSvc.EnsureBootstrap(ctx, ""); err != nil {
		t.Fatal(err)
	}
	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	fleetManager := service.NewManager(repository.NewStubProvider(), sessions, service.ManagerConfig{}, logger, observ.NewMetrics(nil))

	control := freeListener(t)
	data := freeListener(t)
	internalLn := freeListener(t)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: listenerAddr(control), DataAddr: listenerAddr(data),
		ControlListener: control, DataListener: data, InternalListener: internalLn,
		DataHost: "localhost", PipelineURL: "https://example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		CookieCfg: domain.CookieConfig{AccessName: "dagger_kubernetes_access", RefreshName: "dagger_kubernetes_refresh"},
		Users:     usersSvc, Groups: groupsSvc, Projects: projectsSvc, Orgs: orgsSvc,
		Tokens: tokensSvc, TraceMeta: traceMetaRepo, JWT: jwtSvc, DeviceAuth: deviceSvc,
		Secrets:           secretSvc,
		Forge:             service.NewForgeService(forgeRepo, secretSvc, []domain.ForgeProvider{repository.NewGitHubForge(fake.URL)}, "https://example.com", logger),
		GitHubIntegration: integration,
	})
	serverTLS, _ := mintingCA.TLSCertificate()
	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(startCtx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(300 * time.Millisecond)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	payload, _ := json.Marshal(map[string]string{"username": user.Username, "password": "password123"})
	resp, err := client.Post(fmt.Sprintf("http://%s/api/v1/auth/login", control.Addr().String()), "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	return &githubRefreshEnv{base: fmt.Sprintf("http://%s", control.Addr().String()), fake: fake, cookie: client}
}

func (e *githubRefreshEnv) do(t *testing.T, method, path string, body any) (status int, respBody []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.cookie.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ = io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

func (e *githubRefreshEnv) connection(t *testing.T) (conn map[string]any) {
	t.Helper()
	status, body := e.do(t, http.MethodGet, "/api/v1/github/connection", nil)
	if status != http.StatusOK {
		t.Fatalf("connection = %d %s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	conn, _ = out["connection"].(map[string]any)
	return conn
}

// linkDeviceFlow runs one device-flow round trip against the fake token
// endpoint, which stores a short-lived (expiring) credential.
func (e *githubRefreshEnv) linkDeviceFlow(t *testing.T) {
	t.Helper()
	status, body := e.do(t, http.MethodPost, "/api/v1/github/device/code", map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("device code = %d %s", status, body)
	}
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &start); err != nil {
		t.Fatal(err)
	}
	var poll map[string]any
	for attempt := 0; attempt < 3; attempt++ {
		status, body = e.do(t, http.MethodPost, "/api/v1/github/device/poll", map[string]string{"session_id": start.SessionID})
		if status != http.StatusOK {
			t.Fatalf("device poll = %d %s", status, body)
		}
		if err := json.Unmarshal(body, &poll); err != nil {
			t.Fatal(err)
		}
		if poll["status"] != "pending" {
			return
		}
	}
	t.Fatalf("device poll never completed: %v", poll)
}

func TestGitHubRefreshTokenRotationEndToEnd(t *testing.T) {
	env := newGitHubRefreshEnv(t)
	env.linkDeviceFlow(t)

	conn := env.connection(t)
	if conn == nil {
		t.Fatal("device flow did not create a connection")
	}
	for _, field := range []string{"token_expires_at", "refresh_expires_at"} {
		value, _ := conn[field].(string)
		if value == "" || value == "0001-01-01T00:00:00Z" {
			t.Fatalf("%s not populated: %v", field, conn)
		}
	}
	if version, _ := conn["token_version"].(float64); version != 1 {
		t.Fatalf("token_version = %v want 1", conn["token_version"])
	}
	if env.fake.refreshCount() != 0 {
		t.Fatalf("linking must not rotate, refreshes=%d", env.fake.refreshCount())
	}

	// The stored access token expires in 60 s, inside the 5 m skew: a burst of
	// parallel requests must rotate exactly once (per-scope single-flight).
	const burst = 8
	var wg sync.WaitGroup
	errs := make(chan string, burst)
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := env.do(t, http.MethodGet, "/api/v1/github/owners", nil)
			if status != http.StatusOK {
				errs <- fmt.Sprintf("owners = %d %s", status, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for failure := range errs {
		t.Fatal(failure)
	}
	if got := env.fake.refreshCount(); got != 1 {
		t.Fatalf("burst refresh calls = %d want 1", got)
	}

	grants := env.fake.refreshGrants()
	if len(grants) != 1 {
		t.Fatalf("refresh grants = %d", len(grants))
	}
	grant := grants[0]
	if grant.Get("grant_type") != "refresh_token" || grant.Get("client_id") != "client-id" || grant.Get("refresh_token") == "" {
		t.Fatalf("refresh grant = %v", grant)
	}
	if grant.Get("client_secret") != "" {
		t.Fatalf("device-flow refresh must not send a client_secret: %v", grant)
	}

	rotated := env.connection(t)
	if version, _ := rotated["token_version"].(float64); version != 2 {
		t.Fatalf("token_version = %v want 2", rotated["token_version"])
	}
	expires, err := time.Parse(time.RFC3339, rotated["token_expires_at"].(string))
	if err != nil {
		t.Fatalf("token_expires_at = %v", rotated["token_expires_at"])
	}
	if !expires.After(time.Now().Add(time.Hour)) {
		t.Fatalf("rotated token expiry = %v (want ~8h out)", expires)
	}

	// A further request is outside the skew window: no extra rotation.
	if status, body := env.do(t, http.MethodGet, "/api/v1/github/owners", nil); status != http.StatusOK {
		t.Fatalf("post-rotation owners = %d %s", status, body)
	}
	if got := env.fake.refreshCount(); got != 1 {
		t.Fatalf("refresh calls after rotation = %d want 1", got)
	}
}
