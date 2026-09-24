package service

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const oidcTestClientID = "test-client"

// oidcTestEncKey is the AES-256 key (32 bytes) used by tests that exercise the
// encrypted at-rest OAuth credential path.
const oidcTestEncKey = "0123456789abcdef0123456789abcdef"

// fakeOIDCIssuer is a loopback httptest OIDC issuer serving discovery, JWKS,
// token, and userinfo endpoints. go-oidc supports http loopback issuers, so the
// default (real) providerFactory can discover against it.
type fakeOIDCIssuer struct {
	t           *testing.T
	srv         *httptest.Server
	clientID    string
	signKey     *rsa.PrivateKey
	publishKey  *rsa.PrivateKey
	claims      map[string]any
	userinfo    map[string]any // claims served by /userinfo (nil -> empty object)
	tokenStatus int

	// Revalidation test knobs.
	userinfoStatus int    // non-zero HTTP status served by /userinfo
	tokenErrCode   string // OAuth2 error code served with 400 by /token
	tokenErrAfter  int    // succeed this many /token calls before tokenErrCode kicks in (0 = fail from the start)
	tokenExpiresIn int    // non-zero expires_in served by /token
	accessToken    string // access_token served by /token (empty = default)
	refreshToken   string // refresh_token served by /token (empty = omitted)
	tokenCallCount int
}

func newFakeOIDCIssuer(t *testing.T) *fakeOIDCIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	f := &fakeOIDCIssuer{
		t:          t,
		clientID:   oidcTestClientID,
		signKey:    key,
		publishKey: key,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/jwks", f.handleJWKS)
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/userinfo", f.handleUserinfo)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.claims = f.baseClaims()
	return f
}

func (f *fakeOIDCIssuer) baseClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"sub":                "alice-sub",
		"aud":                f.clientID,
		"iss":                f.srv.URL,
		"exp":                now.Add(5 * time.Minute).Unix(),
		"iat":                now.Add(-1 * time.Minute).Unix(),
		"preferred_username": "alice",
		"groups":             []any{"devs"},
	}
}

func (f *fakeOIDCIssuer) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
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

func (f *fakeOIDCIssuer) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	jwk := jose.JSONWebKey{Key: &f.publishKey.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
}

func (f *fakeOIDCIssuer) handleToken(w http.ResponseWriter, _ *http.Request) {
	f.tokenCallCount++
	if f.tokenStatus != 0 {
		w.WriteHeader(f.tokenStatus)
		return
	}
	if f.tokenErrCode != "" && (f.tokenErrAfter == 0 || f.tokenCallCount > f.tokenErrAfter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": f.tokenErrCode})
		return
	}
	accessToken := f.accessToken
	if accessToken == "" {
		accessToken = "test-access-token"
	}
	resp := map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"id_token":     f.mintIDToken(),
	}
	if f.refreshToken != "" {
		resp["refresh_token"] = f.refreshToken
	}
	if f.tokenExpiresIn != 0 {
		resp["expires_in"] = f.tokenExpiresIn
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeOIDCIssuer) handleUserinfo(w http.ResponseWriter, _ *http.Request) {
	if f.userinfoStatus != 0 {
		w.WriteHeader(f.userinfoStatus)
		return
	}
	claims := f.userinfo
	if claims == nil {
		claims = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(claims)
}

// mintIDToken signs f.claims with f.signKey as a compact JWS.
func (f *fakeOIDCIssuer) mintIDToken() string {
	f.t.Helper()
	jwk := &jose.JSONWebKey{Key: f.signKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, nil)
	if err != nil {
		f.t.Fatalf("new signer: %v", err)
	}
	payload, err := json.Marshal(f.claims)
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

func newOIDCService(t *testing.T, cfg *domain.OAuthConfig) (*OIDCOAuthService, *GroupService) {
	t.Helper()
	r := newServiceDB(t)
	logger := testLogger()
	usvc := NewUserService(r.users, r.groups, logger)
	gsvc := NewGroupService(r.groups, r.users, logger)
	jwtSvc := NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 15*time.Minute, 168*time.Hour)
	mapper, err := NewGroupMapper(cfg.GroupMappings)
	if err != nil {
		t.Fatalf("NewGroupMapper: %v", err)
	}
	svc := NewOIDCOAuthService(cfg, mapper, usvc, r.groups, jwtSvc, logger, nil, nil)
	return svc, gsvc
}

func oidcCfg(issuerURL string, mutate func(*domain.OAuthConfig)) *domain.OAuthConfig {
	cfg := &domain.OAuthConfig{
		Enabled:      true,
		Provider:     "oidc",
		ClientID:     oidcTestClientID,
		ClientSecret: "csec",
		RedirectURL:  "https://supv.example.com/api/v1/auth/oauth/oidc/callback",
		IssuerURL:    issuerURL,
		AllowedOrgs:  []string{"devs"},
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func TestOIDCCompleteSuccess(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	access, refresh, u, err := svc.Complete(context.Background(), "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if access == "" || refresh == "" || u == nil {
		t.Fatal("bad result")
	}
	if u.Username != "alice" {
		t.Fatalf("username = %q, want alice", u.Username)
	}
	if u.OAuthProvider != "oidc" || u.OAuthID != "alice-sub" {
		t.Fatalf("oauth = %s/%s, want oidc/alice-sub", u.OAuthProvider, u.OAuthID)
	}
}

func TestOIDCCompleteIdempotent(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))
	ctx := context.Background()

	_, _, u1, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, _, u2, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if u1.ID != u2.ID {
		t.Fatal("second call should return the same user")
	}
}

func TestOIDCCompleteGroupsNotAllowed(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["groups"] = []any{"other"}
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("groups not allowed: %v, want ErrForbidden", err)
	}
}

func TestOIDCCompleteNoAllowedOrgsRestriction(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["groups"] = []any{"anything"}
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.AllowedOrgs = nil
	}))

	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with no org restriction: %v", err)
	}
}

func TestOIDCCompleteUsernameClaimFallback(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	delete(issuer.claims, "preferred_username")
	issuer.claims["email"] = "alice@example.com"
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, u, err := svc.Complete(context.Background(), "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u.Username != "alice@example.com" {
		t.Fatalf("username = %q, want alice@example.com", u.Username)
	}
}

func TestOIDCCompleteUsernameClaimMissing(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	delete(issuer.claims, "preferred_username")
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "no usable username claim") {
		t.Fatalf("Complete error = %v, want no usable username claim", err)
	}
}

func TestOIDCCompleteGroupsClaimString(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["groups"] = "devs"
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with string groups claim: %v", err)
	}
}

func TestOIDCCompleteDefaultGroupAutoJoin(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	svc, gsvc := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.DefaultGroup = "default"
	}))
	ctx := context.Background()

	g, _ := gsvc.Create(ctx, GroupInput{Name: "default", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("user should auto-join configured default group, got %v", groups)
	}
}

func TestOIDCCompleteDiscoveryFailure(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))
	svc.providerFactory = func(_ context.Context, _ string) (oidcProvider, error) {
		return nil, errors.New("discovery boom")
	}

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "discover provider") {
		t.Fatalf("Complete error = %v, want discover provider", err)
	}
}

func TestOIDCCompleteTokenExchangeFailure(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.tokenStatus = http.StatusInternalServerError
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "exchange code") {
		t.Fatalf("Complete error = %v, want exchange code", err)
	}
}

func TestOIDCCompleteIDTokenVerificationFailure(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	issuer.signKey = other
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err = svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "verify id token") {
		t.Fatalf("Complete error = %v, want verify id token", err)
	}
}

func TestOIDCCompleteIDTokenExpired(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "verify id token") {
		t.Fatalf("Complete error = %v, want verify id token", err)
	}
}

func TestOIDCCompleteIDTokenWrongAudience(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["aud"] = "some-other-client"
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "verify id token") {
		t.Fatalf("Complete error = %v, want verify id token", err)
	}
}

func TestOIDCCompleteMissingSub(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	delete(issuer.claims, "sub")
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "missing sub") {
		t.Fatalf("Complete error = %v, want missing sub", err)
	}
}

func TestOIDCCompleteMultiAudienceAzpMatch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["aud"] = []any{oidcTestClientID, "other-client"}
	issuer.claims["azp"] = oidcTestClientID
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with matching azp: %v", err)
	}
}

func TestOIDCCompleteMultiAudienceAzpMismatch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["aud"] = []any{oidcTestClientID, "other-client"}
	issuer.claims["azp"] = "other-client"
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "azp") {
		t.Fatalf("Complete error = %v, want azp mismatch", err)
	}
}

func TestOIDCCompleteMultiAudienceMissingAzp(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["aud"] = []any{oidcTestClientID, "other-client"}
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "missing azp") {
		t.Fatalf("Complete error = %v, want missing azp", err)
	}
}

func TestOIDCLoginURL(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	loginURL := svc.LoginURL("state123")
	u, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	q := u.Query()
	if !strings.HasPrefix(loginURL, issuer.srv.URL+"/auth?") {
		t.Fatalf("login url = %q, want authorization endpoint", loginURL)
	}
	if q.Get("client_id") != oidcTestClientID {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "https://supv.example.com/api/v1/auth/oauth/oidc/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != "state123" {
		t.Fatalf("state = %q", q.Get("state"))
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope = %q, want openid", q.Get("scope"))
	}
}

func TestOIDCLoginURLAppendsOpenIDScope(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.Scopes = []string{"profile", "email"}
	}))

	loginURL := svc.LoginURL("state123")
	u, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse login url: %v", err)
	}
	if !strings.Contains(u.Query().Get("scope"), "openid") {
		t.Fatalf("scope = %q, want openid appended", u.Query().Get("scope"))
	}
}

func TestOIDCIssuerTrailingSlashTrimmed(t *testing.T) {
	svc, _ := newOIDCService(t, oidcCfg("http://localhost:5556/", nil))
	if svc.issuerURL != "http://localhost:5556" {
		t.Fatalf("issuerURL = %q, want trailing slash trimmed", svc.issuerURL)
	}
}

func TestOIDCCompleteUserInfoUsername(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	delete(issuer.claims, "preferred_username")
	issuer.userinfo = map[string]any{
		"preferred_username": "bob",
		"email":              "bob@example.com",
	}
	svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, u, err := svc.Complete(context.Background(), "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u.Username != "bob" {
		t.Fatalf("username = %q, want bob", u.Username)
	}
	if u.OAuthProvider != "oidc" || u.OAuthID != "alice-sub" {
		t.Fatalf("oauth = %s/%s, want oidc/alice-sub", u.OAuthProvider, u.OAuthID)
	}
}

func TestOIDCCompleteUserInfoGroups(t *testing.T) {
	t.Run("intersect", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		delete(issuer.claims, "groups")
		issuer.userinfo = map[string]any{"groups": []any{"devs"}}
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

		if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	})

	t.Run("no intersection", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		delete(issuer.claims, "groups")
		issuer.userinfo = map[string]any{"groups": []any{"other"}}
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

		_, _, _, err := svc.Complete(context.Background(), "code")
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("Complete error = %v, want ErrForbidden", err)
		}
	})
}

func TestOIDCCompleteAllowedGroups(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		wantErr bool
	}{
		{name: "pass", allowed: []string{"devs"}},
		{name: "deny", allowed: []string{"platform"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newFakeOIDCIssuer(t)
			svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
				c.AllowedOrgs = nil
				c.AllowedGroups = tt.allowed
			}))

			_, _, _, err := svc.Complete(context.Background(), "code")
			if tt.wantErr {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatalf("groups not allowed: %v, want ErrForbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Complete with allowed_groups: %v", err)
			}
		})
	}
}

func TestOIDCCompleteAllowedGroupsUnionOrgs(t *testing.T) {
	tests := []struct {
		name    string
		orgs    []string
		groups  []string
		wantErr bool
	}{
		{name: "in allowed_orgs", orgs: []string{"devs"}, groups: []string{"platform"}},
		{name: "in allowed_groups", orgs: []string{"platform"}, groups: []string{"devs"}},
		{name: "deny when in neither", orgs: []string{"a"}, groups: []string{"b"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newFakeOIDCIssuer(t)
			svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
				c.AllowedOrgs = tt.orgs
				c.AllowedGroups = tt.groups
			}))

			_, _, _, err := svc.Complete(context.Background(), "code")
			if tt.wantErr {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatalf("neither allowlist matches: %v, want ErrForbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("union should accept allowlist membership: %v", err)
			}
		})
	}
}

func TestOIDCCompleteGroupMapping(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["groups"] = []any{"devs", "platform"}
	svc, gsvc := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.AllowedOrgs = nil
		c.GroupMappings = []domain.GroupMappingRule{
			{Pattern: "^devs$", Replacement: "dev-mapped"},
			{Pattern: "^platform$", Replacement: "platform-mapped"},
		}
	}))
	ctx := context.Background()

	dev, _ := gsvc.Create(ctx, GroupInput{Name: "dev-mapped", AgentAvailable: true})
	platform, _ := gsvc.Create(ctx, GroupInput{Name: "platform-mapped", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	ids := map[string]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}
	if !ids[dev.ID] || !ids[platform.ID] {
		t.Fatalf("user should be member of mapped groups, got %v", groups)
	}
}

func TestOIDCCompleteGroupMappingAutoCreatesGroup(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["groups"] = []any{"devs"}
	svc, gsvc := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.AllowedOrgs = nil
		c.GroupMappings = []domain.GroupMappingRule{
			{Pattern: "^devs$", Replacement: "auto-dev"},
		}
		c.MappedGroupMaxRunnerSessions = 2
	}))
	ctx := context.Background()

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].Name != "auto-dev" {
		t.Fatalf("user should be member of the auto-created mapped group, got %v", groups)
	}
	if groups[0].MaxRunnerSessions != 2 || !groups[0].AgentAvailable {
		t.Fatalf("auto-created group = %+v, want quota 2 and agent available", groups[0])
	}
}

func TestOIDCCompleteNoGroupMappingsNoSync(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.claims["groups"] = []any{"devs"}
	svc, gsvc := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.AllowedOrgs = nil
	}))
	ctx := context.Background()

	// A group named exactly like the claim must NOT be joined: no mapping rules.
	gsvc.Create(ctx, GroupInput{Name: "devs", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 0 {
		t.Fatalf("no mappings should mean no auto-membership, got %v", groups)
	}
}

func TestOIDCCompleteAdminGroups(t *testing.T) {
	t.Run("promote on match", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.claims["groups"] = []any{"devs", "HM_ADM_Outils"}
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
			c.AllowedOrgs = nil
			c.AdminGroups = []string{"HM_ADM_Outils"}
		}))
		ctx := context.Background()

		_, _, u, err := svc.Complete(ctx, "code")
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if u.Role != domain.RoleAdmin {
			t.Fatalf("role = %v, want admin", u.Role)
		}
		if !u.OAuthAdmin {
			t.Fatal("OAuthAdmin = false, want true")
		}
		if len(u.OAuthGroups) != 2 || u.OAuthGroups[0] != "HM_ADM_Outils" || u.OAuthGroups[1] != "devs" {
			t.Fatalf("OAuthGroups = %v, want sorted [HM_ADM_Outils devs]", u.OAuthGroups)
		}
	})

	t.Run("stays user on non-match", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.claims["groups"] = []any{"devs"}
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
			c.AllowedOrgs = nil
			c.AdminGroups = []string{"HM_ADM_Outils"}
		}))
		ctx := context.Background()

		_, _, u, err := svc.Complete(ctx, "code")
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if u.Role != domain.RoleUser {
			t.Fatalf("role = %v, want user", u.Role)
		}
		if u.OAuthAdmin {
			t.Fatal("OAuthAdmin = true, want false")
		}
	})

	t.Run("manual admin preserved on non-match", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.claims["groups"] = []any{"devs"}
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
			c.AllowedOrgs = nil
			c.AdminGroups = []string{"HM_ADM_Outils"}
		}))
		ctx := context.Background()

		// First login as a matching admin, then manually demote+clear the flag,
		// then promote manually (RoleAdmin, OAuthAdmin=false) and re-login.
		issuer.claims["groups"] = []any{"HM_ADM_Outils"}
		if _, _, u, err := svc.Complete(ctx, "code"); err != nil {
			t.Fatalf("Complete (matching): %v", err)
		} else if !u.OAuthAdmin {
			t.Fatal("expected OAuthAdmin = true after matching login")
		}
		// Second (non-matching) login: OAuth-granted admin is demoted.
		issuer.claims["groups"] = []any{"devs"}
		_, _, u, err := svc.Complete(ctx, "code")
		if err != nil {
			t.Fatalf("Complete (non-matching): %v", err)
		}
		if u.Role != domain.RoleUser || u.OAuthAdmin {
			t.Fatalf("OAuth-granted admin should be demoted, got role=%v oa=%v", u.Role, u.OAuthAdmin)
		}
	})

	t.Run("disabled when empty", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.claims["groups"] = []any{"devs"}
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
			c.AllowedOrgs = nil
			c.AdminGroups = nil
		}))
		ctx := context.Background()

		_, _, u, err := svc.Complete(ctx, "code")
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if u.Role != domain.RoleUser || u.OAuthAdmin {
			t.Fatalf("empty admin_groups must be a no-op, got role=%v oa=%v", u.Role, u.OAuthAdmin)
		}
	})

	t.Run("groups claim absent yields no OAuthGroups", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		delete(issuer.claims, "groups")
		svc, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
			c.AllowedOrgs = nil
		}))
		ctx := context.Background()

		_, _, u, err := svc.Complete(ctx, "code")
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if u.OAuthGroups != nil {
			t.Fatalf("OAuthGroups = %v, want nil when the claim is absent", u.OAuthGroups)
		}
	})
}

// newOIDCServiceForKey returns an OIDCOAuthService with credential encryption
// enabled (newOIDCService passes a nil encKey).
func newOIDCServiceForKey(t *testing.T, cfg *domain.OAuthConfig) *OIDCOAuthService {
	t.Helper()
	svc, _ := newOIDCService(t, cfg)
	svc.encKey = []byte(oidcTestEncKey)
	return svc
}

// seedOIDCUser creates a local user with a stored (optionally encrypted)
// OAuth credential and returns it.
func seedOIDCUser(t *testing.T, svc *OIDCOAuthService, cred *oauthCredential) *domain.User {
	t.Helper()
	ctx := context.Background()
	u, err := svc.users.Create(ctx, "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	u.OAuthProvider = "oidc"
	u.OAuthID = "alice-sub"
	if cred != nil {
		ct, err := encryptOAuthCredential(svc.encKey, cred)
		if err != nil {
			t.Fatalf("encrypt credential: %v", err)
		}
		u.OAuthTokenCiphertext = ct
	}
	if err := svc.users.Update(ctx, u); err != nil {
		t.Fatalf("persist user: %v", err)
	}
	return u
}

// TestOIDCRevalidateCredentialUnusable covers the credential-unusable cases
// (issue #30): they must return errOAuthCredentialExpired — never
// domain.ErrSessionRevoked, which would deactivate the user and delete their
// API token.
func TestOIDCRevalidateCredentialUnusable(t *testing.T) {
	expiredCred := &oauthCredential{
		Provider:     "oidc",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	validCred := &oauthCredential{
		Provider:     "oidc",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	tests := []struct {
		name   string
		cred   *oauthCredential
		mutate func(t *testing.T, issuer *fakeOIDCIssuer, u *domain.User)
	}{
		{
			name: "refresh rejected with invalid_grant",
			cred: expiredCred,
			mutate: func(t *testing.T, issuer *fakeOIDCIssuer, _ *domain.User) {
				t.Helper()
				issuer.tokenErrCode = "invalid_grant"
				issuer.userinfo = map[string]any{"groups": []any{"devs"}}
			},
		},
		{
			name: "userinfo 401 then refresh invalid_grant",
			cred: expiredCred,
			mutate: func(t *testing.T, issuer *fakeOIDCIssuer, _ *domain.User) {
				t.Helper()
				issuer.tokenErrCode = "invalid_grant"
				issuer.tokenErrAfter = 1   // first refresh succeeds, the retry fails
				issuer.tokenExpiresIn = -1 // refreshed access token is already expired
				issuer.userinfoStatus = http.StatusUnauthorized
			},
		},
		{
			name: "userinfo 401 not recoverable by refresh",
			cred: validCred,
			mutate: func(t *testing.T, issuer *fakeOIDCIssuer, _ *domain.User) {
				t.Helper()
				issuer.userinfoStatus = http.StatusUnauthorized
			},
		},
		{
			name: "stored credential fails to decrypt",
			cred: nil,
			mutate: func(t *testing.T, _ *fakeOIDCIssuer, u *domain.User) {
				t.Helper()
				u.OAuthTokenCiphertext = "!!not-a-valid-ciphertext!!"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer := newFakeOIDCIssuer(t)
			issuer.userinfo = map[string]any{"groups": []any{"devs"}}
			svc := newOIDCServiceForKey(t, oidcCfg(issuer.srv.URL, nil))
			u := seedOIDCUser(t, svc, tt.cred)
			tt.mutate(t, issuer, u)

			groups, err := svc.Revalidate(context.Background(), u)
			if !errors.Is(err, errOAuthCredentialExpired) {
				t.Fatalf("Revalidate err = %v, want errOAuthCredentialExpired (groups=%v)", err, groups)
			}
			if errors.Is(err, domain.ErrSessionRevoked) {
				t.Fatal("credential expiry must not be classified as revocation")
			}
		})
	}
}

// TestOIDCRevalidateGroupsFailingAllowlistStillForbidden is the regression for
// positive revocation: userinfo succeeds but the groups no longer satisfy the
// allowlist, which must stay domain.ErrForbidden (destructive revoke path).
func TestOIDCRevalidateGroupsFailingAllowlistStillForbidden(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.userinfo = map[string]any{"groups": []any{"outsiders"}}
	svc := newOIDCServiceForKey(t, oidcCfg(issuer.srv.URL, nil))
	u := seedOIDCUser(t, svc, &oauthCredential{
		Provider:    "oidc",
		AccessToken: "old-access",
		ExpiresAt:   time.Now().Add(time.Hour),
	})

	_, err := svc.Revalidate(context.Background(), u)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("Revalidate err = %v, want domain.ErrForbidden", err)
	}
	if errors.Is(err, errOAuthCredentialExpired) {
		t.Fatal("allowlist miss must not be classified as credential expiry")
	}
}

// TestOIDCRevalidateRefreshRotatesAndPersistsCredential covers the healthy
// refresh path: a rotated refresh token is persisted on the user record.
func TestOIDCRevalidateRefreshRotatesAndPersistsCredential(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.userinfo = map[string]any{"groups": []any{"devs"}}
	issuer.accessToken = "rotated-access"
	issuer.refreshToken = "rotated-refresh"
	svc := newOIDCServiceForKey(t, oidcCfg(issuer.srv.URL, nil))
	u := seedOIDCUser(t, svc, &oauthCredential{
		Provider:     "oidc",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	oldCiphertext := u.OAuthTokenCiphertext

	groups, err := svc.Revalidate(context.Background(), u)
	if err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
	if len(groups) != 1 || groups[0] != "devs" {
		t.Fatalf("groups = %v, want [devs]", groups)
	}
	if u.OAuthTokenCiphertext == oldCiphertext {
		t.Fatal("rotated credential was not persisted on the in-memory user")
	}
	got, err := svc.users.Get(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.OAuthTokenCiphertext == oldCiphertext {
		t.Fatal("rotated credential was not persisted to the store")
	}
	decoded, err := decryptOAuthCredential(svc.encKey, got.OAuthTokenCiphertext)
	if err != nil {
		t.Fatalf("decrypt persisted credential: %v", err)
	}
	if decoded.RefreshToken != "rotated-refresh" || decoded.AccessToken != "rotated-access" {
		t.Fatalf("persisted credential = %+v, want rotated tokens", decoded)
	}
}

// TestOAuthTokenRevokedClassification pins the oauthTokenRevoked semantics:
// invalid_grant/invalid_token/401 are "credential unusable" (ambiguous), any
// other error is a transient IdP-unavailable condition. go-oidc wraps token
// and HTTP errors with %v, so message-based classification must be covered too.
func TestOAuthTokenRevokedClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "retrieve error invalid_grant", err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, want: true},
		{name: "retrieve error invalid_token", err: &oauth2.RetrieveError{ErrorCode: "invalid_token"}, want: true},
		{
			name: "retrieve error 401 without code",
			err:  &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}},
			want: true,
		},
		{
			name: "retrieve error other client error",
			err:  &oauth2.RetrieveError{ErrorCode: "invalid_client", Response: &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}},
			want: false,
		},
		{name: "wrapped invalid_grant message", err: errors.New("oidc: get access token: oauth2: \"invalid_grant\""), want: true},
		{name: "wrapped userinfo 401 message", err: errors.New("401 Unauthorized: {\"error\":\"invalid_token\"}"), want: true},
		{name: "transport error", err: errors.New("oidc: userinfo: Get \"https://dex.example.com/userinfo\": dial tcp: connection refused"), want: false},
		{name: "server error", err: errors.New("oidc: userinfo: 500 Internal Server Error: boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := oauthTokenRevoked(tt.err); got != tt.want {
				t.Fatalf("oauthTokenRevoked(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
