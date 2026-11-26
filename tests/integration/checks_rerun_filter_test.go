package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestCheckRunRerunFilters proves the REST rerun body transports typed filters
// to the runner, validates them, and that the GraphQL mutation accepts the
// additive filters argument.
func TestCheckRunRerunFilters(t *testing.T) {
	runner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, runner, nil)
	seedResidualForgeSource(t, srv)
	runID, _ := seedPushRun(t, srv, runner, "abc123")

	resp, body := srv.do(t, http.MethodPost, fmt.Sprintf("/api/v1/checks/runs/%s/rerun", runID), map[string]any{"tests": []string{"TestGet"}}) //nolint:bodyclose // do closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rerun status = %d body=%s", resp.StatusCode, body)
	}
	var record domain.CheckRunRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Filters) != 1 || record.Filters[0] != "--go-test=TestGet" {
		t.Fatalf("record filters = %v, want [--go-test=TestGet]", record.Filters)
	}
	last := runner.runs[len(runner.runs)-1]
	if len(last.Filters) != 1 || last.Filters[0] != "--go-test=TestGet" {
		t.Fatalf("runner filters = %v", last.Filters)
	}

	// An invalid filter is rejected with a 400 before anything is dispatched.
	dispatched := runner.count()
	resp, body = srv.do(t, http.MethodPost, fmt.Sprintf("/api/v1/checks/runs/%s/rerun", runID), map[string]any{"tests": []string{"--rm -rf /"}}) //nolint:bodyclose // do closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid filter status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "invalid check filter") {
		t.Fatalf("invalid filter body = %s", body)
	}
	if runner.count() != dispatched {
		t.Fatal("invalid filter must not dispatch")
	}

	// GraphQL: rerunChecks(filters:) is accepted and returns the stored filters.
	query := map[string]any{
		"query": fmt.Sprintf(`mutation { rerunChecks(org: "acme", checkIds: ["%s/test"], filters: ["go-test=TestGet"]) { id status filters } }`, runID),
	}
	resp, body = srv.do(t, http.MethodPost, "/query", query) //nolint:bodyclose // do closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("graphql status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), `"errors"`) {
		t.Fatalf("graphql errors: %s", body)
	}
	if !strings.Contains(string(body), `"--go-test=TestGet"`) {
		t.Fatalf("graphql filters missing: %s", body)
	}
}
