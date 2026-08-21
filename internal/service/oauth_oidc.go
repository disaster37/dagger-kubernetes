package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// oidcDiscoverTimeout bounds the lazy OIDC discovery HTTP calls (the
// /.well-known/openid-configuration and JWKS fetch performed by
// oidc.NewProvider). Without it, a slow or malicious issuer could hang the
// login request indefinitely (the control plane disables its global read
// timeout to allow multi-GB cache uploads, so the goroutine would never be
// reaped) — a DoS vector (CWE-400/CWE-668).
const oidcDiscoverTimeout = 15 * time.Second

// OIDCOAuthService implements the generic OIDC authorization-code login flow
// for provider: "oidc" (covers Dex, Keycloak, Google, Auth0, etc.). Design
// notes:
//
//   - Discovery: the provider endpoint is discovered lazily via the issuer's
//     /.well-known/openid-configuration and cached behind a mutex. Only a
//     successful discovery is cached; a failure is retried on the next request
//     (a transient outage must not permanently disable OIDC). The issuer URL
//     is trailing-slash-normalized before discovery (go-oidc is sensitive to
//     trailing slashes in the discovery document URL).
//   - Audience: the ID token audience is verified against our client_id
//     (oidc.Config{ClientID: clientID}).
//   - Nonce: NOT used. The `state` parameter is a signed HS256 JWT issued by
//     JWTService.IssueOAuthState (10m TTL) and validated in the callback via
//     ParseOAuthState, which binds the callback to the login request
//     (CSRF/login-CSRF defense). go-oidc's Verifier does not require a nonce
//     when audience verification is used without WithNonce.
//   - HTTPS: go-oidc requires HTTPS issuers except for loopback
//     (http://127.0.0.1 / http://localhost). Non-loopback http issuers are
//     rejected at discovery time.
type OIDCOAuthService struct {
	clientID      string
	clientSecret  string
	redirectURL   string
	issuerURL     string // trailing slash trimmed
	scopes        []string
	usernameClaim string
	groupsClaim   string
	allowedOrgs   []string
	allowedGroups []string
	defaultGroup  string
	users         *UserService
	groups        domain.GroupRepository
	jwt           *JWTService
	logger        *logrus.Logger
	mapper        *GroupMapper

	// providerFactory is the testability seam. Production: defaultFactory
	// calling oidc.NewProvider. Tests: inject a fake returning a fake
	// oidcProvider backed by an httptest.Server.
	providerFactory func(ctx context.Context, issuerURL string) (oidcProvider, error)

	mu     sync.Mutex
	cached oidcProvider // nil until the first successful discovery
}

// oidcProvider wraps the subset of *oidc.Provider used by OIDCOAuthService so
// tests can inject a fake backed by an httptest.Server.
type oidcProvider interface {
	Endpoint() oauth2.Endpoint
	Verifier(opts *oidc.Config) *oidc.IDTokenVerifier
	UserInfo(ctx context.Context, ts oauth2.TokenSource) (*oidc.UserInfo, error)
}

// defaultOIDCProviderFactory discovers a real OIDC provider.
func defaultOIDCProviderFactory(ctx context.Context, issuerURL string) (oidcProvider, error) {
	return oidc.NewProvider(ctx, issuerURL)
}

// NewOIDCOAuthService returns an OIDCOAuthService. The issuer URL is
// trailing-slash-normalized; "openid" is appended to scopes when missing; and
// empty claim names fall back to preferred_username/groups.
func NewOIDCOAuthService(cfg *domain.OAuthConfig, mapper *GroupMapper, users *UserService, groups domain.GroupRepository, jwtSvc *JWTService, logger *logrus.Logger) *OIDCOAuthService {
	scopes := make([]string, 0, len(cfg.Scopes)+1)
	scopes = append(scopes, cfg.Scopes...)
	hasOpenID := false
	for _, sc := range scopes {
		if sc == "openid" {
			hasOpenID = true
			break
		}
	}
	if !hasOpenID {
		scopes = append(scopes, "openid")
	}

	usernameClaim := cfg.UsernameClaim
	if usernameClaim == "" {
		usernameClaim = "preferred_username"
	}
	groupsClaim := cfg.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = "groups"
	}

	return &OIDCOAuthService{ //nolint:gosec // G101: OAuth client secret is config-derived, not hardcoded.
		clientID:        cfg.ClientID,
		clientSecret:    cfg.ClientSecret,
		redirectURL:     cfg.RedirectURL,
		issuerURL:       strings.TrimRight(cfg.IssuerURL, "/"),
		scopes:          scopes,
		usernameClaim:   usernameClaim,
		groupsClaim:     groupsClaim,
		allowedOrgs:     cfg.AllowedOrgs,
		allowedGroups:   cfg.AllowedGroups,
		defaultGroup:    cfg.DefaultGroup,
		users:           users,
		groups:          groups,
		jwt:             jwtSvc,
		logger:          logger,
		mapper:          mapper,
		providerFactory: defaultOIDCProviderFactory,
	}
}

// oauth2Config builds an oauth2.Config from the discovered endpoint.
func (s *OIDCOAuthService) oauth2Config(ep oauth2.Endpoint) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		RedirectURL:  s.redirectURL,
		Endpoint:     ep,
		Scopes:       s.scopes,
	}
}

// discover lazily discovers the OIDC provider, caching only a successful
// discovery. A discovery failure is retried on the next request. LoginURL and
// Complete share the same cached provider.
func (s *OIDCOAuthService) discover(ctx context.Context) (oidcProvider, error) {
	s.mu.Lock()
	if s.cached != nil {
		p := s.cached
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()

	p, err := s.providerFactory(ctx, s.issuerURL)
	if err != nil {
		s.logger.WithError(err).Warn("oidc: provider discovery failed")
		return nil, fmt.Errorf("oidc: discover provider: %w", err)
	}
	if p == nil {
		s.logger.Warn("oidc: provider discovery returned a nil provider")
		return nil, fmt.Errorf("oidc: discover provider: nil provider")
	}

	s.mu.Lock()
	if s.cached == nil {
		s.cached = p
	} else {
		p = s.cached
	}
	s.mu.Unlock()
	return p, nil
}

// LoginURL returns the OIDC authorize URL with the given state token. The
// provider is discovered lazily on first call; a discovery failure yields an
// empty URL (the handler surfaces an oauth error). Discovery is bounded by
// oidcDiscoverTimeout so a slow issuer cannot hang the login request.
func (s *OIDCOAuthService) LoginURL(state string) string {
	ctx, cancel := context.WithTimeout(context.Background(), oidcDiscoverTimeout)
	defer cancel()
	p, err := s.discover(ctx)
	if err != nil {
		return ""
	}
	return s.oauth2Config(p.Endpoint()).AuthCodeURL(state)
}

// Complete exchanges the code for tokens, verifies the ID token, extracts the
// sub/username/groups claims, enforces allowed_orgs, ensures a local user,
// optionally auto-joins the default group, and issues a JWT pair.
func (s *OIDCOAuthService) Complete(ctx context.Context, code string) (access, refresh string, u *domain.User, err error) {
	p, err := s.discover(ctx)
	if err != nil {
		return "", "", nil, err
	}

	tok, err := s.oauth2Config(p.Endpoint()).Exchange(ctx, code)
	if err != nil {
		return "", "", nil, fmt.Errorf("oidc: exchange code: %w", err)
	}

	rawIDToken, _ := tok.Extra("id_token").(string)
	if rawIDToken == "" {
		return "", "", nil, fmt.Errorf("oidc: no id_token returned")
	}

	verifier := p.Verifier(&oidc.Config{ClientID: s.clientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", "", nil, fmt.Errorf("oidc: verify id token: %w", err)
	}

	sub := idToken.Subject
	if sub == "" {
		return "", "", nil, fmt.Errorf("oidc: id token missing sub claim")
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return "", "", nil, fmt.Errorf("oidc: decode id token claims: %w", err)
	}

	// go-oidc verifies that our client_id is a member of the ID token
	// `aud` claim, but it explicitly does NOT verify the `azp` (authorized
	// party) claim (see go-oidc verify.go). For multi-audience tokens
	// (`aud` contains more than one client_id), OIDC Core §3.1.3.7
	// requires `azp` to be present and equal to our client_id; otherwise a
	// different client of the same issuer could forward a user's ID token
	// to us and authenticate as that user (token mix-up / CWE-287). Enforce
	// it here, fail-closed.
	if len(idToken.Audience) > 1 {
		azp, _ := claims["azp"].(string)
		if azp == "" {
			return "", "", nil, fmt.Errorf("oidc: multi-audience id token missing azp claim")
		}
		if azp != s.clientID {
			return "", "", nil, fmt.Errorf("oidc: id token azp %q does not match client_id", azp)
		}
	}

	// Some providers (Keycloak, Dex) only expose the username/groups claims on
	// the userinfo endpoint, not in the ID token. When any claim we rely on is
	// absent from the ID token, fetch userinfo and merge the missing keys in
	// (ID-token claims take precedence — they are signed). Userinfo failures
	// are non-fatal: login still fails closed later only if the username
	// remains unresolvable.
	if claimMissing(claims, s.usernameClaim) || claimMissing(claims, "email") || claimMissing(claims, s.groupsClaim) {
		s.mergeUserInfo(ctx, p, tok, claims)
	}

	username, err := s.resolveUsername(claims)
	if err != nil {
		return "", "", nil, err
	}
	groups := s.resolveGroups(claims)

	if allowlist := s.effectiveAllowedGroups(); len(allowlist) > 0 && !orgsIntersect(allowlist, groups) {
		return "", "", nil, domain.ErrForbidden
	}

	mappedGroups := s.mapper.mapIfActive(groups)

	access, refresh, u, err = completeOAuthUser(ctx, s.users, s.groups, s.jwt, s.logger, "oidc", sub, username, s.defaultGroup, mappedGroups)
	if err != nil {
		return "", "", nil, fmt.Errorf("oidc oauth: %w", err)
	}
	return access, refresh, u, nil
}

// effectiveAllowedGroups returns allowed_groups ∪ allowed_orgs (allowed_orgs is
// the deprecated OIDC alias), de-duplicated preserving first occurrence.
func (s *OIDCOAuthService) effectiveAllowedGroups() []string {
	out := make([]string, 0, len(s.allowedGroups)+len(s.allowedOrgs))
	seen := make(map[string]struct{}, len(s.allowedGroups)+len(s.allowedOrgs))
	for _, list := range [][]string{s.allowedGroups, s.allowedOrgs} {
		for _, g := range list {
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			out = append(out, g)
		}
	}
	return out
}

// mergeUserInfo fetches the userinfo endpoint and merges any claim keys that
// are absent from the ID-token claims. Failures are logged and swallowed (the
// caller continues with the ID-token claims only).
func (s *OIDCOAuthService) mergeUserInfo(ctx context.Context, p oidcProvider, tok *oauth2.Token, claims map[string]any) {
	ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		s.logger.WithError(err).Warn("oidc: userinfo fetch failed, using id token claims only")
		return
	}
	var userInfoClaims map[string]any
	if err := ui.Claims(&userInfoClaims); err != nil {
		s.logger.WithError(err).Warn("oidc: userinfo claims decode failed, using id token claims only")
		return
	}
	for k, v := range userInfoClaims {
		if _, ok := claims[k]; !ok {
			claims[k] = v
		}
	}
}

// resolveUsername returns the username from the configured claim, falling back
// to the email claim when the configured claim is absent or empty.
func (s *OIDCOAuthService) resolveUsername(claims map[string]any) (string, error) {
	if name := claimString(claims, s.usernameClaim); name != "" {
		return name, nil
	}
	if email := claimString(claims, "email"); email != "" {
		return email, nil
	}
	return "", fmt.Errorf("oidc: no usable username claim (tried %q and %q)", s.usernameClaim, "email")
}

// resolveGroups normalizes the groups claim: []string/[]any (each stringified)
// or a single string (one-element list). An absent claim yields nil.
func (s *OIDCOAuthService) resolveGroups(claims map[string]any) []string {
	raw, ok := claims[s.groupsClaim]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			out = append(out, fmt.Sprintf("%v", e))
		}
		return out
	default:
		return nil
	}
}

// claimString returns the string value of the named claim, or "" when absent
// or not a string.
func claimString(claims map[string]any, key string) string {
	v, ok := claims[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// claimMissing reports whether the named claim key is absent from the claims.
func claimMissing(claims map[string]any, key string) bool {
	_, ok := claims[key]
	return !ok
}

var _ OAuthProvider = (*OIDCOAuthService)(nil)
