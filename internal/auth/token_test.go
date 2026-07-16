package auth

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/observ"
)

func newValidator(t *testing.T, tokensFile string, enabled bool) *TokenValidator {
	t.Helper()
	return NewTokenValidator(tokensFile, enabled, observ.NewTestLogger())
}

func writeTokens(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write tokens: %v", err)
	}
	return p
}

func TestValidateTokenDisabledAcceptsAny(t *testing.T) {
	v := newValidator(t, "", false)
	tok, err := v.ValidateToken("anything-goes")
	if err != nil {
		t.Fatalf("disabled should accept: %v", err)
	}
	if tok != "anything-goes" {
		t.Fatalf("token = %q", tok)
	}
}

func TestValidateTokenDisabledAcceptsEmpty(t *testing.T) {
	v := newValidator(t, "", false)
	tok, err := v.ValidateToken("")
	if err != nil {
		t.Fatalf("disabled should accept empty: %v", err)
	}
	if tok != "" {
		t.Fatalf("token = %q", tok)
	}
}

func TestValidateRequestDisabledAcceptsNoHeader(t *testing.T) {
	v := newValidator(t, "", false)
	c := ut.CreateUtRequestContext("POST", "/v1/engines", nil)
	tok, err := v.ValidateRequest(c)
	if err != nil {
		t.Fatalf("disabled should accept requests without auth: %v", err)
	}
	if tok != "no-auth" {
		t.Fatalf("token = %q, want %q", tok, "no-auth")
	}
}

func TestValidateTokenEmpty(t *testing.T) {
	v := newValidator(t, "", true)
	if _, err := v.ValidateToken(""); err == nil {
		t.Fatal("expected error for empty token when auth enabled")
	}
}

func TestValidateTokenEnabledNoFile(t *testing.T) {
	v := newValidator(t, "", true)
	if _, err := v.ValidateToken("tok"); err == nil {
		t.Fatal("expected error when auth enabled but no tokens file configured")
	}
}

func TestValidateTokenEnabledFileMissing(t *testing.T) {
	v := newValidator(t, "/nonexistent/path/tokens", true)
	if _, err := v.ValidateToken("tok"); err == nil {
		t.Fatal("expected error when enabled and tokens file missing (must fail closed)")
	}
}

func TestValidateTokenEnabledValid(t *testing.T) {
	file := writeTokens(t, "# comment\n\ngood-token\nother-token\n")
	v := newValidator(t, file, true)

	tok, err := v.ValidateToken("good-token")
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if tok != "good-token" {
		t.Fatalf("token = %q", tok)
	}
}

func TestValidateTokenEnabledInvalid(t *testing.T) {
	file := writeTokens(t, "good-token\n")
	v := newValidator(t, file, true)
	if _, err := v.ValidateToken("bad-token"); err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestExtractTokenSchemes(t *testing.T) {
	file := writeTokens(t, "test-token\n")
	v := newValidator(t, file, true)

	tests := []struct {
		name    string
		header  string
		wantErr bool
		wantTok string
	}{
		{"bearer", "Bearer test-token", false, "test-token"},
		{"basic", fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte("test-token:"))), false, "test-token"},
		{"missing", "", true, ""},
		{"unsupported", "Digest abc", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var headers []ut.Header
			if tt.header != "" {
				headers = append(headers, ut.Header{Key: "Authorization", Value: tt.header})
			}
			c := ut.CreateUtRequestContext("POST", "/v1/engines", nil, headers...)
			tok, err := v.ValidateRequest(c)
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
