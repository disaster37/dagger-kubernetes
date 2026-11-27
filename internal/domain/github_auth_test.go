package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGitHubDeterministicIDs(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"account normalizes base and owner", GitHubAccountSourceID("org-a", "https://API.GitHub.com/", "Acme"), GitHubAccountSourceID("org-a", "https://api.github.com", "acme")},
		{"account empty base defaults", GitHubAccountSourceID("org-a", "", "acme"), GitHubAccountSourceID("org-a", "https://api.github.com", "acme")},
		{"materialized normalizes case", MaterializedForgeSourceID("org-a", "Acme/API"), MaterializedForgeSourceID("org-a", "acme/api")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q want %q", tc.got, tc.want)
			}
		})
	}
	if got := GitHubAccountSourceID("org-a", "", "acme"); len(got) != len("ghacct-0123456789abcdef") {
		t.Fatalf("account id length = %d (%q)", len(got), got)
	}
	if got := MaterializedForgeSourceID("org-a", "acme/api"); len(got) != len("ghrepo-0123456789abcdef") {
		t.Fatalf("forge source id length = %d (%q)", len(got), got)
	}
	if GitHubAccountSourceID("org-a", "", "acme") == GitHubAccountSourceID("org-b", "", "acme") {
		t.Fatal("org must be part of the account id")
	}
}

func TestRepoSettingAllowsWebhookEvent(t *testing.T) {
	yes := &RepoSetting{
		RunChecksOnDefaultBranch: true,
		RunChecksOnPullRequest:   true,
		RunChecksOnBranchPush:    false,
	}
	no := &RepoSetting{}
	cases := []struct {
		name    string
		setting *RepoSetting
		event   *WebhookEvent
		want    bool
	}{
		{"nil setting allows", nil, &WebhookEvent{Event: "push"}, true},
		{"nil event allows", yes, nil, true},
		{"pull request on", yes, &WebhookEvent{Event: "pull_request"}, true},
		{"pull request off", no, &WebhookEvent{Event: "pull_request"}, false},
		{"default branch push on", yes, &WebhookEvent{Event: "push", Ref: "refs/heads/main", DefaultBranch: "main"}, true},
		{"default branch push off", no, &WebhookEvent{Event: "push", Ref: "refs/heads/main", DefaultBranch: "main"}, false},
		{"feature branch push off", yes, &WebhookEvent{Event: "push", Ref: "refs/heads/feature", DefaultBranch: "main"}, false},
		{"main fallback when default branch empty", yes, &WebhookEvent{Event: "push", Ref: "refs/heads/main"}, true},
		{"master fallback when default branch empty", yes, &WebhookEvent{Event: "push", Ref: "refs/heads/master"}, true},
		{"feature branch uses branch push", &RepoSetting{RunChecksOnBranchPush: true}, &WebhookEvent{Event: "push", Ref: "refs/heads/x", DefaultBranch: "main"}, true},
		{"release on", yes, &WebhookEvent{Event: "release"}, true},
		{"tag off", no, &WebhookEvent{Event: "tag"}, false},
		{"tag uses branch push", &RepoSetting{RunChecksOnBranchPush: true}, &WebhookEvent{Event: "tag"}, true},
		{"unknown event denied", yes, &WebhookEvent{Event: "issues"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.setting.AllowsWebhookEvent(tc.event); got != tc.want {
				t.Fatalf("AllowsWebhookEvent = %v want %v", got, tc.want)
			}
		})
	}
}

func TestGitHubTokenResultNormalize(t *testing.T) {
	cases := []struct {
		name      string
		in        GitHubTokenResult
		wantScope []string
		wantType  string
	}{
		{"empty", GitHubTokenResult{}, nil, "bearer"},
		{"single scope", GitHubTokenResult{Scope: "repo", TokenType: "bearer"}, []string{"repo"}, "bearer"},
		{"comma separated with spaces", GitHubTokenResult{Scope: "repo, read:org,admin:repo_hook"}, []string{"repo", "read:org", "admin:repo_hook"}, "bearer"},
		{"blank entries dropped", GitHubTokenResult{Scope: " , repo ,,"}, []string{"repo"}, "bearer"},
		{"explicit token type kept", GitHubTokenResult{Scope: "repo", TokenType: "mac"}, []string{"repo"}, "mac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Normalize()
			if len(tc.in.Scopes) != len(tc.wantScope) {
				t.Fatalf("Scopes = %v want %v", tc.in.Scopes, tc.wantScope)
			}
			for i := range tc.wantScope {
				if tc.in.Scopes[i] != tc.wantScope[i] {
					t.Fatalf("Scopes[%d] = %q want %q", i, tc.in.Scopes[i], tc.wantScope[i])
				}
			}
			if tc.in.TokenType != tc.wantType {
				t.Fatalf("TokenType = %q want %q", tc.in.TokenType, tc.wantType)
			}
		})
	}
}

func TestGitHubTokenResultNormalizeResetsScopes(t *testing.T) {
	res := GitHubTokenResult{Scope: "repo", Scopes: []string{"stale"}}
	res.Normalize()
	if len(res.Scopes) != 1 || res.Scopes[0] != "repo" {
		t.Fatalf("Scopes = %v", res.Scopes)
	}
}

func TestGitHubTokenResultDecodesUpstreamBody(t *testing.T) {
	body := []byte(`{"access_token":"gho_a","token_type":"bearer","scope":"repo, read:org","refresh_token":"ghr_r","expires_in":28800,"refresh_token_expires_in":15897600}`)
	var res GitHubTokenResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	res.Normalize()
	if res.RefreshToken != "ghr_r" || res.ExpiresIn != 28800 || res.RefreshTokenExpiresIn != 15897600 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Scopes) != 2 || res.Scopes[1] != "read:org" {
		t.Fatalf("Scopes = %v", res.Scopes)
	}
}

func TestGitHubRefreshSentinels(t *testing.T) {
	if errors.Is(ErrGitHubRefreshInvalid, ErrGitHubTokenRevoked) || errors.Is(ErrGitHubRefreshUnavailable, ErrGitHubTokenRevoked) {
		t.Fatal("refresh sentinels must stay distinct from revocation")
	}
	if ErrGitHubRefreshInvalid.Error() == "" || ErrGitHubRefreshUnavailable.Error() == "" {
		t.Fatal("refresh sentinels need messages")
	}
}

func TestGitHubConnectionRefreshFieldsAreNotSerialized(t *testing.T) {
	conn := UserGitHubConnection{UserID: "u1", TokenCiphertext: "access-ct", RefreshTokenCiphertext: "refresh-ct"}
	raw, err := json.Marshal(conn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "refresh-ct") || strings.Contains(string(raw), "ciphertext") {
		t.Fatalf("connection JSON leaked a ciphertext: %s", raw)
	}
	if !strings.Contains(string(raw), `"user_id":"u1"`) {
		t.Fatalf("connection JSON = %s", raw)
	}

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	acct := GitHubAccountSource{ID: "ghacct-1", TokenCiphertext: "a", RefreshTokenCiphertext: "r", TokenExpiresAt: now, RefreshExpiresAt: now.Add(time.Hour), TokenVersion: 3}
	raw, err = json.Marshal(acct)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"a"`) || strings.Contains(string(raw), `"r"`) {
		t.Fatalf("account JSON leaked a ciphertext: %s", raw)
	}
	for _, want := range []string{`"token_expires_at":"2026-11-27T12:00:00Z"`, `"refresh_expires_at":"2026-11-27T13:00:00Z"`, `"token_version":3`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("account JSON %s missing %s", raw, want)
		}
	}
}

func TestGitHubConnectionRefreshFieldsZeroWhenUnset(t *testing.T) {
	// time.Time is a struct, so encoding/json emits its zero value even with
	// omitempty. Legacy records therefore serialize as the zero timestamp
	// (meaning "non-expiring / unknown") rather than omitting the key.
	raw, err := json.Marshal(UserGitHubConnection{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"token_expires_at":"0001-01-01T00:00:00Z"`, `"refresh_expires_at":"0001-01-01T00:00:00Z"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("legacy connection JSON %s missing %s", raw, want)
		}
	}
	if strings.Contains(string(raw), "token_version") {
		t.Fatalf("zero token version must be omitted: %s", raw)
	}
}

func TestGitHubRateLimitError(t *testing.T) {
	reset := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	err := &GitHubRateLimitError{ResetAt: reset, RetryAfter: time.Second}
	if !errors.Is(err, ErrGitHubRateLimited) {
		t.Fatal("rate limit error must unwrap to ErrGitHubRateLimited")
	}
	if got := err.Error(); got != "github api rate limit exceeded (resets at 2026-01-02T03:04:05Z)" {
		t.Fatalf("Error = %q", got)
	}
	if got := (&GitHubRateLimitError{}).Error(); got != ErrGitHubRateLimited.Error() {
		t.Fatalf("Error without reset = %q", got)
	}
}

func TestOAuthStateUserMismatchError(t *testing.T) {
	err := &OAuthStateUserMismatchError{ExpectedUsername: "alice", CurrentUsername: "bob"}
	if !errors.Is(err, ErrForbidden) {
		t.Fatal("mismatch error must unwrap to ErrForbidden")
	}
	if got := err.Error(); got != `oauth state belongs to user "alice", current user is "bob"` {
		t.Fatalf("Error = %q", got)
	}
}

func TestGitHubOrgAccountScopeID(t *testing.T) {
	cases := []struct {
		name    string
		org     string
		account string
		want    string
		wantErr bool
	}{
		{"valid", "org-a", "ghacct-1", "org-a/ghacct-1", false},
		{"empty org", "", "ghacct-1", "", true},
		{"empty account", "org-a", "", "", true},
		{"slash org", "org/a", "ghacct-1", "", true},
		{"pipe account", "org-a", "ghacct|1", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GitHubOrgAccountScopeID(tc.org, tc.account)
			if tc.wantErr {
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("err = %v want ErrValidation", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err=%v want %q", got, err, tc.want)
			}
		})
	}
}

func TestCanonicalForgeRepo(t *testing.T) {
	cases := []struct {
		provider ForgeKind
		baseURL  string
		repo     string
		want     string
	}{
		{ForgeGitHub, "", "acme/api", "github.com/acme/api"},
		{ForgeGitHub, "https://api.github.com", "acme/api", "github.com/acme/api"},
		{ForgeGitHub, "https://ghes.example.com/api/v3", "acme/api", "ghes.example.com/acme/api"},
		{ForgeGitLab, "", "group/proj", "gitlab.com/group/proj"},
		{ForgeGitLab, "https://gitlab.example.com/api/v4", "group/proj", "gitlab.example.com/group/proj"},
		{ForgeGitHub, "", "github.com/acme/api.git", "github.com/acme/api"},
		{ForgeGitHub, "", "", ""},
	}
	for _, tc := range cases {
		if got := CanonicalForgeRepo(tc.provider, tc.baseURL, tc.repo); got != tc.want {
			t.Fatalf("CanonicalForgeRepo(%s, %q, %q) = %q want %q", tc.provider, tc.baseURL, tc.repo, got, tc.want)
		}
	}
}
