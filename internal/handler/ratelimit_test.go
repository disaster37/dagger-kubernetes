package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
	"github.com/disaster37/go-secguard"
	sgconfig "github.com/disaster37/go-secguard/config"
)

// newSecguardEngine builds a route engine with the security guard middleware
// and the routes exercised by the guardrail tests.
func newSecguardEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.Use(s.secguardMiddleware())
	e.POST("/api/v1/auth/login", s.handleLogin)
	e.GET("/api/v1/auth/providers", s.handleProviders)
	e.GET("/api/v1/auth/me", s.handleMe)
	e.PUT("/api/v1/auth/password", s.handleChangePassword)
	e.GET("/api/v1/security/bans", s.adminOnly(s.handleSecurityBansList))
	e.DELETE("/api/v1/security/bans/:ip", s.adminOnly(s.handleSecurityBanDelete))
	return e
}

// newClockGuard builds a guard whose clock is controlled by the returned
// pointer.
func newClockGuard(t *testing.T, cfg sgconfig.Config) (*secguard.Guard, *time.Time) { //nolint:gocritic // test helper
	t.Helper()
	now := time.Now()
	guard, err := secguard.New(cfg, secguard.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("secguard.New: %v", err)
	}
	return guard, &now
}

func postLogin(e *route.Engine, username, password string, headers ...ut.Header) *ut.ResponseRecorder {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	hs := append([]ut.Header{{Key: "Content-Type", Value: "application/json"}}, headers...)
	return ut.PerformRequest(e, "POST", "/api/v1/auth/login", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, hs...)
}

// TestLoginRateLimited verifies the login endpoint locks out a username+IP
// after repeated failures (CWE-307).
func TestLoginRateLimited(t *testing.T) {
	env := newTestEnv(t)
	e := newSecguardEngine(env.server)

	for i := 0; i < 5; i++ {
		resp := postLogin(e, "admin", "wrong")
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}

	resp := postLogin(e, "admin", "password123")
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("locked login: %d, want 429", resp.Result().StatusCode())
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["message"] == nil {
		t.Fatal("missing message")
	}
}

// TestLoginRateLimitCaseInsensitive verifies username casing cannot bypass a
// lockout (usernames are COLLATE NOCASE).
func TestLoginRateLimitCaseInsensitive(t *testing.T) {
	env := newTestEnv(t)
	e := newSecguardEngine(env.server)

	for i := 0; i < 5; i++ {
		resp := postLogin(e, "ADMIN", "wrong")
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}

	resp := postLogin(e, "admin", "password123")
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("case-variant lockout: %d, want 429", resp.Result().StatusCode())
	}
}

// TestLoginSuccessClearsFailures verifies a successful login resets the
// failure counter for the key.
func TestLoginSuccessClearsFailures(t *testing.T) {
	env := newTestEnv(t)
	e := newSecguardEngine(env.server)

	for i := 0; i < 4; i++ {
		postLogin(e, "admin", "wrong")
	}
	resp := postLogin(e, "admin", "password123")
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("success login: %d, want 200", resp.Result().StatusCode())
	}

	for i := 0; i < 4; i++ {
		resp = postLogin(e, "admin", "wrong")
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("post-reset attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}
}

// TestChangePasswordRateLimited verifies the change-password endpoint (which
// also verifies a password) is rate-limited.
func TestChangePasswordRateLimited(t *testing.T) {
	env := newTestEnv(t)
	e := newSecguardEngine(env.server)
	bearer := env.loginAsAdmin(t)

	body := `{"current_password":"wrong","new_password":"newpassword123"}`
	for i := 0; i < 5; i++ {
		resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Authorization", Value: bearer},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}
	resp := ut.PerformRequest(e, "PUT", "/api/v1/auth/password", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("locked change-password: %d, want 429", resp.Result().StatusCode())
	}
}

// TestCredentialStuffingBansIPAndExpires verifies that many distinct failed
// credentials from one IP trigger a ban (403) that expires with the clock.
func TestCredentialStuffingBansIPAndExpires(t *testing.T) {
	guard, now := newClockGuard(t, sgconfig.DefaultConfig())
	env := newTestEnvWithGuard(t, guard)
	e := newSecguardEngine(env.server)

	for i := 0; i < 10; i++ {
		resp := postLogin(e, fmt.Sprintf("user-%d", i), "wrong")
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}

	// The IP is now banned: even a valid login is rejected with 403.
	resp := postLogin(e, "admin", "password123")
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("banned login: %d, want 403", resp.Result().StatusCode())
	}

	// Advance past the ban duration: the IP is allowed again.
	*now = now.Add(sgconfig.DefaultConfig().Ban.BanDuration + time.Second)
	resp = postLogin(e, "admin", "password123")
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("post-expiry login: %d, want 200", resp.Result().StatusCode())
	}
}

// TestRepeatedSameCredentialLocksAccountWithoutBanningIP verifies that a single
// misconfigured client repeating the same credential is locked out but does not
// ban the shared IP.
func TestRepeatedSameCredentialLocksAccountWithoutBanningIP(t *testing.T) {
	guard, _ := newClockGuard(t, sgconfig.DefaultConfig())
	env := newTestEnvWithGuard(t, guard)
	e := newSecguardEngine(env.server)

	for i := 0; i < 5; i++ {
		postLogin(e, "admin", "wrong")
	}
	// Locked out (429), not banned (403).
	resp := postLogin(e, "admin", "password123")
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("locked login: %d, want 429", resp.Result().StatusCode())
	}
	// A different account from the same IP is not banned.
	resp = postLogin(e, "someone-else", "wrong")
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("other account: %d, want 401 (not banned)", resp.Result().StatusCode())
	}
}

// TestBearerTokenEnumerationBansIP verifies that enumerating invalid bearer
// tokens bans the source IP.
func TestBearerTokenEnumerationBansIP(t *testing.T) {
	guard, _ := newClockGuard(t, sgconfig.DefaultConfig())
	env := newTestEnvWithGuard(t, guard)
	e := newSecguardEngine(env.server)

	for i := 0; i < 10; i++ {
		resp := ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil,
			ut.Header{Key: "Authorization", Value: fmt.Sprintf("Bearer invalid-token-%d", i)})
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, resp.Result().StatusCode())
		}
	}
	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/me", nil,
		ut.Header{Key: "Authorization", Value: "Bearer another-invalid"})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("banned token enumeration: %d, want 403", resp.Result().StatusCode())
	}
}

// TestAuthEndpointDoSRateLimit verifies the per-IP auth rate limit.
func TestAuthEndpointDoSRateLimit(t *testing.T) {
	cfg := sgconfig.DefaultConfig()
	cfg.RateLimit.MaxRequests = 3
	guard, _ := newClockGuard(t, cfg)
	env := newTestEnvWithGuard(t, guard)
	e := newSecguardEngine(env.server)

	for i := 0; i < 3; i++ {
		resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
		if resp.Result().StatusCode() != http.StatusOK {
			t.Fatalf("request %d: %d, want 200", i, resp.Result().StatusCode())
		}
	}
	resp := ut.PerformRequest(e, "GET", "/api/v1/auth/providers", nil)
	if resp.Result().StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("rate limited: %d, want 429", resp.Result().StatusCode())
	}
}

// TestMutationOriginCSRFCheck verifies cross-site mutating requests are
// rejected when a CORS allowlist is configured.
func TestMutationOriginCSRFCheck(t *testing.T) {
	guard, _ := newClockGuard(t, sgconfig.DefaultConfig())
	env := newTestEnvFull(t, guard, []string{"https://ui.example.com"})
	e := newSecguardEngine(env.server)

	resp := postLogin(e, "admin", "password123", ut.Header{Key: "Origin", Value: "https://evil.example.com"})
	if resp.Result().StatusCode() != http.StatusForbidden {
		t.Fatalf("cross-site mutation: %d, want 403", resp.Result().StatusCode())
	}

	resp = postLogin(e, "admin", "password123", ut.Header{Key: "Origin", Value: "https://ui.example.com"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("allowed-origin mutation: %d, want 200", resp.Result().StatusCode())
	}

	// Same-origin (public_url) is always allowed.
	resp = postLogin(e, "admin", "password123", ut.Header{Key: "Origin", Value: "https://supv.example.com"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("same-origin mutation: %d, want 200", resp.Result().StatusCode())
	}
}

// TestAdminSecurityBansListAndUnban verifies the admin ban inspection and
// unban endpoints.
func TestAdminSecurityBansListAndUnban(t *testing.T) {
	guard, _ := newClockGuard(t, sgconfig.DefaultConfig())
	env := newTestEnvWithGuard(t, guard)
	e := newSecguardEngine(env.server)
	bearer := env.loginAsAdmin(t)

	for i := 0; i < 10; i++ {
		postLogin(e, fmt.Sprintf("user-%d", i), "wrong")
	}

	resp := ut.PerformRequest(e, "GET", "/api/v1/security/bans", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("list bans: %d, want 200", resp.Result().StatusCode())
	}
	var bans []map[string]any
	if err := json.Unmarshal(resp.Result().Body(), &bans); err != nil {
		t.Fatalf("decode bans: %v", err)
	}
	if len(bans) == 0 {
		t.Fatal("expected at least one ban")
	}
	ip, _ := bans[0]["ip"].(string)
	if ip == "" {
		t.Fatal("ban entry missing ip")
	}

	resp = ut.PerformRequest(e, "DELETE", "/api/v1/security/bans/"+ip, nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusNoContent {
		t.Fatalf("unban: %d, want 204", resp.Result().StatusCode())
	}

	resp = ut.PerformRequest(e, "DELETE", "/api/v1/security/bans/not-an-ip", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("invalid ip unban: %d, want 400", resp.Result().StatusCode())
	}
}

// TestSecurityHeadersAPICacheControl verifies the go-secguard hardening
// headers add Cache-Control: no-store on API paths.
func TestSecurityHeadersAPICacheControl(t *testing.T) {
	env := newTestEnv(t)
	e := route.NewEngine(config.NewOptions(nil))
	e.Use(env.server.securityHeaders())
	e.GET("/api/v1/ping", func(_ context.Context, c *app.RequestContext) {
		c.SetStatusCode(http.StatusOK)
	})

	resp := ut.PerformRequest(e, "GET", "/api/v1/ping", nil)
	h := &resp.Result().Header
	if got := string(h.Peek("X-Frame-Options")); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
	if got := string(h.Peek("Cache-Control")); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}
