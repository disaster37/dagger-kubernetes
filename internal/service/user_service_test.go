package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newUserService(t *testing.T) (*UserService, *repos) {
	t.Helper()
	_, r := newServiceDB(t)
	svc := NewUserService(r.users, r.groups, testLogger())
	return svc, r
}

func TestUserServiceCreateAndAuthenticate(t *testing.T) {
	svc, _ := newUserService(t)
	ctx := context.Background()

	u, err := svc.Create(ctx, "alice", "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID == "" || u.Role != domain.RoleUser {
		t.Fatalf("bad user: %+v", u)
	}
	if u.PasswordHash == "" || u.PasswordHash == "password123" {
		t.Fatal("password should be hashed")
	}

	got, err := svc.Authenticate(ctx, "alice", "password123")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("id mismatch")
	}
}

func TestUserServiceAuthenticateFailures(t *testing.T) {
	svc, _ := newUserService(t)
	ctx := context.Background()

	if _, err := svc.Authenticate(ctx, "nobody", "x"); err != domain.ErrInvalidCredential {
		t.Fatalf("unknown user: %v, want ErrInvalidCredential", err)
	}

	svc.Create(ctx, "bob", "password123", domain.RoleUser)
	if _, err := svc.Authenticate(ctx, "bob", "wrong"); err != domain.ErrInvalidCredential {
		t.Fatalf("wrong password: %v, want ErrInvalidCredential", err)
	}

	// OAuth user with empty password hash cannot authenticate.
	oauth, _, _ := svc.EnsureOAuthUser(ctx, "github", "1", "ghuser")
	if _, err := svc.Authenticate(ctx, oauth.Username, ""); err != domain.ErrInvalidCredential {
		t.Fatalf("oauth user pw: %v, want ErrInvalidCredential", err)
	}
}

func TestUserServiceValidation(t *testing.T) {
	svc, _ := newUserService(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		username string
		password string
		role     domain.Role
	}{
		{"bad username short", "a", "password123", domain.RoleUser},
		{"bad username chars", "ab cd", "password123", domain.RoleUser},
		{"short password", "alice", "short", domain.RoleUser},
		{"long password", "alice", strings.Repeat("x", maxPasswordLen+1), domain.RoleUser},
		{"bad role", "alice", "password123", "superadmin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Create(ctx, tc.username, tc.password, tc.role); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestUserServiceDuplicateUsername(t *testing.T) {
	svc, _ := newUserService(t)
	ctx := context.Background()
	svc.Create(ctx, "alice", "password123", domain.RoleUser)
	if _, err := svc.Create(ctx, "Alice", "password123", domain.RoleUser); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestUserServiceCRUD(t *testing.T) {
	svc, _ := newUserService(t)
	ctx := context.Background()

	u, _ := svc.Create(ctx, "alice", "password123", domain.RoleUser)

	updated, err := svc.UpdateRole(ctx, u.ID, domain.RoleAdmin)
	if err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if updated.Role != domain.RoleAdmin {
		t.Fatalf("role = %s", updated.Role)
	}

	if _, err := svc.UpdateRole(ctx, "nope", domain.RoleAdmin); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UpdateRole missing: %v", err)
	}

	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}

	if n, _ := svc.Count(ctx); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}

	if err := svc.ResetPassword(ctx, u.ID, "newpassword123"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "alice", "newpassword123"); err != nil {
		t.Fatalf("Authenticate after reset: %v", err)
	}

	if err := svc.ChangePassword(ctx, u.ID, "newpassword123", "anotherpw1"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "alice", "anotherpw1"); err != nil {
		t.Fatalf("Authenticate after change: %v", err)
	}
	if err := svc.ChangePassword(ctx, u.ID, "wrong", "x"); err == nil {
		t.Fatal("ChangePassword with short new pw should fail validation")
	}
	if err := svc.ChangePassword(ctx, u.ID, "wrongcurrent", "tooshort"); err == nil {
		t.Fatal("ChangePassword with short new pw should fail validation")
	}
	if err := svc.ChangePassword(ctx, u.ID, "wrongcurrent", "validnewpassword"); err != domain.ErrInvalidCredential {
		t.Fatalf("ChangePassword wrong current: %v, want ErrInvalidCredential", err)
	}

	if err := svc.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
}

func TestUserServiceEnsureOAuthUserCollision(t *testing.T) {
	svc, _ := newUserService(t)
	ctx := context.Background()

	// Pre-create a user named "ghuser" so the OAuth user must suffix.
	svc.Create(ctx, "ghuser", "password123", domain.RoleUser)

	u1, created1, err := svc.EnsureOAuthUser(ctx, "github", "1", "ghuser")
	if err != nil {
		t.Fatalf("EnsureOAuthUser 1: %v", err)
	}
	if !created1 {
		t.Fatal("should be created")
	}
	if u1.Username != "ghuser-2" {
		t.Fatalf("username = %q, want ghuser-2", u1.Username)
	}

	// Same OAuth id -> returns existing, not created.
	u2, created2, err := svc.EnsureOAuthUser(ctx, "github", "1", "ghuser")
	if err != nil {
		t.Fatalf("EnsureOAuthUser 2: %v", err)
	}
	if created2 {
		t.Fatal("should not be created again")
	}
	if u2.ID != u1.ID {
		t.Fatalf("id mismatch")
	}

	// Different OAuth id, same username collision -> suffix -3.
	u3, created3, err := svc.EnsureOAuthUser(ctx, "github", "2", "ghuser")
	if err != nil {
		t.Fatalf("EnsureOAuthUser 3: %v", err)
	}
	if !created3 {
		t.Fatal("should be created")
	}
	if u3.Username != "ghuser-3" {
		t.Fatalf("username = %q, want ghuser-3", u3.Username)
	}
}

func TestValidateUsername(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"alice", true},
		{"a", false}, // too short (needs 2+ chars total)
		{"ab", true}, // 2 chars ok
		{"1-_.ok", true},
		{"-bad", false},                  // starts with non-alnum
		{strings.Repeat("a", 65), false}, // too long (max 64)
		{"has space", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateUsername(tc.name)
			if tc.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
