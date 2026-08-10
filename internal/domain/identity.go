package domain

import "errors"

// AuthMethod records how an Identity was resolved from a request.
type AuthMethod string

const (
	AuthNone      AuthMethod = "none" // auth disabled
	AuthJWT       AuthMethod = "jwt"
	AuthAPIToken  AuthMethod = "api_token"
	AuthLegacyTok AuthMethod = "legacy_token"
)

// Identity is the resolved principal for an incoming request. GroupIDs are
// re-fetched from the DB at resolve time (JWT claims can be stale).
type Identity struct {
	UserID   string
	Username string
	Role     Role
	GroupIDs []string
	Method   AuthMethod
}

// IsAdmin reports whether the identity has the admin role (which bypasses
// quota and visibility checks).
func (i *Identity) IsAdmin() bool {
	return i != nil && i.Role == RoleAdmin
}

// HasGroup reports whether the identity belongs to the given group.
func (i *Identity) HasGroup(groupID string) bool {
	if i == nil || groupID == "" {
		return false
	}
	for _, g := range i.GroupIDs {
		if g == groupID {
			return true
		}
	}
	return false
}

// Sentinel errors matched with errors.Is in handlers/services.
var (
	ErrUnauthenticated   = errors.New("unauthenticated")
	ErrForbidden         = errors.New("forbidden")
	ErrNoGroups          = errors.New("user is not a member of any group")
	ErrAgentUnavailable  = errors.New("engines not available for any of the user's groups")
	ErrQuotaExhausted    = errors.New("group runner session quota exhausted")
	ErrTokenExists       = errors.New("api token already exists")
	ErrNotFound          = errors.New("not found")
	ErrInvalidCredential = errors.New("invalid credentials")
	ErrValidation        = errors.New("validation error")
)
