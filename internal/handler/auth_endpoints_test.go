package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestHandleLoginSuccess(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("login: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["access_token"] == nil || out["refresh_token"] == nil {
		t.Fatal("missing tokens")
	}
	user := out["user"].(map[string]any)
	if user["username"] != "admin" || user["role"] != "admin" {
		t.Fatalf("user = %v", user)
	}
}

func TestHandleLoginBadCredentials(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"wrong"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("bad login: %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleLoginBadBody(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader("not-json"), Len: 8},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("bad body: %d, want 400", resp.Result().StatusCode())
	}
}

func TestHandleRefreshRotation(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	// Login to get a refresh token.
	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var loginOut map[string]any
	json.Unmarshal(resp.Result().Body(), &loginOut)
	refresh := loginOut["refresh_token"].(string)

	// Refresh.
	body = `{"refresh_token":"` + refresh + `"}`
	resp = ut.PerformRequest(e, "POST", "/api/v1/auth/refresh", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("refresh: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["access_token"] == nil || out["refresh_token"] == nil {
		t.Fatal("missing rotated tokens")
	}
}

func TestHandleRefreshBadToken(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	body := `{"refresh_token":"not-a-jwt"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/refresh", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("bad refresh: %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleMe(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil, ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("me: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["username"] != "admin" || out["role"] != "admin" {
		t.Fatalf("me = %v", out)
	}
}

func TestHandleMeUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("me unauth: %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleProviders(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("providers: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["internal"] != true {
		t.Fatal("internal should be true")
	}
	if out["oauth_github"] != false {
		t.Fatal("oauth_github should be false (not configured)")
	}
	if out["oauth_oidc"] != false {
		t.Fatal("oauth_oidc should be false (not configured)")
	}
}

func TestHandleChangePassword(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"current_password":"password123","new_password":"newpassword123"}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("change pw: %d", resp.Result().StatusCode())
	}

	// Old password no longer works; new one does.
	body = `{"username":"admin","password":"password123"}`
	resp = ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("old pw should fail: %d", resp.Result().StatusCode())
	}
	body = `{"username":"admin","password":"newpassword123"}`
	resp = ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("new pw should work: %d", resp.Result().StatusCode())
	}
}

func TestHandleChangePasswordWrongCurrent(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	bearer := env.loginAsAdmin(t)
	body := `{"current_password":"wrong","new_password":"newpassword123"}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("wrong current: %d, want 401", resp.Result().StatusCode())
	}
}

// TestHandleLoginInternalDisabled verifies that username/password login 404s
// when internal auth is disabled (OAuth-only mode).
func TestHandleLoginInternalDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.server.internalAuthEnabled = false
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("internal disabled login: %d, want 404", resp.Result().StatusCode())
	}
}

// TestHandleProvidersInternalDisabled verifies providers.internal is false when
// internal auth is disabled.
func TestHandleProvidersInternalDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.server.internalAuthEnabled = false
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["internal"] != false {
		t.Fatalf("internal = %v, want false", out["internal"])
	}
}

// fakeOAuthProvider is an in-test OAuthProvider stub.
type fakeOAuthProvider struct{}

func (fakeOAuthProvider) LoginURL(state string) string {
	return fmt.Sprintf("https://provider/auth?state=%s", state)
}
func (fakeOAuthProvider) Complete(ctx context.Context, code string) (string, string, *domain.User, error) {
	return "a", "r", &domain.User{ID: "u1", Username: "alice"}, nil
}

// TestHandleProvidersOIDC verifies providers reports oauth_oidc for the oidc
// provider.
func TestHandleProvidersOIDC(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["oauth_oidc"] != true {
		t.Fatal("oauth_oidc should be true")
	}
	if out["oauth_github"] != false {
		t.Fatal("oauth_github should be false")
	}
}

// TestHandleProvidersGitHub verifies providers reports oauth_github for the
// github provider.
func TestHandleProvidersGitHub(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "github"
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["oauth_github"] != true {
		t.Fatal("oauth_github should be true")
	}
	if out["oauth_oidc"] != false {
		t.Fatal("oauth_oidc should be false")
	}
}

func TestHandleOAuthOIDCLoginNotEnabled(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/oidc/login", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("oidc login not enabled: %d, want 404", resp.Result().StatusCode())
	}
}

func TestHandleOAuthOIDCLoginWrongProvider(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "github"
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/oidc/login", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("oidc login wrong provider: %d, want 404", resp.Result().StatusCode())
	}
}

func TestHandleOAuthGitHubLoginWrongProvider(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/github/login", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("github login wrong provider: %d, want 404", resp.Result().StatusCode())
	}
}

func TestHandleOAuthLoginDisabled(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/github/login", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("oauth disabled: %d, want 404", resp.Result().StatusCode())
	}
}

func TestHandleOAuthCallbackDisabled(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/github/callback?code=x&state=y", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("oauth callback disabled: %d, want 404", resp.Result().StatusCode())
	}
}

// TestSafeRedirectPath verifies the open-redirect guard (CWE-601).
func TestSafeRedirectPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "/pipelines"},
		{"/pipelines", "/pipelines"},
		{"/admin/users?x=1", "/admin/users?x=1"},
		{"//evil.com", "/pipelines"},
		{"///evil.com", "/pipelines"},
		{"/\\evil.com", "/pipelines"},
		{"https://evil.com", "/pipelines"},
		{"evil.com", "/pipelines"},
	}
	for _, tc := range cases {
		if got := safeRedirectPath(tc.in); got != tc.want {
			t.Errorf("safeRedirectPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// oauthCallbackPath builds the OIDC callback URL for a given state.
func oauthCallbackPath(state string) string {
	return fmt.Sprintf("/api/v1/auth/oauth/oidc/callback?code=x&state=%s", state)
}

func TestOAuthCallbackMissingNonceCookie(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	state, err := env.server.jwt.IssueOAuthState("/pipelines", "n1")
	if err != nil {
		t.Fatalf("IssueOAuthState: %v", err)
	}
	resp := ut.PerformRequest(e, "GET", oauthCallbackPath(state), nil)
	if resp.Result().StatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Result().StatusCode())
	}
	if loc := string(resp.Result().Header.Peek("Location")); loc != "/auth/login?error=oauth" {
		t.Fatalf("Location = %q, want oauth error redirect", loc)
	}
}

func TestOAuthCallbackNonceMismatch(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	state, err := env.server.jwt.IssueOAuthState("/pipelines", "n1")
	if err != nil {
		t.Fatalf("IssueOAuthState: %v", err)
	}
	resp := ut.PerformRequest(e, "GET", oauthCallbackPath(state), nil,
		ut.Header{Key: "Cookie", Value: "oauth_state=WRONG"})
	if resp.Result().StatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Result().StatusCode())
	}
	if loc := string(resp.Result().Header.Peek("Location")); loc != "/auth/login?error=oauth" {
		t.Fatalf("Location = %q, want oauth error redirect", loc)
	}
}

func TestOAuthCallbackSuccess(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	state, err := env.server.jwt.IssueOAuthState("/pipelines", "n1")
	if err != nil {
		t.Fatalf("IssueOAuthState: %v", err)
	}
	resp := ut.PerformRequest(e, "GET", oauthCallbackPath(state), nil,
		ut.Header{Key: "Cookie", Value: "oauth_state=n1"})
	if resp.Result().StatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Result().StatusCode())
	}
	if loc := string(resp.Result().Header.Peek("Location")); !strings.HasPrefix(loc, "/auth/callback#access_token=") {
		t.Fatalf("Location = %q, want fragment redirect with access token", loc)
	}
}

func TestOAuthLoginSetsNonceCookie(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/oidc/login", nil)
	if resp.Result().StatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Result().StatusCode())
	}
	if loc := string(resp.Result().Header.Peek("Location")); !strings.HasPrefix(loc, "https://provider/auth?state=") {
		t.Fatalf("Location = %q, want provider authorize URL", loc)
	}
	setCookie := resp.Result().Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "oauth_state=") {
		t.Fatalf("Set-Cookie = %q, want oauth_state nonce cookie", setCookie)
	}
	if !cookieHeaderContains(setCookie, "HttpOnly") {
		t.Fatalf("Set-Cookie = %q, want HttpOnly flag", setCookie)
	}
	if !cookieHeaderContains(setCookie, "SameSite=Lax") {
		t.Fatalf("Set-Cookie = %q, want SameSite=Lax", setCookie)
	}
	if !cookieHeaderContains(setCookie, "Path=/api/v1/auth/oauth") {
		t.Fatalf("Set-Cookie = %q, want Path=/api/v1/auth/oauth", setCookie)
	}
}

func TestOAuthLoginCookieSecure(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	env.server.oauthCookieSecure = true
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/oidc/login", nil)
	if resp.Result().StatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Result().StatusCode())
	}
	setCookie := resp.Result().Header.Get("Set-Cookie")
	if !cookieHeaderContains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, want Secure flag when oauthCookieSecure=true", setCookie)
	}
}

func TestOAuthLoginCookieNotSecure(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = fakeOAuthProvider{}
	env.server.oauthProvider = "oidc"
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/oidc/login", nil)
	if resp.Result().StatusCode() != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Result().StatusCode())
	}
	setCookie := resp.Result().Header.Get("Set-Cookie")
	if cookieHeaderContains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie = %q, want no Secure flag on a non-TLS request with oauthCookieSecure=false", setCookie)
	}
}

// cookieHeaderContains reports whether the Set-Cookie response header contains
// token case-insensitively. hertz serializes the HttpOnly/SameSite attributes
// capitalized but the secure/path attributes lowercase, so a case-insensitive
// match keeps the assertions independent of that serialization detail.
func cookieHeaderContains(setCookie, token string) bool {
	return strings.Contains(strings.ToLower(setCookie), strings.ToLower(token))
}
