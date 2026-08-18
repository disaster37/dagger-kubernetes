package repository

import (
	"context"
	"fmt"
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
	if !domain.ValidTraceIDKey(traceID) {
		return fmt.Errorf("invalid trace ID: %w", domain.ErrValidation)
	}
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
	if !domain.ValidTraceIDKey(m.TraceID) {
		return fmt.Errorf("invalid trace ID: %w", domain.ErrValidation)
	}
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

// ListBefore returns trace_meta rows older than cutoff (see FSM helper).
func (r *TraceMetaRepo) ListBefore(ctx context.Context, cutoff time.Time, protectRunning bool) ([]*domain.TraceMeta, error) {
	return r.store.fsmRead().listTracesBefore(cutoff, protectRunning), nil
}

// Delete removes a single trace_meta row. Idempotent: the FSM delete(map, key)
// on a missing key is a no-op (returns nil).
func (r *TraceMetaRepo) Delete(ctx context.Context, traceID string) error {
	return r.store.applyCtx(ctx, kindDeleteTrace, cmdDeleteTrace{TraceID: traceID})
}

// Stats returns the total trace count and the oldest trace sort key.
func (r *TraceMetaRepo) Stats(ctx context.Context) (int, time.Time, error) {
	count, oldest := r.store.fsmRead().traceStats()
	return count, oldest, nil
}
