package service

import (
	"encoding/hex"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// TraceIngestSummary is the parsed root-span metadata extracted from an OTLP
// HTTP/protobuf body, used to enrich trace_meta during ingest.
type TraceIngestSummary struct {
	TraceID    string
	CIRepo     string // resource attr "dagger.io/ci.repo"
	GitRemote  string // resource attr "dagger.io/git.remote" (org/repo, no scheme)
	CIProvider string // resource attr "dagger.io/ci"
	Version    string // resource attr "dagger.io/engine.version"
	Status     string // root span status code mapping
	DurationMS int64  // root span end-start when both present
	StartedAt  time.Time
}

// spanWithResource pairs a span with the resource attributes of the batch that
// produced it (resource attributes are inherited by every span in the batch).
type spanWithResource struct {
	span     *tracepb.Span
	resource map[string]string
}

// extractTraceSpans walks an ExportTraceServiceRequest envelope and returns
// every span paired with its batch's resource attributes. Malformed bodies
// yield nil. Shared by ExtractTraceSummaries and ExtractTraceIDs.
func extractTraceSpans(body []byte) []spanWithResource {
	if len(body) == 0 {
		return nil
	}

	var spans []spanWithResource
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return nil
		}
		body = body[n:]
		if num == 1 && typ == protowire.BytesType {
			rsBytes, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return nil
			}
			body = body[n:]

			var rs tracepb.ResourceSpans
			if err := proto.Unmarshal(rsBytes, &rs); err != nil {
				return nil
			}
			resource := stringAttrs(rs.GetResource().GetAttributes())
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					spans = append(spans, spanWithResource{span: sp, resource: resource})
				}
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, body)
			if n < 0 {
				return nil
			}
			body = body[n:]
		}
	}

	return spans
}

// ExtractTraceIDs parses an OTLP/HTTP protobuf body (ExportTraceServiceRequest)
// and returns the distinct hex-encoded trace IDs of every span in the payload
// (not just roots). Malformed bodies yield nil; spans without a trace ID are
// skipped.
func ExtractTraceIDs(body []byte) []string {
	spans := extractTraceSpans(body)
	if len(spans) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(spans))
	out := make([]string, 0, len(spans))
	for i := range spans {
		tid := hex.EncodeToString(spans[i].span.GetTraceId())
		if tid == "" || seen[tid] {
			continue
		}
		seen[tid] = true
		out = append(out, tid)
	}
	return out
}

// ExtractLogTraceIDs parses an OTLP/HTTP protobuf body
// (ExportLogsServiceRequest) and returns the distinct hex-encoded trace IDs of
// every log record. The top-level "repeated ResourceLogs resource_logs = 1"
// envelope is walked with protowire (same as the trace path), then each
// ResourceLogs is decoded with the typed logs proto. Records without a trace
// ID are skipped; malformed bodies yield nil.
func ExtractLogTraceIDs(body []byte) []string {
	if len(body) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var out []string

	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return nil
		}
		body = body[n:]
		if num == 1 && typ == protowire.BytesType {
			rlBytes, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return nil
			}
			body = body[n:]

			var rl logspb.ResourceLogs
			if err := proto.Unmarshal(rlBytes, &rl); err != nil {
				return nil
			}
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					tid := hex.EncodeToString(lr.GetTraceId())
					if tid == "" || seen[tid] {
						continue
					}
					seen[tid] = true
					out = append(out, tid)
				}
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, body)
			if n < 0 {
				return nil
			}
			body = body[n:]
		}
	}

	return out
}

// ExtractTraceSummaries parses an OTLP/HTTP protobuf body (ExportTraceServiceRequest)
// and returns one summary per distinct root trace (root = span with no parent
// at all). Malformed bodies yield nil (caller proceeds without metadata).
//
// The top-level ExportTraceServiceRequest message only wraps
// "repeated ResourceSpans resource_spans = 1"; its envelope is walked with
// protowire so the collector package (which drags in the gRPC gateway) is not
// imported, then each ResourceSpans is decoded with the typed trace proto.
func ExtractTraceSummaries(body []byte) []TraceIngestSummary {
	spans := extractTraceSpans(body)
	if len(spans) == 0 {
		return nil
	}

	// Root = span with no parent at all (absent or all-zero parentSpanId).
	// A span whose parent is merely absent from THIS payload batch is not the
	// trace root: a batch that carries only a subtree must not be allowed to
	// drive trace_meta status/duration, otherwise a finished child span would
	// mark the whole trace finished (success/failed) while it is still
	// running. First root per trace wins.
	byTrace := make(map[string]*spanWithResource)
	for i := range spans {
		s := &spans[i]
		if isRootSpan(s.span.GetParentSpanId()) {
			tid := string(s.span.GetTraceId())
			if _, ok := byTrace[tid]; !ok {
				byTrace[tid] = s
			}
		}
	}

	out := make([]TraceIngestSummary, 0, len(byTrace))
	for _, root := range byTrace {
		out = append(out, buildSummary(root))
	}
	return out
}

// isRootSpan reports whether a parent span ID is empty: absent (nil/len 0) or
// all zero bytes. Only spans without any parent are trace roots; every other
// span carries its parent's ID even when that parent is not part of the same
// export batch.
func isRootSpan(parent []byte) bool {
	for _, b := range parent {
		if b != 0 {
			return false
		}
	}
	return true
}

// stringAttrs extracts string-valued attributes from OTLP KeyValue pairs.
// Non-string values (bools, ints, arrays) are ignored.
func stringAttrs(kvs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if v, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok {
			out[kv.GetKey()] = v.StringValue
		}
	}
	return out
}

func buildSummary(s *spanWithResource) TraceIngestSummary {
	root := s.span
	// The Dagger CLI's trace IDs are 16 bytes (32 hex chars); hex-encode to
	// match the trace_id persisted at engine-provision time.
	sum := TraceIngestSummary{TraceID: hex.EncodeToString(root.GetTraceId())}

	// Status code mapping (reuse trace_store semantics). The root span carries
	// no status while the run is in flight, so the default is "running" rather
	// than a distinct "unset" state: a final status must only be surfaced once
	// the root span actually finished.
	switch root.GetStatus().GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		sum.Status = "success"
	case tracepb.Status_STATUS_CODE_ERROR:
		sum.Status = "failed"
	default:
		sum.Status = "running"
	}

	// Start time + duration.
	if startNS := int64(root.GetStartTimeUnixNano()); startNS > 0 {
		sum.StartedAt = time.Unix(0, startNS).UTC()
		if endNS := int64(root.GetEndTimeUnixNano()); endNS > 0 {
			sum.DurationMS = (endNS - startNS) / int64(time.Millisecond)
		}
	}

	// Resource attributes (repo/CI metadata reported by the Dagger CLI).
	sum.CIRepo = s.resource["dagger.io/ci.repo"]
	sum.GitRemote = s.resource["dagger.io/git.remote"]
	sum.Version = s.resource["dagger.io/engine.version"]
	ci := s.resource["dagger.io/ci"]
	ciVendor := s.resource["dagger.io/ci.vendor"]
	if ciVendor == "" {
		ciVendor = s.resource["dagger.io/ci.provider"]
	}

	// Span attributes take precedence (some engines emit these at span level).
	for _, kv := range root.GetAttributes() {
		sv, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue)
		if !ok {
			continue
		}
		switch kv.GetKey() {
		case "dagger.io/ci.repo":
			sum.CIRepo = sv.StringValue
		case "dagger.io/git.remote":
			sum.GitRemote = sv.StringValue
		case "dagger.io/ci":
			ci = sv.StringValue
		case "dagger.io/ci.vendor", "dagger.io/ci.provider":
			if ciVendor == "" {
				ciVendor = sv.StringValue
			}
		case "dagger.io/engine.version":
			sum.Version = sv.StringValue
		}
	}

	// The Dagger CLI reports "dagger.io/ci" as a boolean string ("true"/"false").
	// A real provider name may arrive via "dagger.io/ci.vendor" (or the legacy
	// "dagger.io/ci.provider"); when only "true" is present we label it "ci".
	// Absent or "false" means a manual (local) run, so no provider is recorded
	// and the frontend renders "manual".
	switch ci {
	case "true":
		if ciVendor != "" {
			sum.CIProvider = ciVendor
		} else {
			sum.CIProvider = "ci"
		}
	case "", "false":
		sum.CIProvider = ""
	default:
		sum.CIProvider = ci
	}

	return sum
}
