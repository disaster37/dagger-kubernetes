package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestUserRepoCRUD(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewUserRepo(store)
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

	// Add a second active administrator so the last-admin guard does not reject
	// the delete (u was promoted to admin above).
	other := &domain.User{ID: newID(), Username: "root", Role: domain.RoleAdmin}
	if err := repo.Create(ctx, other); err != nil {
		t.Fatalf("Create second admin: %v", err)
	}
	if err := repo.Delete(ctx, "", u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deleted, err := repo.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get after soft delete: %v", err)
	}
	if deleted.DeletedAt == nil || deleted.PasswordHash != "" || len(deleted.OAuthGroupIDs) != 0 {
		t.Fatalf("soft delete did not tombstone: %+v", deleted)
	}
	// Identity/name reservations survive the tombstone.
	if _, err := repo.GetByOAuth(ctx, "github", "42"); err != nil {
		t.Fatalf("GetByOAuth after soft delete: %v", err)
	}
	// Idempotent on an existing tombstone.
	if err := repo.Delete(ctx, "", u.ID); err != nil {
		t.Fatalf("Delete tombstone: %v", err)
	}
	// Unknown user still ErrNotFound.
	if err := repo.Delete(ctx, "", "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete missing: %v, want ErrNotFound", err)
	}
	// A tombstone cannot be resurrected through a general update.
	if err := repo.Update(ctx, got); !errors.Is(err, domain.ErrAccountBlocked) {
		t.Fatalf("Update tombstone: %v, want ErrAccountBlocked", err)
	}
}

func TestUserRepoDuplicateUsername(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewUserRepo(store)
	ctx := context.Background()

	seedUser(t, store, "bob")

	dup := &domain.User{ID: newID(), Username: "Bob", Role: domain.RoleUser}
	if err := repo.Create(ctx, dup); err == nil {
		t.Fatal("expected duplicate-username error")
	}
}

func TestUserRepoGetMissing(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewUserRepo(store)
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

func TestUserRepoCommitOAuthSetStateDelete(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewUserRepo(store)
	ctx := context.Background()

	u := &domain.User{ID: newID(), Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc", OAuthID: "s1"}
	if err := repo.Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Login commit advances the login generation and returns the committed user.
	res, err := repo.CommitOAuth(ctx, domain.OAuthUserMutation{
		Kind:             domain.OAuthLoginCommit,
		User:             &domain.User{ID: u.ID, OAuthProvider: "oidc", OAuthID: "s1", Role: domain.RoleUser, OAuthTokenCiphertext: "ct"},
		ExpectedRevision: u.Revision,
	})
	if err != nil {
		t.Fatalf("CommitOAuth login: %v", err)
	}
	if res.User.OAuthLoginVersion != 1 {
		t.Fatalf("login version = %d, want 1", res.User.OAuthLoginVersion)
	}

	// Membership commit with the returned revision succeeds; a stale revision
	// is rejected with zero effect.
	if _, err := repo.CommitOAuth(ctx, domain.OAuthUserMutation{
		Kind:             domain.OAuthMembershipCommit,
		User:             &domain.User{ID: u.ID, Role: domain.RoleUser},
		ExpectedRevision: res.User.Revision,
	}); err != nil {
		t.Fatalf("CommitOAuth membership: %v", err)
	}
	if _, err := repo.CommitOAuth(ctx, domain.OAuthUserMutation{
		Kind:             domain.OAuthMembershipCommit,
		User:             &domain.User{ID: u.ID, Role: domain.RoleUser},
		ExpectedRevision: res.User.Revision,
	}); !errors.Is(err, domain.ErrStaleUser) {
		t.Fatalf("stale membership commit: %v, want ErrStaleUser", err)
	}

	// SetState disable then enable.
	if _, err := repo.SetState(ctx, "actor", u.ID, domain.UserDisable); err != nil {
		t.Fatalf("SetState disable: %v", err)
	}
	enabled, err := repo.SetState(ctx, "actor", u.ID, domain.UserEnable)
	if err != nil {
		t.Fatalf("SetState enable: %v", err)
	}
	if enabled.DisabledAt != nil {
		t.Fatal("enable must clear DisabledAt")
	}

	// Revocation commit, then soft delete.
	if _, err := repo.CommitOAuth(ctx, domain.OAuthUserMutation{
		Kind:             domain.OAuthRevocationCommit,
		User:             &domain.User{ID: u.ID},
		ExpectedRevision: enabled.Revision,
	}); err != nil {
		t.Fatalf("CommitOAuth revoke: %v", err)
	}
	if err := repo.Delete(ctx, "actor", u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deleted, err := repo.Get(ctx, u.ID)
	if err != nil || deleted.DeletedAt == nil {
		t.Fatalf("tombstone = %+v err=%v", deleted, err)
	}
}
