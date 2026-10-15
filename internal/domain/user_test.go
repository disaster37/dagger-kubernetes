package domain

import (
	"errors"
	"testing"
	"time"
)

func TestUserAccessBlocked(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		user *User
		want bool
	}{
		{name: "nil user is not blocked", user: nil, want: false},
		{name: "active", user: &User{}, want: false},
		{name: "administrator disabled", user: &User{DisabledAt: &now}, want: true},
		{name: "soft deleted", user: &User{DeletedAt: &now}, want: true},
		{name: "idp deactivated", user: &User{DeactivatedAt: &now}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.user.AccessBlocked(); got != tt.want {
				t.Fatalf("AccessBlocked() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserDeactivated(t *testing.T) {
	now := time.Now().UTC()
	if (&User{}).Deactivated() {
		t.Fatal("active user must not be Deactivated()")
	}
	if !(&User{DeactivatedAt: &now}).Deactivated() {
		t.Fatal("deactivated user must report Deactivated()")
	}
	// A disabled account is blocked but not Deactivated: the two concepts stay
	// distinct (a successful IdP login may clear only DeactivatedAt).
	disabled := &User{DisabledAt: &now}
	if disabled.Deactivated() {
		t.Fatal("disabled user must not be Deactivated()")
	}
	if !disabled.AccessBlocked() {
		t.Fatal("disabled user must be AccessBlocked()")
	}
}

func TestAccountErrorsAreDistinct(t *testing.T) {
	// The account-lifecycle sentinels must not alias each other or the OAuth
	// allowlist error, so handlers can map them to distinct statuses.
	for _, pair := range [][2]error{
		{ErrStaleUser, ErrAccountBlocked},
		{ErrStaleUser, ErrLastAdmin},
		{ErrAccountBlocked, ErrLastAdmin},
		{ErrAccountBlocked, ErrForbidden},
		{ErrAccountBlocked, ErrUnauthenticated},
	} {
		if errors.Is(pair[0], pair[1]) {
			t.Fatalf("%v must not match %v", pair[0], pair[1])
		}
	}
}

func TestUserStateActionConstants(t *testing.T) {
	if UserDisable != "disable" || UserEnable != "enable" || UserRestore != "restore" {
		t.Fatalf("unexpected state action constants: %q %q %q", UserDisable, UserEnable, UserRestore)
	}
}

func TestOAuthUserMutationKindConstants(t *testing.T) {
	kinds := map[OAuthUserMutationKind]string{
		OAuthLoginCommit:      "login",
		OAuthMembershipCommit: "membership",
		OAuthCredentialCommit: "credential",
		OAuthRevocationCommit: "revoke",
	}
	for kind, want := range kinds {
		if string(kind) != want {
			t.Fatalf("kind %v = %q, want %q", kind, kind, want)
		}
	}
}
