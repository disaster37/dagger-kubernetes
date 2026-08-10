package domain

import "time"

type Lease struct {
	CertFP       string
	Version      string
	ReplicaPod   string
	InstanceID   string
	LastActivity time.Time
	InFlight     int
	TraceID      string
	UserID       string // owning user ("" for legacy/anonymous)
	GroupID      string // set only when owner has exactly one group (display aid)
}

type SessionStore interface {
	Register(certFP, version, replicaPod, instanceID, traceID, userID string) *Lease
	Get(certFP string) (*Lease, error)
	Touch(certFP string) error
	IncInFlight(certFP string) error
	DecInFlight(certFP string) error
	Remove(certFP string)
	PinnedSessionsOnReplica(podName string) int
	CountByUser(userID string) int
	// SetGroupID records the display-aid group on an existing lease. It must
	// be used instead of mutating the *Lease returned by Register, which is
	// shared with the store and read concurrently.
	SetGroupID(certFP, groupID string)
	List() []*Lease
}
