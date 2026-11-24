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

type githubCLIFake struct {
	*httptest.Server
	mu            sync.Mutex
	hookSecret    string
	hookDeletes   int
	statuses      []string
	checkRuns     []string
	exchangeCount int
}

func newGitHubCLIFake(t *testing.T) *githubCLIFake {
	t.Helper()
	f := &githubCLIFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *githubCLIFake) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/login/oauth/access_token":
		f.mu.Lock()
		f.exchangeCount++
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"gho_e2e","token_type":"bearer","scope":"repo,read:org,admin:repo_hook"}`)
	case r.URL.Path == "/user":
		_, _ = io.WriteString(w, `{"id":7,"login":"alice","type":"User"}`)
	case r.URL.Path == "/user/orgs":
		_, _ = io.WriteString(w, `[]`)
	case r.URL.Path == "/user/repos":
		_, _ = io.WriteString(w, `[{"id":1,"name":"demo","full_name":"alice/demo","private":true,"default_branch":"main","owner":{"login":"alice"},"permissions":{"admin":true}}]`)
	case r.URL.Path == "/repos/alice/demo":
		_, _ = io.WriteString(w, `{"id":1,"name":"demo","full_name":"alice/demo","private":true,"default_branch":"main","owner":{"login":"alice"},"permissions":{"admin":true}}`)
	case r.URL.Path == "/repos/alice/demo/hooks" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `[]`)
	case r.URL.Path == "/repos/alice/demo/hooks" && r.Method == http.MethodPost:
		var payload struct {
			Config struct {
				Secret string `json:"secret"`
			} `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		f.mu.Lock()
		f.hookSecret = payload.Config.Secret
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":1}`)
	case strings.HasPrefix(r.URL.Path, "/repos/alice/demo/hooks/") && r.Method == http.MethodDelete:
		f.mu.Lock()
		f.hookDeletes++
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	case strings.HasPrefix(r.URL.Path, "/repos/alice/demo/statuses/"):
		f.mu.Lock()
		f.statuses = append(f.statuses, r.URL.Path)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	case r.URL.Path == "/repos/alice/demo/check-runs":
		f.mu.Lock()
		f.checkRuns = append(f.checkRuns, r.URL.Path)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *githubCLIFake) snapshot() (secret string, deletes int, statuses, checkRuns []string, exchanges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hookSecret, f.hookDeletes, append([]string(nil), f.statuses...), append([]string(nil), f.checkRuns...), f.exchangeCount
}

type githubCLIServer struct {
	base     string
	store    *repository.RaftStore
	users    *service.UserService
	orgs     *service.OrgService
	tokens   *service.TokenService
	runner   *recordingCheckRunner
	fake     *githubCLIFake
	internal *domain.User
	repos    domain.RepoSettingRepository
}

// newGitHubCLIServer builds a real Hertz control plane with internal auth only
// (auth.oauth disabled) unless loginOAuth is true. The GitHub forge OAuth app
// and the human GitHub login provider are always independent services.
func newGitHubCLIServer(t *testing.T, loginOAuth bool) *githubCLIServer {
	t.Helper()
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	ctx := context.Background()
	fake := newGitHubCLIFake(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	orgRepo := repository.NewOrgRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)
	checkRunRepo := repository.NewCheckRunRepo(store)
	forgeRepo := repository.NewForgeSourceRepo(store)
	secretRepo := repository.NewSecretRepo(store)
	githubConnRepo := repository.NewGitHubConnectionRepo(store)
	githubAccountRepo := repository.NewGitHubAccountSourceRepo(store)
	repoSettingRepo := repository.NewRepoSettingRepo(store)
	oauthStateRepo := repository.NewGitHubOAuthStateRepo(store)
	encKey := []byte("0123456789abcdef0123456789abcdef")

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, encKey)
	jwtSvc := service.NewJWTService([]byte("github-cli-integration-secret-32!!"), 15*time.Minute, 168*time.Hour)
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	authSvc.SetOrgRepository(orgRepo)
	usersSvc.SetOrgRepository(orgRepo)
	orgsSvc := service.NewOrgService(orgRepo, groupRepo, tokensSvc, logger)
	deviceSvc := service.NewDeviceAuthService(service.NewMemoryDeviceAuthStore(), usersSvc, groupRepo, jwtSvc, 10*time.Minute, 5*time.Second, logger)
	secretSvc := service.NewSecretService(secretRepo, encKey, logger)
	runner := &recordingCheckRunner{}
	checksSvc := service.NewChecksService(forgeRepo, secretSvc, projectsSvc, orgsSvc, runner, checkRunRepo, nil, []domain.ForgeProvider{repository.NewGitHubForge(fake.URL)}, "https://example.com", "", logger).
		WithRepoSettings(repoSettingRepo)
	githubClient, err := repository.NewGitHubClient(repository.GitHubClientConfig{
		ClientID: "client-id", ClientSecret: "client-secret",
		OAuthBaseURL: fake.URL, DefaultAPIURL: fake.URL, HTTPTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	integration := service.NewGitHubIntegrationService(service.GitHubIntegrationConfig{
		PublicURL: "https://example.com", DefaultAPIURL: fake.URL,
		OAuthEnabled: true, ClientID: "client-id", ClientSecret: "client-secret",
		Scopes: []string{"repo", "read:org", "admin:repo_hook"}, StateTTL: 10 * time.Minute,
		DeviceFlowEnabled: true,
	}, service.GitHubIntegrationDeps{
		Connections: githubConnRepo, AccountSources: githubAccountRepo, RepoSettings: repoSettingRepo,
		ForgeSources: forgeRepo, OAuthStates: oauthStateRepo,
		DeviceSessions: service.NewMemoryGitHubDeviceSessionStore(), Users: userRepo,
		Orgs: orgsSvc, Projects: projectsSvc, Sealer: secretSvc, GitHub: githubClient,
		ForgeProvider: repository.NewGitHubForge(fake.URL), Logger: logger,
	})
	graphql := repository.NewCloudGraphQL(userRepo, orgRepo, groupRepo, traceMetaRepo, checkRunRepo, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}).
		WithForgeSources(forgeRepo).WithProjects(projectRepo).WithPublicURL("https://example.com").
		WithGitHubConnections(githubConnRepo).WithGitHubAccountSources(githubAccountRepo).
		WithRepoSettings(repoSettingRepo).WithGitHubIntegrator(integration)

	internal, err := usersSvc.Create(ctx, "internal-dev", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orgsSvc.EnsureBootstrap(ctx, ""); err != nil {
		t.Fatal(err)
	}
	org := &domain.Org{ID: "acme", Name: "acme", DisplayName: "Acme", Features: domain.DefaultOrgFeatures, CreatedAt: time.Now().UTC()}
	if err := orgRepo.Create(ctx, org); err != nil {
		t.Fatal(err)
	}
	if err := orgRepo.SetMember(ctx, "acme", internal.ID, domain.OrgAdmin); err != nil {
		t.Fatal(err)
	}

	var oauthLogin service.OAuthProvider
	oauthProvider := ""
	if loginOAuth {
		mapper, err := service.NewGroupMapper(nil)
		if err != nil {
			t.Fatal(err)
		}
		oauthCfg := &domain.OAuthConfig{Enabled: true, Provider: "github", ClientID: "login-client", ClientSecret: "login-secret", RedirectURL: "https://example.com/api/v1/auth/oauth/github/callback"}
		oauthLogin = service.NewGitHubOAuthService(oauthCfg, mapper, usersSvc, groupRepo, jwtSvc, logger, encKey)
		oauthProvider = "github"
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
		CloudGraphQL: graphql, Checks: checksSvc, Secrets: secretSvc,
		Forge: service.NewForgeService(forgeRepo, secretSvc, []domain.ForgeProvider{repository.NewGitHubForge(fake.URL)}, "https://example.com", logger).
			WithGitHubSync(githubAccountRepo, repoSettingRepo),
		GitHubIntegration: integration, OAuth: oauthLogin, OAuthProvider: oauthProvider,
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
	return &githubCLIServer{
		base: fmt.Sprintf("http://%s", control.Addr().String()), store: store,
		users: usersSvc, orgs: orgsSvc, tokens: tokensSvc,
		runner: runner, fake: fake, internal: internal, repos: repoSettingRepo,
	}
}

func (s *githubCLIServer) apiToken(t *testing.T, user *domain.User) string {
	t.Helper()
	token, _, err := s.tokens.Generate(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (s *githubCLIServer) deviceLogin(t *testing.T) string {
	t.Helper()
	resp, body := postForm(t, s.base, "/oauth/device/code", url.Values{}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device code = %d %s", resp.StatusCode, body)
	}
	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	if err := json.Unmarshal(body, &code); err != nil {
		t.Fatal(err)
	}
	approve, _ := json.Marshal(map[string]string{"user_code": code.UserCode})
	req, _ := http.NewRequest(http.MethodPost, s.base+"/api/v1/auth/device/approve", bytes.NewReader(approve))
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", s.apiToken(t, s.internal)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("approve = %d", resp.StatusCode)
	}
	resp, body = postForm(t, s.base, "/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {code.DeviceCode}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token = %d %s", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatal(err)
	}
	return tok.AccessToken
}

func (s *githubCLIServer) loginCookies(t *testing.T, username string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	payload, _ := json.Marshal(map[string]string{"username": username, "password": "password123"})
	resp, err := client.Post(s.base+"/api/v1/auth/login", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s = %d", username, resp.StatusCode)
	}
	return client
}

func (s *githubCLIServer) postJSON(t *testing.T, client *http.Client, path, token string, body any) (status int, respBody []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, s.base+path, bytes.NewReader(data))
	if token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestGitHubCLIIntegration_InternalAuthOnlyEndToEnd(t *testing.T) {
	srv := newGitHubCLIServer(t, false)
	cliToken := srv.deviceLogin(t)

	res := gqlJSON(t, srv.base, cliToken, `query { githubConnection { githubLogin } sources { id } }`, nil)
	if res["githubConnection"] != nil {
		t.Fatalf("fresh connection = %v", res)
	}
	if len(res["sources"].([]any)) != 0 {
		t.Fatalf("fresh sources = %v", res["sources"])
	}
	auth := gqlJSON(t, srv.base, cliToken, `query($r:String!){ githubOAuthURL(redirectURI:$r) }`, map[string]any{"r": "https://example.com/github/callback"})
	authURL, _ := auth["githubOAuthURL"].(string)
	if !strings.Contains(authURL, "code_challenge=") || !strings.Contains(authURL, "state=") {
		t.Fatalf("authorize url = %q", authURL)
	}
	state := stateFromURL(t, authURL)

	client := srv.loginCookies(t, "internal-dev")
	status, body := srv.postJSON(t, client, "/api/v1/github/oauth/complete", "", map[string]string{"code": "code-1", "state": state})
	if status != http.StatusOK {
		t.Fatalf("complete = %d %s", status, body)
	}
	if strings.Contains(string(body), "token_ciphertext") || strings.Contains(string(body), "gho_e2e") {
		t.Fatalf("complete leaked a token: %s", body)
	}

	conn := gqlJSON(t, srv.base, cliToken, `query { githubConnection { githubLogin status scopes } sources { id name type orgName } }`, nil)
	ghConn, _ := conn["githubConnection"].(map[string]any)
	if ghConn["githubLogin"] != "alice" || ghConn["status"] != "active" {
		t.Fatalf("connection = %v", conn)
	}
	sources := conn["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("sources = %v", sources)
	}
	acctID, _ := sources[0].(map[string]any)["id"].(string)
	if !strings.HasPrefix(acctID, "ghacct-") {
		t.Fatalf("account id = %q", acctID)
	}

	before := gqlJSON(t, srv.base, cliToken, `query($refs:[String!]){ user { repositories(refs:$refs) { ref } } }`, map[string]any{"refs": []string{"github.com/alice/demo"}})
	if len(before["user"].(map[string]any)["repositories"].([]any)) != 0 {
		t.Fatalf("repo must be unclaimed: %v", before)
	}
	repos := gqlJSON(t, srv.base, cliToken, `query($id:ID!,$org:ID){ sourceRepositories(installationId:$id,org:$org) { repository eligible } }`, map[string]any{"id": acctID, "org": "acme"})
	if len(repos["sourceRepositories"].([]any)) != 1 || repos["sourceRepositories"].([]any)[0].(map[string]any)["eligible"] != true {
		t.Fatalf("source repositories = %v", repos)
	}

	if status, body := srv.postJSON(t, nil, "/query", cliToken, map[string]any{
		"query": `mutation($org:ID!,$s:[RepoSettingInput!]!){ updateOrgRepoSettings(org:$org,settings:$s) }`,
		"variables": map[string]any{"org": "acme", "s": []map[string]any{{
			"repo": "github.com/alice/demo", "sourceId": acctID,
			"runChecksOnDefaultBranch": true, "runChecksOnPullRequest": true, "runChecksOnBranchPush": false,
		}}},
	}); status != http.StatusOK || !strings.Contains(string(body), `"updateOrgRepoSettings":true`) {
		t.Fatalf("checks on = %d %s", status, body)
	}
	after := gqlJSON(t, srv.base, cliToken, `query($refs:[String!]){ user { repositories(refs:$refs) { ref settings { effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest effectiveRunChecksOnBranchPush } mappedSource { provider sourceName installationId } } } }`, map[string]any{"refs": []string{"github.com/alice/demo"}})
	list := after["user"].(map[string]any)["repositories"].([]any)
	if len(list) != 1 {
		t.Fatalf("claimed repos = %v", after)
	}
	settings := list[0].(map[string]any)["settings"].(map[string]any)
	if settings["effectiveRunChecksOnDefaultBranch"] != true || settings["effectiveRunChecksOnBranchPush"] != false {
		t.Fatalf("settings = %v", settings)
	}
	canonical := list[0].(map[string]any)["ref"].(string)
	setting, err := srv.repos.Get(context.Background(), "acme", canonical)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, _, _, _ := srv.fake.snapshot()
	if secret == "" {
		t.Fatal("fake GitHub must have received a webhook secret")
	}

	// First signed push dispatches a check job and reports a commit status.
	webhookBody := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"alice/demo","default_branch":"main"}}`)
	status, respBody := postSignedWebhook(t, srv.base, setting.ForgeSourceID, secret, webhookBody)
	if status != http.StatusOK {
		t.Fatalf("webhook = %d %s", status, respBody)
	}
	if srv.runner.count() != 1 {
		t.Fatalf("runner count = %d", srv.runner.count())
	}
	_, _, statuses, checkRuns, _ := srv.fake.snapshot()
	if len(statuses) != 1 || len(checkRuns) != 0 {
		t.Fatalf("statuses=%v checkRuns=%v", statuses, checkRuns)
	}

	// checks off: keep the claim/webhook, flip triggers false.
	if status, body := srv.postJSON(t, nil, "/query", cliToken, map[string]any{
		"query": `mutation($org:ID!,$s:[RepoSettingInput!]!){ updateOrgRepoSettings(org:$org,settings:$s) }`,
		"variables": map[string]any{"org": "acme", "s": []map[string]any{{
			"repo": "github.com/alice/demo", "sourceId": acctID,
			"runChecksOnDefaultBranch": false, "runChecksOnPullRequest": false, "runChecksOnBranchPush": false,
		}}},
	}); status != http.StatusOK {
		t.Fatalf("checks off = %d %s", status, body)
	}
	off := gqlJSON(t, srv.base, cliToken, `query($refs:[String!]){ user { repositories(refs:$refs) { ref settings { effectiveRunChecksOnDefaultBranch effectiveRunChecksOnPullRequest effectiveRunChecksOnBranchPush } } } }`, map[string]any{"refs": []string{"github.com/alice/demo"}})
	offSettings := off["user"].(map[string]any)["repositories"].([]any)[0].(map[string]any)["settings"].(map[string]any)
	if offSettings["effectiveRunChecksOnDefaultBranch"] != false || offSettings["effectiveRunChecksOnPullRequest"] != false {
		t.Fatalf("off settings = %v", offSettings)
	}
	_, deletesBefore, _, _, _ := srv.fake.snapshot()
	status, respBody = postSignedWebhook(t, srv.base, setting.ForgeSourceID, secret, webhookBody)
	if status != http.StatusOK || !strings.Contains(string(respBody), `"reason":"trigger_disabled"`) {
		t.Fatalf("disabled webhook = %d %s", status, respBody)
	}
	if srv.runner.count() != 1 {
		t.Fatalf("disabled trigger must not dispatch, count=%d", srv.runner.count())
	}
	_, deletesAfter, _, _, _ := srv.fake.snapshot()
	if deletesBefore != deletesAfter {
		t.Fatalf("checks off must not delete the webhook (deletes %d -> %d)", deletesBefore, deletesAfter)
	}
}

func TestGitHubCLIIntegration_StateUserMismatchPreservesStateForOwner(t *testing.T) {
	srv := newGitHubCLIServer(t, false)
	ctx := context.Background()
	alice, err := srv.users.Create(ctx, "alice-internal", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := srv.users.Create(ctx, "bob-internal", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.orgs.SetMember(ctx, "acme", alice.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	if err := srv.orgs.SetMember(ctx, "acme", bob.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	auth := gqlJSON(t, srv.base, srv.apiToken(t, alice), `query($r:String!){ githubOAuthURL(redirectURI:$r) }`, map[string]any{"r": "https://example.com/github/callback"})
	state := stateFromURL(t, auth["githubOAuthURL"].(string))

	bobClient := srv.loginCookies(t, "bob-internal")
	status, body := srv.postJSON(t, bobClient, "/api/v1/github/oauth/complete", "", map[string]string{"code": "code-1", "state": state})
	if status != http.StatusForbidden {
		t.Fatalf("mismatch = %d %s", status, body)
	}
	var mismatch map[string]any
	_ = json.Unmarshal(body, &mismatch)
	if mismatch["code"] != "state_user_mismatch" || mismatch["expected_username"] != "alice-internal" || mismatch["current_username"] != "bob-internal" {
		t.Fatalf("mismatch body = %v", mismatch)
	}
	_, _, _, _, exchanges := srv.fake.snapshot()
	if exchanges != 0 {
		t.Fatalf("mismatch must not exchange the code, exchanges=%d", exchanges)
	}

	aliceClient := srv.loginCookies(t, "alice-internal")
	if status, body := srv.postJSON(t, aliceClient, "/api/v1/github/oauth/complete", "", map[string]string{"code": "code-1", "state": state}); status != http.StatusOK {
		t.Fatalf("owner complete = %d %s", status, body)
	}
	if status, _ := srv.postJSON(t, aliceClient, "/api/v1/github/oauth/complete", "", map[string]string{"code": "code-1", "state": state}); status != http.StatusBadRequest {
		t.Fatalf("replay = %d", status)
	}
}

func TestGitHubCLIIntegration_UnauthenticatedCallbackLoginRedirectRoundTrip(t *testing.T) {
	srv := newGitHubCLIServer(t, false)
	auth := gqlJSON(t, srv.base, srv.apiToken(t, srv.internal), `query($r:String!){ githubOAuthURL(redirectURI:$r) }`, map[string]any{"r": "https://example.com/github/callback"})
	state := stateFromURL(t, auth["githubOAuthURL"].(string))

	resp, err := http.Get(fmt.Sprintf("%s/github/callback?code=gh-code&state=%s", srv.base, url.QueryEscape(state)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback SPA = %d", resp.StatusCode)
	}
	if status, _ := srv.postJSON(t, nil, "/api/v1/github/oauth/complete", "", map[string]string{"code": "gh-code", "state": state}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated complete = %d", status)
	}
	client := srv.loginCookies(t, "internal-dev")
	status, body := srv.postJSON(t, client, "/api/v1/github/oauth/complete", "", map[string]string{"code": "gh-code", "state": state})
	if status != http.StatusOK {
		t.Fatalf("complete after login = %d %s", status, body)
	}
}

func TestGitHubCLIIntegration_LoginOAuthAndForgeOAuthCoexistence(t *testing.T) {
	srv := newGitHubCLIServer(t, true)
	// Human GitHub login route is mounted independently.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(fmt.Sprintf("%s/api/v1/auth/oauth/github/login?redirect=%%2Fpipelines", srv.base))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("github login = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Location"), "github.com/login/oauth/authorize") {
		t.Fatalf("login location = %q", resp.Header.Get("Location"))
	}
	// Forge credential linking still completes through its own routes/state.
	auth := gqlJSON(t, srv.base, srv.apiToken(t, srv.internal), `query($r:String!){ githubOAuthURL(redirectURI:$r) }`, map[string]any{"r": "https://example.com/github/callback"})
	state := stateFromURL(t, auth["githubOAuthURL"].(string))
	client := srv.loginCookies(t, "internal-dev")
	if status, body := srv.postJSON(t, client, "/api/v1/github/oauth/complete", "", map[string]string{"code": "code-1", "state": state}); status != http.StatusOK {
		t.Fatalf("forge complete with login oauth enabled = %d %s", status, body)
	}
}

func gqlJSON(t *testing.T, base, token, query string, variables map[string]any) map[string]any {
	t.Helper()
	result, status := gqlPost(t, base, token, query, variables)
	if status != http.StatusOK {
		t.Fatalf("graphql status = %d (%v)", status, result)
	}
	if errs, ok := result["errors"]; ok {
		t.Fatalf("graphql errors = %v", errs)
	}
	data, _ := result["data"].(map[string]any)
	return data
}

func stateFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in %q", raw)
	}
	return state
}

func postSignedWebhook(t *testing.T, base, sourceID, secret string, body []byte) (status int, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v1/webhooks/github/%s", base, sourceID), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}
