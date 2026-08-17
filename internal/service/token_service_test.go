package service

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func testEncKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func newTokenService(t *testing.T) (*TokenService, *UserService) {
	t.Helper()
	return newTokenServiceWithKey(t, testEncKey())
}

func newTokenServiceWithKey(t *testing.T, key []byte) (*TokenService, *UserService) {
	t.Helper()
	r := newServiceDB(t)
	tsvc := NewTokenService(r.tokens, testLogger(), key)
	usvc := NewUserService(r.users, r.groups, testLogger())
	return tsvc, usvc
}

func TestTokenServiceGenerateAndValidate(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	plaintext, meta, err := tsvc.Generate(ctx, u.ID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if plaintext == "" || meta == nil {
		t.Fatal("empty plaintext/meta")
	}
	if len(plaintext) < 12 || plaintext[:4] != "dct_" {
		t.Fatalf("plaintext format = %q", plaintext)
	}
	if meta.Prefix != plaintext[:12] {
		t.Fatalf("prefix = %q, want %q", meta.Prefix, plaintext[:12])
	}

	// Validate round-trip.
	validated, err := tsvc.Validate(ctx, plaintext)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if validated.UserID != u.ID {
		t.Fatalf("user mismatch")
	}

	// Second Generate -> ErrTokenExists.
	if _, _, err := tsvc.Generate(ctx, u.ID); !errors.Is(err, domain.ErrTokenExists) {
		t.Fatalf("second Generate: %v, want ErrTokenExists", err)
	}
}

func TestTokenServiceRegenerateInvalidatesOld(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	plaintext1, _, _ := tsvc.Generate(ctx, u.ID)
	plaintext2, _, err := tsvc.Regenerate(ctx, u.ID)
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if plaintext1 == plaintext2 {
		t.Fatal("regenerate should produce a new token")
	}
	// Old token is invalid immediately.
	if _, err := tsvc.Validate(ctx, plaintext1); err != domain.ErrUnauthenticated {
		t.Fatalf("old token should be invalid: %v", err)
	}
	// New token works.
	if _, err := tsvc.Validate(ctx, plaintext2); err != nil {
		t.Fatalf("new token validate: %v", err)
	}
}

func TestTokenServiceMetaAndRevoke(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	// No token yet -> ErrNotFound.
	if _, err := tsvc.Meta(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Meta before generate: %v", err)
	}

	plaintext, _, _ := tsvc.Generate(ctx, u.ID)
	meta, err := tsvc.Meta(ctx, u.ID)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Prefix != plaintext[:12] {
		t.Fatalf("prefix mismatch")
	}

	if err := tsvc.Revoke(ctx, u.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := tsvc.Meta(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Meta after revoke: %v", err)
	}
	// Revoked token no longer validates.
	if _, err := tsvc.Validate(ctx, plaintext); err != domain.ErrUnauthenticated {
		t.Fatalf("Validate after revoke: %v", err)
	}
}

func TestTokenServiceValidateEmpty(t *testing.T) {
	tsvc, _ := newTokenService(t)
	if _, err := tsvc.Validate(context.Background(), ""); err != domain.ErrUnauthenticated {
		t.Fatalf("empty token: %v", err)
	}
	if _, err := tsvc.Validate(context.Background(), "dct_nope"); err != domain.ErrUnauthenticated {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestTokenServiceImportRaw(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	raw := "legacy-token-123"
	if err := tsvc.ImportRaw(ctx, u.ID, raw); err != nil {
		t.Fatalf("ImportRaw: %v", err)
	}
	validated, err := tsvc.Validate(ctx, raw)
	if err != nil {
		t.Fatalf("Validate imported: %v", err)
	}
	if validated.UserID != u.ID {
		t.Fatalf("user mismatch")
	}
}

func TestHashAPIToken(t *testing.T) {
	h1 := HashAPIToken("dct_abc")
	h2 := HashAPIToken("dct_abc")
	h3 := HashAPIToken("dct_xyz")
	if h1 != h2 {
		t.Fatal("hash should be deterministic")
	}
	if h1 == h3 {
		t.Fatal("different inputs should hash differently")
	}
}

func TestRevealSuccess(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	plaintext, _, err := tsvc.Generate(ctx, u.ID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got, err := tsvc.Reveal(ctx, u.ID)
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if got != plaintext {
		t.Fatalf("Reveal = %q, want %q", got, plaintext)
	}
}

func TestRevealNotFound(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	if _, err := tsvc.Reveal(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Reveal missing: %v, want ErrNotFound", err)
	}
}

func TestRevealPreV2Token(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	// Directly persist a token with no ciphertext (pre-v2 shape).
	if err := tsvc.tokens.Upsert(ctx, &domain.APIToken{
		ID:        newID(),
		UserID:    u.ID,
		TokenHash: HashAPIToken("dct_pre-v2"),
		Prefix:    "dct_pre-v2",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := tsvc.Reveal(ctx, u.ID); !errors.Is(err, domain.ErrTokenNotRecoverable) {
		t.Fatalf("Reveal pre-v2: %v, want ErrTokenNotRecoverable", err)
	}
}

func TestRevealNoKey(t *testing.T) {
	r := newServiceDB(t)
	ctx := context.Background()
	usvc := NewUserService(r.users, r.groups, testLogger())
	u := seedUserSvc(t, usvc, "u1")

	keyed := NewTokenService(r.tokens, testLogger(), testEncKey())
	if _, _, err := keyed.Generate(ctx, u.ID); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Same repo, but no key: the ciphertext is present yet unrecoverable.
	noKey := NewTokenService(r.tokens, testLogger(), nil)
	if _, err := noKey.Reveal(ctx, u.ID); !errors.Is(err, domain.ErrTokenNotRecoverable) {
		t.Fatalf("Reveal with no key: %v, want ErrTokenNotRecoverable", err)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testEncKey()
	ct, err := encryptToken(key, "dct_secret_value")
	if err != nil {
		t.Fatalf("encryptToken: %v", err)
	}
	pt, err := decryptToken(key, ct)
	if err != nil {
		t.Fatalf("decryptToken: %v", err)
	}
	if pt != "dct_secret_value" {
		t.Fatalf("decryptToken = %q", pt)
	}

	// Wrong key must fail.
	if _, err := decryptToken([]byte("11111111111111111111111111111111"), ct); err == nil {
		t.Fatal("decrypt with wrong key: expected error")
	}
}

func TestEncryptTokenDisabled(t *testing.T) {
	if ct, err := encryptToken(nil, "dct_x"); err != nil || ct != "" {
		t.Fatalf("encryptToken(nil) = (%q, %v), want empty/nil", ct, err)
	}
	if ct, err := encryptToken([]byte{}, "dct_x"); err != nil || ct != "" {
		t.Fatalf("encryptToken(empty) = (%q, %v), want empty/nil", ct, err)
	}
}

func TestDecryptTokenInvalid(t *testing.T) {
	key := testEncKey()

	if _, err := decryptToken(key, "!!!not-base64!!!"); err == nil {
		t.Fatal("decryptToken bad base64: expected error")
	}
	if _, err := decryptToken(key, base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("decryptToken short ciphertext: expected error")
	}
}

func TestEncryptDecryptInvalidKey(t *testing.T) {
	short := []byte("too-short")
	if _, err := encryptToken(short, "dct_x"); err == nil {
		t.Fatal("encryptToken with short key: expected error")
	}
	ct, err := encryptToken(testEncKey(), "dct_x")
	if err != nil {
		t.Fatalf("encryptToken: %v", err)
	}
	if _, err := decryptToken(short, ct); err == nil {
		t.Fatal("decryptToken with short key: expected error")
	}
}

func TestGenerateEncryptError(t *testing.T) {
	r := newServiceDB(t)
	usvc := NewUserService(r.users, r.groups, testLogger())
	u := seedUserSvc(t, usvc, "u1")

	// A short key makes encryption fail during Generate.
	tsvc := NewTokenService(r.tokens, testLogger(), []byte("short"))
	if _, _, err := tsvc.Generate(context.Background(), u.ID); err == nil {
		t.Fatal("Generate with short key: expected error")
	}
}

func TestGenerateRepoError(t *testing.T) {
	tsvc := NewTokenService(errorTokenRepo{}, testLogger(), testEncKey())
	if _, _, err := tsvc.Generate(context.Background(), "u1"); err == nil {
		t.Fatal("Generate with repo error: expected error")
	}
}

func TestImportRawEncryptError(t *testing.T) {
	r := newServiceDB(t)
	usvc := NewUserService(r.users, r.groups, testLogger())
	u := seedUserSvc(t, usvc, "u1")

	tsvc := NewTokenService(r.tokens, testLogger(), []byte("short"))
	if err := tsvc.ImportRaw(context.Background(), u.ID, "dct_x"); err == nil {
		t.Fatal("ImportRaw with short key: expected error")
	}
}

func TestRevealDecryptError(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	// Non-empty but corrupt ciphertext: recoverable flag is true, but decrypt
	// fails.
	if err := tsvc.tokens.Upsert(ctx, &domain.APIToken{
		ID:              newID(),
		UserID:          u.ID,
		TokenHash:       HashAPIToken("dct_corrupt"),
		TokenCiphertext: base64.StdEncoding.EncodeToString([]byte("corrupt-ciphertext-that-is-long-enough")),
		Prefix:          "dct_corrupt",
		CreatedAt:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := tsvc.Reveal(ctx, u.ID); err == nil {
		t.Fatal("Reveal with corrupt ciphertext: expected error")
	}
}

func TestUpsertStoresCiphertext(t *testing.T) {
	tsvc, usvc := newTokenService(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	if _, _, err := tsvc.Generate(ctx, u.ID); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	meta, err := tsvc.Meta(ctx, u.ID)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.TokenCiphertext == "" {
		t.Fatal("expected non-empty TokenCiphertext after Generate")
	}
}
