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

// handleDeviceToken implements POST /oauth/token for the device_code and
// refresh_token grants.
func (s *Server) handleDeviceToken(ctx context.Context, c *app.RequestContext) {
	if s.deviceAuth == nil {
		writeError(c, consts.StatusNotFound, "device flow disabled")
		return
	}
	params := oauthTokenParams(c)
	switch params.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		res, err := s.deviceAuth.ExchangeDeviceCode(ctx, params.Get("device_code"))
		if err != nil {
			if errors.Is(err, domain.ErrUnauthenticated) {
				writeError(c, consts.StatusBadRequest, err.Error())
				return
			}
			s.writeServiceError(c, err)
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
		writeError(c, consts.StatusBadRequest, "unsupported grant_type")
	}
}

// oauthTokenParams parses form-encoded or JSON token request parameters.
func oauthTokenParams(c *app.RequestContext) url.Values {
	body, _ := c.Body()
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
