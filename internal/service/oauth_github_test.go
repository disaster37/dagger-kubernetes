package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	svc := NewGitHubOAuthService(cfg, usvc, r.groups, jwtSvc, logger)
	if ghSrv != nil {
		svc.tokenURL = ghSrv.URL + "/login/oauth/access_token"
		svc.apiBaseURL = ghSrv.URL
	}
	return svc, usvc, gsvc
}

func newGitHubServer(t *testing.T, orgs []string) *httptest.Server {
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOAuthCompleteSuccess(t *testing.T) {
	gh := newGitHubServer(t, []string{"acme"})
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
	gh := newGitHubServer(t, []string{"other-org"})
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
	gh := newGitHubServer(t, []string{"anyorg"})
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
	gh := newGitHubServer(t, []string{})
	cfg := domain.OAuthConfig{
		Enabled:      true,
		ClientID:     "cid",
		ClientSecret: "csec",
		AllowedOrgs:  nil,
		DefaultGroup: "auto-join",
	}
	svc, _, gsvc := newOAuthService(t, &cfg, gh)
	ctx := context.Background()

	// Pre-create the default group.
	g, _ := gsvc.Create(ctx, GroupInput{Name: "auto-join", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("user should auto-join default group, got %v", groups)
	}
}

func TestOAuthCompleteUsernameCollision(t *testing.T) {
	gh := newGitHubServer(t, []string{})
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
	if !contains(loginURL, "client_id=cid") || !contains(loginURL, "state=state123") || !contains(loginURL, "redirect_uri=https") {
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

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
