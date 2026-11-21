package service

import (
	"encoding/hex"
	"math"
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
	return traceIDsFromSpans(extractTraceSpans(body))
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
	return summariesFromSpans(extractTraceSpans(body))
}

// summariesFromSpans builds one summary per distinct root trace from an
// already-decoded span set.
func summariesFromSpans(spans []spanWithResource) []TraceIngestSummary {
	if len(spans) == 0 {
		return nil
	}

	// Root = span with no parent at all (absent or all-zero parentSpanId).
	// A span whose parent is merely absent from THIS payload batch is not the
	// trace root: a batch that carries only a subtree must not be allowed to
	// drive trace_meta status/duration, otherwise a finished child span would
	// mark the whole trace finished (success/failed) while it is still
	// running. When a batch carries both the root start record and the root
	// finish record, prefer the finish record (terminal status or a non-zero
	// end time) so the trace is not left "running".
	byTrace := make(map[string]*spanWithResource)
	for i := range spans {
		s := &spans[i]
		if !isRootSpan(s.span.GetParentSpanId()) {
			continue
		}
		tid := string(s.span.GetTraceId())
		existing, ok := byTrace[tid]
		if !ok {
			byTrace[tid] = s
			continue
		}
		if preferRootRecord(s, existing) {
			// Preserve the start time if the finish record lacks one.
			if s.span.GetStartTimeUnixNano() == 0 && existing.span.GetStartTimeUnixNano() != 0 {
				s.span.StartTimeUnixNano = existing.span.StartTimeUnixNano
			}
			byTrace[tid] = s
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

// preferRootRecord reports whether candidate is a better root record for a
// trace than current: a terminal status (OK/ERROR) or a non-zero end time wins
// over a start-only record, so a batch carrying both the root start and root
// finish records keeps the finish record.
func preferRootRecord(candidate, current *spanWithResource) bool {
	if spanHasTerminalStatus(candidate.span) {
		return true
	}
	if spanHasTerminalStatus(current.span) {
		return false
	}
	return candidate.span.GetEndTimeUnixNano() > 0 && current.span.GetEndTimeUnixNano() == 0
}

// spanHasTerminalStatus reports whether a span carries a terminal OTLP status.
func spanHasTerminalStatus(s *tracepb.Span) bool {
	switch s.GetStatus().GetCode() {
	case tracepb.Status_STATUS_CODE_OK, tracepb.Status_STATUS_CODE_ERROR:
		return true
	default:
		return false
	}
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

	// Start time + duration. Proto timestamps are uint64 Unix nanos; guard the
	// int64 conversions against overflow (G115).
	startUnix := root.GetStartTimeUnixNano()
	if startUnix > 0 && startUnix <= math.MaxInt64 {
		startNS := int64(startUnix)
		sum.StartedAt = time.Unix(0, startNS).UTC()
		if endUnix := root.GetEndTimeUnixNano(); endUnix <= math.MaxInt64 {
			endNS := int64(endUnix)
			if endNS > 0 {
				sum.DurationMS = (endNS - startNS) / int64(time.Millisecond)
			}
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

// CacheEvent is a cache-related signal extracted from OTLP telemetry in the
// same single pass as trace/log metadata. It feeds the read-through remote
// cache index (ADR-055).
type CacheEvent struct {
	TraceID string
	Kind    string
	Key     string
	Value   string
	Size    int64
}

// OTLPBatch is the complete result of decoding one OTLP request body exactly
// once: trace summaries, every trace ID present, log trace IDs, and cache
// events. Callers must not re-unmarshal the body after calling ExtractOTLPBatch.
type OTLPBatch struct {
	Summaries   []TraceIngestSummary
	TraceIDs    []string
	LogTraceIDs []string
	CacheEvents []CacheEvent
}

// ExtractOTLPBatch decodes an OTLP/HTTP protobuf body at most once and returns
// every derived signal. signal is "traces", "logs", or "metrics"; metrics
// bodies are not decoded (they carry no trace metadata used by the supervisor).
func ExtractOTLPBatch(signal string, body []byte) OTLPBatch {
	switch signal {
	case "traces":
		spans := extractTraceSpans(body)
		if len(spans) == 0 {
			return OTLPBatch{}
		}
		return OTLPBatch{
			Summaries:   summariesFromSpans(spans),
			TraceIDs:    traceIDsFromSpans(spans),
			CacheEvents: cacheEventsFromSpans(spans),
		}
	case "logs":
		return extractLogBatch(body)
	default:
		return OTLPBatch{}
	}
}

// traceIDsFromSpans returns the distinct hex trace IDs of every span.
func traceIDsFromSpans(spans []spanWithResource) []string {
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

// cacheEventsFromSpans extracts dagger.io/cache.* span attributes.
func cacheEventsFromSpans(spans []spanWithResource) []CacheEvent {
	var out []CacheEvent
	for i := range spans {
		sp := spans[i].span
		tid := hex.EncodeToString(sp.GetTraceId())
		for _, kv := range sp.GetAttributes() {
			key := kv.GetKey()
			if len(key) <= len("dagger.io/cache.") || key[:len("dagger.io/cache.")] != "dagger.io/cache." {
				continue
			}
			sv, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue)
			if !ok {
				continue
			}
			out = append(out, CacheEvent{
				TraceID: tid,
				Kind:    key[len("dagger.io/cache."):],
				Key:     key,
				Value:   sv.StringValue,
			})
		}
	}
	return out
}

// extractLogBatch decodes an ExportLogsServiceRequest once, returning trace IDs
// and cache events from log record attributes.
func extractLogBatch(body []byte) OTLPBatch {
	if len(body) == 0 {
		return OTLPBatch{}
	}
	seen := make(map[string]bool)
	var batch OTLPBatch
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return OTLPBatch{}
		}
		body = body[n:]
		if num == 1 && typ == protowire.BytesType {
			rlBytes, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return OTLPBatch{}
			}
			body = body[n:]

			var rl logspb.ResourceLogs
			if err := proto.Unmarshal(rlBytes, &rl); err != nil {
				return OTLPBatch{}
			}
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					tid := hex.EncodeToString(lr.GetTraceId())
					if tid != "" && !seen[tid] {
						seen[tid] = true
						batch.LogTraceIDs = append(batch.LogTraceIDs, tid)
					}
					batch.CacheEvents = append(batch.CacheEvents, cacheEventsFromLogRecord(tid, lr)...)
				}
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, body)
			if n < 0 {
				return OTLPBatch{}
			}
			body = body[n:]
		}
	}
	return batch
}

// cacheEventsFromLogRecord extracts dagger.io/cache.* and dagger.io/engine.event
// attributes from one log record.
func cacheEventsFromLogRecord(traceID string, lr *logspb.LogRecord) []CacheEvent {
	var out []CacheEvent
	for _, kv := range lr.GetAttributes() {
		key := kv.GetKey()
		sv, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue)
		if !ok {
			continue
		}
		switch {
		case key == "dagger.io/engine.event":
			out = append(out, CacheEvent{TraceID: traceID, Kind: "engine.event", Key: key, Value: sv.StringValue})
		case len(key) > len("dagger.io/cache.") && key[:len("dagger.io/cache.")] == "dagger.io/cache.":
			out = append(out, CacheEvent{TraceID: traceID, Kind: key[len("dagger.io/cache."):], Key: key, Value: sv.StringValue})
		}
	}
	return out
}
