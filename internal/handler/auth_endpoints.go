package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// oauthErrorRedirectBase is where the OAuth callback sends the browser on any
// failure (the SPA surfaces the error message).
const oauthErrorRedirectBase = "/auth/login"

const oauthStateCookie = "oauth_state"
const oauthStateCookiePath = "/api/v1/auth/oauth"
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
}

type groupSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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

	// Usernames are case-insensitive (COLLATE NOCASE); normalize the limiter
	// key so case variations cannot bypass a lockout.
	key := loginLimitKey(req.Username, c.ClientIP())
	if !s.limiter.allow(key) {
		writeError(c, consts.StatusTooManyRequests, "too many failed login attempts, try again later")
		return
	}

	access, refresh, u, err := s.auth.Login(ctx, req.Username, req.Password)
	if err != nil {
		// Only genuine credential failures count toward lockout; internal
		// errors (DB/JWT) must not lock a user out.
		if errors.Is(err, domain.ErrInvalidCredential) {
			s.limiter.recordFailure(key)
		}
		s.writeServiceError(c, err)
		return
	}
	s.limiter.recordSuccess(key)
	s.setAuthCookies(c, access, refresh)
	groups, _ := s.groups.GroupsForUser(ctx, u.ID)
	c.JSON(consts.StatusOK, toAuthMeResponse(u, groups))
}

// loginLimitKey builds the rate-limiter key for a password login attempt.
func loginLimitKey(username, clientIP string) string {
	return fmt.Sprintf("login|%s|%s", strings.ToLower(username), clientIP)
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
	c.JSON(consts.StatusOK, toAuthMeResponse(u, groups))
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
	key := fmt.Sprintf("chpw|%s|%s", id.UserID, c.ClientIP())
	if !s.limiter.allow(key) {
		writeError(c, consts.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if err := s.users.ChangePassword(ctx, id.UserID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, domain.ErrInvalidCredential) {
			s.limiter.recordFailure(key)
		}
		s.writeServiceError(c, err)
		return
	}
	s.limiter.recordSuccess(key)
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
	nonce, err := newOAuthNonce()
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
	c.SetCookie(oauthStateCookie, nonce, oauthStateCookieMaxAge, oauthStateCookiePath, "", protocol.CookieSameSiteLaxMode, s.oauthCookieSecure || requestIsTLS(c), true)
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
	c.SetCookie(oauthStateCookie, "", -1, oauthStateCookiePath, "", protocol.CookieSameSiteLaxMode, s.oauthCookieSecure || requestIsTLS(c), true)
}

// authCookiePath is the Path attribute shared by both session cookies.
const authCookiePath = "/"

// setAuthCookies sets httpOnly access+refresh cookies. Secure = cfg.secure ||
// requestIsTLS. Max-Age is derived from the JWT TTLs.
func (s *Server) setAuthCookies(c *app.RequestContext, access, refresh string) {
	s.setAuthCookie(c, s.cookieCfg.AccessName, access, int(s.jwt.AccessTTL().Seconds()))
	s.setAuthCookie(c, s.cookieCfg.RefreshName, refresh, int(s.jwt.RefreshTTL().Seconds()))
}

// clearAuthCookies expires both session cookies (Max-Age=-1).
func (s *Server) clearAuthCookies(c *app.RequestContext) {
	s.setAuthCookie(c, s.cookieCfg.AccessName, "", -1)
	s.setAuthCookie(c, s.cookieCfg.RefreshName, "", -1)
}

// setAuthCookie writes one httpOnly, SameSite=Lax session cookie at Path "/".
// Secure is forced by config or auto-detected from the request scheme.
func (s *Server) setAuthCookie(c *app.RequestContext, name, value string, maxAge int) {
	secure := s.cookieCfg.Secure || requestIsTLS(c)
	c.SetCookie(name, value, maxAge, authCookiePath, "", protocol.CookieSameSiteLaxMode, secure, true)
}

// safeRedirectPath validates a post-login SPA redirect target. Only internal
// absolute paths are allowed; anything else falls back to /pipelines. net/url
// parsing rejects scheme/host/userinfo, and encoded forms of "//", backslashes
// and control characters are normalized before the checks (CWE-601). Targets
// under the authentication routes are rejected so a successful login can never
// be redirected back into the auth flow.
func safeRedirectPath(redirect string) string {
	const fallback = "/pipelines"
	if redirect == "" || hasUnsafeChars(redirect) {
		return fallback
	}
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

// newOAuthNonce returns a cryptographically random 16-byte nonce, base64url
// encoded, for binding the OAuth state to a cookie.
func newOAuthNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read oauth nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
