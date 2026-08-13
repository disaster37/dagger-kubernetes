package service

import (
	"encoding/hex"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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

// ExtractTraceSummaries parses an OTLP/HTTP protobuf body (ExportTraceServiceRequest)
// and returns one summary per distinct root trace (root = span whose parent is
// absent among the payload's span IDs). Malformed bodies yield nil (caller
// proceeds without metadata).
//
// The top-level ExportTraceServiceRequest message only wraps
// "repeated ResourceSpans resource_spans = 1"; its envelope is walked with
// protowire so the collector package (which drags in the gRPC gateway) is not
// imported, then each ResourceSpans is decoded with the typed trace proto.
func ExtractTraceSummaries(body []byte) []TraceIngestSummary {
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

	if len(spans) == 0 {
		return nil
	}

	// Index span IDs present in this payload (raw bytes as map keys).
	present := make(map[string]bool, len(spans))
	for i := range spans {
		present[string(spans[i].span.GetSpanId())] = true
	}

	// Root = span whose parent is absent in this payload. First root per
	// trace wins.
	byTrace := make(map[string]*spanWithResource)
	for i := range spans {
		s := &spans[i]
		if pid := string(s.span.GetParentSpanId()); pid == "" || !present[pid] {
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

	// Status code mapping (reuse trace_store semantics).
	switch root.GetStatus().GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		sum.Status = "success"
	case tracepb.Status_STATUS_CODE_ERROR:
		sum.Status = "failed"
	default:
		sum.Status = "unset"
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
	sum.CIProvider = s.resource["dagger.io/ci"]
	sum.Version = s.resource["dagger.io/engine.version"]

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
			sum.CIProvider = sv.StringValue
		case "dagger.io/engine.version":
			sum.Version = sv.StringValue
		}
	}

	return sum
}
