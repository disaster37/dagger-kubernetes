package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// handleDeviceCode implements POST /oauth/device/code (RFC 8628 §3.1).
func (s *Server) handleDeviceCode(ctx context.Context, c *app.RequestContext) {
	if s.deviceAuth == nil {
		writeError(c, consts.StatusNotFound, "device flow disabled")
		return
	}
	verification := fmt.Sprintf("%s/device", strings.TrimSuffix(s.cfg.PipelineURL, "/"))
	resp, err := s.deviceAuth.Start(verification)
	if err != nil {
		s.writeServiceError(c, err)
		return
	}
	writeJSON(c, resp)
}

// handleDeviceApprove implements POST /api/v1/auth/device/approve.
func (s *Server) handleDeviceApprove(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if s.deviceAuth == nil {
		writeError(c, consts.StatusNotFound, "device flow disabled")
		return
	}
	var req struct {
		UserCode string `json:"user_code"`
	}
	if !decodeBody(c, &req) {
		return
	}
	if err := s.deviceAuth.Approve(req.UserCode, id.UserID); err != nil {
		s.writeServiceError(c, err)
		return
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// oauthErrorResponse is the RFC 6749 §5.2 / RFC 8628 §3.5 token error payload.
type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// writeOAuthError emits an OAuth token error. Token responses must not be
// cached (RFC 6749 §5.1), so both no-store and no-cache are set.
func writeOAuthError(c *app.RequestContext, status int, code, description string) {
	c.Response.Header.Set("Cache-Control", "no-store")
	c.Response.Header.Set("Pragma", "no-cache")
	c.JSON(status, oauthErrorResponse{Error: code, ErrorDescription: description})
}

// handleDeviceToken implements POST /oauth/token for the device_code and
// refresh_token grants.
func (s *Server) handleDeviceToken(ctx context.Context, c *app.RequestContext) {
	if s.deviceAuth == nil {
		writeError(c, consts.StatusNotFound, "device flow disabled")
		return
	}
	// Bound the body up front: /oauth/token is a public endpoint and c.Body()
	// would otherwise buffer an unbounded stream (CWE-400/CWE-770).
	body, err := readBoundedBody(c, maxControlBody)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeOAuthError(c, consts.StatusRequestEntityTooLarge, "invalid_request", "request body too large")
			return
		}
		writeOAuthError(c, consts.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	params := oauthTokenParams(c, body)
	switch params.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		res, err := s.deviceAuth.ExchangeDeviceCode(ctx, params.Get("device_code"))
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrAuthorizationPending):
				writeOAuthError(c, consts.StatusBadRequest, "authorization_pending", "device authorization is pending user approval")
			case errors.Is(err, domain.ErrExpiredDeviceToken):
				writeOAuthError(c, consts.StatusBadRequest, "expired_token", "device code has expired")
			case errors.Is(err, domain.ErrInvalidGrant):
				writeOAuthError(c, consts.StatusBadRequest, "invalid_grant", "device code is invalid or has already been used")
			default:
				s.writeServiceError(c, err)
			}
			return
		}
		writeJSON(c, res)
	case "refresh_token":
		if s.auth == nil {
			writeError(c, consts.StatusServiceUnavailable, "auth unavailable")
			return
		}
		access, refresh, err := s.auth.Refresh(ctx, params.Get("refresh_token"))
		if err != nil {
			if errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrSessionRevoked) {
				writeOAuthError(c, consts.StatusBadRequest, "invalid_grant", "refresh token is invalid, expired, or revoked")
				return
			}
			s.writeServiceError(c, err)
			return
		}
		writeJSON(c, map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"token_type":    "Bearer",
			"expires_in":    int(s.jwt.AccessTTL().Seconds()),
		})
	default:
		writeOAuthError(c, consts.StatusBadRequest, "unsupported_grant_type", "grant_type is not supported")
	}
}

// oauthTokenParams parses form-encoded or JSON token request parameters from an
// already-bounded body.
func oauthTokenParams(c *app.RequestContext, body []byte) url.Values {
	ct := string(c.Request.Header.ContentType())
	if strings.Contains(ct, "application/json") {
		var m map[string]string
		if err := json.Unmarshal(body, &m); err == nil {
			values := url.Values{}
			for k, v := range m {
				values.Set(k, v)
			}
			return values
		}
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return url.Values{}
	}
	return values
}
