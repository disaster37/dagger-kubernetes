package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// postForm issues an application/x-www-form-urlencoded POST to path and returns
// the response (with its body already closed, kept for status/header checks)
// and body.
func postForm(t *testing.T, base, path string, form url.Values) (resp *http.Response, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, body
}

// oauthErrorBody is the RFC 6749 §5.2 error payload.
type oauthErrorBody struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// TestDeviceAuthFlowEndToEnd exercises the RFC 8628 device flow and its error
// codes over real HTTP, proving golang.org/x/oauth2 Config.DeviceAccessToken
// keeps polling until approval.
func TestDeviceAuthFlowEndToEnd(t *testing.T) {
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, nil, nil)

	// 1. Device code request (public).
	resp, body := postForm(t, srv.base, "/oauth/device/code", url.Values{}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device/code status = %d (%s)", resp.StatusCode, body)
	}
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &dc); err != nil {
		t.Fatal(err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" || dc.ExpiresIn <= 0 {
		t.Fatalf("device/code response = %+v", dc)
	}

	// 2. Polling before approval must return authorization_pending.
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {deviceGrantType}, "device_code": {dc.DeviceCode}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pending status = %d, want 400 (%s)", resp.StatusCode, body)
	}
	var oerr oauthErrorBody
	if err := json.Unmarshal(body, &oerr); err != nil {
		t.Fatal(err)
	}
	if oerr.Error != "authorization_pending" {
		t.Fatalf("pending error = %q, want authorization_pending", oerr.Error)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}

	// 3. Unknown device code -> invalid_grant.
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {deviceGrantType}, "device_code": {"does-not-exist"}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown status = %d, want 400 (%s)", resp.StatusCode, body)
	}
	_ = json.Unmarshal(body, &oerr)
	if oerr.Error != "invalid_grant" {
		t.Fatalf("unknown error = %q, want invalid_grant", oerr.Error)
	}

	// 4. Unsupported grant type.
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {"password"}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d, want 400 (%s)", resp.StatusCode, body)
	}
	_ = json.Unmarshal(body, &oerr)
	if oerr.Error != "unsupported_grant_type" {
		t.Fatalf("unsupported error = %q, want unsupported_grant_type", oerr.Error)
	}

	// 5. Approve the user code (admin bearer).
	approveBody, _ := json.Marshal(map[string]string{"user_code": dc.UserCode})
	req, err := http.NewRequest(http.MethodPost, srv.base+"/api/v1/auth/device/approve", bytes.NewReader(approveBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", srv.admin))
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("approve status = %d, want 204", resp.StatusCode)
	}

	// 6. Polling after approval returns tokens.
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {deviceGrantType}, "device_code": {dc.DeviceCode}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.TokenType != "Bearer" {
		t.Fatalf("token response = %+v", tok)
	}

	// 7. The access token works against the GraphQL API.
	qreq, err := http.NewRequest(http.MethodPost, srv.base+"/query", strings.NewReader(`{"query":"{ user { id nickname } }"}`))
	if err != nil {
		t.Fatal(err)
	}
	qreq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", tok.AccessToken))
	qreq.Header.Set("Content-Type", "application/json")
	qresp, err := http.DefaultClient.Do(qreq)
	if err != nil {
		t.Fatal(err)
	}
	_ = qresp.Body.Close()
	if qresp.StatusCode != http.StatusOK {
		t.Fatalf("query with device token status = %d, want 200", qresp.StatusCode)
	}

	// 8. Replaying the consumed device code -> invalid_grant.
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {deviceGrantType}, "device_code": {dc.DeviceCode}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400 (%s)", resp.StatusCode, body)
	}
	_ = json.Unmarshal(body, &oerr)
	if oerr.Error != "invalid_grant" {
		t.Fatalf("replay error = %q, want invalid_grant", oerr.Error)
	}

	// 9. Refresh works; an invalid refresh token -> invalid_grant.
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	resp, body = postForm(t, srv.base, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"bogus"}}) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid refresh status = %d, want 400 (%s)", resp.StatusCode, body)
	}
	_ = json.Unmarshal(body, &oerr)
	if oerr.Error != "invalid_grant" {
		t.Fatalf("invalid refresh error = %q, want invalid_grant", oerr.Error)
	}
}

// TestDeviceAuthOAuth2Client drives the real golang.org/x/oauth2 device flow:
// DeviceAccessToken must keep polling on authorization_pending and succeed once
// the code is approved out of band.
func TestDeviceAuthOAuth2Client(t *testing.T) {
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, nil, nil)

	conf := &oauth2.Config{
		ClientID: "dagger-cli",
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: srv.base + "/oauth/device/code",
			TokenURL:      srv.base + "/oauth/token",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	da, err := conf.DeviceAuth(ctx)
	if err != nil {
		t.Fatalf("DeviceAuth: %v", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		t.Fatalf("DeviceAuth = %+v", da)
	}
	// Poll quickly; x/oauth2 defaults to 5s.
	da.Interval = 1

	// Approve after ~1.3s so at least one authorization_pending poll precedes
	// the successful exchange.
	approveErr := make(chan error, 1)
	go func() {
		time.Sleep(1300 * time.Millisecond)
		body, _ := json.Marshal(map[string]string{"user_code": da.UserCode})
		req, err := http.NewRequest(http.MethodPost, srv.base+"/api/v1/auth/device/approve", bytes.NewReader(body))
		if err != nil {
			approveErr <- err
			return
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", srv.admin))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			approveErr <- err
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			approveErr <- fmt.Errorf("approve status = %d", resp.StatusCode)
			return
		}
		approveErr <- nil
	}()

	tok, err := conf.DeviceAccessToken(ctx, da)
	if err != nil {
		t.Fatalf("DeviceAccessToken: %v", err)
	}
	if err := <-approveErr; err != nil {
		t.Fatalf("approve: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatalf("access token empty: %+v", tok)
	}
	if tok.RefreshToken == "" {
		t.Fatalf("refresh token empty: %+v", tok)
	}
}
