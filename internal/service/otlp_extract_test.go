package service

import (
	"encoding/hex"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
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

// mustMarshalLogsRequest serializes a ResourceLogs and wraps it in the
// ExportLogsServiceRequest envelope (repeated ResourceLogs = field 1).
func mustMarshalLogsRequest(t *testing.T, rl *logspb.ResourceLogs) []byte {
	t.Helper()
	rlBytes, err := proto.Marshal(rl)
	if err != nil {
		t.Fatalf("marshal resource logs: %v", err)
	}
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, rlBytes)
	return body
}

func TestExtractTraceIDs(t *testing.T) {
	tests := []struct {
		name string
		rs   *tracepb.ResourceSpans
		want []string
	}{
		{
			name: "distinct-across-spans",
			rs: &tracepb.ResourceSpans{
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Spans: []*tracepb.Span{
							{TraceId: tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), SpanId: tid(t, "1111111111111111")},
							{TraceId: tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), SpanId: tid(t, "2222222222222222"), ParentSpanId: tid(t, "1111111111111111")},
							{TraceId: tid(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), SpanId: tid(t, "3333333333333333")},
						},
					},
				},
			},
			want: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
		{
			name: "skips-empty-trace-id",
			rs: &tracepb.ResourceSpans{
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Spans: []*tracepb.Span{
							{SpanId: tid(t, "4444444444444444")},
							{TraceId: tid(t, "cccccccccccccccccccccccccccccccc"), SpanId: tid(t, "5555555555555555")},
						},
					},
				},
			},
			want: []string{"cccccccccccccccccccccccccccccccc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractTraceIDs(mustMarshalRequest(t, tt.rs))
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestExtractTraceIDsMalformed(t *testing.T) {
	if got := ExtractTraceIDs([]byte("not protobuf")); got != nil {
		t.Fatalf("malformed should yield nil, got %v", got)
	}
	if got := ExtractTraceIDs(nil); got != nil {
		t.Fatalf("nil should yield nil, got %v", got)
	}
	if got := ExtractTraceIDs([]byte{}); got != nil {
		t.Fatalf("empty should yield nil, got %v", got)
	}
}

func TestExtractLogTraceIDs(t *testing.T) {
	body := mustMarshalLogsRequest(t, &logspb.ResourceLogs{
		ScopeLogs: []*logspb.ScopeLogs{
			{
				LogRecords: []*logspb.LogRecord{
					{TraceId: tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
					{TraceId: tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}, // duplicate
					{TraceId: tid(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")},
					{}, // empty trace ID, must be skipped
				},
			},
		},
	})

	got := ExtractLogTraceIDs(body)
	want := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestExtractLogTraceIDsMultipleResourceLogs(t *testing.T) {
	rl1 := &logspb.ResourceLogs{
		ScopeLogs: []*logspb.ScopeLogs{
			{LogRecords: []*logspb.LogRecord{{TraceId: tid(t, "cccccccccccccccccccccccccccccccc")}}},
		},
	}
	rl2 := &logspb.ResourceLogs{
		ScopeLogs: []*logspb.ScopeLogs{
			{LogRecords: []*logspb.LogRecord{{TraceId: tid(t, "dddddddddddddddddddddddddddddddd")}}},
		},
	}

	rl1Bytes, err := proto.Marshal(rl1)
	if err != nil {
		t.Fatalf("marshal rl1: %v", err)
	}
	rl2Bytes, err := proto.Marshal(rl2)
	if err != nil {
		t.Fatalf("marshal rl2: %v", err)
	}
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, rl1Bytes)
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, rl2Bytes)

	got := ExtractLogTraceIDs(body)
	want := []string{"cccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddd"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestExtractLogTraceIDsMalformed(t *testing.T) {
	if got := ExtractLogTraceIDs([]byte("not protobuf")); got != nil {
		t.Fatalf("malformed should yield nil, got %v", got)
	}
	if got := ExtractLogTraceIDs(nil); got != nil {
		t.Fatalf("nil should yield nil, got %v", got)
	}
	if got := ExtractLogTraceIDs([]byte{}); got != nil {
		t.Fatalf("empty should yield nil, got %v", got)
	}
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
	if sums[0].CIRepo != "" || sums[0].Status != "running" {
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

func TestExtractTraceSummariesIgnoresSubtreeBatches(t *testing.T) {
	// A batch carrying only a subtree (a child span whose parent is not part
	// of this payload) must not produce a summary: only the true root may
	// drive trace_meta status/duration. Otherwise a finished child span would
	// mark a still-running trace as success/failed.
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{
						TraceId:           tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
						SpanId:            tid(t, "2222222222222222"),
						ParentSpanId:      tid(t, "1111111111111111"),
						StartTimeUnixNano: 1700000000000000000,
						EndTimeUnixNano:   1700000001000000000,
						Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
					},
				},
			},
		},
	})
	if sums := ExtractTraceSummaries(body); len(sums) != 0 {
		t.Fatalf("subtree batch produced summaries: %+v", sums)
	}
}

func TestExtractTraceSummariesAllZeroParentIsRoot(t *testing.T) {
	body := mustMarshalRequest(t, &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{
				Spans: []*tracepb.Span{
					{
						TraceId:      tid(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
						SpanId:       tid(t, "3333333333333333"),
						ParentSpanId: make([]byte, 8),
						Status:       &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
					},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 || sums[0].Status != "success" {
		t.Fatalf("all-zero parent should be a root: %+v", sums)
	}
}

func TestExtractTraceSummariesCIProvider(t *testing.T) {
	tests := []struct {
		name      string
		resource  []*commonpb.KeyValue
		spanAttrs []*commonpb.KeyValue
		want      string
	}{
		{
			name:     "ci-true-with-vendor",
			resource: []*commonpb.KeyValue{strAttr("dagger.io/ci", "true"), strAttr("dagger.io/ci.vendor", "github")},
			want:     "github",
		},
		{
			name:     "ci-true-no-vendor",
			resource: []*commonpb.KeyValue{strAttr("dagger.io/ci", "true")},
			want:     "ci",
		},
		{
			name:     "ci-false-manual",
			resource: []*commonpb.KeyValue{strAttr("dagger.io/ci", "false")},
			want:     "",
		},
		{
			name: "ci-absent-manual",
			want: "",
		},
		{
			name:     "ci-vendor-name-direct",
			resource: []*commonpb.KeyValue{strAttr("dagger.io/ci", "gitlab")},
			want:     "gitlab",
		},
		{
			name:      "span-level-ci-provider",
			resource:  []*commonpb.KeyValue{strAttr("dagger.io/ci", "true")},
			spanAttrs: []*commonpb.KeyValue{strAttr("dagger.io/ci.provider", "circleci")},
			want:      "circleci",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := mustMarshalRequest(t, &tracepb.ResourceSpans{
				Resource: &resourcepb.Resource{Attributes: tt.resource},
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Spans: []*tracepb.Span{
							{
								TraceId:    tid(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
								SpanId:     tid(t, "1111111111111111"),
								Attributes: tt.spanAttrs,
							},
						},
					},
				},
			})
			sums := ExtractTraceSummaries(body)
			if len(sums) != 1 {
				t.Fatalf("got %d summaries, want 1", len(sums))
			}
			if sums[0].CIProvider != tt.want {
				t.Fatalf("ci_provider = %q, want %q", sums[0].CIProvider, tt.want)
			}
		})
	}
}
