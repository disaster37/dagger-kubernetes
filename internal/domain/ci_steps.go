package domain

import "time"

// StepState is the CI-visible lifecycle state of one Dagger operation (span).
type StepState string

const (
	StepStatePending   StepState = "pending"   // discovered via parent, not started
	StepStateRunning   StepState = "running"   // start record seen, no finish record
	StepStateSucceeded StepState = "succeeded" // finished with OK
	StepStateFailed    StepState = "failed"    // finished with error
)

// StepNode is the CI-facing view of one Dagger operation (span), rebuilt from
// the supervisor's reconstructed span tree.
type StepNode struct {
	ID         string    `json:"id"`        // stable; the span id (base64, as Tempo returns it)
	ParentID   string    `json:"parent_id"` // "" for the root step
	Name       string    `json:"name"`      // sanitized span name
	State      StepState `json:"state"`
	Depth      int       `json:"depth"` // 0 = root
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// LogChunk is a bounded batch of log lines attributed to a node.
type LogChunk struct {
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
	Lines     []string  `json:"lines"`
}

// CIEventType enumerates the normalized CI event stream.
type CIEventType string

const (
	CIEventNodeStarted  CIEventType = "node_started"
	CIEventNodeFinished CIEventType = "node_finished"
	CIEventLogChunk     CIEventType = "log_chunk"
	CIEventPipelineDone CIEventType = "pipeline_done" // carries Status: success|failed|canceled
)

// CIEvent is the normalized, ordered event emitted by the step-event builder.
type CIEvent struct {
	Seq       int64       `json:"seq"` // monotonic within a pipeline
	Type      CIEventType `json:"type"`
	TraceID   string      `json:"trace_id"`
	Timestamp time.Time   `json:"timestamp"`
	Node      *StepNode   `json:"node,omitempty"`   // node_started / node_finished
	Log       *LogChunk   `json:"log,omitempty"`    // log_chunk
	Status    string      `json:"status,omitempty"` // pipeline_done ("success"|"failed"|"canceled")
	Error     string      `json:"error,omitempty"`  // failure reason (node_finished / pipeline_done)
}

// CIEventSink consumes normalized CI events. Implementations render to a CI
// system (NDJSON for Jenkins/Drone, a future plugin protocol, a debug file).
type CIEventSink interface {
	Emit(e *CIEvent) error
	Flush() error
}

// TraceSnapshotSource is the data source the CI wrapper uses to rebuild the
// step tree: the supervisor's span-tree reconstruction + per-span logs. The
// wrapper's HTTP client implements it (repository.SupervisorTraceClient).
type TraceSnapshotSource interface {
	GetTrace(traceID string) (*TraceInfo, error)
	QueryTraceLogs(traceID string, start, end time.Time, limit int) ([]LogEntry, error)
}
