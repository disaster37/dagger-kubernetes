package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// seedResidualForgeSource seals the shared webhook secret onto a GitHub source
// for the residual webhook tests.
func seedResidualForgeSource(t *testing.T, srv *parityServer) {
	t.Helper()
	ctx := context.Background()
	tokenCT, _ := srv.secrets.SealRaw(domain.SecretScopeOrg, "acme", "forge-token")
	secretCT, _ := srv.secrets.SealRaw(domain.SecretScopeOrg, "acme", "webhook-secret")
	src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/repo", TokenCiphertext: tokenCT, WebhookSecretCiphertext: secretCT}
	if err := repository.NewForgeSourceRepo(srv.store).Upsert(ctx, src); err != nil {
		t.Fatal(err)
	}
}

// postWebhook sends a signed GitHub delivery and returns the status and body.
func postWebhook(t *testing.T, srv *parityServer, event, secret string, body []byte) (status int, respBody []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.base+"/v1/webhooks/github/src-1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signGitHub(secret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, respBody
}

func TestResidualSignedPingIgnored(t *testing.T) {
	checkRunner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, checkRunner, nil)
	seedResidualForgeSource(t, srv)

	status, body := postWebhook(t, srv, "ping", "webhook-secret", []byte(`{"zen":"hi"}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var res map[string]string
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ignored" || res["reason"] != "ping" || res["event"] != "ping" {
		t.Fatalf("ignored body = %s", body)
	}
	if checkRunner.count() != 0 {
		t.Fatal("ping must not dispatch a check job")
	}
	_, listBody := srv.do(t, http.MethodGet, "/api/v1/orgs/acme/checks", nil) //nolint:bodyclose // srv.do closes the body
	var records []*domain.CheckRunRecord
	if err := json.Unmarshal(listBody, &records); err != nil || len(records) != 0 {
		t.Fatalf("ping must not create records: %s err=%v", listBody, err)
	}
}

func TestResidualSignedWorkflowRunIgnored(t *testing.T) {
	checkRunner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, checkRunner, nil)
	seedResidualForgeSource(t, srv)

	status, body := postWebhook(t, srv, "workflow_run", "webhook-secret", []byte(`{"action":"completed"}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var res map[string]string
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ignored" || res["reason"] != "unsupported_event" {
		t.Fatalf("ignored body = %s", body)
	}
	if checkRunner.count() != 0 {
		t.Fatal("workflow_run must not dispatch a check job")
	}
}

func TestResidualBadSignaturePingUnauthorized(t *testing.T) {
	checkRunner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, checkRunner, nil)
	seedResidualForgeSource(t, srv)

	status, body := postWebhook(t, srv, "ping", "wrong-secret", []byte(`{"zen":"hi"}`))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", status, body)
	}
}

func TestResidualDisabledChecks(t *testing.T) {
	checkRunner := &recordingCheckRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, checkRunner, nil)
	seedResidualForgeSource(t, srv)
	srv.checks.WithEnabled(false)

	status, body := postWebhook(t, srv, "push", "webhook-secret", []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/repo"}}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var res map[string]string
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if res["status"] != "ignored" || res["reason"] != "checks_disabled" {
		t.Fatalf("disabled body = %s", body)
	}
	if checkRunner.count() != 0 {
		t.Fatal("disabled checks must not dispatch")
	}
	// GraphQL rerun keeps the nil-rerunner error path (no rerunner wired).
	if _, status := gqlPost(t, srv.base, srv.admin, `mutation { rerunChecks(org: "acme", checkIds: ["check-x/y"]) { id } }`, nil); status != http.StatusOK {
		t.Fatalf("graphql rerun status = %d", status)
	}
	// REST rerun is gated with 400.
	resp, rerunBody := srv.do(t, http.MethodPost, "/api/v1/checks/runs/check-x-y/rerun", map[string]any{}) //nolint:bodyclose // srv.do closes the body
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rest rerun status = %d body=%s", resp.StatusCode, rerunBody)
	}
}

// failingDispatchRunner fails every dispatch to exercise the failed-record path.
type failingDispatchRunner struct{}

func (failingDispatchRunner) Run(context.Context, *domain.CheckRunSpec) error {
	return fmt.Errorf("simulated dispatch failure")
}
func (failingDispatchRunner) CancelSuperseded(context.Context, string, string) error { return nil }
func (failingDispatchRunner) JobStatus(context.Context, string) (domain.JobState, error) {
	return domain.JobStateActive, nil
}

func TestResidualRunnerFailureMarksFailedAndDelete(t *testing.T) {
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, failingDispatchRunner{}, nil)
	seedResidualForgeSource(t, srv)

	status, body := postWebhook(t, srv, "push", "webhook-secret", []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/repo"}}`))
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", status, body)
	}
	_, listBody := srv.do(t, http.MethodGet, "/api/v1/orgs/acme/checks", nil) //nolint:bodyclose // srv.do closes the body
	var records []*domain.CheckRunRecord
	if err := json.Unmarshal(listBody, &records); err != nil || len(records) != 1 {
		t.Fatalf("records = %s err=%v", listBody, err)
	}
	if records[0].Status != "failed" || records[0].Error == "" {
		t.Fatalf("record must be failed with an error: %+v", records[0])
	}
	id := records[0].ID
	resp, _ := srv.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/checks/runs/%s", id), nil) //nolint:bodyclose // srv.do closes the body
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp, _ = srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/checks/runs/%s", id), nil) //nolint:bodyclose // srv.do closes the body
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status = %d", resp.StatusCode)
	}
}
