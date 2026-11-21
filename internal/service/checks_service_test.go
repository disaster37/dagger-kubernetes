package service

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

type fakeForgeProvider struct {
	kind      domain.ForgeKind
	statuses  []*domain.CheckRun
	checkRuns []*domain.CheckRun
}

func (f *fakeForgeProvider) Kind() domain.ForgeKind { return f.kind }
func (f *fakeForgeProvider) RegisterWebhook(context.Context, *domain.ForgeSource, string, string, string) (string, error) {
	return "1", nil
}
func (f *fakeForgeProvider) DeleteWebhook(context.Context, *domain.ForgeSource, string, string) error {
	return nil
}
func (f *fakeForgeProvider) VerifyWebhook(*domain.ForgeSource, string, map[string]string, []byte) bool {
	return true
}
func (f *fakeForgeProvider) SetCommitStatus(_ context.Context, _ *domain.ForgeSource, _ string, run *domain.CheckRun) error {
	f.statuses = append(f.statuses, run)
	return nil
}
func (f *fakeForgeProvider) CreateCheckRun(_ context.Context, _ *domain.ForgeSource, _ string, run *domain.CheckRun) error {
	f.checkRuns = append(f.checkRuns, run)
	return nil
}

type fakeCheckRunner struct {
	runs   []*domain.CheckRunSpec
	status domain.JobState
}

func (f *fakeCheckRunner) Run(_ context.Context, spec *domain.CheckRunSpec) error {
	f.runs = append(f.runs, spec)
	return nil
}
func (f *fakeCheckRunner) CancelSuperseded(context.Context, string, string) error { return nil }
func (f *fakeCheckRunner) JobStatus(context.Context, string) (domain.JobState, error) {
	return f.status, nil
}

func newChecksServiceForTest(t *testing.T) (*ChecksService, *fakeForgeProvider, *fakeCheckRunner, *repository.RaftStore) {
	t.Helper()
	store, err := repository.NewInmemRaftStore("checks-test", testLogger(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatal(err)
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	orgRepo := repository.NewOrgRepo(store)
	if err := orgRepo.Create(ctx, &domain.Org{ID: "acme", Name: "acme", DisplayName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logger)
	tokensSvc := NewTokenService(repository.NewTokenRepo(store), logger, []byte("0123456789abcdef0123456789abcdef"))
	orgsSvc := NewOrgService(orgRepo, repository.NewGroupRepo(store), tokensSvc, logger)
	projectsSvc := NewProjectService(repository.NewProjectRepo(store), repository.NewGroupRepo(store), logger)
	sources := repository.NewForgeSourceRepo(store)
	provider := &fakeForgeProvider{kind: domain.ForgeGitHub}
	runner := &fakeCheckRunner{}
	checkRuns := repository.NewCheckRunRepo(store)
	workflowSvc := NewWorkflowService(repository.NewWorkflowRepo(store), repository.NewEnvironmentRepo(store), orgsSvc, projectsSvc, sources, &fakeWorkflowParser{wf: &domain.Workflow{Name: "release", AfterChecks: true, On: domain.WorkflowTriggers{Push: true}, Jobs: map[string]domain.WorkflowJob{"build": {Call: "build"}}}}, &fakeEvaluator{}, &fakeArtifactStore{}, &fakeWorkflowRunner{}, secretSvc, WorkflowServiceConfig{}, logger)
	svc := NewChecksService(sources, secretSvc, projectsSvc, orgsSvc, runner, checkRuns, workflowSvc, []domain.ForgeProvider{provider}, "https://example.com", logger)
	return svc, provider, runner, store
}

func seedForgeSource(t *testing.T, store *repository.RaftStore, secretSvc *SecretService) {
	t.Helper()
	ctx := context.Background()
	tokenCT, _ := secretSvc.SealRaw(domain.SecretScopeOrg, "acme", "forge-token")
	secretCT, _ := secretSvc.SealRaw(domain.SecretScopeOrg, "acme", "webhook-secret")
	src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/repo", TokenCiphertext: tokenCT, WebhookSecretCiphertext: secretCT}
	if err := repository.NewForgeSourceRepo(store).Upsert(ctx, src); err != nil {
		t.Fatal(err)
	}
}

func TestChecksServiceNonForkWebhookDispatches(t *testing.T) {
	svc, provider, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo","default_branch":"main"}}`)
	ev, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.SHA != "abc123" || ev.DefaultBranch != "main" {
		t.Fatalf("event = %+v", ev)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("runner runs = %d", len(runner.runs))
	}
	if runner.runs[0].Token == "" {
		t.Fatal("check job must receive an org token")
	}
	if runner.runs[0].RunToken == "" {
		t.Fatal("check job must receive a per-run token")
	}
	record, err := svc.Get(context.Background(), runner.runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRunToken(record.RunTokenHash, runner.runs[0].RunToken) {
		t.Fatal("stored hash must match the dispatched run token")
	}
	if len(provider.statuses) == 0 || provider.statuses[0].State != domain.CheckPending {
		t.Fatalf("forge statuses = %+v", provider.statuses)
	}
}

func TestChecksServiceForkHoldsUntilApproval(t *testing.T) {
	svc, _, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/pull/1/head","after":"abc123","repository":{"full_name":"acme/repo"},"pull_request":{"number":1,"head":{"sha":"abc123","repo":{"id":1}},"base":{"repo":{"id":2}}}}`)
	ev, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "pull_request"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsFork {
		t.Fatal("expected fork event")
	}
	if len(runner.runs) != 0 {
		t.Fatal("fork PR must not launch a job before approval")
	}
	records, err := svc.List(context.Background(), &domain.CheckRunFilter{OrgID: "acme"})
	if err != nil || len(records) != 1 || records[0].Status != "waiting_approval" {
		t.Fatalf("records = %+v err=%v", records, err)
	}
	if _, err := svc.ApproveForkRun(context.Background(), records[0].ID, "member", true, false); err == nil {
		t.Fatal("non-admin include_secrets must be forbidden")
	}
	if _, err := svc.ApproveForkRun(context.Background(), records[0].ID, "member", false, false); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("runner runs = %d", len(runner.runs))
	}
	firstToken := runner.runs[0].RunToken
	record, err := svc.Get(context.Background(), records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRunToken(record.RunTokenHash, firstToken) {
		t.Fatal("approval dispatch hash must match the run token")
	}
	if _, err := svc.Rerun(context.Background(), records[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("runner runs after rerun = %d", len(runner.runs))
	}
	if runner.runs[1].RunToken == "" || runner.runs[1].RunToken == firstToken {
		t.Fatal("rerun must mint a fresh per-run token")
	}
	record, err = svc.Get(context.Background(), records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRunToken(record.RunTokenHash, runner.runs[1].RunToken) {
		t.Fatal("rerun hash must match the new token")
	}
}

func TestChecksServiceLoadCompleteAndReconcile(t *testing.T) {
	svc, provider, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo","default_branch":"main"}}`)
	if _, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body); err != nil {
		t.Fatal(err)
	}
	records, _ := svc.List(context.Background(), &domain.CheckRunFilter{OrgID: "acme"})
	runID := records[0].ID
	loaded, err := svc.RecordLoad(context.Background(), "acme", runID, LoadReportInput{
		Checks:        []string{"test"},
		WorkflowFiles: map[string]string{"release.yaml": "name: release\non:\n  push: true\nafter_checks: true\njobs:\n  build:\n    call: build\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Checks) != 1 || loaded.Checks[0].State != domain.CheckPending {
		t.Fatalf("loaded = %+v", loaded)
	}
	completed, err := svc.RecordComplete(context.Background(), "acme", runID, CompleteReportInput{
		Checks: []domain.CheckItem{{Name: "test", State: domain.CheckSuccess, TraceID: "trace-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "success" {
		t.Fatalf("status = %s", completed.Status)
	}
	foundTerminal := false
	for _, s := range provider.statuses {
		if s.State == domain.CheckSuccess {
			foundTerminal = true
		}
	}
	if !foundTerminal {
		t.Fatalf("forge statuses = %+v", provider.statuses)
	}
	// Reconciler marks a failed Job as failed.
	runner.status = domain.JobStateFailed
	completed.Status = "running"
	if err := repository.NewCheckRunRepo(store).Upsert(context.Background(), completed); err != nil {
		t.Fatal(err)
	}
	svc.reconcile(context.Background())
	updated, err := svc.Get(context.Background(), completed.ID)
	if err != nil || updated.Status != "failed" {
		t.Fatalf("reconciled = %+v err=%v", updated, err)
	}
}

func TestChecksServiceRejectsCrossOrgLoad(t *testing.T) {
	svc, _, _, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo"}}`)
	if _, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body); err != nil {
		t.Fatal(err)
	}
	records, _ := svc.List(context.Background(), &domain.CheckRunFilter{OrgID: "acme"})
	if _, err := svc.RecordLoad(context.Background(), "other", records[0].ID, LoadReportInput{}); err == nil {
		t.Fatal("cross-org load must be forbidden")
	}
}
