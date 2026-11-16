package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func runTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestRunTokenCallbackAuthorization(t *testing.T) {
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, nil, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	checkRepo := repository.NewCheckRunRepo(srv.store)
	checkToken := "check-run-token"
	otherCheckToken := "other-check-run-token"
	for _, rec := range []*domain.CheckRunRecord{
		{ID: "check-token-1", OrgID: "acme", ProjectID: "p1", Status: "running", RunTokenHash: runTokenHash(checkToken), CreatedAt: now, UpdatedAt: now},
		{ID: "check-token-2", OrgID: "acme", ProjectID: "p1", Status: "running", RunTokenHash: runTokenHash(otherCheckToken), CreatedAt: now, UpdatedAt: now},
		{ID: "check-token-empty", OrgID: "acme", ProjectID: "p1", Status: "running", CreatedAt: now, UpdatedAt: now},
	} {
		if err := checkRepo.Upsert(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	loadPath := "/api/v1/checks/runs/check-token-1/load"
	loadBody := map[string]any{"checks": []string{"test"}}
	if resp, body := srv.doWithRunToken(t, loadPath, loadBody, checkToken); resp.StatusCode != http.StatusOK { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("valid check token status = %d body=%s", resp.StatusCode, body)
	}
	if resp, _ := srv.do(t, http.MethodPost, loadPath, loadBody); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // srv.do closes the body
		t.Fatalf("missing check token status = %d", resp.StatusCode)
	}
	if resp, _ := srv.doWithRunToken(t, loadPath, loadBody, "wrong-token"); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("wrong check token status = %d", resp.StatusCode)
	}
	if resp, _ := srv.doWithRunToken(t, loadPath, loadBody, otherCheckToken); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("other-run check token status = %d", resp.StatusCode)
	}
	if resp, _ := srv.doWithRunToken(t, "/api/v1/checks/runs/check-token-empty/load", loadBody, checkToken); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("empty-hash check token status = %d", resp.StatusCode)
	}

	workflowRepo := repository.NewWorkflowRepo(srv.store)
	workflowToken := "workflow-run-token"
	otherWorkflowToken := "other-workflow-run-token"
	prevWorkflowToken := "previous-workflow-run-token"
	currWorkflowToken := "current-workflow-run-token"
	for _, run := range []*domain.WorkflowRun{
		{ID: "run-token-1", OrgID: "acme", ProjectID: "p1", Workflow: "release", Status: "running", Jobs: map[string]domain.WorkflowJobRun{"build": {Name: "build", Status: "running"}}, RunTokenHash: runTokenHash(workflowToken), CreatedAt: now, UpdatedAt: now},
		{ID: "run-token-2", OrgID: "acme", ProjectID: "p1", Workflow: "release", Status: "running", Jobs: map[string]domain.WorkflowJobRun{"build": {Name: "build", Status: "running"}}, RunTokenHash: runTokenHash(otherWorkflowToken), CreatedAt: now, UpdatedAt: now},
		{ID: "run-token-empty", OrgID: "acme", ProjectID: "p1", Workflow: "release", Status: "running", Jobs: map[string]domain.WorkflowJobRun{"build": {Name: "build", Status: "running"}}, CreatedAt: now, UpdatedAt: now},
		{ID: "run-token-prev", OrgID: "acme", ProjectID: "p1", Workflow: "release", Status: "running", Jobs: map[string]domain.WorkflowJobRun{"build": {Name: "build", Status: "running"}}, RunTokenHash: runTokenHash(currWorkflowToken), PreviousRunTokenHash: runTokenHash(prevWorkflowToken), CreatedAt: now, UpdatedAt: now},
	} {
		if err := workflowRepo.UpsertRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	completePath := "/api/v1/workflows/runs/run-token-1/jobs/build/complete"
	completeBody := map[string]any{"outputs": map[string]string{}}
	if resp, body := srv.doWithRunToken(t, completePath, completeBody, workflowToken); resp.StatusCode != http.StatusOK { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("valid workflow token status = %d body=%s", resp.StatusCode, body)
	}
	if resp, _ := srv.do(t, http.MethodPost, completePath, completeBody); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // srv.do closes the body
		t.Fatalf("missing workflow token status = %d", resp.StatusCode)
	}
	if resp, _ := srv.doWithRunToken(t, completePath, completeBody, "wrong-token"); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("wrong workflow token status = %d", resp.StatusCode)
	}
	if resp, _ := srv.doWithRunToken(t, completePath, completeBody, otherWorkflowToken); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("other-run workflow token status = %d", resp.StatusCode)
	}
	if resp, _ := srv.doWithRunToken(t, "/api/v1/workflows/runs/run-token-empty/jobs/build/complete", completeBody, workflowToken); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("empty-hash workflow token status = %d", resp.StatusCode)
	}
	// A run that re-minted after a leader restart must accept both the current
	// and the previous token, and reject anything else.
	prevPath := "/api/v1/workflows/runs/run-token-prev/jobs/build/complete"
	if resp, body := srv.doWithRunToken(t, prevPath, completeBody, currWorkflowToken); resp.StatusCode != http.StatusOK { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("current token after re-mint status = %d body=%s", resp.StatusCode, body)
	}
	if resp, body := srv.doWithRunToken(t, prevPath, completeBody, prevWorkflowToken); resp.StatusCode != http.StatusOK { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("previous token after re-mint status = %d body=%s", resp.StatusCode, body)
	}
	if resp, _ := srv.doWithRunToken(t, prevPath, completeBody, "wrong-token"); resp.StatusCode != http.StatusForbidden { //nolint:bodyclose // doWithRunToken closes the body
		t.Fatalf("wrong token after re-mint status = %d", resp.StatusCode)
	}

	// The hash must never leak through the API.
	_, body := srv.do(t, http.MethodGet, "/api/v1/checks/runs/check-token-1", nil) //nolint:bodyclose // srv.do closes the body
	if bytes.Contains(body, []byte("run_token_hash")) || bytes.Contains(body, []byte("RunTokenHash")) {
		t.Fatalf("check run response leaked run token hash: %s", body)
	}
	_, body = srv.do(t, http.MethodGet, "/api/v1/workflows/runs/run-token-1", nil) //nolint:bodyclose // srv.do closes the body
	if bytes.Contains(body, []byte("run_token_hash")) || bytes.Contains(body, []byte("RunTokenHash")) {
		t.Fatalf("workflow run response leaked run token hash: %s", body)
	}
}
