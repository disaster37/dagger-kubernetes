package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newTestGitHubClient(t *testing.T, base string) *GitHubClient {
	t.Helper()
	client, err := NewGitHubClient(GitHubClientConfig{
		ClientID: "client-id", ClientSecret: "client-secret", OAuthBaseURL: base,
		DefaultAPIURL: base, HTTPTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGitHubAuthorizeURL(t *testing.T) {
	c := newTestGitHubClient(t, "https://github.com")
	raw := c.AuthorizeURL("client-id", "https://supv.example.com/github/callback", "state-1", "challenge-1", []string{"repo", "read:org"})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Fatalf("URL = %s", raw)
	}
	q := u.Query()
	if q.Get("state") != "state-1" || q.Get("code_challenge") != "challenge-1" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("query = %v", q)
	}
	if q.Get("redirect_uri") != "https://supv.example.com/github/callback" || q.Get("scope") != "repo read:org" {
		t.Fatalf("query = %v", q)
	}
}

func TestGitHubExchangeCodePKCE(t *testing.T) {
	var got url.Values
	var gotClient string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/oauth/access_token" {
			http.NotFound(w, r)
			return
		}
		gotClient, _, _ = r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"gho_token","token_type":"bearer","scope":"repo, read:org"}`)
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	res, err := c.ExchangeCode(context.Background(), "code-1", "https://supv.example.com/github/callback", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessToken != "gho_token" || res.TokenType != "bearer" || len(res.Scopes) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got.Get("code") != "code-1" || got.Get("code_verifier") != "verifier-1" || gotClient != "client-id" {
		t.Fatalf("form = %v", got)
	}
}

func TestGitHubDeviceFlow(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = io.WriteString(w, `{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
		case "/login/oauth/access_token":
			n := atomic.AddInt32(&polls, 1)
			switch n {
			case 1:
				_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
			case 2:
				_, _ = io.WriteString(w, `{"error":"slow_down"}`)
			default:
				_, _ = io.WriteString(w, `{"access_token":"gho_dev","token_type":"bearer","scope":"repo"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	prompt, err := c.StartDeviceFlow(context.Background(), []string{"repo", "read:org"})
	if err != nil {
		t.Fatal(err)
	}
	if prompt.DeviceCode != "dev-1" || prompt.UserCode != "ABCD-EFGH" || prompt.Interval != 5 || prompt.ExpiresIn <= 0 {
		t.Fatalf("prompt = %+v", prompt)
	}
	if _, err := c.PollDeviceFlow(context.Background(), "dev-1"); !errors.Is(err, domain.ErrGitHubDeviceAuthorizationDelay) {
		t.Fatalf("pending err = %v", err)
	}
	if _, err := c.PollDeviceFlow(context.Background(), "dev-1"); !errors.Is(err, domain.ErrGitHubDeviceSlowDown) {
		t.Fatalf("slow_down err = %v", err)
	}
	res, err := c.PollDeviceFlow(context.Background(), "dev-1")
	if err != nil || res.AccessToken != "gho_dev" || len(res.Scopes) != 1 {
		t.Fatalf("token = %+v err=%v", res, err)
	}
}

func TestGitHubGetAuthenticatedUserAndOrgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			_, _ = io.WriteString(w, `{"id":42,"login":"alice","avatar_url":"https://example.com/a.png","type":"User"}`)
		case "/user/orgs":
			_, _ = io.WriteString(w, `[{"login":"acme","avatar_url":"https://example.com/o.png"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	profile, scopes, err := c.GetAuthenticatedUser(context.Background(), srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Login != "alice" || profile.ID != 42 || len(scopes) != 2 || scopes[1] != "read:org" {
		t.Fatalf("profile = %+v scopes=%v", profile, scopes)
	}
	orgs, err := c.ListUserOrgs(context.Background(), srv.URL, "tok")
	if err != nil || len(orgs) != 1 || orgs[0].Login != "acme" || orgs[0].Type != "Organization" {
		t.Fatalf("orgs = %+v err=%v", orgs, err)
	}
	if _, _, err := c.GetAuthenticatedUser(context.Background(), srv.URL, "bad"); !errors.Is(err, domain.ErrGitHubTokenRevoked) {
		t.Fatalf("401 err = %v", err)
	}
}

func TestGitHubListAccessibleReposPagination(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		count := 100
		if page == "2" {
			count = 1
		}
		rows := make([]string, 0, count)
		for i := 0; i < count; i++ {
			rows = append(rows, fmt.Sprintf(`{"id":%d,"name":"r%d","full_name":"acme/r%d","private":true,"owner":{"login":"acme"},"permissions":{"admin":true}}`, i, i, i))
		}
		_, _ = io.WriteString(w, fmt.Sprintf("[%s]", strings.Join(rows, ",")))
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	repos, err := c.ListAccessibleRepos(context.Background(), srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 101 || len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("repos=%d pages=%v", len(repos), pages)
	}
	if repos[0].Owner != "acme" || !repos[0].AdminPerm || !repos[0].Private {
		t.Fatalf("repo = %+v", repos[0])
	}
}

func TestGitHubGetRepositoryPathEscaping(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":1,"name":"repo","full_name":"own er/repo","owner":{"login":"own er"},"default_branch":"main"}`)
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	repo, err := c.GetRepository(context.Background(), srv.URL, "tok", "own er", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/own%20er/repo" || repo.FullName != "own er/repo" {
		t.Fatalf("path = %q repo=%+v", gotPath, repo)
	}
	for _, bad := range []string{".", "..", "a/b", "a\\b", ""} {
		if _, err := c.GetRepository(context.Background(), srv.URL, "tok", bad, "repo"); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("owner %q err = %v", bad, err)
		}
	}
}

func TestGitHubRetriesTransientGet(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":1,"login":"alice"}`)
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	if _, _, err := c.GetAuthenticatedUser(context.Background(), srv.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestGitHubRateLimitAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rate":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1767225600")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for ghp_leakedtoken"}`)
		case "/badjson":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{not-json`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	_, err := c.getJSON(context.Background(), srv.URL, "tok", "/rate", nil)
	var rate *domain.GitHubRateLimitError
	if !errors.As(err, &rate) || rate.ResetAt.IsZero() || !errors.Is(err, domain.ErrGitHubRateLimited) {
		t.Fatalf("rate err = %v", err)
	}
	if strings.Contains(err.Error(), "leakedtoken") {
		t.Fatalf("error leaked a token: %v", err)
	}
	var out map[string]any
	if _, err := c.getJSON(context.Background(), srv.URL, "tok", "/badjson", &out); err == nil || !strings.Contains(err.Error(), "decode github response") {
		t.Fatalf("bad json err = %v", err)
	}
	if _, _, err := c.GetAuthenticatedUser(context.Background(), srv.URL, "tok"); err == nil {
		t.Fatal("404 must be an error")
	}
}

func TestGitHubOversizedResponseRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("a", maxGitHubResponse+10) + `"}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	if _, _, err := c.GetAuthenticatedUser(context.Background(), srv.URL, "tok"); err == nil {
		t.Fatal("oversized response must fail")
	}
}

func TestGitHubRefusesCrossHostRedirect(t *testing.T) {
	var leaked int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&leaked, 1)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/steal", other.URL), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := newTestGitHubClient(t, srv.URL)
	if _, _, err := c.GetAuthenticatedUser(context.Background(), srv.URL, "secret-token"); err == nil {
		t.Fatal("cross-host redirect must not be followed")
	}
	if leaked != 0 {
		t.Fatalf("redirect target received %d requests", leaked)
	}
}

func TestGitHubRepositoriesDeletedInSnapshotPreserveCiphertext(t *testing.T) {
	// Guard the snapshot-safe payload conversion used by github_connection_repo.
	conn := cmdGitHubConnectionFrom(&domain.UserGitHubConnection{UserID: "u", TokenCiphertext: "ct"})
	raw, err := json.Marshal(conn)
	if err != nil || !strings.Contains(string(raw), `"token_ciphertext":"ct"`) {
		t.Fatalf("connection payload = %s err=%v", raw, err)
	}
}
