package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// seedPushRun creates a check run through a signed push webhook and returns the
// run ID plus the per-run callback token captured by the recording runner.
func seedPushRun(t *testing.T, srv *parityServer, runner *recordingCheckRunner, sha string) (runID, runToken string) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":"%s","repository":{"full_name":"acme/repo","default_branch":"main"}}`, sha))
	if status, respBody := postWebhook(t, srv, "push", "webhook-secret", body); status != http.StatusOK {
		t.Fatalf("webhook status = %d body=%s", status, respBody)
	}
	runID = fmt.Sprintf("check-src-1-%s", sha)
	runToken = runner.tokenFor(runID)
	if runToken == "" {
		t.Fatalf("no check runner token recorded for %s", runID)
	}
	return runID, runToken
}

// TestCheckRunTestsVisibility proves per-test results reach both the check-run
// API (read-time enrichment from the run's trace) and the trace detail endpoint,
// without persisting tests in the replicated record.
func TestCheckRunTestsVisibility(t *testing.T) {
	runner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, runner, nil)
	seedResidualForgeSource(t, srv)

	const traceID = "trace-tests-visibility"
	srv.traces.set(&domain.TraceInfo{
		TraceID: traceID,
		RootSpan: &domain.SpanNode{
			SpanID: "root", Name: "go test", Status: "success",
			Children: []*domain.SpanNode{
				{
					SpanID: "t1", Name: "TestGet", Status: "success", DurationMS: 42,
					Attributes: map[string]string{"test.case.name": "TestGet", "test.case.result.status": "pass"},
				},
				{
					SpanID: "t2", Name: "TestPut", Status: "success", DurationMS: 7,
					Attributes: map[string]string{"test.case.name": "TestPut", "test.case.result.status": "pass"},
				},
			},
		},
		Status: "success", DurationMS: 49,
	})

	runID, runToken := seedPushRun(t, srv, runner, "abc123")
	if _, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/load", runID), map[string]any{ //nolint:bodyclose // doWithRunToken closes the body
		"checks": []string{"go"},
	}, runToken); len(body) == 0 {
		t.Fatal("empty load response")
	}
	if _, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/complete", runID), map[string]any{ //nolint:bodyclose // doWithRunToken closes the body
		"checks": []map[string]any{{"name": "go", "state": "success", "trace_id": traceID}},
	}, runToken); len(body) == 0 {
		t.Fatal("empty complete response")
	}

	_, body := srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/checks/runs/%s", runID), nil) //nolint:bodyclose // do closes the body
	var record domain.CheckRunRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatalf("decode run: %v (%s)", err, body)
	}
	if len(record.Checks) != 1 || len(record.Checks[0].Tests) != 2 {
		t.Fatalf("check tests = %+v", record.Checks)
	}
	if record.Checks[0].Tests[0].Name != "TestGet" || record.Checks[0].Tests[0].DurationMS != 42 {
		t.Fatalf("first test = %+v", record.Checks[0].Tests[0])
	}

	// The list endpoint enriches too.
	_, listBody := srv.do(t, http.MethodGet, "/api/v1/orgs/acme/checks", nil) //nolint:bodyclose // do closes the body
	var records []domain.CheckRunRecord
	if err := json.Unmarshal(listBody, &records); err != nil || len(records) != 1 {
		t.Fatalf("list = %s err=%v", listBody, err)
	}
	if len(records[0].Checks) != 1 || len(records[0].Checks[0].Tests) != 2 {
		t.Fatalf("listed check tests = %+v", records[0].Checks)
	}

	// The trace detail derives the same flattened list.
	traceResp, traceBody := srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traces/%s", traceID), nil) //nolint:bodyclose // do closes the body
	var trace domain.TraceInfo
	if traceResp.StatusCode != http.StatusOK {
		t.Fatalf("trace status = %d body=%s", traceResp.StatusCode, traceBody)
	}
	if err := json.Unmarshal(traceBody, &trace); err != nil {
		t.Fatalf("decode trace: %v (%s)", err, traceBody)
	}
	if len(trace.Tests) != 2 || trace.Tests[1].Name != "TestPut" {
		t.Fatalf("trace tests = %+v body=%s", trace.Tests, traceBody)
	}

	// A missing trace is not an error and yields no tests.
	srv.traces.set(&domain.TraceInfo{TraceID: "other", RootSpan: &domain.SpanNode{SpanID: "r", Status: "success"}})
	if resp, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/complete", runID), map[string]any{ //nolint:bodyclose // doWithRunToken closes the body
		"checks": []map[string]any{{"name": "go", "state": "success", "trace_id": "absent"}},
	}, runToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status = %d body=%s", resp.StatusCode, body)
	}
	time.Sleep(50 * time.Millisecond)
	_, body = srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/checks/runs/%s", runID), nil) //nolint:bodyclose // do closes the body
	record = domain.CheckRunRecord{}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Checks) != 1 || len(record.Checks[0].Tests) != 0 {
		t.Fatalf("missing trace must yield no tests: %+v body=%s", record.Checks, body)
	}
}
