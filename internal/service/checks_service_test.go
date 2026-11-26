package service

import (
	"context"
	"errors"
	"io"
	"sync"
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
	accept    bool
}

func (f *fakeForgeProvider) Kind() domain.ForgeKind { return f.kind }
func (f *fakeForgeProvider) RegisterWebhook(context.Context, *domain.ForgeSource, string, string, string) (string, error) {
	return "1", nil
}
func (f *fakeForgeProvider) DeleteWebhook(context.Context, *domain.ForgeSource, string, string) error {
	return nil
}
func (f *fakeForgeProvider) VerifyWebhook(*domain.ForgeSource, string, map[string]string, []byte) bool {
	return f.accept
}

// recordingForgeProvider captures the secret handed to VerifyWebhook so tests
// can assert fallback selection.
type recordingForgeProvider struct {
	fakeForgeProvider
	gotSecret string
	accept    bool
}

func (r *recordingForgeProvider) VerifyWebhook(_ *domain.ForgeSource, secret string, _ map[string]string, _ []byte) bool {
	r.gotSecret = secret
	return r.accept
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
	return newChecksServiceForTestWithSecret(t, "")
}

func newChecksServiceForTestWithSecret(t *testing.T, fallbackSecret string) (*ChecksService, *fakeForgeProvider, *fakeCheckRunner, *repository.RaftStore) {
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
	provider := &fakeForgeProvider{kind: domain.ForgeGitHub, accept: true}
	runner := &fakeCheckRunner{}
	checkRuns := repository.NewCheckRunRepo(store)
	workflowSvc := NewWorkflowService(repository.NewWorkflowRepo(store), repository.NewEnvironmentRepo(store), orgsSvc, projectsSvc, sources, &fakeWorkflowParser{wf: &domain.Workflow{Name: "release", AfterChecks: true, On: domain.WorkflowTriggers{Push: true}, Jobs: map[string]domain.WorkflowJob{"build": {Call: "build"}}}}, &fakeEvaluator{}, &fakeArtifactStore{}, &fakeWorkflowRunner{}, secretSvc, WorkflowServiceConfig{}, logger)
	svc := NewChecksService(sources, secretSvc, projectsSvc, orgsSvc, runner, checkRuns, workflowSvc, []domain.ForgeProvider{provider}, "https://example.com", fallbackSecret, logger)
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

func TestChecksServiceRepoSettingsTriggerGate(t *testing.T) {
	svc, provider, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	repoSets := repository.NewRepoSettingRepo(store)
	svc.WithRepoSettings(repoSets)
	ctx := context.Background()
	if err := repoSets.BatchUpsert(ctx, "acme", []domain.RepoSettingWrite{{Setting: &domain.RepoSetting{
		OrgID: "acme", Repo: "github.com/acme/repo", Provider: domain.ForgeGitHub,
	}}}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo","default_branch":"main"}}`)
	res, err := svc.HandleWebhook(ctx, domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ignored || res.Reason != "trigger_disabled" {
		t.Fatalf("result = %+v", res)
	}
	if len(runner.runs) != 0 || len(provider.statuses) != 0 {
		t.Fatalf("disabled trigger must not dispatch or report: runs=%d statuses=%d", len(runner.runs), len(provider.statuses))
	}
	// Re-enable the default-branch trigger. An OAuth/PAT source with an
	// InstallationID but no AppID must report via commit status, not check-run.
	if err := repoSets.BatchUpsert(ctx, "acme", []domain.RepoSettingWrite{{Setting: &domain.RepoSetting{
		OrgID: "acme", Repo: "github.com/acme/repo", Provider: domain.ForgeGitHub,
		RunChecksOnDefaultBranch: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	src, err := repository.NewForgeSourceRepo(store).Get(ctx, "src-1")
	if err != nil {
		t.Fatal(err)
	}
	src.InstallationID = "12345"
	if err := repository.NewForgeSourceRepo(store).Upsert(ctx, src); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.HandleWebhook(ctx, domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("runner runs = %d", len(runner.runs))
	}
	if len(provider.statuses) == 0 || len(provider.checkRuns) != 0 {
		t.Fatalf("statuses=%d checkRuns=%d (PAT/OAuth sources must use commit statuses)", len(provider.statuses), len(provider.checkRuns))
	}
}

func TestChecksServiceNonForkWebhookDispatches(t *testing.T) {
	svc, provider, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo","default_branch":"main"}}`)
	res, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || res.Event.SHA != "abc123" || res.Event.DefaultBranch != "main" {
		t.Fatalf("result = %+v", res)
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
	res, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "pull_request"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event == nil || !res.Event.IsFork {
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

// newChecksServiceWithRecordingProvider builds a ChecksService whose provider
// records the secret passed to VerifyWebhook. sourceSecret is sealed onto the
// seeded source ("" stores an empty ciphertext).
func newChecksServiceWithRecordingProvider(t *testing.T, fallbackSecret, sourceSecret string) (*ChecksService, *recordingForgeProvider, *repository.RaftStore) {
	t.Helper()
	store, err := repository.NewInmemRaftStore("checks-fallback-test", testLogger(), 5*time.Second)
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
	provider := &recordingForgeProvider{fakeForgeProvider: fakeForgeProvider{kind: domain.ForgeGitHub}, accept: true}
	checkRuns := repository.NewCheckRunRepo(store)
	tokenCT, _ := secretSvc.SealRaw(domain.SecretScopeOrg, "acme", "forge-token")
	secretCT, _ := secretSvc.SealRaw(domain.SecretScopeOrg, "acme", sourceSecret)
	src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/repo", TokenCiphertext: tokenCT, WebhookSecretCiphertext: secretCT}
	if err := sources.Upsert(ctx, src); err != nil {
		t.Fatal(err)
	}
	svc := NewChecksService(sources, secretSvc, projectsSvc, orgsSvc, &fakeCheckRunner{}, checkRuns, nil, []domain.ForgeProvider{provider}, "https://example.com", fallbackSecret, logger)
	return svc, provider, store
}

func TestChecksServiceWebhookSecretFallback(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo"}}`)
	headers := map[string]string{"X-GitHub-Event": "push"}
	cases := []struct {
		name          string
		fallback      string
		sourceSecret  string
		wantSecret    string
		wantVerifyErr bool
	}{
		{name: "fallback used when source secret empty", fallback: "fallback-secret", sourceSecret: "", wantSecret: "fallback-secret"},
		{name: "source secret wins over fallback", fallback: "fallback-secret", sourceSecret: "source-secret", wantSecret: "source-secret"},
		{name: "both empty fails closed", fallback: "", sourceSecret: "", wantSecret: "", wantVerifyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, provider, _ := newChecksServiceWithRecordingProvider(t, tc.fallback, tc.sourceSecret)
			provider.accept = !tc.wantVerifyErr
			_, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", headers, body)
			if tc.wantVerifyErr {
				if err == nil {
					t.Fatal("expected verification failure when both secrets are empty")
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider.gotSecret != tc.wantSecret {
				t.Fatalf("VerifyWebhook secret = %q, want %q", provider.gotSecret, tc.wantSecret)
			}
		})
	}
}

// failingCheckRunner fails every dispatch to exercise the failDispatch path.
type failingCheckRunner struct {
	fakeCheckRunner
	err error
}

func (f *failingCheckRunner) Run(context.Context, *domain.CheckRunSpec) error { return f.err }

func TestChecksServiceIgnoresNonActionableEvents(t *testing.T) {
	cases := []struct {
		name     string
		headers  map[string]string
		body     string
		wantReas string
	}{
		{name: "ping", headers: map[string]string{"X-GitHub-Event": "ping"}, body: `{"zen":"hi"}`, wantReas: "ping"},
		{name: "workflow_run", headers: map[string]string{"X-GitHub-Event": "workflow_run"}, body: `{"action":"completed"}`, wantReas: "unsupported_event"},
		{name: "deleted ref", headers: map[string]string{"X-GitHub-Event": "push"}, body: `{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","repository":{"full_name":"acme/repo"}}`, wantReas: "ref_deleted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, provider, runner, store := newChecksServiceForTest(t)
			secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
			seedForgeSource(t, store, secretSvc)
			res, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", tc.headers, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if !res.Ignored || res.Reason != tc.wantReas {
				t.Fatalf("result = %+v, want ignored %q", res, tc.wantReas)
			}
			if len(runner.runs) != 0 {
				t.Fatalf("ignored delivery must not dispatch: %d runs", len(runner.runs))
			}
			if len(provider.statuses) != 0 || len(provider.checkRuns) != 0 {
				t.Fatal("ignored delivery must not report to the forge")
			}
			records, _ := svc.List(context.Background(), &domain.CheckRunFilter{OrgID: "acme"})
			if len(records) != 0 {
				t.Fatalf("ignored delivery must not create records: %+v", records)
			}
		})
	}
}

func TestChecksServiceInvalidSignatureForbidden(t *testing.T) {
	svc, provider, _, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	provider.accept = false
	body := []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/repo"}}`)
	_, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestChecksServiceDisabled(t *testing.T) {
	svc, provider, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	svc.WithEnabled(false)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo"}}`)
	res, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ignored || res.Reason != "checks_disabled" {
		t.Fatalf("result = %+v, want checks_disabled", res)
	}
	if len(runner.runs) != 0 {
		t.Fatal("disabled service must not dispatch")
	}
	if len(provider.statuses) != 0 {
		t.Fatal("disabled service must not report to the forge")
	}
	records, _ := svc.List(context.Background(), &domain.CheckRunFilter{OrgID: "acme"})
	if len(records) != 0 {
		t.Fatalf("disabled service must not create records: %+v", records)
	}
	if _, err := svc.Rerun(context.Background(), "check-src-1-abc123"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("rerun err = %v, want ErrValidation", err)
	}
	if _, err := svc.ApproveForkRun(context.Background(), "check-src-1-abc123", "u", false, true); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("approve err = %v, want ErrValidation", err)
	}
}

func TestChecksServiceDispatchFailureMarksFailed(t *testing.T) {
	svc, provider, _, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	svc.runner = &failingCheckRunner{err: errors.New("boom")}
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo"}}`)
	_, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body)
	if err == nil {
		t.Fatal("dispatch failure must surface an error")
	}
	records, _ := svc.List(context.Background(), &domain.CheckRunFilter{OrgID: "acme"})
	if len(records) != 1 || records[0].Status != "failed" || records[0].Error == "" {
		t.Fatalf("record must be failed with an error: %+v", records)
	}
	foundFailure := false
	for _, s := range provider.statuses {
		if s.State == domain.CheckFailure {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("forge must see a failure status: %+v", provider.statuses)
	}
}

func TestChecksServiceDuplicateDeliveryIdempotent(t *testing.T) {
	svc, _, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo"}}`)
	headers := map[string]string{"X-GitHub-Event": "push"}
	if _, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", headers, body); err != nil {
		t.Fatal(err)
	}
	firstToken := runner.runs[0].RunToken
	if _, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", headers, body); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 1 {
		t.Fatalf("duplicate delivery must not re-dispatch: %d runs", len(runner.runs))
	}
	record, err := svc.Get(context.Background(), runner.runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyRunToken(record.RunTokenHash, firstToken) {
		t.Fatal("duplicate delivery must not rotate the run token")
	}
}

func TestChecksServiceDeleteIdempotent(t *testing.T) {
	svc, _, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo"}}`)
	if _, err := svc.HandleWebhook(context.Background(), domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body); err != nil {
		t.Fatal(err)
	}
	id := runner.runs[0].ID
	if err := svc.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(context.Background(), id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(context.Background(), id); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}
}

func TestChecksServiceReconcilerTimeoutUnknownJob(t *testing.T) {
	svc, provider, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	checkRuns := newFakeCheckRunRepo()
	svc.checkRuns = checkRuns
	runner.status = domain.JobStateUnknown

	// A record stuck running with an unknown Job for > 5m is failed.
	stale := &domain.CheckRunRecord{
		ID: "check-stale", OrgID: "acme", SourceID: "src-1", Provider: domain.ForgeGitHub,
		Repo: "acme/repo", SHA: "abc", Status: "running",
		CreatedAt: time.Now().UTC().Add(-10 * time.Minute), UpdatedAt: time.Now().UTC().Add(-6 * time.Minute),
	}
	if err := checkRuns.Upsert(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	// A record running for < 5m with an unknown Job is left untouched.
	fresh := &domain.CheckRunRecord{
		ID: "check-fresh", OrgID: "acme", SourceID: "src-1", Provider: domain.ForgeGitHub,
		Repo: "acme/repo", SHA: "def", Status: "running",
		CreatedAt: time.Now().UTC().Add(-10 * time.Minute), UpdatedAt: time.Now().UTC().Add(-4 * time.Minute),
	}
	if err := checkRuns.Upsert(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}

	svc.reconcile(context.Background())

	updated, err := svc.Get(context.Background(), "check-stale")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "failed" || updated.Error == "" {
		t.Fatalf("timed-out record = %+v", updated)
	}
	untouched, err := svc.Get(context.Background(), "check-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Status != "running" {
		t.Fatalf("fresh record must stay running: %+v", untouched)
	}
	foundFailure := false
	for _, s := range provider.statuses {
		if s.State == domain.CheckFailure {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Fatalf("forge must see a failure status: %+v", provider.statuses)
	}
}

// fakeCheckRunRepo is an in-memory CheckRunRepository that preserves the
// caller-supplied UpdatedAt (the Raft repo always stamps it), so the
// reconciler-timeout test can seed a stale record.
type fakeCheckRunRepo struct {
	mu      sync.Mutex
	records map[string]*domain.CheckRunRecord
}

func newFakeCheckRunRepo() *fakeCheckRunRepo {
	return &fakeCheckRunRepo{records: map[string]*domain.CheckRunRecord{}}
}

func (f *fakeCheckRunRepo) Upsert(_ context.Context, r *domain.CheckRunRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.records[r.ID] = &cp
	return nil
}

func (f *fakeCheckRunRepo) Get(_ context.Context, id string) (*domain.CheckRunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *rec
	return &cp, nil
}

func (f *fakeCheckRunRepo) List(_ context.Context, filter *domain.CheckRunFilter) ([]*domain.CheckRunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.CheckRunRecord
	for _, rec := range f.records {
		if filter != nil && filter.Status != "" && rec.Status != filter.Status {
			continue
		}
		cp := *rec
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeCheckRunRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, id)
	return nil
}

// TestRerunWithFilters proves typed filters merge into the record, reach the
// dispatched spec as validated argv, and that an invalid filter fails with
// ErrValidation before the record is mutated or a Job is dispatched.
func TestRerunWithFilters(t *testing.T) {
	svc, _, runner, store := newChecksServiceForTest(t)
	secretSvc := NewSecretService(repository.NewSecretRepo(store), []byte("0123456789abcdef0123456789abcdef"), logrus.New())
	seedForgeSource(t, store, secretSvc)
	ctx := context.Background()
	body := []byte(`{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/repo","default_branch":"main"}}`)
	if _, err := svc.HandleWebhook(ctx, domain.ForgeGitHub, "src-1", map[string]string{"X-GitHub-Event": "push"}, body); err != nil {
		t.Fatal(err)
	}
	runID := "check-src-1-abc123"
	if len(runner.runs) != 1 {
		t.Fatalf("dispatch count = %d, want 1", len(runner.runs))
	}

	record, err := svc.RerunWithFilters(ctx, runID, domain.CheckRunFilters{Tests: []string{"TestGet"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Filters; len(got) != 1 || got[0] != "--go-test=TestGet" {
		t.Fatalf("record filters = %v, want [--go-test=TestGet]", got)
	}
	if got := runner.runs[len(runner.runs)-1].Filters; len(got) != 1 || got[0] != "--go-test=TestGet" {
		t.Fatalf("spec filters = %v, want [--go-test=TestGet]", got)
	}

	// A second rerun merges rather than replaces.
	record, err = svc.RerunWithFilters(ctx, runID, domain.CheckRunFilters{
		Tests: []string{"TestGet", "vitest-test-file=app.test.ts"},
		Files: []string{"./pkg/..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--go-test=TestGet", "--vitest-test-file=app.test.ts", "--go-package=./pkg/..."}
	if len(record.Filters) != len(want) {
		t.Fatalf("merged filters = %v, want %v", record.Filters, want)
	}
	for i := range want {
		if record.Filters[i] != want[i] {
			t.Fatalf("merged filters = %v, want %v", record.Filters, want)
		}
	}

	dispatched := len(runner.runs)
	for _, bad := range []domain.CheckRunFilters{
		{Tests: []string{"--rm -rf /"}},
		{Files: []string{"$(whoami)"}},
		{Checks: []string{""}},
	} {
		if _, err := svc.RerunWithFilters(ctx, runID, bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("filter %+v err = %v, want ErrValidation", bad, err)
		}
	}
	if len(runner.runs) != dispatched {
		t.Fatal("invalid filters must not dispatch")
	}
	unchanged, err := svc.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Filters) != len(want) {
		t.Fatalf("record mutated by invalid filters: %v", unchanged.Filters)
	}

	// A plain rerun keeps the stored filters.
	if _, err := svc.Rerun(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if got := runner.runs[len(runner.runs)-1].Filters; len(got) != len(want) {
		t.Fatalf("plain rerun lost filters: %v", got)
	}

	// Fork runs still require approval, filters or not.
	svc.checkRuns.Upsert(ctx, &domain.CheckRunRecord{ID: "check-src-1-fork", OrgID: "acme", ProjectID: "p1", SourceID: "src-1", IsFork: true, Status: "waiting_approval"})
	if _, err := svc.RerunWithFilters(ctx, "check-src-1-fork", domain.CheckRunFilters{Tests: []string{"TestGet"}}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("fork rerun err = %v, want ErrForbidden", err)
	}
}
