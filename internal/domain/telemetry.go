package domain

import (
	"context"
	"errors"
	"time"
)

type SpanNode struct {
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id"`
	TraceID      string            `json:"trace_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	StartTime    time.Time         `json:"start_time"`
	Duration     time.Duration     `json:"duration_ns"`
	DurationMS   int64             `json:"duration_ms"`
	Attributes   map[string]string `json:"attributes"`
	Children     []*SpanNode       `json:"children"`
	Logs         []SpanLog         `json:"logs,omitempty"`
}

type SpanLog struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
}

type TraceInfo struct {
	TraceID    string        `json:"trace_id"`
	RootSpan   *SpanNode     `json:"root_span"`
	Status     string        `json:"status"`
	StartTime  time.Time     `json:"start_time"`
	Duration   time.Duration `json:"duration_ns"`
	DurationMS int64         `json:"duration_ms"`
	Version    string        `json:"version"`
	CIProvider string        `json:"ci_provider,omitempty"`
	CIRepo     string        `json:"ci_repo,omitempty"`
	UserID     string        `json:"user_id,omitempty"`  // owner from trace_meta
	Username   string        `json:"username,omitempty"` // joined from users table
	URL        string        `json:"url,omitempty"`      // self-hosted pipeline view URL
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
	SpanID    string    `json:"span_id,omitempty"`
}

type MetricResult struct {
	Metric map[string]string `json:"metric"`
	Values [][]interface{}   `json:"values"`
	Value  []interface{}     `json:"value"`
}

// MetricPoint is one sample of a time series (unix seconds + value).
type MetricPoint struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// MetricSeries is one named, unit-tagged time series for the pipeline view.
type MetricSeries struct {
	Name   string        `json:"name"`
	Label  string        `json:"label"`
	Unit   string        `json:"unit"`
	Points []MetricPoint `json:"points"`
}

// TraceMetrics is the trace-scoped engine resource metrics payload served by
// GET /api/v1/traces/:traceID/metrics.
type TraceMetrics struct {
	TraceID     string         `json:"trace_id"`
	StartTime   time.Time      `json:"start_time"`
	EndTime     time.Time      `json:"end_time"`
	StepSeconds int64          `json:"step_seconds"`
	Series      []MetricSeries `json:"series"`
}

// FleetStorage is the per-version aggregate engine storage usage.
type FleetStorage struct {
	UsedBytes     int64   `json:"used_bytes"`
	CapacityBytes int64   `json:"capacity_bytes"`
	Percent       float64 `json:"percent"` // 0..100; -1 when capacity is unknown
}

// FleetMetrics is the per-version aggregate engine resource metrics payload
// served by GET /api/v1/fleet/:version/metrics.
type FleetMetrics struct {
	Version     string         `json:"version"`
	StartTime   time.Time      `json:"start_time"`
	EndTime     time.Time      `json:"end_time"`
	StepSeconds int64          `json:"step_seconds"`
	Series      []MetricSeries `json:"series"`
	Storage     *FleetStorage  `json:"storage,omitempty"` // nil = no storage data
}

// MetricsQueryer runs one PromQL range query against the metrics backend and
// returns the aggregate (summed across matched series) points.
type MetricsQueryer interface {
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]MetricPoint, error)
}

type TraceRepository interface {
	GetTrace(traceID string) (*TraceInfo, error)
}

// ErrInvalidRegex is wrapped by LogRepository.SearchTraceLogs when the query
// mode is "regex" and the pattern does not compile.
var ErrInvalidRegex = errors.New("invalid regex")

// LogSearchMode enumerates supported log text match modes.
type LogSearchMode string

const (
	LogSearchContains LogSearchMode = "contains"
	LogSearchRegex    LogSearchMode = "regex"
)

// LogSearchRequest is a subtree-scoped, text-filtered, paginated log query.
type LogSearchRequest struct {
	SpanIDs       []string      // descendant span IDs (base64) to include; nil/empty = all spans
	Query         string        // text to match; empty = no text filter
	Mode          LogSearchMode // LogSearchContains | LogSearchRegex
	Start         time.Time     // inclusive window start (handler defaults to last 24h)
	End           time.Time     // inclusive window end (handler defaults to now)
	Limit         int           // max matching entries to return this page
	Cursor        int64         // unix nanos; return entries strictly after this timestamp
	IncludeCounts bool          // compute per-span matching counts (first page only)
}

// LogSearchPage is one page of subtree-scoped search results.
type LogSearchPage struct {
	Entries []LogEntry       `json:"entries"`
	Next    int64            `json:"next,omitempty"`   // cursor for the next page; 0 = no more
	Counts  map[string]int64 `json:"counts,omitempty"` // span_id -> matching log count (first page only)
}

type LogRepository interface {
	QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error)
	// SearchTraceLogs returns one page of trace logs filtered by the request
	// (span set + text match), ascending by timestamp, after Cursor.
	SearchTraceLogs(ctx context.Context, traceID string, req LogSearchRequest) (LogSearchPage, error)
	// DeleteTraceLogs requests deletion of all log streams for traceID from
	// Loki (POST /loki/api/v1/delete). Best-effort: returns nil on 204;
	// returns a wrapped error on non-2xx. Requires Loki compactor + deletion
	// enabled on the backend.
	DeleteTraceLogs(ctx context.Context, traceID string) error
}
