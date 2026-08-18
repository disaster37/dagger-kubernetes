package domain

import (
	"context"
	"errors"
)

// ErrHistoryPurgeRunning is returned when a purge-all/sweeper attempt is
// already in flight (concurrency guard). Lives in domain so the handler can
// map it to 409.
var ErrHistoryPurgeRunning = errors.New("history purge already in progress")

// HistoryStats is the payload of GET /api/v1/history.
type HistoryStats struct {
	TraceCount      int            `json:"trace_count"`
	OldestUpdatedAt string         `json:"oldest_updated_at,omitempty"` // RFC3339; "" when no traces
	CollectedAt     string         `json:"collected_at"`                // RFC3339 UTC
	GC              HistoryGCRules `json:"gc"`
}

// HistoryGCRules describes the history auto-purge config + last/next run.
type HistoryGCRules struct {
	Enabled        bool                 `json:"enabled"`
	MaxAge         string               `json:"max_age"`               // duration string e.g. "720h"
	Schedule       string               `json:"schedule"`              // duration string e.g. "1h"
	LastRunAt      string               `json:"last_run_at,omitempty"` // RFC3339
	LastRunSummary *HistoryGCRunSummary `json:"last_run_summary,omitempty"`
	NextRunAt      string               `json:"next_run_at,omitempty"` // RFC3339 (estimated)
}

// HistoryGCRunSummary is the result of one history GC sweep / purge-all.
type HistoryGCRunSummary struct {
	StartedAt       string `json:"started_at"` // RFC3339
	FinishedAt      string `json:"finished_at"`
	PurgedTraces    int    `json:"purged_traces"`
	SkippedRunning  int    `json:"skipped_running"`
	LogsDeleted     int    `json:"logs_deleted"`
	MetricsDeleted  int    `json:"metrics_deleted"`
	TelemetryErrors int    `json:"telemetry_errors"` // Loki+VM delete failures
	Errors          int    `json:"errors"`           // trace_meta delete failures
	Message         string `json:"message,omitempty"`
}

// HistoryPurgeRequest is the body of POST /api/v1/history/purge.
type HistoryPurgeRequest struct {
	TraceID string `json:"trace_id"` // optional; when set, purge a single trace (admin override, ignores running protection)
}

// HistoryPurgeResult is the response of the history purge endpoints.
type HistoryPurgeResult struct {
	PurgedTraces   int      `json:"purged_traces"`
	LogsDeleted    int      `json:"logs_deleted"`
	MetricsDeleted int      `json:"metrics_deleted"`
	TraceIDs       []string `json:"trace_ids"`      // affected trace IDs
	AlreadyPurged  int      `json:"already_purged"` // trace_meta rows already absent
	Message        string   `json:"message,omitempty"`
}

// HistoryStatsProvider reports history stats + GC rules.
type HistoryStatsProvider interface {
	Stats(ctx context.Context) (*HistoryStats, error)
	GCRules() HistoryGCRules
}

// HistoryPurger purges pipeline history.
type HistoryPurger interface {
	Purge(ctx context.Context, req HistoryPurgeRequest) (*HistoryPurgeResult, error)
	PurgeAll(ctx context.Context) (*HistoryPurgeResult, error)
}
