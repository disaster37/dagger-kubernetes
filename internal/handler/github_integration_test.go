package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

type handlerFakeGitHub struct {
	pollErr   error
	pollToken *domain.GitHubTokenResult
}

func (f *handlerFakeGitHub) AuthorizeURL(_, redirectURI, state, challenge string, scopes []string) string {
	return fmt.Sprintf("https://github.com/login/oauth/authorize?redirect_uri=%s&state=%s&code_challenge=%s&scope=%s",
		url.QueryEscape(redirectURI), state, challenge, url.QueryEscape(strings.Join(scopes, " ")))
}
func (f *handlerFakeGitHub) ExchangeCode(context.Context, string, string, string) (*domain.GitHubTokenResult, error) {
	return &domain.GitHubTokenResult{AccessToken: "gho_exchanged", Scopes: []string{"repo"}}, nil
}
func (f *handlerFakeGitHub) StartDeviceFlow(context.Context, []string) (*domain.GitHubDeviceCodePrompt, error) {
	return &domain.GitHubDeviceCodePrompt{DeviceCode: "dev", UserCode: "ABCD-EFGH", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5}, nil
}
func (f *handlerFakeGitHub) PollDeviceFlow(context.Context, string) (*domain.GitHubTokenResult, error) {
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	return f.pollToken, nil
}
func (f *handlerFakeGitHub) GetAuthenticatedUser(context.Context, string, string) (*domain.GitHubUserProfile, []string, error) {
	return &domain.GitHubUserProfile{ID: 1, Login: "alice", Type: "User"}, []string{"repo"}, nil
}
func (f *handlerFakeGitHub) ListUserOrgs(context.Context, string, string) ([]domain.GitHubOwner, error) {
	return []domain.GitHubOwner{{Login: "acme", Type: "Organization"}}, nil
}
func (f *handlerFakeGitHub) ListAccessibleRepos(context.Context, string, string) ([]domain.GitHubRemoteRepo, error) {
	return []domain.GitHubRemoteRepo{{ID: 1, Name: "api", FullName: "acme/api", Owner: "acme", Private: true, DefaultBranch: "main", AdminPerm: true}}, nil
}
func (f *handlerFakeGitHub) GetRepository(context.Context, string, string, string, string) (*domain.GitHubRemoteRepo, error) {
	return &domain.GitHubRemoteRepo{ID: 1, Name: "api", FullName: "acme/api", Owner: "acme", Private: true, DefaultBranch: "main", AdminPerm: true}, nil
}

type handlerFakeForge struct{}

func (handlerFakeForge) Kind() domain.ForgeKind { return domain.ForgeGitHub }
func (handlerFakeForge) RegisterWebhook(context.Context, *domain.ForgeSource, string, string, string) (string, error) {
	return "1", nil
}
func (handlerFakeForge) DeleteWebhook(context.Context, *domain.ForgeSource, string, string) error {
	return nil
}
func (handlerFakeForge) VerifyWebhook(*domain.ForgeSource, string, map[string]string, []byte) bool {
	return true
}
func (handlerFakeForge) SetCommitStatus(context.Context, *domain.ForgeSource, string, *domain.CheckRun) error {
	return nil
}
func (handlerFakeForge) CreateCheckRun(context.Context, *domain.ForgeSource, string, *domain.CheckRun) error {
	return nil
}

func newGitHubHandlerEnv(t *testing.T) (*testEnv, *handlerFakeGitHub) {
	t.Helper()
	return newGitHubHandlerEnvWithConfig(t, &service.GitHubIntegrationConfig{
		PublicURL: "https://supv.example.com", DefaultAPIURL: "https://api.github.com",
		OAuthEnabled: true, ClientID: "client", ClientSecret: "secret",
		Scopes: []string{"repo"}, StateTTL: 10 * time.Minute, DeviceFlowEnabled: true,
	})
}

func newGitHubHandlerEnvWithConfig(t *testing.T, cfg *service.GitHubIntegrationConfig) (*testEnv, *handlerFakeGitHub) {
	t.Helper()
	env := newTestEnv(t)
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	store := env.store
	sealer := service.NewSecretService(repository.NewSecretRepo(store), testTokenEncKey(), logger)
	gh := &handlerFakeGitHub{}
	integration := service.NewGitHubIntegrationService(*cfg, service.GitHubIntegrationDeps{
		Connections: repository.NewGitHubConnectionRepo(store), AccountSources: repository.NewGitHubAccountSourceRepo(store),
		RepoSettings: repository.NewRepoSettingRepo(store), ForgeSources: repository.NewForgeSourceRepo(store),
		OAuthStates: repository.NewGitHubOAuthStateRepo(store), DeviceSessions: service.NewMemoryGitHubDeviceSessionStore(),
		Users: repository.NewUserRepo(store), Orgs: env.server.orgs, Sealer: sealer, GitHub: gh,
		ForgeProvider: handlerFakeForge{}, Logger: observ.NewTestLogger(),
	})
	env.server.githubIntegration = integration
	users, err := env.users.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == "admin" {
			if err := env.server.orgs.SetMember(context.Background(), env.orgID, u.ID, domain.OrgAdmin); err != nil {
				t.Fatal(err)
			}
		}
	}
	return env, gh
}

func TestGitHubIntegrationDeviceOnlyConnectionFlags(t *testing.T) {
	env, _ := newGitHubHandlerEnvWithConfig(t, &service.GitHubIntegrationConfig{
		PublicURL: "https://supv.example.com", DefaultAPIURL: "https://api.github.com",
		OAuthEnabled: false, ClientID: "device-client", Scopes: []string{"repo"},
		StateTTL: 10 * time.Minute, DeviceFlowEnabled: true,
	})
	e := newAuthEngine(env.server)
	admin := env.loginAsAdmin(t)

	resp := ut.PerformRequest(e, "GET", "/api/v1/github/connection", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("connection status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	var state map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &state)
	if state["oauth_enabled"] != false || state["device_flow_enabled"] != true {
		t.Fatalf("device-only connection state = %v", state)
	}

	// A client ID alone is enough to start the device flow.
	resp = ut.PerformRequest(e, "POST", "/api/v1/github/device/code", &ut.Body{Body: strings.NewReader(`{}`), Len: 2},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("device code status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
}

func TestGitHubIntegrationDeviceFlowWithoutClientID(t *testing.T) {
	env, _ := newGitHubHandlerEnvWithConfig(t, &service.GitHubIntegrationConfig{
		PublicURL: "https://supv.example.com", DefaultAPIURL: "https://api.github.com",
		OAuthEnabled: false, Scopes: []string{"repo"}, StateTTL: 10 * time.Minute,
		DeviceFlowEnabled: true,
	})
	e := newAuthEngine(env.server)
	admin := env.loginAsAdmin(t)

	resp := ut.PerformRequest(e, "GET", "/api/v1/github/connection", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("connection status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	var state map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &state)
	if state["oauth_enabled"] != false || state["device_flow_enabled"] != false {
		t.Fatalf("no-client connection state = %v", state)
	}

	resp = ut.PerformRequest(e, "POST", "/api/v1/github/device/code", &ut.Body{Body: strings.NewReader(`{}`), Len: 2},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("device code without client id status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	if !strings.Contains(string(resp.Result().Body()), "not configured") {
		t.Fatalf("device code body = %s", resp.Result().Body())
	}
}

func TestGitHubIntegrationRoutes(t *testing.T) {
	env, gh := newGitHubHandlerEnv(t)
	e := newAuthEngine(env.server)
	admin := env.loginAsAdmin(t)

	if resp := ut.PerformRequest(e, "GET", "/api/v1/github/connection", nil); resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.Result().StatusCode())
	}
	resp := ut.PerformRequest(e, "GET", "/api/v1/github/connection", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("connection status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	var state map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &state)
	if state["connected"] != false || state["oauth_enabled"] != true || state["device_flow_enabled"] != true {
		t.Fatalf("connection state = %v", state)
	}

	// PAT connect.
	body := `{"token":"ghp_handler"}`
	resp = ut.PerformRequest(e, "POST", "/api/v1/github/pat", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("pat status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	if strings.Contains(string(resp.Result().Body()), "token_ciphertext") || strings.Contains(string(resp.Result().Body()), "ghp_handler") {
		t.Fatalf("response leaked a token: %s", resp.Result().Body())
	}
	resp = ut.PerformRequest(e, "GET", "/api/v1/github/owners", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("owners status = %d", resp.Result().StatusCode())
	}

	// Device flow: pending, then completion.
	resp = ut.PerformRequest(e, "POST", "/api/v1/github/device/code", &ut.Body{Body: strings.NewReader(`{}`), Len: 2},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("device code status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	var device map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &device)
	sessionID, _ := device["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("device = %v", device)
	}
	gh.pollErr = domain.ErrGitHubDeviceAuthorizationDelay
	pollBody := `{"session_id":"` + sessionID + `"}`
	resp = ut.PerformRequest(e, "POST", "/api/v1/github/device/poll", &ut.Body{Body: strings.NewReader(pollBody), Len: len(pollBody)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	var poll map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &poll)
	if resp.Result().StatusCode() != http.StatusOK || poll["status"] != "pending" {
		t.Fatalf("poll = %d %v", resp.Result().StatusCode(), poll)
	}

	// A non-admin org member cannot manage org accounts.
	memberBearer, member := env.createUserAndToken(t)
	if err := env.server.orgs.SetMember(context.Background(), env.orgID, member.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	memberBody := `{"owner":"acme","token":"ghp_member"}`
	resp = ut.PerformRequest(e, "POST", env.orgPath("")+"/github/accounts", &ut.Body{Body: strings.NewReader(memberBody), Len: len(memberBody)},
		ut.Header{Key: "Authorization", Value: memberBearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("member account create = %d", resp.Result().StatusCode())
	}

	// Oversized bodies are rejected before decoding.
	oversized := `{"token":"` + strings.Repeat("a", maxControlBody) + `"}`
	resp = ut.PerformRequest(e, "POST", "/api/v1/github/pat", &ut.Body{Body: strings.NewReader(oversized), Len: len(oversized)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", resp.Result().StatusCode())
	}

	// Org OAuth start + accounts.
	orgPath := env.orgPath("")
	startBody := `{"redirect_uri":"https://supv.example.com/github/callback"}`
	resp = ut.PerformRequest(e, "POST", orgPath+"/github/oauth/start", &ut.Body{Body: strings.NewReader(startBody), Len: len(startBody)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK || !strings.Contains(string(resp.Result().Body()), "authorize_url") {
		t.Fatalf("oauth start = %d %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	resp = ut.PerformRequest(e, "GET", orgPath+"/github/accounts", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("accounts list = %d", resp.Result().StatusCode())
	}
	acctBody := `{"owner":"acme","token":"ghp_org"}`
	resp = ut.PerformRequest(e, "POST", orgPath+"/github/accounts", &ut.Body{Body: strings.NewReader(acctBody), Len: len(acctBody)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusCreated {
		t.Fatalf("account create = %d %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	var acct map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &acct)
	acctID, _ := acct["id"].(string)
	if acctID == "" {
		t.Fatalf("account = %v", acct)
	}
	resp = ut.PerformRequest(e, "GET", orgPath+"/github/repositories?account_id="+url.QueryEscape(acctID), nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusOK || !strings.Contains(string(resp.Result().Body()), "github.com/acme/api") {
		t.Fatalf("repositories = %d %s", resp.Result().StatusCode(), resp.Result().Body())
	}

	// Sources, settings, claim.
	srcBody := `{"account_id":"` + acctID + `","repos":[{"repo":"github.com/acme/api","run_checks_on_default_branch":true,"run_checks_on_pull_request":true}]}`
	resp = ut.PerformRequest(e, "POST", orgPath+"/github/sources", &ut.Body{Body: strings.NewReader(srcBody), Len: len(srcBody)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("sources = %d %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	settingsBody := `{"run_checks_on_branch_push":true}`
	resp = ut.PerformRequest(e, "PUT", orgPath+"/github/repos/acme/api/settings", &ut.Body{Body: strings.NewReader(settingsBody), Len: len(settingsBody)},
		ut.Header{Key: "Authorization", Value: admin}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("settings = %d %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	resp = ut.PerformRequest(e, "DELETE", orgPath+"/github/repos/acme/api/claim", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("claim delete = %d", resp.Result().StatusCode())
	}
	resp = ut.PerformRequest(e, "DELETE", orgPath+"/github/accounts/"+url.QueryEscape(acctID), nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("account delete = %d", resp.Result().StatusCode())
	}
	resp = ut.PerformRequest(e, "DELETE", "/api/v1/github/connection", nil, ut.Header{Key: "Authorization", Value: admin})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("connection delete = %d", resp.Result().StatusCode())
	}
}

func TestGitHubIntegrationOAuthCompleteUserMismatch(t *testing.T) {
	env, _ := newGitHubHandlerEnv(t)
	e := newAuthEngine(env.server)
	admin := env.loginAsAdmin(t)
	adminID := ""
	for _, u := range mustUsers(t, env) {
		if u.Username == "admin" {
			adminID = u.ID
		}
	}
	if adminID == "" {
		t.Fatal("admin not found")
	}
	authURL, err := env.server.githubIntegration.BuildOAuthURL(context.Background(), adminID, "", "https://supv.example.com/github/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	stateID := u.Query().Get("state")
	bearer, _ := env.createUserAndToken(t)
	body := `{"code":"code-1","state":"` + stateID + `"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/github/oauth/complete", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("mismatch status = %d body=%s", resp.Result().StatusCode(), resp.Result().Body())
	}
	var mismatch map[string]any
	_ = json.Unmarshal(resp.Result().Body(), &mismatch)
	if mismatch["code"] != "state_user_mismatch" || mismatch["expected_username"] != "admin" || mismatch["current_username"] != "alice" {
		t.Fatalf("mismatch body = %v", mismatch)
	}
	if got := string(resp.Result().Header.Peek("Cache-Control")); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	_ = admin
}

func mustUsers(t *testing.T, env *testEnv) []*domain.User {
	t.Helper()
	users, err := env.users.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return users
}
