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
	if out["access_token"] != nil || out["refresh_token"] != nil {
		t.Fatal("tokens must not be in the login body (httpOnly cookies only)")
	}
	if out["username"] != "admin" || out["role"] != "admin" {
		t.Fatalf("user = %v", out)
	}
	cookies := responseSetCookies(resp)
	for _, name := range []string{"dagger_kubernetes_access", "dagger_kubernetes_refresh"} {
		setCookie, ok := cookies[name]
		if !ok {
			t.Fatalf("missing %s cookie in %v", name, cookies)
		}
		for _, attr := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
			if !cookieHeaderContains(setCookie, attr) {
				t.Fatalf("%s = %q, want %s", name, setCookie, attr)
			}
		}
		if cookieHeaderContains(setCookie, "Secure") {
			t.Fatalf("%s = %q, want no Secure flag on plain http", name, setCookie)
		}
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

	// Login to get the refresh cookie.
	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	cookies := responseSetCookies(resp)
	refresh := cookieValueFromSetCookie(cookies["dagger_kubernetes_refresh"], "dagger_kubernetes_refresh")
	if refresh == "" {
		t.Fatal("missing refresh cookie")
	}

	// Refresh with the cookie (no body).
	resp = ut.PerformRequest(e, "POST", "/api/v1/auth/refresh", nil,
		ut.Header{Key: "Cookie", Value: fmt.Sprintf("dagger_kubernetes_refresh=%s", refresh)})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("refresh: %d", resp.Result().StatusCode())
	}
	if len(resp.Result().Body()) != 0 {
		t.Fatalf("refresh body must be empty, got %q", resp.Result().Body())
	}
	rotated := responseSetCookies(resp)
	if _, ok := rotated["dagger_kubernetes_access"]; !ok {
		t.Fatal("refresh must set a new access cookie")
	}
	if _, ok := rotated["dagger_kubernetes_refresh"]; !ok {
		t.Fatal("refresh must rotate the refresh cookie")
	}
}

func TestHandleRefreshFromBody(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	cookies := responseSetCookies(resp)
	refresh := cookieValueFromSetCookie(cookies["dagger_kubernetes_refresh"], "dagger_kubernetes_refresh")

	// Backwards-compat: refresh token supplied in the JSON body still works.
	body = fmt.Sprintf(`{"refresh_token":%q}`, refresh)
	resp = ut.PerformRequest(e, "POST", "/api/v1/auth/refresh", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("refresh from body: %d", resp.Result().StatusCode())
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
func (fakeOAuthProvider) Complete(ctx context.Context, code string) (accessToken, refreshToken string, user *domain.User, err error) {
	return "a", "r", &domain.User{ID: "u1", Username: "alice"}, nil
}

// forbiddenOAuthProvider is an OAuthProvider stub whose Complete fails with
// ErrForbidden (allowlist denial).
type forbiddenOAuthProvider struct{}

func (forbiddenOAuthProvider) LoginURL(state string) string {
	return fmt.Sprintf("https://provider/auth?state=%s", state)
}
func (forbiddenOAuthProvider) Complete(ctx context.Context, code string) (accessToken, refreshToken string, user *domain.User, err error) {
	return "", "", nil, domain.ErrForbidden
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
	if loc := string(resp.Result().Header.Peek("Location")); !strings.HasPrefix(loc, "/auth/callback?redirect=") {
		t.Fatalf("Location = %q, want query redirect (no fragment)", loc)
	}
	loc := string(resp.Result().Header.Peek("Location"))
	if strings.Contains(loc, "access_token") || strings.Contains(loc, "refresh_token") {
		t.Fatalf("Location = %q must not carry tokens", loc)
	}
	cookies := responseSetCookies(resp)
	if _, ok := cookies["dagger_kubernetes_access"]; !ok {
		t.Fatal("oauth callback must set the access cookie")
	}
	if _, ok := cookies["dagger_kubernetes_refresh"]; !ok {
		t.Fatal("oauth callback must set the refresh cookie")
	}
}

func TestOAuthCallbackForbidden(t *testing.T) {
	env := newTestEnv(t)
	env.server.oauth = forbiddenOAuthProvider{}
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
	if loc := string(resp.Result().Header.Peek("Location")); loc != "/auth/login?error=group_required" {
		t.Fatalf("Location = %q, want group_required error redirect", loc)
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

// TestHandleLoginCookieSecure verifies the Secure flag is forced on both auth
// cookies when auth.cookie.secure is true (TLS-terminating proxy).
func TestHandleLoginCookieSecure(t *testing.T) {
	env := newTestEnv(t)
	env.server.cookieCfg.Secure = true
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("login: %d", resp.Result().StatusCode())
	}
	cookies := responseSetCookies(resp)
	for _, name := range []string{"dagger_kubernetes_access", "dagger_kubernetes_refresh"} {
		setCookie, ok := cookies[name]
		if !ok {
			t.Fatalf("missing %s cookie", name)
		}
		if !cookieHeaderContains(setCookie, "Secure") {
			t.Fatalf("%s = %q, want Secure flag when cookieCfg.Secure=true", name, setCookie)
		}
	}
}

// TestHandleLoginCookieSecureHTTPS verifies the Secure flag is auto-set when
// the request arrives over https (requestIsTLS).
func TestHandleLoginCookieSecureHTTPS(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "https://localhost/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("login: %d", resp.Result().StatusCode())
	}
	cookies := responseSetCookies(resp)
	for _, name := range []string{"dagger_kubernetes_access", "dagger_kubernetes_refresh"} {
		setCookie, ok := cookies[name]
		if !ok {
			t.Fatalf("missing %s cookie", name)
		}
		if !cookieHeaderContains(setCookie, "Secure") {
			t.Fatalf("%s = %q, want Secure flag on an https request", name, setCookie)
		}
	}
}

// TestHandleLogoutClearsCookies verifies logout expires both auth cookies.
func TestHandleLogoutClearsCookies(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/logout", nil)
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.Result().StatusCode())
	}
	cookies := responseSetCookies(resp)
	for _, name := range []string{"dagger_kubernetes_access", "dagger_kubernetes_refresh"} {
		setCookie, ok := cookies[name]
		if !ok {
			t.Fatalf("missing cleared %s cookie in %v", name, cookies)
		}
		for _, attr := range []string{"max-age=0", "HttpOnly", "SameSite=Lax", "Path=/"} {
			if !cookieHeaderContains(setCookie, attr) {
				t.Fatalf("%s = %q, want %s", name, setCookie, attr)
			}
		}
	}
}

// TestResolveIdentityCookieFallback verifies the access cookie authenticates
// when no Authorization header is present (header stays primary for CI).
func TestResolveIdentityCookieFallback(t *testing.T) {
	env := newTestEnv(t)
	e := newAuthEngine(env.server)

	// Login to obtain the access cookie.
	body := `{"username":"admin","password":"password123"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	cookies := responseSetCookies(resp)
	access := cookieValueFromSetCookie(cookies["dagger_kubernetes_access"], "dagger_kubernetes_access")
	if access == "" {
		t.Fatal("missing access cookie")
	}

	resp = ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil,
		ut.Header{Key: "Cookie", Value: fmt.Sprintf("dagger_kubernetes_access=%s", access)})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("me with cookie: %d, want 200", resp.Result().StatusCode())
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["username"] != "admin" {
		t.Fatalf("me = %v", out)
	}
}

// TestCORSAllowDeny verifies the CORS middleware echoes an allowed origin (with
// credentials) and stays silent for disallowed/no origins.
func TestCORSAllowDeny(t *testing.T) {
	env := newTestEnv(t)
	env.server.corsAllowedOrigins = []string{"https://ui.example.com"}
	e := newAuthEngine(env.server)

	// Allowed origin -> ACAO echoed + credentials + Vary.
	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil,
		ut.Header{Key: "Origin", Value: "https://ui.example.com"})
	if got := resp.Result().Header.Get("Access-Control-Allow-Origin"); got != "https://ui.example.com" {
		t.Fatalf("ACAO = %q, want echoed origin", got)
	}
	if got := resp.Result().Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC = %q, want true", got)
	}
	if got := resp.Result().Header.Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}

	// Disallowed origin -> no CORS headers, never "*".
	resp = ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil,
		ut.Header{Key: "Origin", Value: "https://evil.example.com"})
	if got := resp.Result().Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want empty for disallowed origin", got)
	}
	// Vary: Origin must still be set so a shared cache cannot reuse this
	// response for a different (allowed) origin (CWE-349).
	if got := resp.Result().Header.Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin even for disallowed origin", got)
	}

	// No origin -> same-origin, no CORS headers.
	resp = ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
	if got := resp.Result().Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO = %q, want empty when no origin", got)
	}
}

// TestCORSPreflight verifies OPTIONS preflight for an allowed origin is
// answered with 204 + allow methods/headers.
func TestCORSPreflight(t *testing.T) {
	env := newTestEnv(t)
	env.server.corsAllowedOrigins = []string{"https://ui.example.com"}
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "OPTIONS", "/api/v1/auth/me", nil,
		ut.Header{Key: "Origin", Value: "https://ui.example.com"})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("preflight: %d, want 204", resp.Result().StatusCode())
	}
	if got := resp.Result().Header.Get("Access-Control-Allow-Methods"); got != "GET,POST,PUT,DELETE,OPTIONS" {
		t.Fatalf("ACAM = %q", got)
	}
	if got := resp.Result().Header.Get("Access-Control-Allow-Headers"); got != "Authorization, Content-Type" {
		t.Fatalf("ACAH = %q", got)
	}
}

// responseSetCookies returns the response's Set-Cookie values keyed by cookie
// name (via VisitAllCookie, which iterates cookies set with SetCookie).
func responseSetCookies(resp *ut.ResponseRecorder) map[string]string {
	out := map[string]string{}
	resp.Result().Header.VisitAllCookie(func(key, value []byte) {
		out[string(key)] = string(value)
	})
	return out
}

// cookieValueFromSetCookie extracts name=value from a Set-Cookie header value.
func cookieValueFromSetCookie(setCookie, name string) string {
	prefix := name + "="
	for _, part := range strings.Split(setCookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, prefix) {
			return strings.TrimPrefix(part, prefix)
		}
	}
	return ""
}

// cookieHeaderContains reports whether the Set-Cookie response header contains
// token case-insensitively. hertz serializes the HttpOnly/SameSite attributes
// capitalized but the secure/path attributes lowercase, so a case-insensitive
// match keeps the assertions independent of that serialization detail.
func cookieHeaderContains(setCookie, token string) bool {
	return strings.Contains(strings.ToLower(setCookie), strings.ToLower(token))
}
