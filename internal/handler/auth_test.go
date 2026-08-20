package handler

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
)

func TestExtractTokenSchemes(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		query   string
		wantErr bool
		wantTok string
	}{
		{"bearer", "Bearer test-token", "", false, "test-token"},
		{"basic", fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte("test-token:"))), "", false, "test-token"},
		// extractToken is header-only; the ?token= fallback is limited to the
		// SSE /live route (requireAuthWithQueryFallback).
		{"query ignored", "", "tok-from-query", true, ""},
		{"missing", "", "", true, ""},
		{"unsupported", "Digest abc", "", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/v1/engines"
			if tt.query != "" {
				path = fmt.Sprintf("%s?token=%s", path, tt.query)
			}
			var headers []ut.Header
			if tt.header != "" {
				headers = append(headers, ut.Header{Key: "Authorization", Value: tt.header})
			}
			c := ut.CreateUtRequestContext("POST", path, nil, headers...)
			tok, err := extractToken(c)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok != tt.wantTok {
				t.Fatalf("token = %q, want %q", tok, tt.wantTok)
			}
		})
	}
}

// TestRequireAuthEnabledRejects verifies that auth-enabled mode rejects
// requests without a token.
func TestRequireAuthEnabledRejects(t *testing.T) {
	env := newTestEnv(t)
	e := newTestEngine(env.server)

	resp := ut.PerformRequest(e, "GET", "/api/v1/fleet", nil)
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("enabled auth should reject, got %d", resp.Result().StatusCode())
	}
}

// TestRequireAuthWithQueryFallback verifies the ?token= query-param fallback
// used by the SSE /live route (D14): EventSource clients cannot set headers.
func TestRequireAuthWithQueryFallback(t *testing.T) {
	env := newTestEnv(t)

	bearer, _ := env.createUserAndToken(t)
	token := strings.TrimPrefix(bearer, "Bearer ")

	// Token via query param authenticates.
	c := ut.CreateUtRequestContext("GET", "/api/v1/traces/t1/live?token="+token, nil)
	if !env.server.requireAuthWithQueryFallback(c) {
		t.Fatal("query token should authenticate")
	}
	if id := identityOf(c); id == nil || id.Username != "alice" {
		t.Fatalf("identity = %+v", id)
	}

	// Authorization header still wins when present.
	c = ut.CreateUtRequestContext("GET", "/api/v1/traces/t1/live", nil, ut.Header{Key: "Authorization", Value: bearer})
	if !env.server.requireAuthWithQueryFallback(c) {
		t.Fatal("header token should authenticate")
	}

	// Access cookie authenticates when no header/query is present
	// (header → cookie → query order).
	access, _, _, err := env.auth.Login(context.Background(), "admin", "password123")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	c = ut.CreateUtRequestContext("GET", "/api/v1/traces/t1/live", nil,
		ut.Header{Key: "Cookie", Value: fmt.Sprintf("dagger_kubernetes_access=%s", access)})
	if !env.server.requireAuthWithQueryFallback(c) {
		t.Fatal("access cookie should authenticate")
	}
	if id := identityOf(c); id == nil || id.Username != "admin" {
		t.Fatalf("identity = %+v", id)
	}

	// No token at all -> rejected.
	c = ut.CreateUtRequestContext("GET", "/api/v1/traces/t1/live", nil)
	if env.server.requireAuthWithQueryFallback(c) {
		t.Fatal("missing token should be rejected")
	}
}
