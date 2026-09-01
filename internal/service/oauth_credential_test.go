package service

import (
	"testing"
	"time"
)

func TestEncryptDecryptOAuthCredential(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	cred := &oauthCredential{
		Provider:     "github",
		AccessToken:  "gh_access_123",
		RefreshToken: "gh_refresh_456",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	ct, err := encryptOAuthCredential(key, cred)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == "" {
		t.Fatal("expected non-empty ciphertext")
	}

	decrypted, err := decryptOAuthCredential(key, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted.Provider != "github" || decrypted.AccessToken != "gh_access_123" {
		t.Fatalf("round-trip failed: %+v", decrypted)
	}
}

func TestEncryptOAuthCredentialNilKey(t *testing.T) {
	cred := &oauthCredential{Provider: "github", AccessToken: "tok"}
	ct, err := encryptOAuthCredential(nil, cred)
	if err != nil {
		t.Fatalf("encrypt with nil key: %v", err)
	}
	if ct != "" {
		t.Fatalf("expected empty ciphertext with nil key, got %q", ct)
	}
}

func TestEncryptOAuthCredentialEmptyKey(t *testing.T) {
	cred := &oauthCredential{Provider: "github", AccessToken: "tok"}
	ct, err := encryptOAuthCredential([]byte{}, cred)
	if err != nil {
		t.Fatalf("encrypt with empty key: %v", err)
	}
	if ct != "" {
		t.Fatalf("expected empty ciphertext with empty key, got %q", ct)
	}
}

func TestEncryptOAuthCredentialNilCred(t *testing.T) {
	key := make([]byte, 32)
	ct, err := encryptOAuthCredential(key, nil)
	if err != nil {
		t.Fatalf("encrypt nil credential: %v", err)
	}
	if ct != "" {
		t.Fatalf("expected empty ciphertext for nil credential, got %q", ct)
	}
}

func TestDecryptOAuthCredentialEmpty(t *testing.T) {
	decrypted, err := decryptOAuthCredential(make([]byte, 32), "")
	if err != nil {
		t.Fatalf("decrypt empty: %v", err)
	}
	if decrypted != nil {
		t.Fatalf("expected nil for empty ciphertext, got %+v", decrypted)
	}
}

func TestDecryptOAuthCredentialNoKey(t *testing.T) {
	_, err := decryptOAuthCredential([]byte{}, "some-ciphertext")
	if err == nil {
		t.Fatal("expected error when decrypting with no key")
	}
}
