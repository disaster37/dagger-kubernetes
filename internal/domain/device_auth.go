package domain

import "time"

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
