package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// forgeHookServer records the requests a provider makes against a fake forge
// API and serves a canned hook list.
type forgeHookServer struct {
	*httptest.Server
	hookListBody string
	requests     []string
}

func newForgeHookServer(t *testing.T, hookListBody string) *forgeHookServer {
	t.Helper()
	f := &forgeHookServer{hookListBody: hookListBody}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/hooks") {
			_, _ = io.WriteString(w, f.hookListBody)
			return
		}
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *forgeHookServer) deletePaths() []string {
	var out []string
	for _, req := range f.requests {
		if strings.HasPrefix(req, "DELETE ") {
			out = append(out, strings.TrimPrefix(req, "DELETE "))
		}
	}
	return out
}

// githubHookJSON builds a realistic GitHub hook entry: the top-level url is the
// hook's API URL, and the callback lives in config.url. A correct hookURL must
// ignore the API url, so this fixture fails on the old (top-level-first)
// precedence.
func githubHookJSON(id int64, callbackURL string) string {
	return fmt.Sprintf(`{"id":%d,"url":"https://api.github.com/repos/acme/repo/hooks/%d","config":{"url":%q}}`, id, id, callbackURL)
}

// giteaHookJSON builds a Gitea hook entry with only config.url (no top-level
// url), locking the config.url branch of hookURL.
func giteaHookJSON(id int64, callbackURL string) string {
	return fmt.Sprintf(`{"id":%d,"config":{"url":%q}}`, id, callbackURL)
}

// TestGitHubRegisterWebhookNarrowsEvents locks the Cloud Checks event policy:
// the GitHub hook must subscribe only to push/pull_request/release, not "*".
func TestGitHubRegisterWebhookNarrowsEvents(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	t.Cleanup(srv.Close)

	p := NewGitHubForge(srv.URL)
	src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/repo", BaseURL: srv.URL}
	id, err := p.RegisterWebhook(context.Background(), src, "token", "https://example.com/v1/webhooks/github/src-1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if id != "42" {
		t.Fatalf("hook id = %q, want 42", id)
	}
	var payload struct {
		Events []string `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	want := []string{"push", "pull_request", "release"}
	if len(payload.Events) != len(want) {
		t.Fatalf("events = %v, want %v", payload.Events, want)
	}
	for i := range want {
		if payload.Events[i] != want[i] {
			t.Fatalf("events = %v, want %v", payload.Events, want)
		}
	}
}

func TestGitHubForgeEscapesPathSegments(t *testing.T) {
	srv := newForgeHookServer(t, "[]")
	src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "own er/re po", BaseURL: srv.URL}
	p := NewGitHubForge(srv.URL)
	if _, err := p.RegisterWebhook(context.Background(), src, "token", "https://example.com/hook", "secret"); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) == 0 || srv.requests[0] != "POST /repos/own%20er/re%20po/hooks" {
		t.Fatalf("requests = %v", srv.requests)
	}
	src.Repo = "acme/../evil"
	if _, err := p.RegisterWebhook(context.Background(), src, "token", "https://example.com/hook", "secret"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("traversal repo err = %v", err)
	}
}

func TestForgeProvidersDeleteWebhookByURL(t *testing.T) {
	const repo = "acme/repo"
	cases := []struct {
		name         string
		kind         domain.ForgeKind
		hookListBody string
		wantDelete   []string
	}{
		{
			name:         "github match deletes via config.url",
			kind:         domain.ForgeGitHub,
			hookListBody: "[" + githubHookJSON(1, "https://example.com/v1/webhooks/github/src-1") + "," + githubHookJSON(2, "https://example.com/v1/webhooks/github/other") + "]",
			wantDelete:   []string{"/repos/acme/repo/hooks/1"},
		},
		{
			name:         "github no match is idempotent",
			kind:         domain.ForgeGitHub,
			hookListBody: "[" + githubHookJSON(2, "https://example.com/v1/webhooks/github/other") + "]",
			wantDelete:   nil,
		},
		{
			name:         "gitlab match deletes via top-level url",
			kind:         domain.ForgeGitLab,
			hookListBody: `[{"id":7,"url":"https://example.com/v1/webhooks/gitlab/src-1"}]`,
			wantDelete:   []string{"/projects/acme%2Frepo/hooks/7"},
		},
		{
			name:         "gitlab no match is idempotent",
			kind:         domain.ForgeGitLab,
			hookListBody: `[{"id":7,"url":"https://example.com/v1/webhooks/gitlab/other"}]`,
			wantDelete:   nil,
		},
		{
			name:         "gitea match deletes via config.url",
			kind:         domain.ForgeGitea,
			hookListBody: "[" + giteaHookJSON(3, "https://example.com/v1/webhooks/gitea/src-1") + "]",
			wantDelete:   []string{"/repos/acme/repo/hooks/3"},
		},
		{
			name:         "gitea no match is idempotent",
			kind:         domain.ForgeGitea,
			hookListBody: "[" + giteaHookJSON(3, "https://example.com/v1/webhooks/gitea/other") + "]",
			wantDelete:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newForgeHookServer(t, tc.hookListBody)
			src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: tc.kind, Repo: repo, BaseURL: srv.URL}
			var p domain.ForgeProvider
			switch tc.kind {
			case domain.ForgeGitHub:
				p = NewGitHubForge(srv.URL)
			case domain.ForgeGitLab:
				p = NewGitLabForge(srv.URL)
			case domain.ForgeGitea:
				p = NewGiteaForge(srv.URL)
			}
			if err := p.DeleteWebhook(context.Background(), src, "token", ""); err != nil {
				t.Fatalf("DeleteWebhook: %v", err)
			}
			got := srv.deletePaths()
			if len(got) != len(tc.wantDelete) {
				t.Fatalf("DELETE paths = %v, want %v", got, tc.wantDelete)
			}
			for i := range got {
				if got[i] != tc.wantDelete[i] {
					t.Fatalf("DELETE paths = %v, want %v", got, tc.wantDelete)
				}
			}
		})
	}
}

func TestForgeProvidersVerifyWebhookFailsClosedOnEmptySecret(t *testing.T) {
	cases := []struct {
		name string
		kind domain.ForgeKind
	}{
		{name: "github", kind: domain.ForgeGitHub},
		{name: "gitlab", kind: domain.ForgeGitLab},
		{name: "gitea", kind: domain.ForgeGitea},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: tc.kind, Repo: "acme/repo"}
			var p domain.ForgeProvider
			switch tc.kind {
			case domain.ForgeGitHub:
				p = NewGitHubForge("")
			case domain.ForgeGitLab:
				p = NewGitLabForge("")
			case domain.ForgeGitea:
				p = NewGiteaForge("")
			}
			headers := map[string]string{
				"X-Hub-Signature-256": "sha256=deadbeef",
				"X-Gitea-Signature":   "deadbeef",
				"X-Gitlab-Token":      "",
			}
			if p.VerifyWebhook(src, "", headers, []byte(`{}`)) {
				t.Fatal("VerifyWebhook must fail closed when the secret is empty")
			}
		})
	}
}

func TestForgeProvidersDeleteWebhookByID(t *testing.T) {
	cases := []struct {
		name       string
		kind       domain.ForgeKind
		wantDelete string
	}{
		{name: "github direct id", kind: domain.ForgeGitHub, wantDelete: "/repos/acme/repo/hooks/42"},
		{name: "gitlab direct id", kind: domain.ForgeGitLab, wantDelete: "/projects/acme%2Frepo/hooks/42"},
		{name: "gitea direct id", kind: domain.ForgeGitea, wantDelete: "/repos/acme/repo/hooks/42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newForgeHookServer(t, "[]")
			src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: tc.kind, Repo: "acme/repo", BaseURL: srv.URL}
			var p domain.ForgeProvider
			switch tc.kind {
			case domain.ForgeGitHub:
				p = NewGitHubForge(srv.URL)
			case domain.ForgeGitLab:
				p = NewGitLabForge(srv.URL)
			case domain.ForgeGitea:
				p = NewGiteaForge(srv.URL)
			}
			if err := p.DeleteWebhook(context.Background(), src, "token", "42"); err != nil {
				t.Fatalf("DeleteWebhook: %v", err)
			}
			got := srv.deletePaths()
			if len(got) != 1 || got[0] != tc.wantDelete {
				t.Fatalf("DELETE paths = %v, want [%s]", got, tc.wantDelete)
			}
			// Direct-ID path must not list hooks first.
			for _, req := range srv.requests {
				if strings.HasPrefix(req, "GET ") {
					t.Fatalf("direct-ID delete must not list hooks, got %v", srv.requests)
				}
			}
		})
	}
}
