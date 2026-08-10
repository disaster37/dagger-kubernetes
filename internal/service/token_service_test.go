package service

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newTokenService(t *testing.T) (*TokenService, *UserService) {
	t.Helper()
	r := newServiceDB(t)
	tsvc := NewTokenService(r.tokens, testLogger())
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
