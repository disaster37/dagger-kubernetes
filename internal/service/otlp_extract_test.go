package service

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExtractTraceSummariesResourceSpans(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{
							map[string]any{
								"traceId":           "trace1",
								"spanId":            "span1",
								"parentSpanId":      "",
								"startTimeUnixNano": "1700000000000000000",
								"endTimeUnixNano":   "1700000001000000000",
								"status":            map[string]any{"code": float64(1)},
								"attributes": []any{
									map[string]any{"key": "dagger.io/ci.repo", "value": map[string]any{"stringValue": "github.com/acme/api"}},
									map[string]any{"key": "dagger.io/ci", "value": map[string]any{"stringValue": "github"}},
									map[string]any{"key": "dagger.io/engine.version", "value": map[string]any{"stringValue": "v0.21.4"}},
								},
							},
							map[string]any{
								"traceId":      "trace1",
								"spanId":       "span2",
								"parentSpanId": "span1",
							},
						},
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
	if s.TraceID != "trace1" {
		t.Fatalf("trace_id = %q", s.TraceID)
	}
	if s.CIRepo != "github.com/acme/api" {
		t.Fatalf("ci_repo = %q", s.CIRepo)
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

func TestExtractTraceSummariesBatchesShape(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"batches": []any{
			map[string]any{
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{
							map[string]any{
								"traceId":      "t2",
								"spanId":       "s1",
								"parentSpanId": "",
								"status":       map[string]any{"code": float64(2)},
							},
						},
					},
				},
			},
		},
	})
	sums := ExtractTraceSummaries(body)
	if len(sums) != 1 || sums[0].TraceID != "t2" || sums[0].Status != "failed" {
		t.Fatalf("batches shape: %+v", sums)
	}
}

func TestExtractTraceSummariesMultipleTraces(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{
							map[string]any{"traceId": "t1", "spanId": "s1", "parentSpanId": ""},
							map[string]any{"traceId": "t2", "spanId": "s2", "parentSpanId": ""},
						},
					},
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
	if !seen["t1"] || !seen["t2"] {
		t.Fatalf("traces = %v", seen)
	}
}

func TestExtractTraceSummariesMissingAttrs(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{
							map[string]any{"traceId": "t1", "spanId": "s1", "parentSpanId": ""},
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
	if sums[0].CIRepo != "" || sums[0].Status != "unset" {
		t.Fatalf("missing attrs: %+v", sums[0])
	}
}

func TestExtractTraceSummariesMalformed(t *testing.T) {
	if sums := ExtractTraceSummaries([]byte("not json")); sums != nil {
		t.Fatalf("malformed should yield nil, got %v", sums)
	}
	if sums := ExtractTraceSummaries(nil); sums != nil {
		t.Fatalf("nil should yield nil, got %v", sums)
	}
	if sums := ExtractTraceSummaries([]byte(`{}`)); sums != nil {
		t.Fatalf("empty object should yield nil, got %v", sums)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
