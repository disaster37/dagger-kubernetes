package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// reloginFixture is a booted supervisor pointed at a loopback OIDC issuer with
// an injected atomic revalidation clock (so cache expiry is advanced without
// wall-clock sleeps).
type reloginFixture struct {
	issuer  *oidcIssuer
	baseURL string
	clock   *atomic.Int64 // unix nanos
	users   *service.UserService
}

// newReloginFixture builds the full OIDC wiring (server + issuer + revalidator)
// with a fake clock. It mirrors the production dependency graph.
func newReloginFixture(t *testing.T, allowedGroups []string) *reloginFixture {
	t.Helper()
	const clientID = "relogin-client"
	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr, dataAddr := listenerAddr(controlLn), listenerAddr(dataLn)
	issuer := newOIDCIssuer(t, clientID, []any{"devs"})

	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("integration-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)

	oauthCfg := &domain.OAuthConfig{
		Enabled:       true,
		Provider:      "oidc",
		ClientID:      clientID,
		ClientSecret:  "csec",
		RedirectURL:   fmt.Sprintf("http://localhost%s/api/v1/auth/oauth/oidc/callback", controlAddr),
		IssuerURL:     issuer.srv.URL,
		Scopes:        []string{"openid", "profile", "email"},
		UsernameClaim: "preferred_username",
		GroupsClaim:   "groups",
		AllowedGroups: allowedGroups,
	}
	mapper, err := service.NewGroupMapper([]domain.GroupMappingRule{{Pattern: "^devs$", Replacement: "devs"}})
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	var leader atomic.Bool
	leader.Store(true)
	oauthSvc := service.NewOIDCOAuthService(oauthCfg, mapper, usersSvc, groupRepo, jwtSvc, logger, nil, []byte("0123456789abcdef0123456789abcdef"), leader.Load)

	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC).UnixNano())
	revalidator := service.NewOAuthRevalidator(oauthSvc, mapper, nil, 0, usersSvc, groupRepo, logger, service.OAuthRevalidatorConfig{
		Interval: time.Minute,
		Grace:    time.Hour,
		Clock:    func() time.Time { return time.Unix(0, clock.Load()).UTC() },
	})
	authSvc.SetOAuthRevalidator(revalidator)

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger), groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        dataAddr,
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient,
		JWT: jwtSvc, OAuth: oauthSvc, OAuthProvider: "oidc",
		CookieCfg: domain.CookieConfig{
			AccessName:  "dagger_kubernetes_access",
			RefreshName: "dagger_kubernetes_refresh",
		},
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	baseURL := fmt.Sprintf("http://localhost%s", controlAddr)
	waitReady(t, baseURL)

	return &reloginFixture{
		issuer: issuer, baseURL: baseURL, clock: clock,
		users: usersSvc,
	}
}

// waitReady polls /healthz until the control plane answers (bounded by a
// deadline), so tests never sleep for a fixed duration after Start.
func waitReady(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("control plane did not become ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// noRedirectClient returns a client that does not follow redirects.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// oidcLogin performs a full OIDC login against the fixture and returns the
// access-cookie value (the httpOnly session JWT).
func (f *reloginFixture) oidcLogin(t *testing.T) string {
	t.Helper()
	noRedirect := noRedirectClient()
	loginResp, err := noRedirect.Get(f.baseURL + "/api/v1/auth/oauth/oidc/login?redirect=/pipelines")
	if err != nil {
		t.Fatalf("GET oidc login: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", loginResp.StatusCode)
	}
	nonce := responseCookie(loginResp, "oauth_state")
	if nonce == "" {
		t.Fatal("login must set the oauth_state nonce cookie")
	}
	parsed, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL must carry the state token")
	}
	callbackURL := f.baseURL + "/api/v1/auth/oauth/oidc/callback?code=code&state=" + url.QueryEscape(state)
	req, err := http.NewRequest("GET", callbackURL, http.NoBody)
	if err != nil {
		t.Fatalf("new callback request: %v", err)
	}
	req.Header.Set("Cookie", "oauth_state="+nonce)
	cbResp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET oidc callback: %v", err)
	}
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", cbResp.StatusCode)
	}
	if loc := cbResp.Header.Get("Location"); !strings.HasPrefix(loc, "/auth/callback?redirect=") {
		t.Fatalf("callback Location = %q, want /auth/callback", loc)
	}
	access := responseCookie(cbResp, "dagger_kubernetes_access")
	if access == "" {
		t.Fatal("callback must set the access cookie")
	}
	return access
}

// me returns the /me status and decoded body for the given access cookie.
func (f *reloginFixture) me(t *testing.T, access string) (status int, body map[string]any) {
	t.Helper()
	req, err := http.NewRequest("GET", f.baseURL+"/api/v1/auth/me", http.NoBody)
	if err != nil {
		t.Fatalf("new me request: %v", err)
	}
	req.Header.Set("Cookie", "dagger_kubernetes_access="+access)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET /me: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestOIDCReloginClearsUnavailableDenial is the end-to-end regression for the
// same-account re-login loop: a cached stateUnavailable denial (IdP transport
// failure) must not survive a fresh, accepted login of the SAME identity. The
// injected clock is advanced past the negative TTL; the fresh login advances
// OAuthLoginVersion and the next /me re-checks the IdP and succeeds
// immediately, without an account switch or waiting out the TTL.
func TestOIDCReloginClearsUnavailableDenial(t *testing.T) {
	f := newReloginFixture(t, []string{"devs"})

	// 1. Login + /me 200: a healthy cached success.
	access := f.oidcLogin(t)
	if status, body := f.me(t, access); status != http.StatusOK {
		t.Fatalf("initial /me status = %d, want 200 (body=%v)", status, body)
	}

	// 2. Advance the clock past the successful TTL AND grace, then fail the
	// IdP userinfo: the revalidation now denies.
	f.clock.Store(time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC).UnixNano())
	f.issuer.userinfoErrCode.Store(http.StatusServiceUnavailable)
	if status, _ := f.me(t, access); status != http.StatusUnauthorized {
		t.Fatalf("denied /me status = %d, want 401 while IdP is down", status)
	}

	// 3. Restore the IdP and re-login the SAME account WITHOUT advancing the
	// clock. The negative TTL has not expired, but the new login generation must
	// force an immediate re-check that now succeeds.
	f.issuer.userinfoErrCode.Store(0)
	access = f.oidcLogin(t)
	status, body := f.me(t, access)
	if status != http.StatusOK {
		t.Fatalf("post-re-login /me status = %d, want 200 (body=%v)", status, body)
	}
	if body["oauth_provider"] != "oidc" {
		t.Fatalf("oauth_provider = %v, want oidc", body["oauth_provider"])
	}

	// 4. Repeat the cycle a second time to prove the recovery is stable, not a
	// one-off.
	f.clock.Store(time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC).UnixNano())
	f.issuer.userinfoErrCode.Store(http.StatusServiceUnavailable)
	if status, _ := f.me(t, access); status != http.StatusUnauthorized {
		t.Fatalf("second denial /me status = %d, want 401", status)
	}
	f.issuer.userinfoErrCode.Store(0)
	access = f.oidcLogin(t)
	if status, _ := f.me(t, access); status != http.StatusOK {
		t.Fatalf("second re-login /me status = %d, want 200", status)
	}
}

// TestOAuthAccountLifecycle exercises the administrator disable/enable and
// soft-delete/restore lifecycle through the real HTTP surface: a disabled
// account's outstanding JWT is rejected, internal login is blocked, enable
// restores login, delete reserves the identity, and restore + password reset +
// enable makes the account usable again.
func TestOAuthAccountLifecycle(t *testing.T) {
	f := newReloginFixture(t, []string{"devs"})

	// Bootstrap an administrator and a victim account through internal auth.
	if _, err := f.users.Create(context.Background(), "admin", "admin-pass-123", domain.RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	victim, err := f.users.Create(context.Background(), "victim", "victim-pass-123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}

	adminAccess := loginCookie(t, f.baseURL, "admin", "admin-pass-123")
	victimAccess := loginCookie(t, f.baseURL, "victim", "victim-pass-123")

	setState := func(t *testing.T, id, action string, wantStatus int) {
		t.Helper()
		body := fmt.Sprintf(`{"action":%q}`, action)
		req, err := http.NewRequest("POST", f.baseURL+"/api/v1/users/"+id+"/state", strings.NewReader(body))
		if err != nil {
			t.Fatalf("new state request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", "dagger_kubernetes_access="+adminAccess)
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatalf("state %s: %v", action, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("state %s status = %d, want %d", action, resp.StatusCode, wantStatus)
		}
	}

	// Disable the victim: their outstanding JWT is rejected and login blocked.
	setState(t, victim.ID, "disable", http.StatusOK)
	if status, _ := f.me(t, victimAccess); status != http.StatusUnauthorized {
		t.Fatalf("disabled victim /me status = %d, want 401", status)
	}
	if status, _ := loginInternal(t, f.baseURL, "victim", "victim-pass-123"); status != http.StatusUnauthorized {
		t.Fatalf("disabled victim login status = %d, want 401", status)
	}

	// Enable: login works again, but the pre-disable JWT stays revoked.
	setState(t, victim.ID, "enable", http.StatusOK)
	if status, _ := loginInternal(t, f.baseURL, "victim", "victim-pass-123"); status != http.StatusOK {
		t.Fatalf("re-enabled victim login status = %d, want 200", status)
	}
	if status, _ := f.me(t, victimAccess); status != http.StatusUnauthorized {
		t.Fatalf("old victim JWT after enable = %d, want 401", status)
	}

	// Delete (soft): the identity is reserved and login is blocked.
	deleteUserHTTP(t, f.baseURL, victim.ID, adminAccess, http.StatusNoContent)
	got, err := f.users.Get(context.Background(), victim.ID)
	if err != nil {
		t.Fatalf("tombstone must remain resolvable: %v", err)
	}
	if got.DeletedAt == nil {
		t.Fatal("tombstone must carry deleted_at")
	}
	if status, _ := loginInternal(t, f.baseURL, "victim", "victim-pass-123"); status != http.StatusUnauthorized {
		t.Fatalf("deleted victim login status = %d, want 401", status)
	}

	// Restore leaves the account disabled + credentialless (login blocked);
	// an admin password reset then enable makes it usable again.
	setState(t, victim.ID, "restore", http.StatusOK)
	restored, err := f.users.Get(context.Background(), victim.ID)
	if err != nil {
		t.Fatalf("restored victim: %v", err)
	}
	if restored.DeletedAt != nil || restored.DisabledAt == nil {
		t.Fatalf("restore must clear deletion and keep disabled: deleted=%v disabled=%v", restored.DeletedAt, restored.DisabledAt)
	}
	if status, _ := loginInternal(t, f.baseURL, "victim", "victim-pass-123"); status != http.StatusUnauthorized {
		t.Fatalf("restored (disabled) victim login status = %d, want 401", status)
	}
	setState(t, victim.ID, "enable", http.StatusOK)
	resetPasswordHTTP(t, f.baseURL, victim.ID, adminAccess, "victim-new-pass-456", http.StatusNoContent)
	if status, _ := loginInternal(t, f.baseURL, "victim", "victim-new-pass-456"); status != http.StatusOK {
		t.Fatalf("restored+enabled+reset victim login status = %d, want 200", status)
	}
}

// loginInternal performs an internal JSON login and returns the status and the
// access cookie (empty on failure).
func loginInternal(t *testing.T, baseURL, username, password string) (status int, access string) {
	t.Helper()
	payload := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	resp, err := http.Post(baseURL+"/api/v1/auth/login", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, responseCookie(resp, "dagger_kubernetes_access")
}

// loginCookie logs in an internal user and returns their access cookie,
// failing the test on any non-200.
func loginCookie(t *testing.T, baseURL, username, password string) string {
	t.Helper()
	status, access := loginInternal(t, baseURL, username, password)
	if status != http.StatusOK || access == "" {
		t.Fatalf("login %s status = %d, want 200 with access cookie", username, status)
	}
	return access
}

// deleteUserHTTP soft-deletes a user through the admin DELETE endpoint.
func deleteUserHTTP(t *testing.T, baseURL, id, access string, wantStatus int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", baseURL+"/api/v1/users/"+id, http.NoBody)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	req.Header.Set("Cookie", "dagger_kubernetes_access="+access)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("delete user: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("delete status = %d, want %d", resp.StatusCode, wantStatus)
	}
}

// resetPasswordHTTP sets a user's password through the admin endpoint.
func resetPasswordHTTP(t *testing.T, baseURL, id, access, newPassword string, wantStatus int) {
	t.Helper()
	body := fmt.Sprintf(`{"password":%q}`, newPassword)
	req, err := http.NewRequest("PUT", baseURL+"/api/v1/users/"+id+"/password", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new reset password request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "dagger_kubernetes_access="+access)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("reset password: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("reset password status = %d, want %d", resp.StatusCode, wantStatus)
	}
}
