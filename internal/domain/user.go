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

	// OAuthGroups are the raw upstream provider group names captured at the last
	// successful OAuth login or revalidation (OIDC `groups` claim values; GitHub
	// org logins plus "org/team" team slugs). Informational only — surfaced in the
	// UI. Never used for authorization or membership reconciliation (that uses
	// OAuthGroupIDs).
	OAuthGroups []string `json:"oauth_groups,omitempty"`

	// OAuthAdmin records that the user's admin role was granted by the
	// auth.oauth.admin_groups allowlist (not manual promotion). Revalidation uses
	// it to demote OAuth-granted admins whose upstream groups no longer match,
	// while never auto-demoting a manually-promoted admin (OAuthAdmin=false).
	OAuthAdmin bool `json:"oauth_admin,omitempty"`

	// DeactivatedAt is set when IdP revalidation revokes access; identity
	// resolution and refresh reject deactivated users cluster-wide.
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`

	// DisabledAt is set by an explicit administrator Disable. Unlike
	// DeactivatedAt (IdP policy) a successful IdP login never clears it.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`

	// DeletedAt marks a soft-deleted (tombstoned) account. The row, username
	// and (provider, subject) identity are reserved forever; credentials and
	// memberships are removed. Restore clears only this timestamp.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`

	// Revision is the optimistic-concurrency counter for stale-write
	// protection. It is incremented on every accepted mutation and is not
	// exposed to browsers.
	Revision uint64 `json:"-"`

	// OAuthLoginVersion is incremented on every accepted OAuth login, even
	// when encryption is disabled and the credential ciphertext is empty. It
	// invalidates cached negative revalidation results for the same identity.
	OAuthLoginVersion uint64 `json:"-"`

	// TokenVersion is the server-side JWT revocation counter (ADR-045).
	// Every issued JWT carries the value at issue time; logout and password
	// changes increment it, and identity resolution rejects tokens whose
	// claim no longer matches. Pre-upgrade records lack the JSON key and
	// decode to 0, matching the 0 carried by legacy tokens.
	TokenVersion int `json:"token_version"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Deactivated reports whether the user's access has been revoked by IdP
// revalidation.
func (u *User) Deactivated() bool { return u != nil && u.DeactivatedAt != nil }

// AccessBlocked reports whether the account may not authenticate through any
// mechanism: administrator disable, soft deletion, or IdP revocation. A nil
// user is not blocked.
func (u *User) AccessBlocked() bool {
	if u == nil {
		return false
	}
	return u.DisabledAt != nil || u.DeletedAt != nil || u.DeactivatedAt != nil
}

// UserStateAction is an administrator account-lifecycle action.
type UserStateAction string

const (
	UserDisable UserStateAction = "disable"
	UserEnable  UserStateAction = "enable"
	UserRestore UserStateAction = "restore"
)

// OAuthUserMutationKind identifies the atomic account mutation applied by an
// OAuth login/revalidation. Each kind has a strict whitelist of writable
// fields; the FSM never copies all supplied fields blindly.
type OAuthUserMutationKind string

const (
	OAuthLoginCommit      OAuthUserMutationKind = "login"
	OAuthMembershipCommit OAuthUserMutationKind = "membership"
	OAuthCredentialCommit OAuthUserMutationKind = "credential"
	OAuthRevocationCommit OAuthUserMutationKind = "revoke"
)

// OAuthUserMutation is a revision-checked atomic mutation of a user record
// plus its OAuth-managed group edges.
type OAuthUserMutation struct {
	Kind             OAuthUserMutationKind
	User             *User
	ExpectedRevision uint64
	// ManagedGroupIDs is the desired OAuth-managed membership set; used by the
	// login and membership kinds only.
	ManagedGroupIDs []string
	// FallbackGroupID is applied by the login kind only, and only when the
	// effective membership is still empty. Empty means "no fallback".
	FallbackGroupID string
}

// OAuthUserResult is the committed snapshot returned by an OAuth mutation.
type OAuthUserResult struct {
	User *User
	// GroupIDs is the effective current membership after the mutation, sorted.
	// It is a copy and never aliases FSM-owned state.
	GroupIDs []string
}

// UserRepository is the persistence interface for users.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	Get(ctx context.Context, id string) (*User, error)
	GetByUsername(ctx context.Context, username string) (*User, error)
	GetByOAuth(ctx context.Context, provider, oauthID string) (*User, error)
	List(ctx context.Context) ([]*User, error)
	// Update persists u using u.Revision as an optimistic-concurrency
	// precondition and advances u.Revision on success.
	Update(ctx context.Context, u *User) error
	// Delete soft-deletes the account identified by id, recording actorID as
	// the administrator performing the action.
	Delete(ctx context.Context, actorID, id string) error
	// SetState applies an administrator lifecycle action atomically.
	SetState(ctx context.Context, actorID, id string, action UserStateAction) (*User, error)
	// CommitOAuth applies a revision-checked atomic OAuth account mutation.
	CommitOAuth(ctx context.Context, mutation OAuthUserMutation) (*OAuthUserResult, error)
	Count(ctx context.Context) (int, error)
}
