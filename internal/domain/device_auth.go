package domain

import (
	"errors"
	"time"
)

// RFC 8628 §3.5 / RFC 6749 §5.2 device-token exchange sentinel errors. The
// handler maps these onto OAuth error codes so clients such as
// golang.org/x/oauth2 Config.DeviceAccessToken can react correctly (keep
// polling on ErrAuthorizationPending, stop on the others).
var (
	// ErrAuthorizationPending indicates the user has not yet completed approval
	// for an unexpired, unconsumed device code.
	ErrAuthorizationPending = errors.New("authorization pending")
	// ErrExpiredDeviceToken indicates the device code has passed ExpiresAt.
	ErrExpiredDeviceToken = errors.New("device code expired")
	// ErrInvalidGrant indicates an unknown, malformed, already-consumed, or
	// resource-owner-invalidated authorization grant.
	ErrInvalidGrant = errors.New("invalid grant")
)

// DeviceAuthorization is an RFC 8628 device-code grant. Codes are ephemeral
// (default 10m) and single-use.
type DeviceAuthorization struct {
	DeviceCode string        `json:"device_code"`
	UserCode   string        `json:"user_code"`
	UserID     string        `json:"user_id,omitempty"`
	Approved   bool          `json:"approved"`
	Consumed   bool          `json:"consumed"`
	ExpiresAt  time.Time     `json:"expires_at"`
	Interval   time.Duration `json:"interval"`
}

// DeviceAuthStore persists short-lived device authorizations.
type DeviceAuthStore interface {
	Create(a *DeviceAuthorization) error
	GetByDeviceCode(code string) (*DeviceAuthorization, error)
	GetByUserCode(code string) (*DeviceAuthorization, error)
	Approve(userCode, userID string) error
	Consume(deviceCode string) error
}
