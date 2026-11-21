package handler

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/disaster37/go-auth/httpcookie"
	goauthoauth "github.com/disaster37/go-auth/oauth"
	"github.com/disaster37/go-secguard/lockout"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// oauthErrorRedirectBase is where the OAuth callback sends the browser on any
// failure (the SPA surfaces the error message).
const oauthErrorRedirectBase = "/auth/login"

const oauthStateCookie = "oauth_state"
const oauthStateCookieMaxAge = 600 // seconds; matches the 10m oauth state TTL

// allowedOAuthErrorCodes is the fixed whitelist of error hints the browser may
// receive. Anything else is collapsed to "oauth" so IdP-supplied
// error_description values can never reach the user.
var allowedOAuthErrorCodes = map[string]struct{}{
	"oauth":            {},
	"group_required":   {},
	"account_disabled": {},
	"auth_conflict":    {},
}

// failOAuth logs a safe, structured cause and redirects the browser to the
// fixed login error screen. It never logs tokens, codes, state, cookies,
// ciphertext, full URLs or IdP response bodies.
func (s *Server) failOAuth(c *app.RequestContext, code, stage string, err error) {
	if _, ok := allowedOAuthErrorCodes[code]; !ok {
		code = "oauth"
	}
	c.Response.Header.Set("Cache-Control", "no-store")
	fields := logrus.Fields{
		"provider":   oauthProviderFromPath(string(c.Path())),
		"stage":      stage,
		"error_code": code,
		"status":     consts.StatusFound,
	}
	if err != nil {
		fields["cause"] = sanitizeOAuthCause(err)
	}
	s.logger.WithFields(fields).Warn("oauth: authentication failed")
	redirectOAuthErrorCode(c, code)
}

// oauthProviderFromPath extracts the provider slug from an OAuth request path.
func oauthProviderFromPath(path string) string {
	switch {
	case strings.Contains(path, "/oauth/github/"):
		return "github"
	case strings.Contains(path, "/oauth/oidc/"):
		return "oidc"
	default:
		return ""
	}
}

// sanitizeOAuthCause returns a short, non-sensitive category for logging. It
// never returns the raw error (which may embed an IdP response body or token).
func sanitizeOAuthCause(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrForbidden):
		return "forbidden"
	case errors.Is(err, domain.ErrAccountBlocked):
		return "account blocked"
	case errors.Is(err, domain.ErrStaleUser):
		return "stale account write"
	case errors.Is(err, domain.ErrLastAdmin):
		return "last administrator"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrNotLeader):
		return "not leader"
	case errors.Is(err, domain.ErrRaftTimeout):
		return "raft timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "request canceled"
	default:
		return "oauth failure"
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type authMeResponse struct {
	ID            string         `json:"id"`
	Username      string         `json:"username"`
	Role          domain.Role    `json:"role"`
	OAuthProvider string         `json:"oauth_provider,omitempty"`
	OAuthGroups   []string       `json:"oauth_groups,omitempty"`
	Groups        []groupSummary `json:"groups"`
	Orgs          []orgSummary   `json:"orgs"`
}

type groupSummary struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id,omitempty"`
	Name  string `json:"name"`
}

type orgSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// orgSummaries lists the organizations visible to (userID, access) with the
// caller's role in each, sorted by slug.
func (s *Server) orgSummaries(ctx context.Context, userID string, access domain.Role) []orgSummary {
	if s.orgs == nil {
		return []orgSummary{}
	}
	var orgs []*domain.Org
	if access == domain.RoleAdmin {
		all, err := s.orgs.List(ctx)
		if err != nil {
			return []orgSummary{}
		}
		orgs = all
	} else {
		list, err := s.orgs.OrgsForUser(ctx, userID)
		if err != nil {
			return []orgSummary{}
		}
		orgs = list
	}
	out := make([]orgSummary, 0, len(orgs))
	for _, o := range orgs {
		role := "member"
		if r, ok, err := s.orgs.MemberRole(ctx, o.ID, userID); err == nil && ok {
			role = string(r)
		} else if access == domain.RoleAdmin {
			role = "admin"
		}
		out = append(out, orgSummary{ID: o.ID, Name: o.Name, DisplayName: o.DisplayName, Role: role})
	}
	return out
}

// authMe builds the /me response including org memberships.
func (s *Server) authMe(ctx context.Context, u *domain.User, groups []*domain.Group) authMeResponse {
	resp := toAuthMeResponse(u, groups)
	resp.Orgs = s.orgSummaries(ctx, u.ID, u.Role)
	return resp
}

type providersResponse struct {
	Internal    bool `json:"internal"`
	OAuthGitHub bool `json:"oauth_github"`
	OAuthOIDC   bool `json:"oauth_oidc"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleLogin authenticates a user and issues a JWT pair in httpOnly cookies.
// Password attempts are rate-limited per username + client IP (CWE-307).
func (s *Server) handleLogin(ctx context.Context, c *app.RequestContext) {
	if !s.internalAuthEnabled {
		writeError(c, consts.StatusNotFound, "internal auth disabled")
		return
	}
	var req loginRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(c, consts.StatusBadRequest, "username and password are required")
		return
	}

	// Usernames are case-insensitive (COLLATE NOCASE); the lockout key hashes
	// the normalized username so case variations cannot bypass a lockout.
	ip := s.clientIP(c)
	key := lockout.LoginLimitKey(req.Username, ip)
	if s.guard != nil {
		if s.guard.Config.Lockout.Enabled && !s.guard.Lockout.Allow(key) {
			writeError(c, consts.StatusTooManyRequests, "too many failed login attempts, try again later")
			return
		}
		if s.guard.Config.RateLimit.Enabled && !s.guard.LoginThrottle.Allow(key, s.guard.Config.RateLimit.LoginThrottleAttempts, s.guard.Config.RateLimit.LoginThrottleWindow) {
			writeError(c, consts.StatusTooManyRequests, "too many login attempts, try again later")
			return
		}
	}

	access, refresh, u, err := s.auth.Login(ctx, req.Username, req.Password)
	if err != nil {
		// Only genuine credential failures count toward lockout; internal
		// errors (DB/JWT) must not lock a user out.
		if errors.Is(err, domain.ErrInvalidCredential) {
			s.recordLoginFailure(req.Username, ip)
		}
		s.writeServiceError(c, err)
		return
	}
	if s.guard != nil {
		if s.guard.Config.Lockout.Enabled {
			s.guard.Lockout.RecordSuccess(key)
		}
		s.guard.LoginThrottle.Clear(key)
	}
	s.setAuthCookies(c, access, refresh)
	groups, _ := s.groups.GroupsForUser(ctx, u.ID)
	c.JSON(consts.StatusOK, s.authMe(ctx, u, groups))
}

// handleRefresh rotates a refresh token (from the refresh cookie first, then
// the JSON body for backwards compat) into a new pair and sets fresh cookies.
func (s *Server) handleRefresh(ctx context.Context, c *app.RequestContext) {
	refresh := string(c.Cookie(s.cookieCfg.RefreshName))
	if refresh == "" {
		var req refreshRequest
		if !decodeBody(c, &req) {
			return
		}
		refresh = req.RefreshToken
	}
	if refresh == "" {
		writeError(c, consts.StatusBadRequest, "refresh_token is required")
		return
	}
	access, refreshed, err := s.auth.Refresh(ctx, refresh)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	s.setAuthCookies(c, access, refreshed)
	c.SetStatusCode(consts.StatusNoContent)
}

// handleLogout clears the session cookies and returns 204. When the request
// still carries a resolvable session JWT it also bumps the user's
// token_version (best-effort, ADR-045) so every JWT issued before this logout
// stops resolving. Failure only logs a WARN — logout always succeeds because
// the cookies are cleared either way. Legacy flat-file identities have no user
// row to bump.
func (s *Server) handleLogout(ctx context.Context, c *app.RequestContext) {
	s.clearAuthCookies(c)
	if id, err := s.auth.Resolve(ctx, s.bearerFromRequest(c)); err == nil && id.Method == domain.AuthJWT {
		if verr := s.users.IncrementTokenVersion(ctx, id.UserID); verr != nil {
			s.logger.WithError(verr).WithField("user_id", id.UserID).Warn("logout: token version bump failed; outstanding sessions may remain valid")
		}
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleMe returns the current user's profile + groups.
func (s *Server) handleMe(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	// Synthetic identities (legacy flat-file) have no users-table row; answer
	// from the identity itself.
	if id.Method == domain.AuthLegacyTok {
		c.JSON(consts.StatusOK, syntheticUserResponse())
		return
	}
	u, err := s.users.Get(ctx, id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	groups, _ := s.groups.GroupsForUser(ctx, id.UserID)
	c.JSON(consts.StatusOK, s.authMe(ctx, u, groups))
}

// syntheticUserResponse builds the /me + login user object for the synthetic
// legacy-token identity that has no users-table row.
func syntheticUserResponse() authMeResponse {
	name := "legacy"
	return authMeResponse{
		ID:       name,
		Username: name,
		Role:     domain.RoleAdmin,
		Groups:   []groupSummary{},
		Orgs:     []orgSummary{},
	}
}

// handleProviders reports which auth providers are enabled.
func (s *Server) handleProviders(_ context.Context, c *app.RequestContext) {
	c.JSON(consts.StatusOK, providersResponse{
		Internal:    s.internalAuthEnabled,
		OAuthGitHub: s.oauthEnabled("github"),
		OAuthOIDC:   s.oauthEnabled("oidc"),
	})
}

// oauthEnabled reports whether provider is the active OAuth provider.
func (s *Server) oauthEnabled(provider string) bool {
	return s.oauth != nil && s.oauthProvider == provider
}

// requireOAuthProvider returns false and sends the browser to the fixed OAuth
// error screen when provider is not the active OAuth provider. Browser OAuth
// endpoints must never render a raw 404 body or bounce the user back into the
// authorize endpoint.
func (s *Server) requireOAuthProvider(c *app.RequestContext, provider string) bool {
	if s.oauthEnabled(provider) {
		return true
	}
	s.failOAuth(c, "oauth", "provider_unavailable", nil)
	return false
}

// handleChangePassword verifies the current password and sets a new one.
// Current-password verification is rate-limited per user + client IP (CWE-307).
func (s *Server) handleChangePassword(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	// Synthetic identities (legacy flat-file) have no users-table row to
	// change a password on.
	if id.Method == domain.AuthLegacyTok {
		writeError(c, consts.StatusBadRequest, "password changes require a real user account")
		return
	}
	var req changePasswordRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeError(c, consts.StatusBadRequest, "current_password and new_password are required")
		return
	}
	key := lockout.PasswordChangeLimitKey(id.UserID, s.clientIP(c))
	if s.guard != nil && s.guard.Config.Lockout.Enabled && !s.guard.Lockout.Allow(key) {
		writeError(c, consts.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if err := s.users.ChangePassword(ctx, id.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, domain.ErrInvalidCredential) && s.guard != nil && s.guard.Config.Lockout.Enabled {
			s.guard.Lockout.RecordFailure(key)
		}
		s.writeServiceError(c, err)
		return
	}
	if s.guard != nil && s.guard.Config.Lockout.Enabled {
		s.guard.Lockout.RecordSuccess(key)
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// handleOAuthLogin redirects to the GitHub authorize URL with a state token
// encoding the redirect path (for post-login navigation).
func (s *Server) handleOAuthLogin(_ context.Context, c *app.RequestContext) {
	if !s.requireOAuthProvider(c, "github") {
		return
	}
	s.startOAuthLogin(c)
}

// handleOAuthOIDCLogin redirects to the OIDC authorize URL with a state token
// encoding the redirect path (for post-login navigation).
func (s *Server) handleOAuthOIDCLogin(_ context.Context, c *app.RequestContext) {
	if !s.requireOAuthProvider(c, "oidc") {
		return
	}
	s.startOAuthLogin(c)
}

// startOAuthLogin is the shared OAuth login flow (nonce cookie + state
// issuance + redirect).
func (s *Server) startOAuthLogin(c *app.RequestContext) {
	redirect := safeRedirectPath(c.Query("redirect"))

	// Bind the state token to a cookie nonce so the callback can only be
	// completed by the browser that initiated the login (login-CSRF, CWE-352).
	// The cookie is written only after the state and authorize URL are ready so
	// a failed start does not leave a half-initialized flow behind.
	nonce, err := goauthoauth.GenerateNonce()
	if err != nil {
		s.failOAuth(c, "oauth", "nonce", err)
		return
	}
	state, err := s.jwt.IssueOAuthState(redirect, nonce)
	if err != nil {
		s.failOAuth(c, "oauth", "state", err)
		return
	}
	loginURL := s.oauth.LoginURL(state)
	if loginURL == "" {
		s.failOAuth(c, "oauth", "discovery", nil)
		return
	}
	stateSpec := httpcookie.IssueOAuthStateSpec(s.oauthCookieConfig(), requestIsTLS(c), nonce, oauthStateCookieMaxAge)
	s.writeCookieSpec(c, &stateSpec)
	c.Redirect(consts.StatusFound, []byte(loginURL))
}

// handleOAuthCallback exchanges the GitHub code for tokens, sets the session
// cookies, and redirects to the SPA.
func (s *Server) handleOAuthCallback(ctx context.Context, c *app.RequestContext) {
	if !s.requireOAuthProvider(c, "github") {
		return
	}
	s.completeOAuthCallback(ctx, c)
}

// handleOAuthOIDCCallback exchanges the OIDC code for tokens, sets the session
// cookies, and redirects to the SPA.
func (s *Server) handleOAuthOIDCCallback(ctx context.Context, c *app.RequestContext) {
	if !s.requireOAuthProvider(c, "oidc") {
		return
	}
	s.completeOAuthCallback(ctx, c)
}

// completeOAuthCallback is the shared OAuth callback flow (nonce verification,
// state validation, code exchange, cookie issuance, query redirect).
func (s *Server) completeOAuthCallback(ctx context.Context, c *app.RequestContext) {
	code := c.Query("code")
	state := c.Query("state")
	providerErr := c.Query("error")

	// The state token must be accompanied by the nonce cookie set at login
	// time; compare in constant time (login-CSRF, CWE-352). Only a fully
	// matching flow consumes the nonce cookie: a missing/expired/invalid state
	// or nonce mismatch MUST NOT erase another concurrently running flow's
	// cookie.
	cookieVal := c.Cookie(oauthStateCookie)
	stateClaims, err := s.jwt.ParseOAuthState(state)
	if state == "" || err != nil || len(cookieVal) == 0 {
		s.failOAuth(c, "oauth", "state_validation", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(stateClaims.Nonce), cookieVal) != 1 {
		s.failOAuth(c, "oauth", "nonce_mismatch", nil)
		return
	}
	// Matching flow: consume the nonce before any external call.
	s.clearOAuthStateCookie(c)

	// A canceled/denied IdP callback passed validation: show the generic
	// failure without exchanging anything. The IdP error_description is never
	// surfaced.
	if providerErr != "" || code == "" {
		s.failOAuth(c, "oauth", "provider_error", nil)
		return
	}

	// Exchange the code. Any failure lands the browser back on the login screen
	// with a fixed, whitelisted error hint.
	access, refresh, _, err := s.oauth.Complete(ctx, code)
	if err != nil {
		s.failOAuth(c, oauthCallbackErrorCode(err), "complete", err)
		return
	}

	s.setAuthCookies(c, access, refresh)
	// Re-validate the stored redirect at callback time (defense in depth).
	redirectPath := safeRedirectPath(stateClaims.Username)
	// Hand tokens to the SPA via httpOnly cookies (no URL fragment); the
	// redirect path is percent-encoded so it cannot corrupt the query.
	target := fmt.Sprintf("/auth/callback?redirect=%s", url.QueryEscape(redirectPath))
	c.Redirect(consts.StatusFound, []byte(target))
}

// oauthCallbackErrorCode maps an OAuth completion error to a whitelisted hint.
func oauthCallbackErrorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrForbidden):
		return "group_required"
	case errors.Is(err, domain.ErrAccountBlocked):
		return "account_disabled"
	case errors.Is(err, domain.ErrStaleUser), errors.Is(err, domain.ErrLastAdmin), errors.Is(err, domain.ErrConflict):
		return "auth_conflict"
	default:
		return "oauth"
	}
}

// clearOAuthStateCookie removes the login nonce cookie (best-effort).
func (s *Server) clearOAuthStateCookie(c *app.RequestContext) {
	clearSpec := httpcookie.ClearOAuthStateSpec(s.oauthCookieConfig(), requestIsTLS(c))
	s.writeCookieSpec(c, &clearSpec)
}

// cookieConfig builds the go-auth cookie configuration from the supervisor's
// cookie settings.
func (s *Server) cookieConfig() httpcookie.Config {
	cfg := httpcookie.DefaultConfig()
	cfg.AccessName = s.cookieCfg.AccessName
	cfg.RefreshName = s.cookieCfg.RefreshName
	cfg.Secure = s.cookieCfg.Secure
	return cfg
}

// oauthCookieConfig is the cookie configuration for the OAuth state cookie,
// whose Secure flag is controlled by auth.oauth.cookie_secure.
func (s *Server) oauthCookieConfig() httpcookie.Config {
	cfg := s.cookieConfig()
	cfg.Secure = s.oauthCookieSecure
	return cfg
}

// writeCookieSpec writes a go-auth cookie spec to the Hertz response.
func (s *Server) writeCookieSpec(c *app.RequestContext, spec *httpcookie.Spec) {
	c.SetCookie(spec.Name, spec.Value, spec.MaxAge, spec.Path, spec.Domain, protocol.CookieSameSite(spec.SameSite), spec.Secure, spec.HttpOnly)
}

// setAuthCookies sets httpOnly access+refresh cookies. Secure = cfg.secure ||
// requestIsTLS. Max-Age is derived from the JWT TTLs.
func (s *Server) setAuthCookies(c *app.RequestContext, access, refresh string) {
	accessSpec, refreshSpec := httpcookie.IssuePairSpecs(
		s.cookieConfig(), requestIsTLS(c), access, refresh,
		int(s.jwt.AccessTTL().Seconds()), int(s.jwt.RefreshTTL().Seconds()),
	)
	s.writeCookieSpec(c, &accessSpec)
	s.writeCookieSpec(c, &refreshSpec)
}

// clearAuthCookies expires both session cookies (Max-Age=-1).
func (s *Server) clearAuthCookies(c *app.RequestContext) {
	accessSpec, refreshSpec := httpcookie.ClearPairSpecs(s.cookieConfig(), requestIsTLS(c))
	s.writeCookieSpec(c, &accessSpec)
	s.writeCookieSpec(c, &refreshSpec)
}

// safeRedirectPath validates a post-login SPA redirect target. Only internal
// absolute paths are allowed; anything else falls back to /pipelines. net/url
// parsing rejects scheme/host/userinfo, and encoded forms of "//", backslashes
// and control characters are normalized before the checks (CWE-601). Targets
// under the authentication routes are rejected so a successful login can never
// be redirected back into the auth flow.
func safeRedirectPath(redirect string) string {
	const fallback = "/pipelines"
	// Base sanitization via go-auth (rejects empty, non-"/", "//", "/\",
	// backslash, CR/LF, and control characters).
	if httpcookie.SafeRedirectPath(redirect, fallback) == fallback {
		return fallback
	}
	// net/url parsing additionally normalizes encoded forms of "//" and
	// backslashes before the checks (CWE-601).
	u, err := url.Parse(redirect)
	if err != nil {
		return fallback
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return fallback
	}
	p := u.Path
	if !isSafeLocalPath(p) || isAuthRedirectPath(p) {
		return fallback
	}
	if u.RawQuery == "" {
		return p
	}
	return fmt.Sprintf("%s?%s", p, u.RawQuery)
}

// hasUnsafeChars reports whether s contains a control character or backslash.
func hasUnsafeChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return true
		}
	}
	return false
}

// isSafeLocalPath reports whether p is an internal absolute path (rejecting
// protocol-relative and backslash forms).
func isSafeLocalPath(p string) bool {
	return p != "" && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\") && !hasUnsafeChars(p)
}

// isAuthRedirectPath reports whether p targets an authentication route, which
// must never be used as a post-login destination.
func isAuthRedirectPath(p string) bool {
	return p == "/auth" || strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/api/v1/auth/")
}

// redirectOAuthErrorCode sends the browser to the SPA login screen with the
// given (whitelisted) error hint and disables caching of the redirect.
func redirectOAuthErrorCode(c *app.RequestContext, code string) {
	if _, ok := allowedOAuthErrorCodes[code]; !ok {
		code = "oauth"
	}
	c.Redirect(consts.StatusFound, []byte(fmt.Sprintf("%s?error=%s", oauthErrorRedirectBase, code)))
}

// requestIsTLS reports whether the request arrived over TLS. hertz does not
// expose an IsTLS getter; the URI scheme is set to "https" by the TLS server
// via Request.SetIsTLS.
func requestIsTLS(c *app.RequestContext) bool {
	return string(c.Request.Scheme()) == "https"
}

// toAuthMeResponse builds the user-facing me/login user object.
func toAuthMeResponse(u *domain.User, groups []*domain.Group) authMeResponse {
	return authMeResponse{
		ID:            u.ID,
		Username:      u.Username,
		Role:          u.Role,
		OAuthProvider: u.OAuthProvider,
		OAuthGroups:   u.OAuthGroups,
		Groups:        toGroupSummaries(groups),
	}
}
