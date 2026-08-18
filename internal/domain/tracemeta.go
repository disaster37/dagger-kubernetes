package domain

import (
	"context"
	"regexp"
	"time"
)

// validTraceIDRe bounds client-supplied trace IDs: they are persisted as the
// trace_meta primary key and reflected into delete selectors, so their length
// and charset are constrained. Real Dagger trace IDs are 32-char hex; the
// 16..128 range tolerates shorter/longer hex IDs while rejecting unbounded
// inputs that would blow up delete-selector URLs or FSM memory (CWE-20/CWE-400).
var validTraceIDRe = regexp.MustCompile(`^[a-fA-F0-9]{16,128}$`)

// ValidTraceID reports whether id is a syntactically valid trace ID (hex, at
// least 16 chars) — the same shape the Loki/VictoriaMetrics delete paths and
// the trace_meta primary key require.
func ValidTraceID(id string) bool {
	return validTraceIDRe.MatchString(id)
}

// validTraceIDKeyRe bounds the tolerant client-supplied trace_meta primary key
// charset: a leading alphanumeric followed by up to 127 alphanumerics, dots,
// underscores, or hyphens (CWE-20/CWE-770). This is NOT the hex-only
// delete-target charset enforced by ValidTraceID.
var validTraceIDKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidTraceIDKey reports whether id is a valid trace_meta primary key (the
// tolerant client-supplied charset; NOT the hex-only delete-target charset
// enforced by ValidTraceID).
func ValidTraceIDKey(id string) bool {
	return validTraceIDKeyRe.MatchString(id)
}

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
	UpsertProvision(ctx context.Context, traceID, userID, version string) error
	UpsertIngest(ctx context.Context, m *TraceMeta) error
	Get(ctx context.Context, traceID string) (*TraceMeta, error)
	List(ctx context.Context, f TraceFilter) ([]*TraceListResult, error)

	// ListBefore returns trace_meta rows whose COALESCE(started_at, updated_at)
	// is older than cutoff. When protectRunning is true, rows with status ""
	// or "running" are excluded. Used by the history GC sweeper to enumerate
	// purge candidates without the 500-row List cap.
	ListBefore(ctx context.Context, cutoff time.Time, protectRunning bool) ([]*TraceMeta, error)

	// Delete removes a single trace_meta row. Idempotent: a missing row
	// returns nil.
	Delete(ctx context.Context, traceID string) error

	// Stats returns the total trace count and the oldest COALESCE(started_at,
	// updated_at) timestamp (zero time when no traces exist). Cheap FSM read.
	Stats(ctx context.Context) (count int, oldest time.Time, err error)
}
