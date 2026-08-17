package repository

import (
	"context"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TraceMetaRepo is the Raft implementation of domain.TraceMetaRepository.
type TraceMetaRepo struct {
	store *RaftStore
}

var _ domain.TraceMetaRepository = (*TraceMetaRepo)(nil)

// NewTraceMetaRepo returns a TraceMetaRepo backed by store.
func NewTraceMetaRepo(store *RaftStore) *TraceMetaRepo {
	return &TraceMetaRepo{store: store}
}

// UpsertProvision records trace_id -> user_id (and the engine version) at
// provision time. The user_id is set only when the existing row's user_id is
// empty (first writer wins); the version follows the same first-non-empty-wins
// rule so a later empty version never wipes a recorded one.
func (r *TraceMetaRepo) UpsertProvision(ctx context.Context, traceID, userID, version string) error {
	return r.store.applyCtx(ctx, kindUpsertTraceProvision, cmdUpsertTraceProvision{
		TraceID:   traceID,
		UserID:    userID,
		Version:   version,
		UpdatedAt: time.Now().UTC(),
	})
}

// UpsertIngest enriches an existing trace_meta row with OTLP-derived fields.
// The group_id is set once (first non-empty writer wins); other fields take
// the newer non-empty value.
func (r *TraceMetaRepo) UpsertIngest(ctx context.Context, m *domain.TraceMeta) error {
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now().UTC()
	}
	return r.store.applyCtx(ctx, kindUpsertTraceIngest, m)
}

func (r *TraceMetaRepo) Get(ctx context.Context, traceID string) (*domain.TraceMeta, error) {
	return r.store.fsmRead().readTrace(traceID)
}

// List returns trace metadata scoped by the filter, joined with group/user
// names for display.
func (r *TraceMetaRepo) List(ctx context.Context, f domain.TraceFilter) ([]*domain.TraceListResult, error) {
	return r.store.fsmRead().listTraces(f), nil
}
