package domain

// PipelinesTopic is the SSE subscription key for pipeline-list change
// notifications (the "pipelines overview" live stream). It is a sentinel string
// that cannot collide with a per-trace key: per-trace live connections are keyed
// by the trace ID path parameter, and real Dagger trace IDs are hex, while this
// value is a fixed non-hex literal.
const PipelinesTopic = "__pipelines__"
