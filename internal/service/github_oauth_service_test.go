package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

type fakeGitHubAPI struct {
	mu                  sync.Mutex
	profile             domain.GitHubUserProfile
	scopes              []string
	orgs                []domain.GitHubOwner
	repos               []domain.GitHubRemoteRepo
	repoErr             error
	profileErr          error
	reposErr            error
	exchangeCalls       int
	pollToken           *domain.GitHubTokenResult
	pollErr             error
	refreshToken        *domain.GitHubTokenResult
	refreshErr          error
	refreshCalls        int
	profileRevokedCalls int
	reposRevokedCalls   int
}

func (f *fakeGitHubAPI) AuthorizeURL(clientID, redirectURI, state, codeChallenge string, scopes []string) string {
	return fmt.Sprintf("https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&state=%s&code_challenge=%s&code_challenge_method=S256&scope=%s",
		url.QueryEscape(clientID), url.QueryEscape(redirectURI), url.QueryEscape(state), url.QueryEscape(codeChallenge), url.QueryEscape(strings.Join(scopes, " ")))
}

func (f *fakeGitHubAPI) ExchangeCode(context.Context, string, string, string) (*domain.GitHubTokenResult, error) {
	f.exchangeCalls++
	return &domain.GitHubTokenResult{AccessToken: "gho_exchanged", TokenType: "bearer", Scopes: f.scopes}, nil
}

func (f *fakeGitHubAPI) StartDeviceFlow(context.Context, []string) (*domain.GitHubDeviceCodePrompt, error) {
	return &domain.GitHubDeviceCodePrompt{DeviceCode: "device-code", UserCode: "ABCD-EFGH", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5}, nil
}

func (f *fakeGitHubAPI) PollDeviceFlow(context.Context, string) (*domain.GitHubTokenResult, error) {
	if f.pollErr != nil {
		return nil, f.pollErr
	}
	if f.pollToken != nil {
		return f.pollToken, nil
	}
	return &domain.GitHubTokenResult{AccessToken: "gho_device", TokenType: "bearer", Scopes: f.scopes}, nil
}

func (f *fakeGitHubAPI) RefreshToken(context.Context, string) (*domain.GitHubTokenResult, error) {
	f.mu.Lock()
	f.refreshCalls++
	refreshErr := f.refreshErr
	cp := f.refreshToken
	scopes := append([]string(nil), f.scopes...)
	f.mu.Unlock()
	if refreshErr != nil {
		return nil, refreshErr
	}
	if cp == nil {
		cp = &domain.GitHubTokenResult{
			AccessToken: "gho_refreshed", RefreshToken: "ghr_refreshed", TokenType: "bearer",
			Scopes: scopes, ExpiresIn: 28800, RefreshTokenExpiresIn: 15897600,
		}
	}
	out := *cp
	return &out, nil
}

func (f *fakeGitHubAPI) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

func (f *fakeGitHubAPI) GetAuthenticatedUser(context.Context, string, string) (*domain.GitHubUserProfile, []string, error) {
	f.mu.Lock()
	revoked := f.profileRevokedCalls > 0
	if revoked {
		f.profileRevokedCalls--
	}
	profileErr := f.profileErr
	f.mu.Unlock()
	if revoked {
		return nil, nil, domain.ErrGitHubTokenRevoked
	}
	if profileErr != nil {
		return nil, nil, profileErr
	}
	cp := f.profile
	return &cp, append([]string(nil), f.scopes...), nil
}

func (f *fakeGitHubAPI) ListUserOrgs(context.Context, string, string) ([]domain.GitHubOwner, error) {
	return append([]domain.GitHubOwner(nil), f.orgs...), nil
}

func (f *fakeGitHubAPI) ListAccessibleRepos(context.Context, string, string) ([]domain.GitHubRemoteRepo, error) {
	f.mu.Lock()
	revoked := f.reposRevokedCalls > 0
	if revoked {
		f.reposRevokedCalls--
	}
	reposErr := f.reposErr
	f.mu.Unlock()
	if revoked {
		return nil, domain.ErrGitHubTokenRevoked
	}
	if reposErr != nil {
		return nil, reposErr
	}
	return append([]domain.GitHubRemoteRepo(nil), f.repos...), nil
}

func (f *fakeGitHubAPI) GetRepository(_ context.Context, _, _, owner, repo string) (*domain.GitHubRemoteRepo, error) {
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	for _, candidate := range f.repos {
		if strings.EqualFold(candidate.FullName, fmt.Sprintf("%s/%s", owner, repo)) {
			cp := candidate
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("404 not found")
}

// failingRepoSettingRepo injects a Raft apply failure for rollback tests.
type failingRepoSettingRepo struct {
	domain.RepoSettingRepository
}

func (f *failingRepoSettingRepo) BatchUpsert(context.Context, string, []domain.RepoSettingWrite) error {
	return errors.New("raft apply failed")
}

// failingGitHubConnectionRepo injects a transient store failure for error
// mapping tests.
type failingGitHubConnectionRepo struct {
	domain.GitHubConnectionRepository
	err error
}

func (f *failingGitHubConnectionRepo) GetByUser(context.Context, string) (*domain.UserGitHubConnection, error) {
	return nil, f.err
}

type gitHubEnv struct {
	svc         *GitHubIntegrationService
	gh          *fakeGitHubAPI
	provider    *capturingForgeProvider
	store       *repository.RaftStore
	org         *domain.Org
	alice       *domain.User
	bob         *domain.User
	connections domain.GitHubConnectionRepository
	accounts    domain.GitHubAccountSourceRepository
	repoSets    domain.RepoSettingRepository
	forges      domain.ForgeSourceRepository
	oauthState  domain.GitHubOAuthStateRepository
}

func newGitHubEnv(t *testing.T) *gitHubEnv {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	store := newServiceStore(t)
	ctx := context.Background()

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	orgRepo := repository.NewOrgRepo(store)
	tokensSvc := NewTokenService(repository.NewTokenRepo(store), logger, []byte("0123456789abcdef0123456789abcdef"))
	usersSvc := NewUserService(userRepo, groupRepo, logger)
	alice := seedUserSvc(t, usersSvc, "alice-supervisor")
	bob := seedUserSvc(t, usersSvc, "bob-supervisor")
	org := &domain.Org{ID: "org-a", Name: "default", DisplayName: "Default", Features: domain.DefaultOrgFeatures, CreatedAt: time.Now().UTC()}
	if err := orgRepo.Create(ctx, org); err != nil {
		t.Fatal(err)
	}
	if err := orgRepo.SetMember(ctx, org.ID, alice.ID, domain.OrgAdmin); err != nil {
		t.Fatal(err)
	}
	if err := orgRepo.SetMember(ctx, org.ID, bob.ID, domain.OrgMember); err != nil {
		t.Fatal(err)
	}
	orgsSvc := NewOrgService(orgRepo, groupRepo, tokensSvc, logger)

	secrets := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logger)
	connections := repository.NewGitHubConnectionRepo(store)
	accounts := repository.NewGitHubAccountSourceRepo(store)
	repoSets := repository.NewRepoSettingRepo(store)
	forges := repository.NewForgeSourceRepo(store)
	states := repository.NewGitHubOAuthStateRepo(store)
	gh := &fakeGitHubAPI{
		profile: domain.GitHubUserProfile{ID: 1, Login: "alice", Type: "User"},
		scopes:  []string{"repo", "read:org"},
		orgs:    []domain.GitHubOwner{{Login: "acme", Type: "Organization"}},
		repos: []domain.GitHubRemoteRepo{
			{ID: 1, Name: "api", FullName: "alice/api", Owner: "alice", Private: true, DefaultBranch: "main", AdminPerm: true},
			{ID: 2, Name: "web", FullName: "alice/web", Owner: "alice", Private: false, DefaultBranch: "main", AdminPerm: true},
		},
	}
	provider := &capturingForgeProvider{kind: domain.ForgeGitHub}
	svc := NewGitHubIntegrationService(GitHubIntegrationConfig{
		PublicURL: "https://supv.example.com", DefaultAPIURL: "https://api.github.com",
		OAuthEnabled: true, ClientID: "client-id", ClientSecret: "client-secret",
		Scopes: []string{"repo", "read:org", "admin:repo_hook"}, StateTTL: 10 * time.Minute,
		DeviceFlowEnabled: true, AllowedRedirectURIs: []string{"https://alt.example.com/github/callback"},
		RefreshEnabled: true,
	}, GitHubIntegrationDeps{
		Connections: connections, AccountSources: accounts, RepoSettings: repoSets,
		ForgeSources: forges, OAuthStates: states, DeviceSessions: NewMemoryGitHubDeviceSessionStore(),
		Users: userRepo, Orgs: orgsSvc, Sealer: secrets, GitHub: gh, ForgeProvider: provider, Logger: logger,
	})
	return &gitHubEnv{
		svc: svc, gh: gh, provider: provider, store: store, org: org, alice: alice, bob: bob,
		connections: connections, accounts: accounts, repoSets: repoSets, forges: forges, oauthState: states,
	}
}

func TestGitHubIntegrationBuildOAuthURLDisabledAndRedirects(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.svc.cfg.OAuthEnabled = false
	_, err := env.svc.BuildOAuthURL(ctx, env.alice.ID, "", "https://supv.example.com/github/callback")
	if !errors.Is(err, domain.ErrGitHubOAuthDisabled) || !strings.Contains(err.Error(), "GitHub OAuth app integration is not configured") {
		t.Fatalf("disabled err = %v", err)
	}
	env.svc.cfg.OAuthEnabled = true

	valid := []string{
		"https://supv.example.com/github/callback",
		"https://alt.example.com/github/callback",
	}
	for _, redirect := range valid {
		got, err := env.svc.BuildOAuthURL(ctx, env.alice.ID, "", redirect)
		if err != nil {
			t.Fatalf("redirect %q: %v", redirect, err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatal(err)
		}
		if u.Query().Get("state") == "" || u.Query().Get("code_challenge") == "" || u.Query().Get("code_challenge_method") != "S256" {
			t.Fatalf("authorize URL = %s", got)
		}
	}
	invalid := []string{
		"https://evil.example.com/github/callback",
		"https://user:pass@supv.example.com/github/callback",
		"https://supv.example.com/github/callback?x=1",
		"https://supv.example.com/github/callback#frag",
		"//evil.example.com/github/callback",
		"https://supv.example.com/other",
		"http://supv.example.com/github/callback",
		"https://supv.example.com\\@evil.example.com/github/callback",
	}
	for _, redirect := range invalid {
		if _, err := env.svc.BuildOAuthURL(ctx, env.alice.ID, "", redirect); !errors.Is(err, domain.ErrInvalidRedirectURI) {
			t.Fatalf("redirect %q err = %v", redirect, err)
		}
	}
	if _, err := env.svc.BuildOAuthURL(ctx, "", "", valid[0]); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("missing user err = %v", err)
	}
	if _, err := env.svc.BuildOAuthURL(ctx, env.bob.ID, env.org.ID, valid[0]); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-admin org flow err = %v", err)
	}
}

func TestGitHubIntegrationCompleteOAuthAutoConnects(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	authURL, err := env.svc.BuildOAuthURL(ctx, env.alice.ID, "", "https://supv.example.com/github/callback")
	if err != nil {
		t.Fatal(err)
	}
	stateID := stateFromAuthorizeURL(t, authURL)
	result, err := env.svc.CompleteOAuth(ctx, env.alice.ID, false, "code-1", stateID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Connection.GitHubLogin != "alice" || result.Connection.TokenCiphertext == "" {
		t.Fatalf("connection = %+v", result.Connection)
	}
	if result.AutoConnectedOrg == nil || result.AutoConnectedOrg.Owner != "alice" || result.TargetOrgSlug != "default" {
		t.Fatalf("auto connect = %+v target=%q", result.AutoConnectedOrg, result.TargetOrgSlug)
	}
	if len(result.AvailableOwners) != 2 || result.AvailableOwners[1].Login != "acme" {
		t.Fatalf("owners = %+v", result.AvailableOwners)
	}
	accounts, err := env.accounts.ListByOrg(ctx, env.org.ID)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %v err=%v", accounts, err)
	}
	if accounts[0].TokenCiphertext == "" || strings.Contains(accounts[0].TokenCiphertext, "gho_") {
		t.Fatalf("account ciphertext = %q", accounts[0].TokenCiphertext)
	}
	// Replay of the consumed state is rejected.
	if _, err := env.svc.CompleteOAuth(ctx, env.alice.ID, false, "code-1", stateID); !errors.Is(err, domain.ErrOAuthStateInvalid) {
		t.Fatalf("replay err = %v", err)
	}
}

func TestGitHubIntegrationUserMismatchDoesNotConsumeState(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	authURL, err := env.svc.BuildOAuthURL(ctx, env.alice.ID, "", "https://supv.example.com/github/callback")
	if err != nil {
		t.Fatal(err)
	}
	stateID := stateFromAuthorizeURL(t, authURL)
	_, err = env.svc.CompleteOAuth(ctx, env.bob.ID, false, "code-1", stateID)
	var mismatch *domain.OAuthStateUserMismatchError
	if !errors.As(err, &mismatch) || !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("mismatch err = %v", err)
	}
	if mismatch.ExpectedUsername != "alice-supervisor" || mismatch.CurrentUsername != "bob-supervisor" {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	if env.gh.exchangeCalls != 0 {
		t.Fatalf("code must not be exchanged on mismatch, calls=%d", env.gh.exchangeCalls)
	}
	state, err := env.oauthState.Get(ctx, stateID)
	if err != nil || state.Consumed {
		t.Fatalf("state must remain unconsumed, state=%+v err=%v", state, err)
	}
	// The legitimate owner can still complete the same state.
	if _, err := env.svc.CompleteOAuth(ctx, env.alice.ID, false, "code-1", stateID); err != nil {
		t.Fatalf("owner completion: %v", err)
	}
	if env.gh.exchangeCalls != 1 {
		t.Fatalf("exchange calls = %d", env.gh.exchangeCalls)
	}
}

func TestGitHubIntegrationDeviceFlow(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	start, err := env.svc.StartDeviceFlow(ctx, env.alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if start.SessionID == "" || start.UserCode != "ABCD-EFGH" || start.Interval != 5 {
		t.Fatalf("start = %+v", start)
	}
	if _, err := env.svc.PollDeviceFlow(ctx, env.bob.ID, false, start.SessionID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("wrong user err = %v", err)
	}
	env.gh.pollErr = domain.ErrGitHubDeviceSlowDown
	if _, err := env.svc.PollDeviceFlow(ctx, env.alice.ID, false, start.SessionID); !errors.Is(err, domain.ErrGitHubDeviceSlowDown) {
		t.Fatalf("slow_down err = %v", err)
	}
	env.gh.pollErr = nil
	result, err := env.svc.PollDeviceFlow(ctx, env.alice.ID, false, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Connection.GitHubLogin != "alice" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := env.svc.PollDeviceFlow(ctx, env.alice.ID, false, start.SessionID); !errors.Is(err, domain.ErrOAuthStateInvalid) {
		t.Fatalf("replay err = %v", err)
	}
	// Device flow only needs a client ID: it stays available while the redirect
	// flow is disabled, and disappears when the client ID is removed.
	env.svc.cfg.OAuthEnabled = false
	if !env.svc.DeviceFlowEnabled() {
		t.Fatal("device flow must stay enabled with a client ID when redirect OAuth is disabled")
	}
	env.svc.cfg.ClientID = ""
	if env.svc.DeviceFlowEnabled() {
		t.Fatal("device flow must be disabled without a client ID")
	}
	if _, err := env.svc.StartDeviceFlow(ctx, env.alice.ID, ""); !errors.Is(err, domain.ErrGitHubDeviceFlowDisabled) {
		t.Fatalf("no client id err = %v", err)
	}
	env.svc.cfg.ClientID = "client-id"
	env.svc.cfg.DeviceFlowEnabled = false
	if _, err := env.svc.StartDeviceFlow(ctx, env.alice.ID, ""); !errors.Is(err, domain.ErrGitHubDeviceFlowDisabled) {
		t.Fatalf("disabled err = %v", err)
	}
}

func TestGitHubIntegrationDeviceFlowEnabled(t *testing.T) {
	cases := []struct {
		name          string
		oauthEnabled  bool
		clientID      string
		deviceEnabled bool
		want          bool
	}{
		{name: "device only", clientID: "client-id", deviceEnabled: true, want: true},
		{name: "device only with redirect flow disabled", oauthEnabled: false, clientID: "client-id", deviceEnabled: true, want: true},
		{name: "redirect flow enabled does not imply device flow", oauthEnabled: true, clientID: "client-id", deviceEnabled: false, want: false},
		{name: "no client id", oauthEnabled: true, deviceEnabled: true, want: false},
		{name: "empty client id and device flag off", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewGitHubIntegrationService(GitHubIntegrationConfig{
				OAuthEnabled:      tc.oauthEnabled,
				ClientID:          tc.clientID,
				DeviceFlowEnabled: tc.deviceEnabled,
			}, GitHubIntegrationDeps{})
			if got := svc.DeviceFlowEnabled(); got != tc.want {
				t.Fatalf("DeviceFlowEnabled() = %v want %v", got, tc.want)
			}
		})
	}
}

func TestGitHubIntegrationConnectOrgAccountValidation(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	if _, err := env.svc.ConnectOrgAccount(ctx, env.bob.ID, false, env.org.ID, "alice", "", "ghp_token"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-admin err = %v", err)
	}
	if _, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "evil", "", "ghp_token"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("owner spoof err = %v", err)
	}
	if _, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "http://169.254.169.254", "ghp_token"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ssrf err = %v", err)
	}
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "acme", "", "ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	if acct.ID != domain.GitHubAccountSourceID(env.org.ID, "https://api.github.com", "acme") || acct.OwnerType != "Organization" {
		t.Fatalf("account = %+v", acct)
	}
	if _, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_token"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ConnectUserPAT(ctx, env.alice.ID, "", "bad token"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("pat whitespace err = %v", err)
	}
	if _, err := env.svc.ConnectUserPAT(ctx, env.alice.ID, "https://api.github.com", "ghp_user"); err != nil {
		t.Fatal(err)
	}
	conn, err := env.svc.GetUserConnection(ctx, env.alice.ID)
	if err != nil || conn.GitHubLogin != "alice" {
		t.Fatalf("user connection = %+v err=%v", conn, err)
	}
	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.DisconnectUser(ctx, env.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.GetUserConnection(ctx, env.alice.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disconnect err = %v", err)
	}
}

func TestGitHubIntegrationUpdateOrgRepoSettings(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	in := []domain.RepoSettingMutation{{
		Repo: "github.com/alice/api", SourceID: &acct.ID,
		RunChecksOnDefaultBranch: &yes, RunChecksOnPullRequest: &yes, RunChecksOnBranchPush: &no,
	}}
	if err := env.svc.UpdateOrgRepoSettings(ctx, env.bob.ID, false, env.org, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-admin err = %v", err)
	}
	if err := env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, in); err != nil {
		t.Fatal(err)
	}
	if env.provider.registerCalls != 1 {
		t.Fatalf("register calls = %d", env.provider.registerCalls)
	}
	setting, err := env.repoSets.Get(ctx, env.org.ID, "github.com/alice/api")
	if err != nil {
		t.Fatal(err)
	}
	if setting.SourceID != acct.ID || setting.ForgeSourceID == "" || !setting.RunChecksOnDefaultBranch || setting.RunChecksOnBranchPush {
		t.Fatalf("setting = %+v", setting)
	}
	src, err := env.forges.Get(ctx, setting.ForgeSourceID)
	if err != nil || src.AccountSourceID != acct.ID || src.Repo != "alice/api" || !strings.HasPrefix(setting.ForgeSourceID, "ghrepo-") {
		t.Fatalf("forge source = %+v err=%v", src, err)
	}

	// checks off: triggers flip false but the webhook is not deleted.
	off := []domain.RepoSettingMutation{{
		Repo: "github.com/alice/api", SourceID: &acct.ID,
		RunChecksOnDefaultBranch: &no, RunChecksOnPullRequest: &no, RunChecksOnBranchPush: &no,
	}}
	if err := env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, off); err != nil {
		t.Fatal(err)
	}
	if env.provider.registerCalls != 1 || env.provider.deleteCalls != 1 { // one pre-clean only
		t.Fatalf("register=%d delete=%d", env.provider.registerCalls, env.provider.deleteCalls)
	}
	setting, err = env.repoSets.Get(ctx, env.org.ID, "github.com/alice/api")
	if err != nil || setting.RunChecksOnDefaultBranch || setting.RunChecksOnPullRequest {
		t.Fatalf("off setting = %+v err=%v", setting, err)
	}

	// Re-enabling flips the triggers back without registering a second hook.
	onAgain := []domain.RepoSettingMutation{{
		Repo: "github.com/alice/api", SourceID: &acct.ID,
		RunChecksOnDefaultBranch: &yes, RunChecksOnPullRequest: &yes,
	}}
	if err := env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, onAgain); err != nil {
		t.Fatal(err)
	}
	if env.provider.registerCalls != 1 {
		t.Fatalf("re-enable must not duplicate hooks, register=%d", env.provider.registerCalls)
	}
	setting, err = env.repoSets.Get(ctx, env.org.ID, "github.com/alice/api")
	if err != nil || !setting.RunChecksOnDefaultBranch || !setting.RunChecksOnPullRequest {
		t.Fatalf("re-enabled setting = %+v err=%v", setting, err)
	}

	// Claim release cleans the claim and webhook.
	if err := env.svc.DeleteOrgRepoSetting(ctx, env.alice.ID, false, env.org, "github.com/alice/api"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.repoSets.Get(ctx, env.org.ID, "github.com/alice/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("claim must be gone, err=%v", err)
	}
}

func TestGitHubIntegrationUpdateRollsBackWebhook(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	env.svc.repoSettings = &failingRepoSettingRepo{RepoSettingRepository: env.repoSets}
	yes := true
	err = env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, []domain.RepoSettingMutation{{
		Repo: "github.com/alice/web", SourceID: &acct.ID,
		RunChecksOnDefaultBranch: &yes, RunChecksOnPullRequest: &yes,
	}})
	if err == nil {
		t.Fatal("raft failure must propagate")
	}
	if env.provider.registerCalls != 1 || env.provider.deleteCalls < 2 {
		t.Fatalf("register=%d delete=%d (pre-clean + rollback expected)", env.provider.registerCalls, env.provider.deleteCalls)
	}
	if _, err := env.repoSets.Get(ctx, env.org.ID, "github.com/alice/web"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rollback must not persist a claim, err=%v", err)
	}
}

func TestGitHubIntegrationUpdateRejectsInaccessibleRepo(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	err = env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, []domain.RepoSettingMutation{{
		Repo: "github.com/alice/missing", SourceID: &acct.ID, RunChecksOnDefaultBranch: &yes, RunChecksOnPullRequest: &yes,
	}})
	if !errors.Is(err, domain.ErrRepoInaccessible) || !strings.Contains(err.Error(), "is not available through this GitHub connection") {
		t.Fatalf("inaccessible err = %v", err)
	}
	err = env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, []domain.RepoSettingMutation{{
		Repo: "github.com/acme/api", SourceID: &acct.ID, RunChecksOnDefaultBranch: &yes,
	}})
	if !errors.Is(err, domain.ErrRepoNotInInstallation) {
		t.Fatalf("owner mismatch err = %v", err)
	}
}

func TestGitHubIntegrationRevokedTokenStatus(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	env.gh.reposErr = domain.ErrGitHubTokenRevoked
	if _, err := env.svc.ListSourceRepositories(ctx, env.org, acct.ID); !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("list err = %v", err)
	}
	stored, err := env.accounts.Get(ctx, acct.ID)
	if err != nil || stored.Status != domain.GitHubConnectionRevoked {
		t.Fatalf("account status = %+v err=%v", stored, err)
	}
	if _, err := env.svc.ConnectUserPAT(ctx, env.alice.ID, "https://api.github.com", "ghp_user"); err != nil {
		t.Fatal(err)
	}
	env.gh.profileErr = domain.ErrGitHubTokenRevoked
	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("owners err = %v", err)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.Status != domain.GitHubConnectionRevoked {
		t.Fatalf("connection status = %+v err=%v", conn, err)
	}
}

func TestGitHubIntegrationUserTokenErrorMapping(t *testing.T) {
	cases := []struct {
		name        string
		storeErr    error
		wantNotConn bool
	}{
		{
			name:        "missing connection is a business state",
			wantNotConn: true,
		},
		{
			name:     "transient store error is not an auth failure",
			storeErr: errors.New("raft apply timeout"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newGitHubEnv(t)
			if tc.storeErr != nil {
				env.svc.connections = &failingGitHubConnectionRepo{GitHubConnectionRepository: env.connections, err: tc.storeErr}
			}
			_, err := env.svc.ListUserOwners(context.Background(), env.alice.ID)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, domain.ErrGitHubNotConnected); got != tc.wantNotConn {
				t.Fatalf("ErrGitHubNotConnected = %v, want %v (err=%v)", got, tc.wantNotConn, err)
			}
			if errors.Is(err, domain.ErrUnauthenticated) {
				t.Fatalf("business/store error must not be ErrUnauthenticated: %v", err)
			}
			if tc.storeErr != nil && !strings.Contains(err.Error(), "get github connection") {
				t.Fatalf("store error = %v, want a wrapped store failure", err)
			}
		})
	}
}

func TestGitHubIntegrationListRepositoriesAndDisconnectCascade(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_token")
	if err != nil {
		t.Fatal(err)
	}
	repos, err := env.svc.ListSourceRepositories(ctx, env.org, acct.ID)
	if err != nil || len(repos) != 2 {
		t.Fatalf("repos = %+v err=%v", repos, err)
	}
	if repos[0].Repository != "github.com/alice/api" || !repos[0].Eligible {
		t.Fatalf("repo = %+v", repos[0])
	}
	yes := true
	if err := env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, []domain.RepoSettingMutation{{
		Repo: "github.com/alice/api", SourceID: &acct.ID, RunChecksOnDefaultBranch: &yes, RunChecksOnPullRequest: &yes,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.DisconnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, acct.ID, false); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("no-cascade err = %v", err)
	}
	if err := env.svc.DisconnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, acct.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := env.accounts.Get(ctx, acct.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("account must be gone, err=%v", err)
	}
	if _, err := env.repoSets.Get(ctx, env.org.ID, "github.com/alice/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("claim must be gone, err=%v", err)
	}
}

func stateFromAuthorizeURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in %s", raw)
	}
	return state
}

func TestMemoryGitHubDeviceSessionStoreBounds(t *testing.T) {
	store := NewMemoryGitHubDeviceSessionStore()
	now := time.Now().UTC()
	for i := 0; i < maxGitHubDeviceSessions; i++ {
		if err := store.Create(&domain.GitHubDeviceSession{SessionID: newID(), UserID: "u", ExpiresAt: now.Add(time.Duration(i+1) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Create(&domain.GitHubDeviceSession{SessionID: "extra", UserID: "u", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("extra"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateInterval("extra", 10); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume("extra"); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume("extra"); !errors.Is(err, domain.ErrOAuthStateInvalid) {
		t.Fatalf("double consume err = %v", err)
	}
	store.Delete("extra")
	if _, err := store.Get("extra"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete err = %v", err)
	}
}

// seedRefreshableUser stores a user connection with sealed access/refresh
// tokens expiring in expiresIn, so the proactive-refresh window is controllable.
func (env *gitHubEnv) seedRefreshableUser(t *testing.T, userID, refreshToken string, expiresIn time.Duration) *domain.UserGitHubConnection {
	t.Helper()
	ctx := context.Background()
	accessCT, err := env.svc.sealer.SealRaw(domain.SecretScopeGitHubUser, userID, "gho_old")
	if err != nil {
		t.Fatal(err)
	}
	var refreshCT string
	if refreshToken != "" {
		if refreshCT, err = env.svc.sealer.SealRaw(domain.SecretScopeGitHubUser, userID, refreshToken); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	conn := &domain.UserGitHubConnection{
		UserID: userID, GitHubUserID: 1, GitHubLogin: "alice", BaseURL: "https://api.github.com",
		Scopes: []string{"repo"}, TokenCiphertext: accessCT, RefreshTokenCiphertext: refreshCT,
		TokenExpiresAt: now.Add(expiresIn), RefreshExpiresAt: now.Add(180 * 24 * time.Hour),
		TokenVersion: 1, Status: domain.GitHubConnectionActive, ConnectedAt: now, UpdatedAt: now,
	}
	if err := env.connections.Upsert(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

// flakyConnectionRepo injects UpsertIfVersion failures for persist-retry tests.
type flakyConnectionRepo struct {
	domain.GitHubConnectionRepository
	mu       sync.Mutex
	failures int
	calls    int
	err      error
}

func (f *flakyConnectionRepo) UpsertIfVersion(ctx context.Context, conn *domain.UserGitHubConnection, expected uint64) error {
	f.mu.Lock()
	f.calls++
	fail := f.calls <= f.failures
	f.mu.Unlock()
	if fail {
		return f.err
	}
	return f.GitHubConnectionRepository.UpsertIfVersion(ctx, conn, expected)
}

func (f *flakyConnectionRepo) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestGitHubIntegrationProactiveRefreshWindow(t *testing.T) {
	cases := []struct {
		name          string
		expiresIn     time.Duration
		wantRefreshed bool
	}{
		{"inside the skew window", time.Minute, true},
		{"outside the skew window", time.Hour, false},
		{"already expired", -time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newGitHubEnv(t)
			env.seedRefreshableUser(t, env.alice.ID, "ghr_old", tc.expiresIn)
			if _, err := env.svc.ListUserOwners(context.Background(), env.alice.ID); err != nil {
				t.Fatal(err)
			}
			if got := env.gh.refreshCount(); (got == 1) != tc.wantRefreshed {
				t.Fatalf("refresh calls = %d, want refreshed=%v", got, tc.wantRefreshed)
			}
			conn, err := env.connections.GetByUser(context.Background(), env.alice.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRefreshed {
				if conn.TokenVersion != 2 || conn.RefreshTokenCiphertext == "" || !conn.TokenExpiresAt.After(time.Now().Add(time.Hour)) {
					t.Fatalf("rotated connection = %+v", conn)
				}
				token, err := env.svc.sealer.OpenRaw(domain.SecretScopeGitHubUser, env.alice.ID, conn.TokenCiphertext)
				if err != nil || token != "gho_refreshed" {
					t.Fatalf("rotated access token = %q err=%v", token, err)
				}
			} else if conn.TokenVersion != 1 {
				t.Fatalf("token must not rotate outside the window: %+v", conn)
			}
		})
	}
}

func TestGitHubIntegrationReactiveRefreshRetriesOnce(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Hour)
	env.gh.mu.Lock()
	env.gh.profileRevokedCalls = 1
	env.gh.mu.Unlock()

	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); err != nil {
		t.Fatalf("reactive refresh retry: %v", err)
	}
	if got := env.gh.refreshCount(); got != 1 {
		t.Fatalf("refresh calls = %d want 1", got)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.Status != domain.GitHubConnectionActive || conn.TokenVersion != 2 {
		t.Fatalf("connection = %+v err=%v", conn, err)
	}
}

func TestGitHubIntegrationRefreshInvalidRevokes(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	env.gh.mu.Lock()
	env.gh.refreshErr = fmt.Errorf("%w: bad_refresh_token", domain.ErrGitHubRefreshInvalid)
	env.gh.mu.Unlock()

	_, err := env.svc.ListUserOwners(ctx, env.alice.ID)
	if !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("err = %v want ErrGitHubTokenRevoked", err)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.Status != domain.GitHubConnectionRevoked {
		t.Fatalf("connection = %+v err=%v", conn, err)
	}
	env.gh.mu.Lock()
	env.gh.refreshErr = nil
	env.gh.mu.Unlock()
}

func TestGitHubIntegrationRefreshTransientDoesNotRevoke(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	env.gh.mu.Lock()
	env.gh.refreshErr = errors.New("github token endpoint: status 503")
	env.gh.mu.Unlock()

	_, err := env.svc.ListUserOwners(ctx, env.alice.ID)
	if !errors.Is(err, domain.ErrGitHubRefreshUnavailable) {
		t.Fatalf("err = %v want ErrGitHubRefreshUnavailable", err)
	}
	if errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("transient failure must not revoke: %v", err)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.Status != domain.GitHubConnectionActive {
		t.Fatalf("connection = %+v err=%v", conn, err)
	}
}

func TestGitHubIntegrationExpiredRefreshTokenRevokesWithoutRoundTrip(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	conn := env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	conn.RefreshExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := env.connections.Upsert(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("err = %v want ErrGitHubTokenRevoked", err)
	}
	if got := env.gh.refreshCount(); got != 0 {
		t.Fatalf("a doomed refresh must not call GitHub, calls=%d", got)
	}
	stored, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || stored.Status != domain.GitHubConnectionRevoked {
		t.Fatalf("connection = %+v err=%v", stored, err)
	}
}

func TestGitHubIntegrationWithoutRefreshTokenKeepsLegacyBehavior(t *testing.T) {
	cases := []struct {
		name         string
		refreshToken string
		expiresIn    time.Duration
	}{
		{"pat style record", "", 0},
		{"non-expiring oauth record", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newGitHubEnv(t)
			ctx := context.Background()
			env.seedRefreshableUser(t, env.alice.ID, tc.refreshToken, tc.expiresIn)
			env.gh.mu.Lock()
			env.gh.profileRevokedCalls = 1
			env.gh.mu.Unlock()
			if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); !errors.Is(err, domain.ErrGitHubTokenRevoked) {
				t.Fatalf("err = %v want ErrGitHubTokenRevoked", err)
			}
			if got := env.gh.refreshCount(); got != 0 {
				t.Fatalf("no refresh token means no rotation, calls=%d", got)
			}
			conn, err := env.connections.GetByUser(ctx, env.alice.ID)
			if err != nil || conn.Status != domain.GitHubConnectionRevoked {
				t.Fatalf("connection = %+v err=%v", conn, err)
			}
		})
	}
}

func TestGitHubIntegrationLegacyRecordSkipsRefresh(t *testing.T) {
	// A pre-refresh Raft record encodes the new fields as zero values.
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "", 0)
	legacy, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy.TokenExpiresAt = time.Time{}
	legacy.RefreshExpiresAt = time.Time{}
	legacy.TokenVersion = 0
	if err := env.connections.Upsert(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.gh.refreshCount(); got != 0 {
		t.Fatalf("legacy records must not refresh, calls=%d", got)
	}
}

func TestGitHubIntegrationFollowerNeverRotates(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	env.svc.leaderFn = func() bool { return false }

	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); err != nil {
		t.Fatalf("follower must serve the current token: %v", err)
	}
	if got := env.gh.refreshCount(); got != 0 {
		t.Fatalf("follower rotated the token, calls=%d", got)
	}

	env.gh.mu.Lock()
	env.gh.profileRevokedCalls = 1
	env.gh.mu.Unlock()
	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); !errors.Is(err, domain.ErrNotLeader) {
		t.Fatalf("reactive 401 on a follower = %v want ErrNotLeader", err)
	}
	if got := env.gh.refreshCount(); got != 0 {
		t.Fatalf("follower rotated on the reactive path, calls=%d", got)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.Status != domain.GitHubConnectionActive {
		t.Fatalf("follower must not revoke: %+v err=%v", conn, err)
	}
}

func TestGitHubIntegrationRefreshSingleFlight(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.svc.ListUserOwners(ctx, env.alice.ID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent refresh: %v", err)
		}
	}
	if got := env.gh.refreshCount(); got != 1 {
		t.Fatalf("concurrent bursts must rotate once, calls=%d", got)
	}
}

func TestGitHubIntegrationRefreshPersistRetriesThenFails(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	flaky := &flakyConnectionRepo{GitHubConnectionRepository: env.connections, failures: githubRefreshPersistAttempts, err: domain.ErrNotLeader}
	env.svc.connections = flaky

	_, err := env.svc.ListUserOwners(ctx, env.alice.ID)
	if !errors.Is(err, domain.ErrGitHubRefreshUnavailable) || !errors.Is(err, domain.ErrNotLeader) {
		t.Fatalf("err = %v want ErrGitHubRefreshUnavailable wrapping ErrNotLeader", err)
	}
	if errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("persist failure must not revoke: %v", err)
	}
	if got := flaky.callCount(); got != githubRefreshPersistAttempts {
		t.Fatalf("persist attempts = %d want %d", got, githubRefreshPersistAttempts)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.Status != domain.GitHubConnectionActive || conn.TokenVersion != 1 {
		t.Fatalf("connection = %+v err=%v", conn, err)
	}
}

func TestGitHubIntegrationRefreshPersistRecoversOnRetry(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	flaky := &flakyConnectionRepo{GitHubConnectionRepository: env.connections, failures: 1, err: domain.ErrRaftTimeout}
	env.svc.connections = flaky

	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); err != nil {
		t.Fatalf("transient persist failure should recover: %v", err)
	}
	if got := flaky.callCount(); got != 2 {
		t.Fatalf("persist attempts = %d want 2", got)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.TokenVersion != 2 {
		t.Fatalf("connection = %+v err=%v", conn, err)
	}
}

func TestGitHubIntegrationRefreshKillSwitch(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.seedRefreshableUser(t, env.alice.ID, "ghr_old", time.Minute)
	env.svc.cfg.RefreshEnabled = false
	if _, err := env.svc.ListUserOwners(ctx, env.alice.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.gh.refreshCount(); got != 0 {
		t.Fatalf("refresh_enabled=false must disable rotation, calls=%d", got)
	}
}

func TestGitHubIntegrationAccountRefreshResealsForgeSources(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_org")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := env.svc.UpdateOrgRepoSettings(ctx, env.alice.ID, false, env.org, []domain.RepoSettingMutation{{
		Repo: "github.com/alice/api", SourceID: &acct.ID, RunChecksOnDefaultBranch: &yes,
	}}); err != nil {
		t.Fatal(err)
	}
	setting, err := env.repoSets.Get(ctx, env.org.ID, "github.com/alice/api")
	if err != nil {
		t.Fatal(err)
	}

	// Attach refresh material to the account and make it due.
	scopeID, err := domain.GitHubOrgAccountScopeID(env.org.ID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshCT, err := env.svc.sealer.SealRaw(domain.SecretScopeGitHubOrgAccount, scopeID, "ghr_acct")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := env.accounts.Get(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.RefreshTokenCiphertext = refreshCT
	stored.TokenExpiresAt = time.Now().UTC().Add(time.Minute)
	stored.RefreshExpiresAt = time.Now().UTC().Add(180 * 24 * time.Hour)
	if err := env.accounts.Upsert(ctx, stored); err != nil {
		t.Fatal(err)
	}

	if _, err := env.svc.ListSourceRepositories(ctx, env.org, acct.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.gh.refreshCount(); got != 1 {
		t.Fatalf("account refresh calls = %d want 1", got)
	}
	rotated, err := env.accounts.Get(ctx, acct.ID)
	if err != nil || rotated.TokenVersion != 2 {
		t.Fatalf("account = %+v err=%v", rotated, err)
	}
	src, err := env.forges.Get(ctx, setting.ForgeSourceID)
	if err != nil {
		t.Fatal(err)
	}
	resealed, err := env.svc.sealer.OpenRaw(domain.SecretScopeOrg, env.org.ID, src.TokenCiphertext)
	if err != nil || resealed != "gho_refreshed" {
		t.Fatalf("runtime forge source token = %q err=%v", resealed, err)
	}
}

func TestGitHubIntegrationAccountRefreshInvalidRevokes(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()
	acct, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_org")
	if err != nil {
		t.Fatal(err)
	}
	scopeID, err := domain.GitHubOrgAccountScopeID(env.org.ID, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	refreshCT, err := env.svc.sealer.SealRaw(domain.SecretScopeGitHubOrgAccount, scopeID, "ghr_acct")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := env.accounts.Get(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.RefreshTokenCiphertext = refreshCT
	stored.TokenExpiresAt = time.Now().UTC().Add(time.Minute)
	if err := env.accounts.Upsert(ctx, stored); err != nil {
		t.Fatal(err)
	}
	env.gh.mu.Lock()
	env.gh.refreshErr = fmt.Errorf("%w: incorrect_client_credentials", domain.ErrGitHubRefreshInvalid)
	env.gh.mu.Unlock()

	if _, err := env.svc.ListSourceRepositories(ctx, env.org, acct.ID); !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("err = %v want ErrGitHubTokenRevoked", err)
	}
	revoked, err := env.accounts.Get(ctx, acct.ID)
	if err != nil || revoked.Status != domain.GitHubConnectionRevoked {
		t.Fatalf("account = %+v err=%v", revoked, err)
	}
}

func TestGitHubIntegrationFileTransferGuard(t *testing.T) {
	// completeWithToken must capture refresh material and thread it into the
	// auto-connected org account (device/redirect flow with expiring tokens).
	env := newGitHubEnv(t)
	ctx := context.Background()
	env.gh.pollToken = &domain.GitHubTokenResult{
		AccessToken: "gho_expiring", RefreshToken: "ghr_expiring", Scope: "repo,read:org",
		ExpiresIn: 28800, RefreshTokenExpiresIn: 15897600,
	}
	start, err := env.svc.StartDeviceFlow(ctx, env.alice.ID, env.org.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := env.svc.PollDeviceFlow(ctx, env.alice.ID, false, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	conn := result.Connection
	if conn.TokenVersion != 1 || conn.RefreshTokenCiphertext == "" || conn.TokenExpiresAt.IsZero() || conn.RefreshExpiresAt.IsZero() {
		t.Fatalf("connection = %+v", conn)
	}
	if result.AutoConnectedOrg == nil || result.AutoConnectedOrg.RefreshTokenCiphertext == "" || result.AutoConnectedOrg.TokenExpiresAt.IsZero() {
		t.Fatalf("auto-connected account = %+v", result.AutoConnectedOrg)
	}
	scopeID, err := domain.GitHubOrgAccountScopeID(env.org.ID, result.AutoConnectedOrg.ID)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := env.svc.sealer.OpenRaw(domain.SecretScopeGitHubOrgAccount, scopeID, result.AutoConnectedOrg.RefreshTokenCiphertext)
	if err != nil || refresh != "ghr_expiring" {
		t.Fatalf("account refresh token = %q err=%v", refresh, err)
	}
}

func TestGitHubIntegrationReauthBumpsUserTokenVersion(t *testing.T) {
	// Re-authing must advance TokenVersion (existing+1), not reset it to 1.
	// Otherwise a rotation already in flight (holding the old version) would
	// pass its CAS and clobber the fresh re-auth token pair.
	env := newGitHubEnv(t)
	ctx := context.Background()

	env.gh.pollToken = &domain.GitHubTokenResult{
		AccessToken: "gho_v1", RefreshToken: "ghr_v1", Scope: "repo",
		ExpiresIn: 28800, RefreshTokenExpiresIn: 15897600,
	}
	start, err := env.svc.StartDeviceFlow(ctx, env.alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.PollDeviceFlow(ctx, env.alice.ID, false, start.SessionID); err != nil {
		t.Fatal(err)
	}
	conn, err := env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.TokenVersion != 1 {
		t.Fatalf("initial version = %+v err=%v", conn, err)
	}

	env.gh.pollToken = &domain.GitHubTokenResult{
		AccessToken: "gho_v2", RefreshToken: "ghr_v2", Scope: "repo",
		ExpiresIn: 28800, RefreshTokenExpiresIn: 15897600,
	}
	start, err = env.svc.StartDeviceFlow(ctx, env.alice.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.PollDeviceFlow(ctx, env.alice.ID, false, start.SessionID); err != nil {
		t.Fatal(err)
	}
	conn, err = env.connections.GetByUser(ctx, env.alice.ID)
	if err != nil || conn.TokenVersion != 2 {
		t.Fatalf("re-auth version = %d (want 2) conn=%+v err=%v", conn.TokenVersion, conn, err)
	}
}

func TestGitHubIntegrationAccountRelinkBumpsTokenVersion(t *testing.T) {
	env := newGitHubEnv(t)
	ctx := context.Background()

	if _, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_org"); err != nil {
		t.Fatal(err)
	}
	acct, err := env.accounts.GetByOrgAndOwner(ctx, env.org.ID, "alice")
	if err != nil || acct.TokenVersion != 1 {
		t.Fatalf("initial account = %+v err=%v", acct, err)
	}
	if _, err := env.svc.ConnectOrgAccount(ctx, env.alice.ID, false, env.org.ID, "alice", "", "ghp_org"); err != nil {
		t.Fatal(err)
	}
	acct, err = env.accounts.GetByOrgAndOwner(ctx, env.org.ID, "alice")
	if err != nil || acct.TokenVersion != 2 {
		t.Fatalf("re-link account version = %d (want 2) acct=%+v err=%v", acct.TokenVersion, acct, err)
	}
}
