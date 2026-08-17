package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestTokenRepoUpsertReplaces(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTokenRepo(store)
	ctx := context.Background()

	u := seedUser(t, store, "u")

	tok1 := &domain.APIToken{
		ID:        newID(),
		UserID:    u.ID,
		TokenHash: "hash1",
		Prefix:    "dct_aaaaaaaa",
	}
	if err := repo.Upsert(ctx, tok1); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByUser: %v", err)
	}
	if got.TokenHash != "hash1" {
		t.Fatalf("hash = %q", got.TokenHash)
	}

	// Upsert again replaces the hash (one token per user).
	tok2 := &domain.APIToken{
		ID:        newID(),
		UserID:    u.ID,
		TokenHash: "hash2",
		Prefix:    "dct_bbbbbbbb",
	}
	if err := repo.Upsert(ctx, tok2); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}
	got, _ = repo.GetByUser(ctx, u.ID)
	if got.TokenHash != "hash2" {
		t.Fatalf("hash = %q, want hash2", got.TokenHash)
	}
	if got.Prefix != "dct_bbbbbbbb" {
		t.Fatalf("prefix = %q", got.Prefix)
	}

	byHash, err := repo.GetByHash(ctx, "hash2")
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if byHash.UserID != u.ID {
		t.Fatalf("user mismatch")
	}

	if _, err := repo.GetByHash(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByHash missing: %v", err)
	}
	if _, err := repo.GetByUser(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByUser missing: %v", err)
	}
}

func TestTokenRepoTouchLastUsed(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTokenRepo(store)
	ctx := context.Background()

	u := seedUser(t, store, "u")
	tok := &domain.APIToken{ID: newID(), UserID: u.ID, TokenHash: "h", Prefix: "dct_aaaaaaaa"}
	repo.Upsert(ctx, tok)

	now := time.Now().UTC()
	if err := repo.TouchLastUsed(ctx, tok.ID, now); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}
	got, _ := repo.GetByUser(ctx, u.ID)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(now) {
		t.Fatalf("last_used_at = %v, want %v", got.LastUsedAt, now)
	}
}

func TestTokenRepoDelete(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTokenRepo(store)
	ctx := context.Background()

	u := seedUser(t, store, "u")
	tok := &domain.APIToken{ID: newID(), UserID: u.ID, TokenHash: "h", Prefix: "dct_aaaaaaaa"}
	repo.Upsert(ctx, tok)

	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByUser(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByUser after delete: %v", err)
	}
}

func TestTokenRepoDeleteUserCascadesToken(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTokenRepo(store)
	urepo := NewUserRepo(store)
	ctx := context.Background()

	u := seedUser(t, store, "u")
	tok := &domain.APIToken{ID: newID(), UserID: u.ID, TokenHash: "h", Prefix: "dct_aaaaaaaa"}
	repo.Upsert(ctx, tok)

	if err := urepo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete user: %v", err)
	}
	if _, err := repo.GetByUser(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("token should cascade-delete with user, got %v", err)
	}
}

func TestTokenRepoUpsertStoresCiphertext(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTokenRepo(store)
	ctx := context.Background()

	u := seedUser(t, store, "u")
	tok := &domain.APIToken{
		ID:              newID(),
		UserID:          u.ID,
		TokenHash:       "hash1",
		TokenCiphertext: "ciphertext-base64",
		Prefix:          "dct_aaaaaaaa",
	}
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByUser: %v", err)
	}
	if got.TokenCiphertext != "ciphertext-base64" {
		t.Fatalf("ciphertext = %q, want ciphertext-base64", got.TokenCiphertext)
	}
}

func TestTokenRepoGetByUserPreV2Column(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTokenRepo(store)
	ctx := context.Background()

	u := seedUser(t, store, "u")
	// A pre-v2 row has no ciphertext (empty string).
	tok := &domain.APIToken{ID: newID(), UserID: u.ID, TokenHash: "hash1", Prefix: "dct_aaaaaaaa"}
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByUser: %v", err)
	}
	if got.TokenCiphertext != "" {
		t.Fatalf("ciphertext = %q, want empty", got.TokenCiphertext)
	}
}
