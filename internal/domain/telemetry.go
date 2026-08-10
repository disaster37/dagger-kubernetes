package domain

import "time"

type SpanNode struct {
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id"`
	TraceID      string            `json:"trace_id"`
	Name         string            `json:"name"`
	Status       string            `json:"status"`
	StartTime    time.Time         `json:"start_time"`
	Duration     time.Duration     `json:"duration_ms"`
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
	Duration   time.Duration `json:"duration_ms"`
	Version    string        `json:"version"`
	CIProvider string        `json:"ci_provider,omitempty"`
	CIRepo     string        `json:"ci_repo,omitempty"`
}

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

type MetricResult struct {
	Metric map[string]string `json:"metric"`
	Values [][]interface{}   `json:"values"`
	Value  []interface{}     `json:"value"`
}

type TraceRepository interface {
	GetTrace(traceID string) (*TraceInfo, error)
}

type LogRepository interface {
	QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error)
}
