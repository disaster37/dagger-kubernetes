package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// SessionRepo persists session-lease mutations through Raft so every pod's
// local session store mirrors the cluster state. Without this, a data-plane
// tunnel landing on a pod that did not handle the original engine-provision
// request cannot resolve the client certificate to its lease ("lease not
// found"), because the in-memory store is pod-local.
type SessionRepo struct {
	store *RaftStore
}

var _ domain.SessionRegistry = (*SessionRepo)(nil)

// NewSessionRepo returns a SessionRepo backed by store.
func NewSessionRepo(store *RaftStore) *SessionRepo {
	return &SessionRepo{store: store}
}

// maxSessionFieldLen bounds the free-form lease fields (trace id, user id,
// group id, instance id) before they are persisted, mirroring the input
// caps used by the other repos (CWE-770).
const maxSessionFieldLen = 256

// Register persists a new session lease (or replaces the lease for the same
// certificate fingerprint). The FSM forwards it to every pod's local session
// store, so any pod can later serve the data-plane tunnel for certFP.
func (r *SessionRepo) Register(ctx context.Context, certFP, version, replicaPod, instanceID, traceID, userID, groupID string) error {
	if certFP == "" {
		return fmt.Errorf("empty cert fingerprint: %w", domain.ErrValidation)
	}
	lease := domain.Lease{
		CertFP:     certFP,
		Version:    version,
		ReplicaPod: replicaPod,
		InstanceID: clipField(instanceID),
		TraceID:    clipField(traceID),
		UserID:     clipField(userID),
		GroupID:    clipField(groupID),
	}
	return r.store.applyCtx(ctx, kindUpsertSession, cmdUpsertSession{
		Lease: lease,
		At:    time.Now().UTC(),
	})
}

// Touch refreshes the lease's LastActivity through Raft so every pod's
// reaper and the fleet sweeper see the session as live (the tunnel heartbeat
// may run on any pod).
func (r *SessionRepo) Touch(ctx context.Context, certFP string) error {
	if certFP == "" {
		return fmt.Errorf("empty cert fingerprint: %w", domain.ErrValidation)
	}
	return r.store.applyCtx(ctx, kindTouchSession, cmdTouchSession{
		CertFP: certFP,
		At:     time.Now().UTC(),
	})
}

// clipField bounds a free-form lease field to maxSessionFieldLen bytes.
func clipField(s string) string {
	if len(s) <= maxSessionFieldLen {
		return s
	}
	return s[:maxSessionFieldLen]
}
