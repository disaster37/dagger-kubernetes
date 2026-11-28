package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TestCheckRunLoadDescriptors proves the additive load-phase descriptors become
// check filters and typed-artifact refs on the replicated record, and that the
// pre-descriptor payload shape is still accepted.
func TestCheckRunLoadDescriptors(t *testing.T) {
	runner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, runner, nil)
	seedResidualForgeSource(t, srv)
	runID, runToken := seedPushRun(t, srv, runner, "abc123")

	resp, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/load", runID), map[string]any{ //nolint:bodyclose // doWithRunToken closes the body
		"checks": []string{"get-item"},
		"check_descriptors": []map[string]any{{
			"name":    "get-item",
			"filters": map[string]string{"MODULE": "go", "GO-TEST": "TestGet", "LINK": "dag://go/TestGet"},
		}},
	}, runToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load status = %d body=%s", resp.StatusCode, body)
	}

	_, body = srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/checks/runs/%s", runID), nil) //nolint:bodyclose // do closes the body
	var record domain.CheckRunRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Checks) != 1 || record.Checks[0].Filters["GO-TEST"] != "TestGet" {
		t.Fatalf("check filters = %+v", record.Checks)
	}
	if len(record.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", record.Artifacts)
	}
	artifact := record.Artifacts[0]
	if artifact.Kind != "go" || artifact.Name != "get-item" || artifact.Link != "dag://go/TestGet" {
		t.Fatalf("artifact = %+v", artifact)
	}
	if artifact.Filters["MODULE"] != "go" {
		t.Fatalf("artifact filters = %+v", artifact.Filters)
	}

	// Old runner payload (names only) is still accepted and clears artifacts.
	resp, body = srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/load", runID), map[string]any{ //nolint:bodyclose // doWithRunToken closes the body
		"checks": []string{"plain"},
	}, runToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy load status = %d body=%s", resp.StatusCode, body)
	}
	_, body = srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/checks/runs/%s", runID), nil) //nolint:bodyclose // do closes the body
	record = domain.CheckRunRecord{}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Checks) != 1 || len(record.Checks[0].Filters) != 0 || len(record.Artifacts) != 0 {
		t.Fatalf("legacy load = %+v artifacts=%+v", record.Checks, record.Artifacts)
	}
}
