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

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// oauthErrorRedirectBase is where the OAuth callback sends the browser on any
// failure (the SPA surfaces the error message).
const oauthErrorRedirectBase = "/auth/login"

const oauthStateCookie = "oauth_state"
const oauthStateCookiePath = "/api/v1/auth/oauth"
const oauthStateCookieMaxAge = 600 // seconds; matches the 10m oauth state TTL

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
func (s *Server) handleLogin(_ context.Context, c *app.RequestContext) {
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

	access, refresh, u, err := s.auth.Login(context.Background(), req.Username, req.Password)
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
	groups, _ := s.groups.GroupsForUser(context.Background(), u.ID)
	c.JSON(consts.StatusOK, toAuthMeResponse(u, groups))
}

// loginLimitKey builds the rate-limiter key for a password login attempt.
func loginLimitKey(username, clientIP string) string {
	return fmt.Sprintf("login|%s|%s", strings.ToLower(username), clientIP)
}

// handleRefresh rotates a refresh token (from the refresh cookie first, then
// the JSON body for backwards compat) into a new pair and sets fresh cookies.
func (s *Server) handleRefresh(_ context.Context, c *app.RequestContext) {
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
	access, refreshed, err := s.auth.Refresh(context.Background(), refresh)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	s.setAuthCookies(c, access, refreshed)
	c.SetStatusCode(consts.StatusNoContent)
}

// handleLogout clears the session cookies and returns 204.
func (s *Server) handleLogout(_ context.Context, c *app.RequestContext) {
	s.clearAuthCookies(c)
	c.SetStatusCode(consts.StatusNoContent)
}

// handleMe returns the current user's profile + groups.
func (s *Server) handleMe(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}
	// Synthetic identities (legacy flat-file) have no users-table row; answer
	// from the identity itself.
	if id.Method == domain.AuthLegacyTok {
		c.JSON(consts.StatusOK, syntheticUserResponse())
		return
	}
	u, err := s.users.Get(context.Background(), id.UserID)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	groups, _ := s.groups.GroupsForUser(context.Background(), id.UserID)
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

// requireOAuthProvider writes a 404 and returns false when provider is not the
// active OAuth provider (mirrors the requireAuth bool convention).
func (s *Server) requireOAuthProvider(c *app.RequestContext, provider string) bool {
	if s.oauthEnabled(provider) {
		return true
	}
	writeError(c, consts.StatusNotFound, "oauth not enabled")
	return false
}

// handleChangePassword verifies the current password and sets a new one.
// Current-password verification is rate-limited per user + client IP (CWE-307).
func (s *Server) handleChangePassword(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
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
	if err := s.users.ChangePassword(context.Background(), id.UserID, req.CurrentPassword, req.NewPassword); err != nil {
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
	nonce, err := newOAuthNonce()
	if err != nil {
		writeError(c, consts.StatusInternalServerError, "oauth state error")
		return
	}
	c.SetCookie(oauthStateCookie, nonce, oauthStateCookieMaxAge, oauthStateCookiePath, "", protocol.CookieSameSiteLaxMode, s.oauthCookieSecure || requestIsTLS(c), true)

	state, err := s.jwt.IssueOAuthState(redirect, nonce)
	if err != nil {
		writeError(c, consts.StatusInternalServerError, "oauth state error")
		return
	}
	loginURL := s.oauth.LoginURL(state)
	if loginURL == "" {
		redirectOAuthErrorCode(c, "oauth")
		return
	}
	c.Redirect(consts.StatusFound, []byte(loginURL))
}

// handleOAuthCallback exchanges the GitHub code for tokens, sets the session
// cookies, and redirects to the SPA.
func (s *Server) handleOAuthCallback(_ context.Context, c *app.RequestContext) {
	if !s.requireOAuthProvider(c, "github") {
		return
	}
	s.completeOAuthCallback(c)
}

// handleOAuthOIDCCallback exchanges the OIDC code for tokens, sets the session
// cookies, and redirects to the SPA.
func (s *Server) handleOAuthOIDCCallback(_ context.Context, c *app.RequestContext) {
	if !s.requireOAuthProvider(c, "oidc") {
		return
	}
	s.completeOAuthCallback(c)
}

// completeOAuthCallback is the shared OAuth callback flow (nonce verification,
// state validation, code exchange, cookie issuance, query redirect).
func (s *Server) completeOAuthCallback(c *app.RequestContext) {
	code := c.Query("code")
	state := c.Query("state")

	// The state token must be accompanied by the nonce cookie set at login
	// time; compare in constant time (login-CSRF, CWE-352).
	cookieVal := c.Cookie(oauthStateCookie)
	stateClaims, err := s.jwt.ParseOAuthState(state)
	if code == "" || state == "" || err != nil || len(cookieVal) == 0 {
		s.clearOAuthStateCookie(c)
		redirectOAuthErrorCode(c, "oauth")
		return
	}
	if subtle.ConstantTimeCompare([]byte(stateClaims.Nonce), cookieVal) != 1 {
		s.clearOAuthStateCookie(c)
		redirectOAuthErrorCode(c, "oauth")
		return
	}
	s.clearOAuthStateCookie(c)

	// Validate the state token, then exchange the code. Any failure lands the
	// browser back on the login screen with an error hint.
	access, refresh, _, err := s.oauth.Complete(context.Background(), code)
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			redirectOAuthErrorCode(c, "forbidden")
			return
		}
		redirectOAuthErrorCode(c, "oauth")
		return
	}

	s.setAuthCookies(c, access, refresh)
	redirectPath := safeRedirectPath(stateClaims.Username)
	// Hand tokens to the SPA via httpOnly cookies (no URL fragment); the
	// redirect path is percent-encoded so it cannot corrupt the query.
	target := fmt.Sprintf("/auth/callback?redirect=%s", url.QueryEscape(redirectPath))
	c.Redirect(consts.StatusFound, []byte(target))
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
// absolute paths are allowed; anything else falls back to /pipelines. This
// prevents open redirects via the OAuth flow (CWE-601): "//host" is rejected
// (protocol-relative URL) and so are backslashes, which some browsers
// historically normalized into slashes.
func safeRedirectPath(redirect string) string {
	if redirect == "" || !strings.HasPrefix(redirect, "/") ||
		strings.HasPrefix(redirect, "//") || strings.Contains(redirect, "\\") {
		return "/pipelines"
	}
	return redirect
}

// redirectOAuthErrorCode sends the browser to the SPA login screen with the
// given error hint.
func redirectOAuthErrorCode(c *app.RequestContext, code string) {
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
		Groups:        toGroupSummaries(groups),
	}
}
