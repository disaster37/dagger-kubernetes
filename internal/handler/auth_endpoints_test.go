package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

func TestHandleLoginSuccess(t *testing.T) {
	env := newTestEnv(t, false)
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
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	body := `{"username":"admin","password":"wrong"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("bad login: %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleLoginBadBody(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader("not-json"), Len: 8},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("bad body: %d, want 400", resp.Result().StatusCode())
	}
}

func TestHandleRefreshRotation(t *testing.T) {
	env := newTestEnv(t, false)
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
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	body := `{"refresh_token":"not-a-jwt"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/refresh", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("bad refresh: %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleMe(t *testing.T) {
	env := newTestEnv(t, false)
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
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("me unauth: %d, want 401", resp.Result().StatusCode())
	}
}

func TestHandleProviders(t *testing.T) {
	env := newTestEnv(t, false)
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
}

func TestHandleChangePassword(t *testing.T) {
	env := newTestEnv(t, false)
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
	env := newTestEnv(t, false)
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

// TestHandleLoginAuthDisabled verifies dev-mode (auth disabled) parity (D9):
// login accepts any credentials and returns the anonymous admin identity so the
// UI flow works without a users-table entry.
func TestHandleLoginAuthDisabled(t *testing.T) {
	env := newTestEnv(t, true)
	e := newAuthEngine(env.server)

	body := `{"username":"anything","password":"whatever"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("disabled login: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	user := out["user"].(map[string]any)
	if user["username"] != "anonymous" || user["role"] != "admin" {
		t.Fatalf("user = %v", user)
	}
}

// TestHandleMeAuthDisabled verifies /me answers the synthetic anonymous admin
// instead of failing with a 404 users-table lookup.
func TestHandleMeAuthDisabled(t *testing.T) {
	env := newTestEnv(t, true)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil)
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("disabled me: %d", resp.Result().StatusCode())
	}
	var out map[string]any
	json.Unmarshal(resp.Result().Body(), &out)
	if out["username"] != "anonymous" || out["role"] != "admin" {
		t.Fatalf("me = %v", out)
	}
}

// TestMyTokenCreateAuthDisabled verifies synthetic identities cannot create API
// tokens (there is no users-table row; the FK would fail with a 500).
func TestMyTokenCreateAuthDisabled(t *testing.T) {
	env := newTestEnv(t, true)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/api/v1/tokens/me", nil)
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("anonymous token create: %d, want 400", resp.Result().StatusCode())
	}
}

func TestHandleOAuthLoginDisabled(t *testing.T) {
	env := newTestEnv(t, false)
	e := newAuthEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/oauth/github/login", nil)
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("oauth disabled: %d, want 404", resp.Result().StatusCode())
	}
}

func TestHandleOAuthCallbackDisabled(t *testing.T) {
	env := newTestEnv(t, false)
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

// TestHandleRefreshAuthDisabled verifies dev-mode parity (D9): refresh with
// the placeholder tokens succeeds instead of failing JWT validation.
func TestHandleRefreshAuthDisabled(t *testing.T) {
	env := newTestEnv(t, true)
	e := newAuthEngine(env.server)

	body := `{"refresh_token":"anonymous"}`
	resp := ut.PerformRequest(e, "POST", "/api/v1/auth/refresh", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("disabled refresh: %d, want 200", resp.Result().StatusCode())
	}
}

// TestHandleChangePasswordAuthDisabled verifies synthetic identities get a
// 400 (not a 404 users-table lookup) in dev mode.
func TestHandleChangePasswordAuthDisabled(t *testing.T) {
	env := newTestEnv(t, true)
	e := newAuthEngine(env.server)

	body := `{"current_password":"x","new_password":"newpassword123"}`
	resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("disabled change password: %d, want 400", resp.Result().StatusCode())
	}
}
