package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newOAuthService(t *testing.T, cfg *domain.OAuthConfig, ghSrv *httptest.Server) (*GitHubOAuthService, *UserService, *GroupService) {
	t.Helper()
	r := newServiceDB(t)
	logger := testLogger()
	usvc := NewUserService(r.users, r.groups, logger)
	gsvc := NewGroupService(r.groups, r.users, logger)
	jwtSvc := NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 15*time.Minute, 168*time.Hour)
	mapper, err := NewGroupMapper(cfg.GroupMappings)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	svc := NewGitHubOAuthService(cfg, mapper, usvc, r.groups, jwtSvc, logger)
	if ghSrv != nil {
		svc.tokenURL = ghSrv.URL + "/login/oauth/access_token"
		svc.apiBaseURL = ghSrv.URL
	}
	return svc, usvc, gsvc
}

func newGitHubServer(t *testing.T, orgs, teams []string) *httptest.Server {
	t.Helper()
	return newGitHubServerTeamsHandler(t, orgs, githubTeamsHandler(teams))
}

// newGitHubServerTeamsHandler builds the loopback GitHub API stub with the
// given orgs and an explicit /user/teams handler (used to inject failures).
func newGitHubServerTeamsHandler(t *testing.T, orgs []string, teams http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gh-token"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "login": "ghuser"})
	})
	mux.HandleFunc("/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		orgList := make([]map[string]string, 0, len(orgs))
		for _, o := range orgs {
			orgList = append(orgList, map[string]string{"login": o})
		}
		_ = json.NewEncoder(w).Encode(orgList)
	})
	mux.HandleFunc("/user/teams", teams)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// githubTeamsHandler returns a /user/teams handler serving the given "org/team"
// slugs.
func githubTeamsHandler(teams []string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		teamList := make([]map[string]any, 0, len(teams))
		for _, team := range teams {
			org, slug, _ := strings.Cut(team, "/")
			teamList = append(teamList, map[string]any{
				"slug":         slug,
				"organization": map[string]string{"login": org},
			})
		}
		_ = json.NewEncoder(w).Encode(teamList)
	}
}

// githubTeamsFailHandler is a /user/teams handler that always fails.
func githubTeamsFailHandler(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "boom", http.StatusInternalServerError)
}

func TestOAuthCompleteSuccess(t *testing.T) {
	gh := newGitHubServer(t, []string{"acme"}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		RedirectURL:  "https://supv.example.com/api/v1/auth/oauth/github/callback",
		AllowedOrgs:  []string{"acme"},
	}
	svc, usvc, _ := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	access, refresh, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if access == "" || refresh == "" || u == nil {
		t.Fatal("bad result")
	}
	if u.Username != "ghuser" {
		t.Fatalf("username = %q, want ghuser", u.Username)
	}
	if u.OAuthProvider != "github" || u.OAuthID != "42" {
		t.Fatalf("oauth = %s/%s", u.OAuthProvider, u.OAuthID)
	}

	// Second call returns the same user (idempotent).
	_, _, u2, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if u2.ID != u.ID {
		t.Fatal("should return same user")
	}
	_ = usvc
}

func TestOAuthCompleteOrgNotAllowed(t *testing.T) {
	gh := newGitHubServer(t, []string{"other-org"}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  []string{"acme"},
	}
	svc, _, _ := newOAuthService(t, &cfg, gh)
	_, _, _, err := svc.Complete(context.Background(), "code")
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("org not allowed: %v, want ErrForbidden", err)
	}
}

func TestOAuthCompleteNoAllowedOrgsRestriction(t *testing.T) {
	gh := newGitHubServer(t, []string{"anyorg"}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  nil, // empty = allow all
	}
	svc, _, _ := newOAuthService(t, &cfg, gh)
	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with no org restriction: %v", err)
	}
}

func TestOAuthCompleteDefaultGroupAutoJoin(t *testing.T) {
	gh := newGitHubServer(t, []string{}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  nil,
		DefaultGroup: "unused-param", // ignored: fallback is now hardcoded "default"
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Pre-create the hardcoded "default" group (the only group that matters now).
	g, _ := gsvc.Create(ctx, GroupInput{Name: "default", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("user should auto-join hardcoded default group, got %v", groups)
	}
}

func TestOAuthCompleteUsernameCollision(t *testing.T) {
	gh := newGitHubServer(t, []string{}, nil)
	cfg := domain.OAuthConfig{Enabled: true, ClientID: "cid", ClientSecret: "csec", AllowedOrgs: nil}
	svc, usvc, _ := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Pre-create a user named "ghuser".
	usvc.Create(ctx, "ghuser", "password123", domain.RoleUser)

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u.Username != "ghuser-2" {
		t.Fatalf("username = %q, want ghuser-2", u.Username)
	}
}

func TestOAuthLoginURL(t *testing.T) {
	cfg := domain.OAuthConfig{Enabled: true, ClientID: "cid", RedirectURL: "https://supv.example.com/cb"}
	svc, _, _ := newOAuthService(t, &cfg, nil)
	loginURL := svc.LoginURL("state123")
	if !strings.Contains(loginURL, "client_id=cid") || !strings.Contains(loginURL, "state=state123") || !strings.Contains(loginURL, "redirect_uri=https") {
		t.Fatalf("login url = %q", loginURL)
	}
}

// TestOAuthLoginURLEscapesParams verifies config values with reserved
// characters are percent-encoded and cannot corrupt the query string.
func TestOAuthLoginURLEscapesParams(t *testing.T) {
	cfg := domain.OAuthConfig{Enabled: true, ClientID: "cid&evil=1", RedirectURL: "https://supv.example.com/cb?x=1&y=2"}
	svc, _, _ := newOAuthService(t, &cfg, nil)
	u, err := url.Parse(svc.LoginURL("state123"))
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "cid&evil=1" {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "https://supv.example.com/cb?x=1&y=2" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("evil") != "" {
		t.Fatal("client_id must not inject extra params")
	}
	if q.Get("state") != "state123" || q.Get("scope") != "read:org" {
		t.Fatalf("state/scope = %q/%q", q.Get("state"), q.Get("scope"))
	}
}

func TestOAuthCompleteAllowedTeams(t *testing.T) {
	tests := []struct {
		name    string
		teams   []string
		allowed []string
		wantErr bool
	}{
		{name: "pass", teams: []string{"acme/eng"}, allowed: []string{"acme/eng"}},
		{name: "deny", teams: []string{"other/eng"}, allowed: []string{"acme/eng"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newGitHubServer(t, []string{"acme"}, tt.teams)
			cfg := domain.OAuthConfig{
				Enabled:      true,
				ClientID:     "cid",
				ClientSecret: "csec",
				AllowedTeams: tt.allowed,
			}
			svc, _, _ := newOAuthService(t, &cfg, gh)
			_, _, _, err := svc.Complete(context.Background(), "code")
			if tt.wantErr {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatalf("team not allowed: %v, want ErrForbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Complete with allowed team: %v", err)
			}
		})
	}
}

func TestOAuthCompleteOrgsAndTeamsBoth(t *testing.T) {
	tests := []struct {
		name         string
		orgs, teams  []string
		allowedOrgs  []string
		allowedTeams []string
		wantErr      bool
	}{
		{name: "both satisfied", orgs: []string{"acme"}, teams: []string{"acme/eng"}, allowedOrgs: []string{"acme"}, allowedTeams: []string{"acme/eng"}},
		{name: "org satisfied team not", orgs: []string{"acme"}, teams: []string{"other/eng"}, allowedOrgs: []string{"acme"}, allowedTeams: []string{"acme/eng"}, wantErr: true},
		{name: "team satisfied org not", orgs: []string{"other"}, teams: []string{"acme/eng"}, allowedOrgs: []string{"acme"}, allowedTeams: []string{"acme/eng"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newGitHubServer(t, tt.orgs, tt.teams)
			cfg := domain.OAuthConfig{
				Enabled:      true,
				ClientID:     "cid",
				ClientSecret: "csec",
				AllowedOrgs:  tt.allowedOrgs,
				AllowedTeams: tt.allowedTeams,
			}
			svc, _, _ := newOAuthService(t, &cfg, gh)
			_, _, _, err := svc.Complete(context.Background(), "code")
			if tt.wantErr {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatalf("allowlist mismatch: %v, want ErrForbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Complete with org+team: %v", err)
			}
		})
	}
}

func TestOAuthCompleteGroupMapping(t *testing.T) {
	gh := newGitHubServer(t, []string{"acme"}, []string{"acme/eng"})
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		GroupMappings: []domain.GroupMappingRule{
			{Pattern: "^acme$", Replacement: "acme-all"},
			{Pattern: "^acme/eng$", Replacement: "acme-eng"},
		},
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Pre-create the target groups.
	all, _ := gsvc.Create(ctx, GroupInput{Name: "acme-all", AgentAvailable: true})
	eng, _ := gsvc.Create(ctx, GroupInput{Name: "acme-eng", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	ids := map[string]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}
	if !ids[all.ID] || !ids[eng.ID] {
		t.Fatalf("user should be member of mapped groups, got %v", groups)
	}
}

func TestOAuthCompleteGroupMappingMissingGroup(t *testing.T) {
	gh := newGitHubServer(t, []string{"acme"}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		GroupMappings: []domain.GroupMappingRule{
			{Pattern: "^acme$", Replacement: "does-not-exist"},
		},
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 0 {
		t.Fatalf("missing mapped group should be skipped, got %v", groups)
	}
}

func TestOAuthCompleteNoGroupMappingsNoSync(t *testing.T) {
	gh := newGitHubServer(t, []string{"acme"}, []string{"acme/eng"})
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Pre-create a group whose name matches an org/team — it must NOT be joined
	// because no mapping rules are configured (identity at Map level, but the
	// service skips sync via Active()).
	gsvc.Create(ctx, GroupInput{Name: "acme", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 0 {
		t.Fatalf("no mappings should mean no auto-membership, got %v", groups)
	}
}

func TestOAuthCompleteTeamsFetchFailureBestEffort(t *testing.T) {
	gh := newGitHubServerTeamsHandler(t, []string{"acme"}, githubTeamsFailHandler)

	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		GroupMappings: []domain.GroupMappingRule{
			{Pattern: "^acme$", Replacement: "acme-mapped"},
		},
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()
	g, _ := gsvc.Create(ctx, GroupInput{Name: "acme-mapped", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("teams fetch failure should be best-effort when only mapping needs teams: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("mapping should fall back to orgs only, got %v", groups)
	}
}

func TestOAuthCompleteTeamsFetchFailureFatal(t *testing.T) {
	gh := newGitHubServerTeamsHandler(t, []string{"acme"}, githubTeamsFailHandler)

	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedTeams: []string{"acme/eng"},
	}
	svc, _, _ := newOAuthService(t, &cfg, gh)

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "fetch github teams") {
		t.Fatalf("teams fetch failure with allowed_teams should be fatal, got %v", err)
	}
}

func TestCompleteOAuthUserFallbackToDefault(t *testing.T) {
	gh := newGitHubServer(t, []string{}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  nil,
		DefaultGroup: "unused-param", // ignored by the new logic
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Create the "default" group.
	dg, _ := gsvc.Create(ctx, GroupInput{Name: "default", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].ID != dg.ID {
		t.Fatalf("user should fall back to default group, got %v", groups)
	}
}

func TestCompleteOAuthUserWithMappedGroups(t *testing.T) {
	gh := newGitHubServer(t, []string{"acme"}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  nil,
		GroupMappings: []domain.GroupMappingRule{
			{Pattern: "^acme$", Replacement: "acme-mapped"},
		},
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Create both the mapped group and the "default" group.
	mapped, _ := gsvc.Create(ctx, GroupInput{Name: "acme-mapped", AgentAvailable: true})
	dg, _ := gsvc.Create(ctx, GroupInput{Name: "default", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	ids := map[string]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}
	if !ids[mapped.ID] {
		t.Fatalf("user should be member of mapped group, got %v", groups)
	}
	if ids[dg.ID] {
		t.Fatal("user should NOT be member of default group when mapping rules matched")
	}
}

func TestCompleteOAuthUserNoDefaultGroupExists(t *testing.T) {
	gh := newGitHubServer(t, []string{}, nil)
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  nil,
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Do NOT create a "default" group.

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 0 {
		t.Fatalf("user should have 0 memberships, got %v", groups)
	}
}
