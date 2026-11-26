package handler

import (
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestEnrichCheckTests covers the read-time per-test enrichment: each check
// item is annotated from its own trace, missing traces and empty trace IDs are
// tolerated, and a nil trace repository is a no-op.
func TestEnrichCheckTests(t *testing.T) {
	env := newTestEnv(t)
	trace := &domain.TraceInfo{TraceID: "tr-1", RootSpan: &domain.SpanNode{
		SpanID: "root", Name: "go test",
		Children: []*domain.SpanNode{{
			SpanID:     "t1",
			Attributes: map[string]string{"test.case.name": "TestGet", "test.case.result.status": "pass"},
		}},
	}}
	env.server.traces = &stubTraceRepo{trace: trace}

	record := &domain.CheckRunRecord{Checks: []domain.CheckItem{
		{Name: "go", TraceID: "tr-1"},
		{Name: "load"},
	}}
	env.server.enrichCheckTests(record)
	if len(record.Checks[0].Tests) != 1 || record.Checks[0].Tests[0].Name != "TestGet" {
		t.Fatalf("check tests = %+v, want TestGet", record.Checks[0].Tests)
	}
	if len(record.Checks[1].Tests) != 0 {
		t.Fatalf("check without a trace must have no tests: %+v", record.Checks[1].Tests)
	}

	// A trace that cannot be read is skipped, not an error.
	env.server.traces = &stubTraceRepo{err: domain.ErrNotFound}
	failing := &domain.CheckRunRecord{Checks: []domain.CheckItem{{Name: "go", TraceID: "gone"}}}
	env.server.enrichCheckTests(failing)
	if len(failing.Checks[0].Tests) != 0 {
		t.Fatalf("missing trace must yield no tests: %+v", failing.Checks[0].Tests)
	}

	// nil records / nil repo are no-ops.
	env.server.enrichCheckTests(nil)
	env.server.traces = nil
	env.server.enrichCheckTests(record)
}
