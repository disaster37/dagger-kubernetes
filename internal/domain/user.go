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

// User is a platform user (human or CI identity). Stored in the Raft FSM.
type User struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Role          Role   `json:"role"`
	PasswordHash  string `json:"-"`
	OAuthProvider string `json:"oauth_provider,omitempty"`
	OAuthID       string `json:"oauth_id,omitempty"`

	// OAuthTokenCiphertext is AES-256-GCM(nonce||ciphertext||tag), base64, of
	// the JSON-encoded service.oauthCredential (access + optional refresh
	// token) captured at OAuth login. Empty for pre-revalidation users and for
	// non-OAuth users.
	OAuthTokenCiphertext string `json:"-"`

	// OAuthGroupIDs are the supervisor group IDs currently auto-managed by
	// OAuth group mapping (see ADR-027). Reconciliation only adds/removes
	// memberships within this set; admin-managed memberships are never touched.
	OAuthGroupIDs []string `json:"oauth_group_ids,omitempty"`

	// DeactivatedAt is set when IdP revalidation revokes access; identity
	// resolution and refresh reject deactivated users cluster-wide.
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Deactivated reports whether the user's access has been revoked by IdP
// revalidation.
func (u *User) Deactivated() bool { return u != nil && u.DeactivatedAt != nil }

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
