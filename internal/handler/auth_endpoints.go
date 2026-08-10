package handler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// oauthErrorRedirect is where the OAuth callback sends the browser on any
// failure (the SPA surfaces the error message).
const oauthErrorRedirect = "/auth/login?error=oauth"

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	User         authMeResponse `json:"user"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
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
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleLogin authenticates a user and issues a JWT pair. Password attempts
// are rate-limited per username + client IP (CWE-307).
func (s *Server) handleLogin(_ context.Context, c *app.RequestContext) {
	var req loginRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(c, consts.StatusBadRequest, "username and password are required")
		return
	}

	// Auth disabled (dev mode, D9): every request resolves to the anonymous
	// admin identity, so login accepts anything and hands back placeholder
	// tokens (Resolve ignores them). This keeps the UI flow working without
	// a users table entry for "anonymous".
	if s.authDisabled {
		c.JSON(consts.StatusOK, loginResponse{
			AccessToken:  "anonymous",
			RefreshToken: "anonymous",
			User:         syntheticUserResponse(domain.AuthNone),
		})
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
	groups, _ := s.groups.GroupsForUser(context.Background(), u.ID)
	c.JSON(consts.StatusOK, loginResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         toAuthMeResponse(u, groups),
	})
}

// loginLimitKey builds the rate-limiter key for a password login attempt.
func loginLimitKey(username, clientIP string) string {
	return fmt.Sprintf("login|%s|%s", strings.ToLower(username), clientIP)
}

// handleRefresh rotates a refresh token into a new pair.
func (s *Server) handleRefresh(_ context.Context, c *app.RequestContext) {
	var req refreshRequest
	if !decodeBody(c, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeError(c, consts.StatusBadRequest, "refresh_token is required")
		return
	}
	// Auth-disabled parity (D9): placeholder tokens are never validated.
	if s.authDisabled {
		c.JSON(consts.StatusOK, refreshResponse{AccessToken: "anonymous", RefreshToken: "anonymous"})
		return
	}
	access, refresh, err := s.auth.Refresh(context.Background(), req.RefreshToken)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.JSON(consts.StatusOK, refreshResponse{AccessToken: access, RefreshToken: refresh})
}

// handleMe returns the current user's profile + groups.
func (s *Server) handleMe(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}
	// Synthetic identities (auth-disabled anonymous, legacy flat-file) have no
	// users-table row; answer from the identity itself.
	if id.Method == domain.AuthNone || id.Method == domain.AuthLegacyTok {
		c.JSON(consts.StatusOK, syntheticUserResponse(id.Method))
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

// syntheticUserResponse builds the /me + login user object for synthetic
// identities that have no users-table row (anonymous dev mode, legacy token).
func syntheticUserResponse(method domain.AuthMethod) authMeResponse {
	name := "legacy"
	if method == domain.AuthNone {
		name = "anonymous"
	}
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
		Internal:    !s.authDisabled,
		OAuthGitHub: s.oauth != nil,
	})
}

// handleChangePassword verifies the current password and sets a new one.
// Current-password verification is rate-limited per user + client IP (CWE-307).
func (s *Server) handleChangePassword(_ context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}
	// Synthetic identities (auth-disabled anonymous, legacy flat-file) have no
	// users-table row to change a password on.
	if id.Method == domain.AuthNone || id.Method == domain.AuthLegacyTok {
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
	if s.oauth == nil {
		writeError(c, consts.StatusNotFound, "oauth not enabled")
		return
	}
	redirect := safeRedirectPath(string(c.Query("redirect")))
	state, err := s.jwt.IssueOAuthState(redirect)
	if err != nil {
		writeError(c, consts.StatusInternalServerError, "oauth state error")
		return
	}
	c.Redirect(consts.StatusFound, []byte(s.oauth.LoginURL(state)))
}

// handleOAuthCallback exchanges the GitHub code for tokens and redirects to
// the SPA with the JWT pair in the URL fragment (never logged).
func (s *Server) handleOAuthCallback(_ context.Context, c *app.RequestContext) {
	if s.oauth == nil {
		writeError(c, consts.StatusNotFound, "oauth not enabled")
		return
	}
	code := string(c.Query("code"))
	state := string(c.Query("state"))

	// Validate the state token, then exchange the code. Any failure lands the
	// browser back on the login screen with an error hint.
	stateClaims, err := s.jwt.ParseOAuthState(state)
	if code == "" || state == "" || err != nil {
		redirectOAuthError(c)
		return
	}
	access, refresh, _, err := s.oauth.Complete(context.Background(), code)
	if err != nil {
		redirectOAuthError(c)
		return
	}

	redirectPath := safeRedirectPath(stateClaims.Username)
	// Hand tokens to the SPA via the fragment (not logged by proxies). The
	// redirect path is percent-encoded so it cannot corrupt the fragment.
	fragment := fmt.Sprintf("/auth/callback#access_token=%s&refresh_token=%s&redirect=%s", access, refresh, url.QueryEscape(redirectPath))
	c.Redirect(consts.StatusFound, []byte(fragment))
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

func redirectOAuthError(c *app.RequestContext) {
	c.Redirect(consts.StatusFound, []byte(oauthErrorRedirect))
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
