package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// oidcIssuer is a loopback httptest OIDC issuer serving discovery, JWKS, and
// token endpoints. go-oidc supports http loopback issuers, so the real
// defaultOIDCProviderFactory can discover against it.
type oidcIssuer struct {
	t        *testing.T
	srv      *httptest.Server
	clientID string
	signKey  *rsa.PrivateKey
	groups   []any
}

func newOIDCIssuer(t *testing.T, clientID string, groups []any) *oidcIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	f := &oidcIssuer{t: t, clientID: clientID, signKey: key, groups: groups}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks", f.handleJWKS)
	mux.HandleFunc("/token", f.handleToken)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *oidcIssuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                 f.srv.URL,
		"authorization_endpoint": f.srv.URL + "/auth",
		"token_endpoint":         f.srv.URL + "/token",
		"jwks_uri":               f.srv.URL + "/jwks",
		"userinfo_endpoint":      f.srv.URL + "/userinfo",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *oidcIssuer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	jwk := jose.JSONWebKey{Key: &f.signKey.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
}

func (f *oidcIssuer) handleToken(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"id_token":     f.mintIDToken(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *oidcIssuer) mintIDToken() string {
	f.t.Helper()
	now := time.Now()
	claims := map[string]any{
		"sub":                "alice-sub",
		"aud":                f.clientID,
		"iss":                f.srv.URL,
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Add(-1 * time.Minute).Unix(),
		"preferred_username": "alice",
		"groups":             f.groups,
	}
	jwk := &jose.JSONWebKey{Key: f.signKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, nil)
	if err != nil {
		f.t.Fatalf("new signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatalf("marshal claims: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatalf("sign: %v", err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		f.t.Fatalf("serialize: %v", err)
	}
	return s
}

// TestOIDCLoginForbiddenFlow is a black-box OIDC flow test: it starts a
// supervisor pointed at a loopback OIDC issuer, drives the login endpoint to an
// authorize redirect, then completes the callback for a user whose groups are
// outside `allowed_groups` and asserts the SPA is redirected to
// `/auth/login?error=group_required`.
func TestOIDCLoginForbiddenFlow(t *testing.T) {
	const clientID = "integration-client"
	controlAddr, dataAddr := freeAddr(t), freeAddr(t)
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
		AllowedGroups: []string{"platform"},
	}
	mapper, err := service.NewGroupMapper(nil)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	oauthSvc := service.NewOIDCOAuthService(oauthCfg, mapper, usersSvc, groupRepo, jwtSvc, logger, nil, nil)

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger), groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: controlAddr,
		DataAddr:    dataAddr,
		DataHost:    "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store), CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient,
		JWT: jwtSvc, OAuth: oauthSvc, OAuthProvider: "oidc",
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
	time.Sleep(500 * time.Millisecond)

	baseURL := fmt.Sprintf("http://localhost%s", controlAddr)
	// 1. Login: must 302 to the OIDC authorize URL and set the nonce cookie.
	noRedirect := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	loginResp, err := noRedirect.Get(baseURL + "/api/v1/auth/oauth/oidc/login?redirect=/pipelines")
	if err != nil {
		t.Fatalf("GET oidc login: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", loginResp.StatusCode)
	}
	location := loginResp.Header.Get("Location")
	if !strings.HasPrefix(location, issuer.srv.URL+"/auth?") {
		t.Fatalf("login Location = %q, want authorize endpoint", location)
	}
	nonce := cookieValue(loginResp.Header.Get("Set-Cookie"), "oauth_state")
	if nonce == "" {
		t.Fatal("login must set the oauth_state nonce cookie")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL must carry the state token")
	}

	// 2. Callback for a user outside allowed_groups → forbidden redirect.
	callbackURL := baseURL + "/api/v1/auth/oauth/oidc/callback?code=code&state=" + url.QueryEscape(state)
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
	if loc := cbResp.Header.Get("Location"); loc != "/auth/login?error=group_required" {
		t.Fatalf("callback Location = %q, want /auth/login?error=group_required", loc)
	}
}

// cookieValue extracts name=value from a Set-Cookie header value.
func cookieValue(setCookie, name string) string {
	prefix := name + "="
	for _, part := range strings.Split(setCookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, prefix) {
			return strings.TrimPrefix(part, prefix)
		}
	}
	return ""
}
