package domain

import (
	"context"
	"time"
)

type Lease struct {
	CertFP       string
	OrgID        string
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
	Register(orgID, certFP, version, replicaPod, instanceID, traceID, userID string) *Lease
	Get(certFP string) (*Lease, error)
	Touch(certFP string) error
	IncInFlight(certFP string) error
	DecInFlight(certFP string) error
	// DecInFlightAndGet decrements the in-flight count for certFP and returns
	// the resulting count. Returns 0 and a non-nil error when the lease is gone.
	DecInFlightAndGet(certFP string) (int, error)
	Remove(certFP string)
	PinnedSessionsOnReplica(podName string) int
	CountByUser(userID string) int
	// CountByOrg returns the number of active leases in orgID.
	CountByOrg(orgID string) int
	// RemoveByUserAndOrg removes and returns the cert fingerprints of
	// userID's leases in orgID.
	RemoveByUserAndOrg(userID, orgID string) []string
	// RemoveByOrg removes and returns the cert fingerprints of every lease in
	// orgID.
	RemoveByOrg(orgID string) []string
	// SetGroupID records the display-aid group on an existing lease. It must
	// be used instead of mutating the *Lease returned by Register, which is
	// shared with the store and read concurrently.
	SetGroupID(certFP, groupID string)
	List() []*Lease
}

// SessionRegistry persists session-lease mutations through the replicated
// store (Raft) so every pod's local SessionStore mirrors the cluster state.
// Multi-pod data-plane routing depends on it: a tunnel can arrive on any pod,
// and the pod must find the lease for the client certificate.
type SessionRegistry interface {
	Register(ctx context.Context, orgID, certFP, version, replicaPod, instanceID, traceID, userID, groupID string) error
	Touch(ctx context.Context, certFP string) error
}

// SessionStateSink receives session-lease updates replicated by the Raft FSM.
// Implementations update the pod-local SessionStore deterministically (the
// payload timestamps come from the leader, so replay produces identical state
// on every pod).
type SessionStateSink interface {
	ApplySessionRegistered(lease *Lease)
	ApplySessionTouched(certFP string, at time.Time)
	// ApplyBootstrapOrg backfills Lease.OrgID on leases replayed from main's
	// log tail before kindBootstrapOrg.
	ApplyBootstrapOrg(bootstrapOrgID string)
	// ApplyUserOrgRevoked evicts userID's leases in orgID.
	ApplyUserOrgRevoked(orgID, userID string)
	// ApplyOrgDeleted evicts every lease in orgID.
	ApplyOrgDeleted(orgID string)
}
