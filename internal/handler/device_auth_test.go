package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// newDeviceEngine registers the POST /oauth/token + device approve routes used
// by the RFC 8628 flow (mirrors production registration in server.go).
func newDeviceEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.POST("/oauth/device/code", s.handleDeviceCode)
	e.POST("/oauth/token", s.handleDeviceToken)
	e.POST("/api/v1/auth/device/approve", s.handleDeviceApprove)
	return e
}

// seedDevice inserts a device authorization directly into the test store.
func (e *testEnv) seedDevice(t *testing.T, deviceCode, userCode string, approved bool, userID string, expiresAt time.Time, consumed bool) {
	t.Helper()
	if err := e.device.Create(&domain.DeviceAuthorization{
		DeviceCode: deviceCode,
		UserCode:   userCode,
		Approved:   approved,
		UserID:     userID,
		ExpiresAt:  expiresAt,
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if consumed {
		if err := e.device.Consume(deviceCode); err != nil {
			t.Fatalf("seed consume: %v", err)
		}
	}
}

// deviceTokenBody renders the device_code grant request body in the requested
// content type ("json" or "form").
func deviceTokenBody(contentType, deviceCode string) (body, ct string) {
	if contentType == "json" {
		return fmt.Sprintf(`{"grant_type":%q,"device_code":%q}`, "urn:ietf:params:oauth:grant-type:device_code", deviceCode), "application/json"
	}
	return fmt.Sprintf("grant_type=urn%%3Aietf%%3Aparams%%3Aoauth%%3Agrant-type%%3Adevice_code&device_code=%s", deviceCode), "application/x-www-form-urlencoded"
}

// oauthErrBody is the decoded OAuth error payload.
type oauthErrBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Message          string `json:"message"`
}

func TestHandleDeviceToken(t *testing.T) {
	now := time.Now().UTC()

	cases := []struct {
		name         string
		contentType  string
		body         func(env *testEnv, t *testing.T) (string, string)
		wantStatus   int
		wantError    string
		wantNoCache  bool
		wantMessage  bool // OAuth errors must NOT carry the standard "message" key
		successCheck func(t *testing.T, raw []byte)
	}{
		{
			name:        "pending_form",
			contentType: "form",
			body: func(env *testEnv, t *testing.T) (string, string) {
				env.seedDevice(t, "pend", "PEND", false, "", now.Add(time.Minute), false)
				return deviceTokenBody("form", "pend")
			},
			wantStatus:  http.StatusBadRequest,
			wantError:   "authorization_pending",
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:        "pending_json",
			contentType: "json",
			body: func(env *testEnv, t *testing.T) (string, string) {
				env.seedDevice(t, "pendj", "PENDJ", false, "", now.Add(time.Minute), false)
				return deviceTokenBody("json", "pendj")
			},
			wantStatus:  http.StatusBadRequest,
			wantError:   "authorization_pending",
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:       "unknown_device_code",
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_grant",
			body: func(*testEnv, *testing.T) (string, string) {
				return deviceTokenBody("form", "nope")
			},
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:       "empty_device_code",
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_grant",
			body: func(*testEnv, *testing.T) (string, string) {
				return deviceTokenBody("form", "")
			},
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:       "expired",
			wantStatus: http.StatusBadRequest,
			wantError:  "expired_token",
			body: func(env *testEnv, t *testing.T) (string, string) {
				env.seedDevice(t, "exp", "EXP", true, "u1", now.Add(-time.Minute), false)
				return deviceTokenBody("form", "exp")
			},
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:       "approved_success",
			wantStatus: http.StatusOK,
			body: func(env *testEnv, t *testing.T) (string, string) {
				u, err := env.users.Create(context.Background(), "device-ok", "password123", domain.RoleUser)
				if err != nil {
					t.Fatal(err)
				}
				env.seedDevice(t, "ok", "OK", true, u.ID, now.Add(time.Minute), false)
				return deviceTokenBody("form", "ok")
			},
			successCheck: func(t *testing.T, raw []byte) {
				var out struct {
					AccessToken  string `json:"access_token"`
					RefreshToken string `json:"refresh_token"`
					TokenType    string `json:"token_type"`
				}
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatal(err)
				}
				if out.AccessToken == "" || out.RefreshToken == "" || out.TokenType != "Bearer" {
					t.Fatalf("token response = %+v", out)
				}
			},
		},
		{
			name:       "consumed_replay",
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_grant",
			body: func(env *testEnv, t *testing.T) (string, string) {
				env.seedDevice(t, "used", "USED", true, "u1", now.Add(time.Minute), true)
				return deviceTokenBody("form", "used")
			},
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:       "unsupported_grant_type",
			wantStatus: http.StatusBadRequest,
			wantError:  "unsupported_grant_type",
			body: func(*testEnv, *testing.T) (string, string) {
				return "grant_type=password", "application/x-www-form-urlencoded"
			},
			wantNoCache: true,
			wantMessage: false,
		},
		{
			name:       "missing_grant_type",
			wantStatus: http.StatusBadRequest,
			wantError:  "unsupported_grant_type",
			body: func(*testEnv, *testing.T) (string, string) {
				return "", "application/x-www-form-urlencoded"
			},
			wantNoCache: true,
			wantMessage: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			e := newDeviceEngine(env.server)
			body, ct := tc.body(env, t)
			resp := ut.PerformRequest(e, "POST", "/oauth/token", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
				ut.Header{Key: "Content-Type", Value: ct})
			if got := resp.Result().StatusCode(); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", got, tc.wantStatus, resp.Result().Body())
			}
			if got := string(resp.Result().Header.ContentType()); !strings.Contains(got, "application/json") {
				t.Fatalf("content-type = %q, want application/json", got)
			}
			if tc.wantNoCache {
				if got := string(resp.Result().Header.Peek("Cache-Control")); got != "no-store" {
					t.Fatalf("Cache-Control = %q, want no-store", got)
				}
			}
			if tc.wantError != "" {
				var out oauthErrBody
				if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
					t.Fatalf("decode: %v (body=%s)", err, resp.Result().Body())
				}
				if out.Error != tc.wantError {
					t.Fatalf("error = %q, want %q", out.Error, tc.wantError)
				}
				if out.ErrorDescription == "" {
					t.Fatalf("missing error_description in %s", resp.Result().Body())
				}
				if tc.wantMessage == false && out.Message != "" {
					t.Fatalf("OAuth error must not carry a message key: %s", resp.Result().Body())
				}
			}
			if tc.successCheck != nil {
				tc.successCheck(t, resp.Result().Body())
			}
		})
	}
}

func TestHandleDeviceTokenBodyTooLarge(t *testing.T) {
	env := newTestEnv(t)
	e := newDeviceEngine(env.server)

	big := "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code&device_code=" + strings.Repeat("x", maxControlBody+1)
	resp := ut.PerformRequest(e, "POST", "/oauth/token", &ut.Body{Body: strings.NewReader(big), Len: len(big)},
		ut.Header{Key: "Content-Type", Value: "application/x-www-form-urlencoded"})
	if got := resp.Result().StatusCode(); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", got)
	}
	var out oauthErrBody
	if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", out.Error)
	}
	if got := string(resp.Result().Header.Peek("Cache-Control")); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestHandleDeviceTokenFlowDisabled(t *testing.T) {
	env := newTestEnv(t)
	env.server.deviceAuth = nil
	e := newDeviceEngine(env.server)

	resp := ut.PerformRequest(e, "POST", "/oauth/token", &ut.Body{Body: strings.NewReader("grant_type=password"), Len: 18},
		ut.Header{Key: "Content-Type", Value: "application/x-www-form-urlencoded"})
	if got := resp.Result().StatusCode(); got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got)
	}
}

func TestHandleDeviceTokenRefreshGrant(t *testing.T) {
	env := newTestEnv(t)
	e := newDeviceEngine(env.server)

	// Obtain a valid refresh token via admin login.
	access, refresh, _, err := env.auth.Login(context.Background(), "admin", "password123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if access == "" {
		t.Fatal("missing access token")
	}

	cases := []struct {
		name       string
		refresh    string
		wantStatus int
		wantError  string
	}{
		{name: "valid", refresh: refresh, wantStatus: http.StatusOK},
		{name: "malformed", refresh: "not-a-jwt", wantStatus: http.StatusBadRequest, wantError: "invalid_grant"},
		{name: "empty", refresh: "", wantStatus: http.StatusBadRequest, wantError: "invalid_grant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "grant_type=refresh_token&refresh_token=" + tc.refresh
			resp := ut.PerformRequest(e, "POST", "/oauth/token", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
				ut.Header{Key: "Content-Type", Value: "application/x-www-form-urlencoded"})
			if got := resp.Result().StatusCode(); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", got, tc.wantStatus, resp.Result().Body())
			}
			if tc.wantError != "" {
				var out oauthErrBody
				if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
					t.Fatal(err)
				}
				if out.Error != tc.wantError {
					t.Fatalf("error = %q, want %q", out.Error, tc.wantError)
				}
				if out.Message != "" {
					t.Fatalf("refresh OAuth error must not carry message: %s", resp.Result().Body())
				}
			}
		})
	}
}

// TestHandleDeviceTokenRefreshRevoked covers a refresh token whose token
// version no longer matches the user's (post-logout / password-change session
// revocation) mapping to 400 invalid_grant rather than a generic error.
func TestHandleDeviceTokenRefreshRevoked(t *testing.T) {
	env := newTestEnv(t)
	e := newDeviceEngine(env.server)

	_, refresh, _, err := env.auth.Login(context.Background(), "admin", "password123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// Bump the admin's token version so the refresh token is stale.
	if err := env.users.IncrementTokenVersion(context.Background(), adminID(t, env)); err != nil {
		t.Fatalf("increment token version: %v", err)
	}

	body := "grant_type=refresh_token&refresh_token=" + refresh
	resp := ut.PerformRequest(e, "POST", "/oauth/token", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/x-www-form-urlencoded"})
	if got := resp.Result().StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", got, resp.Result().Body())
	}
	var out oauthErrBody
	if err := json.Unmarshal(resp.Result().Body(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error != "invalid_grant" {
		t.Fatalf("error = %q, want invalid_grant", out.Error)
	}
}

// adminID returns the bootstrap admin's user ID.
func adminID(t *testing.T, env *testEnv) string {
	t.Helper()
	u, err := env.users.GetByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	return u.ID
}

func TestHandleDeviceApproveSemanticsUnchanged(t *testing.T) {
	now := time.Now().UTC()

	t.Run("unauthenticated", func(t *testing.T) {
		env := newTestEnv(t)
		e := newDeviceEngine(env.server)
		body := `{"user_code":"ABCD-EFGH"}`
		resp := ut.PerformRequest(e, "POST", "/api/v1/auth/device/approve", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if got := resp.Result().StatusCode(); got != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", got)
		}
	})

	t.Run("unknown_user_code", func(t *testing.T) {
		env := newTestEnv(t)
		e := newDeviceEngine(env.server)
		body := `{"user_code":"ZZZZ-ZZZZ"}`
		resp := ut.PerformRequest(e, "POST", "/api/v1/auth/device/approve", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Authorization", Value: env.loginAsAdmin(t)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if got := resp.Result().StatusCode(); got != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", got)
		}
	})

	t.Run("expired", func(t *testing.T) {
		env := newTestEnv(t)
		e := newDeviceEngine(env.server)
		env.seedDevice(t, "expd", "EXPD-CODE", false, "", now.Add(-time.Minute), false)
		body := `{"user_code":"EXPD-CODE"}`
		resp := ut.PerformRequest(e, "POST", "/api/v1/auth/device/approve", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Authorization", Value: env.loginAsAdmin(t)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if got := resp.Result().StatusCode(); got != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", got)
		}
	})

	t.Run("valid_lowercase", func(t *testing.T) {
		env := newTestEnv(t)
		e := newDeviceEngine(env.server)
		env.seedDevice(t, "vald", "VALID-CODE", false, "", now.Add(time.Minute), false)
		body := `{"user_code":"valid-code"}`
		resp := ut.PerformRequest(e, "POST", "/api/v1/auth/device/approve", &ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Authorization", Value: env.loginAsAdmin(t)},
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if got := resp.Result().StatusCode(); got != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body=%s)", got, resp.Result().Body())
		}
	})
}
