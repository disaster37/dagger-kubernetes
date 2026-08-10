package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Role is the authorization role assigned to a user.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// errInvalidRole is the sentinel wrapped by ParseRole for unknown roles.
var errInvalidRole = errors.New("must be admin or user")

// ParseRole validates and returns a Role. Anything other than "admin" or
// "user" yields an error.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if r == RoleAdmin || r == RoleUser {
		return r, nil
	}
	return "", fmt.Errorf("invalid role %q: %w", s, errInvalidRole)
}

// User is a platform user (human or CI identity). Stored in SQLite.
type User struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Role          Role      `json:"role"`
	PasswordHash  string    `json:"-"`
	OAuthProvider string    `json:"oauth_provider,omitempty"`
	OAuthID       string    `json:"oauth_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// UserRepository is the persistence interface for users.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	Get(ctx context.Context, id string) (*User, error)
	GetByUsername(ctx context.Context, username string) (*User, error)
	GetByOAuth(ctx context.Context, provider, oauthID string) (*User, error)
	List(ctx context.Context) ([]*User, error)
	Update(ctx context.Context, u *User) error
	Delete(ctx context.Context, id string) error
	Count(ctx context.Context) (int, error)
}
