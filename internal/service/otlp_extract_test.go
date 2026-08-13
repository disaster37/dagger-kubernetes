package service

import (
	"encoding/hex"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func tid(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode trace id: %v", err)
	}
	return b
}

// mustMarshalRequest serializes a ResourceSpans and wraps it in the
// ExportTraceServiceRequest envelope (repeated ResourceSpans = field 1).
func mustMarshalRequest(t *testing.T, rs *tracepb.ResourceSpans) []byte {
	t.Helper()
	rsBytes, err := proto.Marshal(rs)
	if err != nil {
		t.Fatalf("marshal resource spans: %v", err)
	}
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, rsBytes)
	return body
}

func TestExtractTraceSummariesResourceSpans(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strAttr("dagger.io/ci.repo", "github.com/acme/api"),
				strAttr("dagger.io/git.remote", "github.com/acme/api"),
				strAttr("dagger.io/ci", "github"),
				strAttr("dagger.io/engine.version", "v0.21.4"),
			},
		},
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{
						TraceId:           tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
						SpanId:            tid(t, "1111111111111111"),
						StartTimeUnixNano: 1700000000000000000,
						EndTimeUnixNano:   1700000001000000000,
						Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
					},
					{
						TraceId:      tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
						SpanId:       tid(t, "2222222222222222"),
						ParentSpanId: tid(t, "1111111111111111"),
					},
				},
			},
		},
	})

	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 {
		t.Fatalf("got %d summaries, want 1", len(sums))
	}
	s := sums[0]
	if s.TraceID != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trace_id = %q", s.TraceID)
	}
	if s.CIRepo != "github.com/acme/api" {
		t.Fatalf("ci_repo = %q", s.CIRepo)
	}
	if s.GitRemote != "github.com/acme/api" {
		t.Fatalf("git_remote = %q", s.GitRemote)
	}
	if s.CIProvider != "github" {
		t.Fatalf("ci_provider = %q", s.CIProvider)
	}
	if s.Version != "v0.21.4" {
		t.Fatalf("version = %q", s.Version)
	}
	if s.Status != "success" {
		t.Fatalf("status = %q", s.Status)
	}
	if s.DurationMS != 1000 {
		t.Fatalf("duration_ms = %d, want 1000", s.DurationMS)
	}
	if !s.StartedAt.Equal(time.Unix(0, 1700000000000000000).UTC()) {
		t.Fatalf("started_at = %v", s.StartedAt)
	}
}

func TestExtractTraceSummariesGitRemoteOnly(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		Resource: &resourcepb.Resource{
			Attributes: []*commonpb.KeyValue{
				strAttr("dagger.io/git.remote", "github.com/acme/api"),
			},
		},
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{
						TraceId: tid(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
						SpanId:  tid(t, "3333333333333333"),
					},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 {
		t.Fatalf("got %d, want 1", len(sums))
	}
	if sums[0].GitRemote != "github.com/acme/api" {
		t.Fatalf("git_remote = %q", sums[0].GitRemote)
	}
	if sums[0].CIRepo != "" {
		t.Fatalf("ci_repo = %q, want empty (local runs have no ci.repo)", sums[0].CIRepo)
	}
}

func TestExtractTraceSummariesSpanAttributes(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{
						TraceId: tid(t, "cccccccccccccccccccccccccccccccc"),
						SpanId:  tid(t, "4444444444444444"),
						Attributes: []*commonpb.KeyValue{
							strAttr("dagger.io/ci.repo", "span/level/repo"),
						},
					},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 {
		t.Fatalf("got %d, want 1", len(sums))
	}
	if sums[0].CIRepo != "span/level/repo" {
		t.Fatalf("ci_repo = %q, want span-level value", sums[0].CIRepo)
	}
}

func TestExtractTraceSummariesMultipleTraces(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{TraceId: tid(t, "dddddddddddddddddddddddddddddddd"), SpanId: tid(t, "aaaaaaaaaaaaaaaa")},
					{TraceId: tid(t, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"), SpanId: tid(t, "bbbbbbbbbbbbbbbb")},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 2 {
		t.Fatalf("got %d, want 2", len(sums))
	}
	seen := map[string]bool{}
	for _, s := range sums {
		seen[s.TraceID] = true
	}
	if !seen["dddddddddddddddddddddddddddddddd"] || !seen["eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"] {
		t.Fatalf("traces = %v", seen)
	}
}

func TestExtractTraceSummariesMissingAttrs(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{TraceId: tid(t, "ffffffffffffffffffffffffffffffff"), SpanId: tid(t, "cccccccccccccccc")},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 {
		t.Fatalf("got %d, want 1", len(sums))
	}
	if sums[0].CIRepo != "" || sums[0].Status != "unset" {
		t.Fatalf("missing attrs: %+v", sums[0])
	}
}

func TestExtractTraceSummariesFailedStatus(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{
						TraceId: tid(t, "11111111111111111111111111111111"),
						SpanId:  tid(t, "dddddddddddddddd"),
						Status:  &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR},
					},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 || sums[0].Status != "failed" {
		t.Fatalf("failed status: %+v", sums)
	}
}

func TestExtractTraceSummariesMalformed(t *testing.T) {
	if sums := ExtractTraceSummaries([]byte("not protobuf")); sums != nil {
		t.Fatalf("malformed should yield nil, got %v", sums)
	}
	if sums := ExtractTraceSummaries(nil); sums != nil {
		t.Fatalf("nil should yield nil, got %v", sums)
	}
	if sums := ExtractTraceSummaries([]byte{}); sums != nil {
		t.Fatalf("empty should yield nil, got %v", sums)
	}
}
