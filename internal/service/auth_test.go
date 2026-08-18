package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/observ"
)

func newValidator(t *testing.T, tokensFile string) *TokenValidator {
	t.Helper()
	return NewTokenValidator(tokensFile, observ.NewTestLogger())
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

func TestValidateTokenEmpty(t *testing.T) {
	v := newValidator(t, "")
	if _, err := v.ValidateToken(""); err == nil {
		t.Fatal("expected error for empty token when auth enabled")
	}
}

func TestValidateTokenEnabledNoFile(t *testing.T) {
	v := newValidator(t, "")
	if _, err := v.ValidateToken("tok"); err == nil {
		t.Fatal("expected error when auth enabled but no tokens file configured")
	}
}

func TestValidateTokenEnabledFileMissing(t *testing.T) {
	v := newValidator(t, "/nonexistent/path/tokens")
	if _, err := v.ValidateToken("tok"); err == nil {
		t.Fatal("expected error when enabled and tokens file missing (must fail closed)")
	}
}

func TestValidateTokenEnabledValid(t *testing.T) {
	file := writeTokens(t, "# comment\n\ngood-token\nother-token\n")
	v := newValidator(t, file)

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
	v := newValidator(t, file)
	if _, err := v.ValidateToken("bad-token"); err == nil {
		t.Fatal("expected error for invalid token")
	}
}
