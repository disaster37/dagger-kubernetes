package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// traceMetaCols is the canonical column list shared by all trace_meta queries.
const traceMetaCols = `trace_id, user_id, group_id, project_name, status, version, ci_provider, ci_repo, duration_ms, started_at, updated_at`

// TraceMetaRepo is the SQLite implementation of domain.TraceMetaRepository.
type TraceMetaRepo struct {
	db *sql.DB
}

var _ domain.TraceMetaRepository = (*TraceMetaRepo)(nil)

// NewTraceMetaRepo returns a TraceMetaRepo backed by db.
func NewTraceMetaRepo(db *sql.DB) *TraceMetaRepo {
	return &TraceMetaRepo{db: db}
}

// UpsertProvision records trace_id -> user_id at engine provision time. The
// user_id is set only when the existing row's user_id is empty (first writer
// wins), so a later re-provision by a different user does not steal the trace.
func (r *TraceMetaRepo) UpsertProvision(ctx context.Context, traceID, userID string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO trace_meta(trace_id, user_id, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(trace_id) DO UPDATE SET user_id = COALESCE(NULLIF(trace_meta.user_id, ''), excluded.user_id), updated_at = excluded.updated_at`,
		traceID, nullString(userID), time.Now().UTC())
	if err != nil {
		return fmt.Errorf("upsert provision %s: %w", traceID, err)
	}
	return nil
}

// UpsertIngest enriches an existing trace_meta row with OTLP-derived fields.
// The group_id is set once (first non-empty writer wins via COALESCE); other
// fields take the newer non-empty value.
func (r *TraceMetaRepo) UpsertIngest(ctx context.Context, m *domain.TraceMeta) error {
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now().UTC()
	}
	var startedAt any
	if !m.StartedAt.IsZero() {
		startedAt = m.StartedAt
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO trace_meta(%s)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trace_id) DO UPDATE SET
			group_id = COALESCE(NULLIF(trace_meta.group_id, ''), excluded.group_id),
			project_name = COALESCE(NULLIF(excluded.project_name, ''), trace_meta.project_name),
			status = COALESCE(NULLIF(excluded.status, ''), trace_meta.status),
			version = COALESCE(NULLIF(excluded.version, ''), trace_meta.version),
			ci_provider = COALESCE(NULLIF(excluded.ci_provider, ''), trace_meta.ci_provider),
			ci_repo = COALESCE(NULLIF(excluded.ci_repo, ''), trace_meta.ci_repo),
			duration_ms = CASE WHEN excluded.duration_ms != 0 THEN excluded.duration_ms ELSE trace_meta.duration_ms END,
			started_at = COALESCE(excluded.started_at, trace_meta.started_at),
			updated_at = excluded.updated_at`, traceMetaCols),
		m.TraceID, nullString(m.UserID), nullString(m.GroupID), m.ProjectName, m.Status, m.Version, m.CIProvider, m.CIRepo, m.DurationMS, startedAt, m.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert ingest %s: %w", m.TraceID, err)
	}
	return nil
}

func (r *TraceMetaRepo) Get(ctx context.Context, traceID string) (*domain.TraceMeta, error) {
	m, err := scanTraceMeta(r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM trace_meta WHERE trace_id = ?`, traceMetaCols), traceID))
	if err != nil {
		return nil, fmt.Errorf("get trace_meta %s: %w", traceID, err)
	}
	return m, nil
}

// List returns trace metadata scoped by the filter, joined with group/user
// names for display. Admin (no GroupIDs) sees all; a user sees traces in their
// groups plus their own unassigned traces.
func (r *TraceMetaRepo) List(ctx context.Context, f domain.TraceFilter) ([]*domain.TraceListResult, error) {
	where, args := traceFilterWhere(f)

	//nolint:gosec // G201: column list is a static constant, not user input.
	q := fmt.Sprintf(`SELECT %s, COALESCE(g.name, ''), COALESCE(u.username, '')
		FROM trace_meta tm
		LEFT JOIN groups g ON g.id = tm.group_id
		LEFT JOIN users u ON u.id = tm.user_id`, prefixedColumns("tm", traceMetaCols))
	if where != "" {
		q = fmt.Sprintf("%s WHERE %s", q, where)
	}
	q = fmt.Sprintf("%s ORDER BY COALESCE(tm.started_at, tm.updated_at) DESC LIMIT ?", q)
	args = append(args, clampLimit(f.Limit))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list trace_meta: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*domain.TraceListResult
	for rows.Next() {
		m := &domain.TraceListResult{}
		if err := scanTraceMetaInto(rows, &m.TraceMeta, &m.GroupName, &m.Username); err != nil {
			return nil, fmt.Errorf("scan trace_meta: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trace_meta rows: %w", err)
	}
	return out, nil
}

// traceFilterWhere builds the WHERE clause (without the WHERE keyword) and
// bind args for a TraceFilter. An empty clause means no restriction (admin).
func traceFilterWhere(f domain.TraceFilter) (where string, args []any) {
	switch {
	case f.UnassignedOnly:
		// Admin "unassigned" view: only traces without a group.
		where = "tm.group_id IS NULL"
	case len(f.GroupIDs) > 0:
		for _, gid := range f.GroupIDs {
			args = append(args, gid)
		}
		where = fmt.Sprintf("tm.group_id IN (%s)", strings.TrimRight(strings.Repeat("?,", len(f.GroupIDs)), ","))
		if f.UserID != "" {
			where = fmt.Sprintf("%s OR (tm.group_id IS NULL AND tm.user_id = ?)", where)
			args = append(args, f.UserID)
		}
	case !f.IncludeUnassigned && f.UserID != "":
		// Non-admin fallback (defensive; admin path sets IncludeUnassigned).
		where = "tm.user_id = ?"
		args = append(args, f.UserID)
	}
	return where, args
}

// clampLimit bounds n to the [1, MaxTraceLimit] range, defaulting to
// DefaultTraceLimit for non-positive values.
func clampLimit(n int) int {
	if n <= 0 {
		return domain.DefaultTraceLimit
	}
	if n > domain.MaxTraceLimit {
		return domain.MaxTraceLimit
	}
	return n
}

// scanTraceMeta scans a single trace_meta row.
func scanTraceMeta(row scanner) (*domain.TraceMeta, error) {
	m := &domain.TraceMeta{}
	if err := scanTraceMetaInto(row, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("trace_meta: %w", domain.ErrNotFound)
		}
		return nil, err
	}
	return m, nil
}

// scanTraceMetaInto scans a trace_meta row (plus any joined display columns)
// into m. Nullable columns map to their zero value when unset.
func scanTraceMetaInto(row scanner, m *domain.TraceMeta, extra ...any) error {
	var userID, groupID sql.NullString
	var startedAt sql.NullTime
	dests := make([]any, 0, 11+len(extra))
	dests = append(dests, &m.TraceID, &userID, &groupID, &m.ProjectName, &m.Status, &m.Version, &m.CIProvider, &m.CIRepo, &m.DurationMS, &startedAt, &m.UpdatedAt)
	dests = append(dests, extra...)
	if err := row.Scan(dests...); err != nil {
		return err
	}
	m.UserID = userID.String
	m.GroupID = groupID.String
	m.StartedAt = startedAt.Time
	return nil
}
