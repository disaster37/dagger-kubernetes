package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestUserRepoCRUD(t *testing.T) {
	db := newTestDB(t)
	repo := NewUserRepo(db)
	ctx := context.Background()

	u := &domain.User{
		ID:            newID(),
		Username:      "Alice",
		Role:          domain.RoleUser,
		PasswordHash:  "hash",
		OAuthProvider: "github",
		OAuthID:       "42",
	}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Username != "Alice" {
		t.Fatalf("username = %q", got.Username)
	}
	if got.Role != domain.RoleUser {
		t.Fatalf("role = %q", got.Role)
	}
	if got.PasswordHash != "hash" {
		t.Fatalf("password_hash = %q", got.PasswordHash)
	}

	byName, err := repo.GetByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetByUsername (case-insensitive): %v", err)
	}
	if byName.ID != u.ID {
		t.Fatalf("id mismatch")
	}

	byOAuth, err := repo.GetByOAuth(ctx, "github", "42")
	if err != nil {
		t.Fatalf("GetByOAuth: %v", err)
	}
	if byOAuth.ID != u.ID {
		t.Fatalf("oauth id mismatch")
	}

	got.Role = domain.RoleAdmin
	got.PasswordHash = "newhash"
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, _ := repo.Get(ctx, u.ID)
	if updated.Role != domain.RoleAdmin || updated.PasswordHash != "newhash" {
		t.Fatalf("update did not persist: role=%s hash=%s", updated.Role, updated.PasswordHash)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}

	n, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}

	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete missing: %v, want ErrNotFound", err)
	}
	if err := repo.Update(ctx, got); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Update missing: %v, want ErrNotFound", err)
	}
}

func TestUserRepoDuplicateUsername(t *testing.T) {
	db := newTestDB(t)
	repo := NewUserRepo(db)
	ctx := context.Background()

	seedUser(t, db, "bob")

	dup := &domain.User{ID: newID(), Username: "Bob", Role: domain.RoleUser}
	if err := repo.Create(ctx, dup); err == nil {
		t.Fatal("expected duplicate-username error")
	}
}

func TestUserRepoGetMissing(t *testing.T) {
	db := newTestDB(t)
	repo := NewUserRepo(db)
	ctx := context.Background()

	if _, err := repo.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByUsername(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByUsername missing: %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByOAuth(ctx, "github", "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByOAuth missing: %v, want ErrNotFound", err)
	}
}
