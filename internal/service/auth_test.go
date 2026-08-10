package service

import (
	"os"
	"path/filepath"
	"testing"

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
