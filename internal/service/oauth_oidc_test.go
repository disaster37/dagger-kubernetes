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

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const oidcTestClientID = "test-client"

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
}

func newFakeOIDCIssuer(t *testing.T, clientID string) *fakeOIDCIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	f := &fakeOIDCIssuer{
		t:          t,
		clientID:   clientID,
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
	if f.tokenStatus != 0 {
		w.WriteHeader(f.tokenStatus)
		return
	}
	resp := map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"id_token":     f.mintIDToken(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeOIDCIssuer) handleUserinfo(w http.ResponseWriter, _ *http.Request) {
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

func newOIDCService(t *testing.T, cfg *domain.OAuthConfig) (*OIDCOAuthService, *UserService, *GroupService) {
	t.Helper()
	r := newServiceDB(t)
	logger := testLogger()
	usvc := NewUserService(r.users, r.groups, logger)
	gsvc := NewGroupService(r.groups, r.users, logger)
	jwtSvc := NewJWTService([]byte("test-secret-32-bytes-long-enough!!"), 15*time.Minute, 168*time.Hour)
	svc := NewOIDCOAuthService(cfg, usvc, r.groups, jwtSvc, logger)
	return svc, usvc, gsvc
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
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

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
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))
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
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["groups"] = []any{"other"}
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("groups not allowed: %v, want ErrForbidden", err)
	}
}

func TestOIDCCompleteNoAllowedOrgsRestriction(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["groups"] = []any{"anything"}
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.AllowedOrgs = nil
	}))

	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with no org restriction: %v", err)
	}
}

func TestOIDCCompleteUsernameClaimFallback(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	delete(issuer.claims, "preferred_username")
	issuer.claims["email"] = "alice@example.com"
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, u, err := svc.Complete(context.Background(), "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u.Username != "alice@example.com" {
		t.Fatalf("username = %q, want alice@example.com", u.Username)
	}
}

func TestOIDCCompleteUsernameClaimMissing(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	delete(issuer.claims, "preferred_username")
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "no usable username claim") {
		t.Fatalf("Complete error = %v, want no usable username claim", err)
	}
}

func TestOIDCCompleteGroupsClaimString(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["groups"] = "devs"
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with string groups claim: %v", err)
	}
}

func TestOIDCCompleteDefaultGroupAutoJoin(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	svc, _, gsvc := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
		c.DefaultGroup = "auto-join"
	}))
	ctx := context.Background()

	g, _ := gsvc.Create(ctx, GroupInput{Name: "auto-join", AgentAvailable: true})

	_, _, u, err := svc.Complete(ctx, "code")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	groups, _ := gsvc.GroupsForUser(ctx, u.ID)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("user should auto-join default group, got %v", groups)
	}
}

func TestOIDCCompleteDiscoveryFailure(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))
	svc.providerFactory = func(_ context.Context, _ string) (oidcProvider, error) {
		return nil, errors.New("discovery boom")
	}

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "discover provider") {
		t.Fatalf("Complete error = %v, want discover provider", err)
	}
}

func TestOIDCCompleteTokenExchangeFailure(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.tokenStatus = http.StatusInternalServerError
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "exchange code") {
		t.Fatalf("Complete error = %v, want exchange code", err)
	}
}

func TestOIDCCompleteIDTokenVerificationFailure(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	issuer.signKey = other
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err = svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "verify id token") {
		t.Fatalf("Complete error = %v, want verify id token", err)
	}
}

func TestOIDCCompleteIDTokenExpired(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "verify id token") {
		t.Fatalf("Complete error = %v, want verify id token", err)
	}
}

func TestOIDCCompleteIDTokenWrongAudience(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["aud"] = "some-other-client"
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "verify id token") {
		t.Fatalf("Complete error = %v, want verify id token", err)
	}
}

func TestOIDCCompleteMissingSub(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	delete(issuer.claims, "sub")
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "missing sub") {
		t.Fatalf("Complete error = %v, want missing sub", err)
	}
}

func TestOIDCCompleteMultiAudienceAzpMatch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["aud"] = []any{oidcTestClientID, "other-client"}
	issuer.claims["azp"] = oidcTestClientID
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
		t.Fatalf("Complete with matching azp: %v", err)
	}
}

func TestOIDCCompleteMultiAudienceAzpMismatch(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["aud"] = []any{oidcTestClientID, "other-client"}
	issuer.claims["azp"] = "other-client"
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "azp") {
		t.Fatalf("Complete error = %v, want azp mismatch", err)
	}
}

func TestOIDCCompleteMultiAudienceMissingAzp(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	issuer.claims["aud"] = []any{oidcTestClientID, "other-client"}
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

	_, _, _, err := svc.Complete(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "missing azp") {
		t.Fatalf("Complete error = %v, want missing azp", err)
	}
}

func TestOIDCLoginURL(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

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
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, func(c *domain.OAuthConfig) {
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
	svc, _, _ := newOIDCService(t, oidcCfg("http://localhost:5556/", nil))
	if svc.issuerURL != "http://localhost:5556" {
		t.Fatalf("issuerURL = %q, want trailing slash trimmed", svc.issuerURL)
	}
}

func TestOIDCCompleteUserInfoUsername(t *testing.T) {
	issuer := newFakeOIDCIssuer(t, oidcTestClientID)
	delete(issuer.claims, "preferred_username")
	issuer.userinfo = map[string]any{
		"preferred_username": "bob",
		"email":              "bob@example.com",
	}
	svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

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
		issuer := newFakeOIDCIssuer(t, oidcTestClientID)
		delete(issuer.claims, "groups")
		issuer.userinfo = map[string]any{"groups": []any{"devs"}}
		svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

		if _, _, _, err := svc.Complete(context.Background(), "code"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	})

	t.Run("no intersection", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t, oidcTestClientID)
		delete(issuer.claims, "groups")
		issuer.userinfo = map[string]any{"groups": []any{"other"}}
		svc, _, _ := newOIDCService(t, oidcCfg(issuer.srv.URL, nil))

		_, _, _, err := svc.Complete(context.Background(), "code")
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("Complete error = %v, want ErrForbidden", err)
		}
	})
}
