package domain

import (
	"context"
	"time"
)

// TraceMeta is the persisted metadata for a trace, used for scoped listing
// and visibility checks.
type TraceMeta struct {
	TraceID     string    `json:"trace_id"`
	UserID      string    `json:"user_id,omitempty"`
	GroupID     string    `json:"group_id,omitempty"`
	ProjectName string    `json:"project_name,omitempty"`
	Status      string    `json:"status,omitempty"`
	Version     string    `json:"version,omitempty"`
	CIProvider  string    `json:"ci_provider,omitempty"`
	CIRepo      string    `json:"ci_repo,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TraceListResult enriches TraceMeta with joined display names.
type TraceListResult struct {
	TraceMeta
	GroupName string `json:"group_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Listing limits shared by the handler (query parsing) and the repository
// (defensive clamping).
const (
	DefaultTraceLimit = 100
	MaxTraceLimit     = 500
)

// TraceFilter scopes a trace listing query.
type TraceFilter struct {
	GroupIDs          []string // empty = no group restriction (admin)
	IncludeUnassigned bool     // admin: true (all); user: only their own user_id fallback
	UnassignedOnly    bool     // admin "unassigned" view: only traces without a group
	UserID            string   // owner fallback for unassigned traces
	Limit             int      // default DefaultTraceLimit, max MaxTraceLimit
}

// TraceMetaRepository is the persistence interface for trace metadata.
type TraceMetaRepository interface {
	UpsertProvision(ctx context.Context, traceID, userID string) error
	UpsertIngest(ctx context.Context, m *TraceMeta) error
	Get(ctx context.Context, traceID string) (*TraceMeta, error)
	List(ctx context.Context, f TraceFilter) ([]*TraceListResult, error)
}
