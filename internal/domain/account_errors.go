package domain

import "errors"

// Account-lifecycle sentinel errors. They are deliberately distinct from
// ErrForbidden (which means OAuth allowlist failure) and from
// ErrUnauthenticated so handlers can map them to precise, non-leaking HTTP
// responses.
var (
	// ErrStaleUser indicates an optimistic-concurrency conflict: the target
	// record changed after it was read. The failed mutation had no effect.
	ErrStaleUser = errors.New("user record was modified concurrently")
	// ErrAccountBlocked indicates the account is administratively disabled,
	// softly deleted, or IdP-deactivated and may not be used.
	ErrAccountBlocked = errors.New("account access is disabled")
	// ErrLastAdmin indicates an action would remove the last active
	// administrator and was rejected atomically.
	ErrLastAdmin = errors.New("cannot remove the last active administrator")
)
