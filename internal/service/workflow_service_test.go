package service

import (
	"context"
	"errors"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type fakeWorkflowParser struct{ wf *domain.Workflow }

func (f *fakeWorkflowParser) Parse(string, []byte) (*domain.Workflow, error) { return f.wf, nil }

type fakeEvaluator struct{}

func (f *fakeEvaluator) Eval(string, map[string]any, string) (any, error) { return true, nil }

type fakeArtifactStore struct{}

func (f *fakeArtifactStore) Put(context.Context, string, string, string, string, []byte) (string, error) {
	return "key", nil
}
func (f *fakeArtifactStore) Get(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fakeArtifactStore) List(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}
func (f *fakeArtifactStore) DeleteRun(context.Context, string, string, string) error { return nil }

type fakeWorkflowRunner struct {
	calls int
	specs []*domain.WorkflowCallSpec
}

func (f *fakeWorkflowRunner) RunCall(_ context.Context, spec *domain.WorkflowCallSpec) (map[string]string, error) {
	f.calls++
	f.specs = append(f.specs, spec)
	return map[string]string{"out": "ok"}, nil
}

type fakeWorkflowRepo struct {
	defs   map[string]*domain.WorkflowDef
	runs   map[string]*domain.WorkflowRun
	nextID int64
}

func newFakeWorkflowRepo() *fakeWorkflowRepo {
	return &fakeWorkflowRepo{defs: map[string]*domain.WorkflowDef{}, runs: map[string]*domain.WorkflowRun{}}
}

func (f *fakeWorkflowRepo) SyncProjectCatalog(_ context.Context, orgID, projectID string, defs []*domain.WorkflowDef) error {
	for _, def := range defs {
		f.defs[orgID+"/"+projectID+"/"+def.Workflow.Name] = def
	}
	return nil
}
func (f *fakeWorkflowRepo) ListDefs(_ context.Context, orgID, projectID string) ([]*domain.WorkflowDef, error) {
	var out []*domain.WorkflowDef
	for _, def := range f.defs {
		if orgID != "" && def.OrgID != orgID {
			continue
		}
		if projectID != "" && def.ProjectID != projectID {
			continue
		}
		out = append(out, def)
	}
	return out, nil
}
func (f *fakeWorkflowRepo) GetDef(_ context.Context, orgID, projectID, name string) (*domain.WorkflowDef, error) {
	def, ok := f.defs[orgID+"/"+projectID+"/"+name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return def, nil
}
func (f *fakeWorkflowRepo) CreateRun(_ context.Context, run *domain.WorkflowRun) (*domain.WorkflowRun, error) {
	f.nextID++
	run.Number = f.nextID
	run.ID = "run-" + run.Workflow
	f.runs[run.ID] = run
	return run, nil
}
func (f *fakeWorkflowRepo) UpsertRun(_ context.Context, run *domain.WorkflowRun) error {
	f.runs[run.ID] = run
	return nil
}
func (f *fakeWorkflowRepo) GetRun(_ context.Context, id string) (*domain.WorkflowRun, error) {
	run, ok := f.runs[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return run, nil
}
func (f *fakeWorkflowRepo) ListRuns(_ context.Context, filter *domain.WorkflowRunFilter) ([]*domain.WorkflowRun, error) {
	var out []*domain.WorkflowRun
	for _, run := range f.runs {
		if filter != nil && filter.Status != "" && run.Status != filter.Status {
			continue
		}
		out = append(out, run)
	}
	return out, nil
}
func (f *fakeWorkflowRepo) DeleteRun(_ context.Context, id string) error {
	delete(f.runs, id)
	return nil
}

type fakeEnvRepo struct {
	envs map[string]*domain.Environment
}

func (f *fakeEnvRepo) Upsert(_ context.Context, env *domain.Environment) error {
	f.envs[env.OrgID+"/"+env.Name] = env
	return nil
}
func (f *fakeEnvRepo) Get(_ context.Context, orgID, name string) (*domain.Environment, error) {
	env, ok := f.envs[orgID+"/"+name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return env, nil
}
func (f *fakeEnvRepo) ListByOrg(context.Context, string) ([]*domain.Environment, error) {
	return nil, nil
}
func (f *fakeEnvRepo) Delete(context.Context, string, string) error { return nil }

func newWorkflowServiceForTest(t *testing.T, wf *domain.Workflow, runner *fakeWorkflowRunner, envs *fakeEnvRepo) (*WorkflowService, *fakeWorkflowRepo) {
	t.Helper()
	repo := newFakeWorkflowRepo()
	svc := NewWorkflowService(repo, envs, nil, nil, nil, &fakeWorkflowParser{wf: wf}, &fakeEvaluator{}, &fakeArtifactStore{}, runner, nil, WorkflowServiceConfig{DefaultTimeout: 0, MaxParallel: 1}, logrus.New())
	return svc, repo
}

func TestWorkflowServiceStart(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		Jobs: map[string]domain.WorkflowJob{
			"build":  {Call: "build"},
			"deploy": {Call: "deploy", Environment: "prod"},
		},
	}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	run, err := svc.Start(context.Background(), StartWorkflowInput{
		Workflow: wf, OrgID: "acme", ProjectID: "p1", SHA: "sha", Ref: "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["build"].Status != "running" {
		t.Fatalf("build = %+v", run.Jobs["build"])
	}
	if run.Jobs["deploy"].Status != "waiting_approval" {
		t.Fatalf("deploy = %+v", run.Jobs["deploy"])
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d", runner.calls)
	}
	if run.RunTokenHash == "" || runner.specs[0].RunToken == "" {
		t.Fatal("workflow run must carry a per-run token")
	}
	if !VerifyRunToken(run.RunTokenHash, runner.specs[0].RunToken) {
		t.Fatal("stored hash must match the dispatched run token")
	}
	completed, err := svc.CompleteJob(context.Background(), run.ID, "build", map[string]string{"out": "ok"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if completed.Jobs["build"].Status != "success" {
		t.Fatalf("build after complete = %+v", completed.Jobs["build"])
	}
}

func TestEnsureRunTokenReMintPreservesPreviousHash(t *testing.T) {
	svc := NewWorkflowService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, WorkflowServiceConfig{}, logrus.New())
	oldToken, oldHash := newRunToken()
	run := &domain.WorkflowRun{ID: "run-1", RunTokenHash: oldHash}

	token, err := svc.ensureRunToken(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || token == oldToken {
		t.Fatal("re-mint must produce a fresh token")
	}
	if run.PreviousRunTokenHash != oldHash {
		t.Fatalf("previous hash = %q, want %q", run.PreviousRunTokenHash, oldHash)
	}
	if !VerifyRunToken(run.RunTokenHash, token) {
		t.Fatal("new hash must match the new token")
	}
	if !VerifyRunToken(run.PreviousRunTokenHash, oldToken) {
		t.Fatal("previous hash must still match the old token")
	}

	// A second call must return the cached token without re-minting.
	again, err := svc.ensureRunToken(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Fatal("second call must return the cached token")
	}
	if run.PreviousRunTokenHash != oldHash {
		t.Fatal("previous hash must not change on a cached hit")
	}
}

func TestWorkflowServiceApproveResumesDownstream(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		Jobs: map[string]domain.WorkflowJob{
			"build":  {Call: "build"},
			"deploy": {Call: "deploy", Environment: "prod", Needs: []string{"build"}},
			"notify": {Call: "notify", Needs: []string{"deploy"}},
		},
	}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	run, err := svc.Start(context.Background(), StartWorkflowInput{
		Workflow: wf, OrgID: "acme", ProjectID: "p1", SHA: "sha", Ref: "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = svc.CompleteJob(context.Background(), run.ID, "build", map[string]string{"out": "ok"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["deploy"].Status != "waiting_approval" {
		t.Fatalf("deploy = %+v", run.Jobs["deploy"])
	}
	approved, err := svc.Approve(context.Background(), run.ID, "deploy", &domain.Identity{UserID: "admin", Role: domain.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Jobs["deploy"].Status != "running" {
		t.Fatalf("deploy after approve = %+v", approved.Jobs["deploy"])
	}
	approved, err = svc.CompleteJob(context.Background(), run.ID, "deploy", map[string]string{"out": "ok"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Jobs["notify"].Status != "running" {
		t.Fatalf("notify after deploy = %+v", approved.Jobs["notify"])
	}
	approved, err = svc.CompleteJob(context.Background(), run.ID, "notify", map[string]string{"out": "ok"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "success" {
		t.Fatalf("status = %s", approved.Status)
	}
}

func TestWorkflowServiceApproveRBACAndSelfApproval(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		Jobs: map[string]domain.WorkflowJob{
			"deploy": {Call: "deploy", Environment: "prod"},
		},
	}
	envs := &fakeEnvRepo{envs: map[string]*domain.Environment{
		"acme/prod": {OrgID: "acme", Name: "prod", ApproverGroupIDs: []string{"release-approvers"}, PreventSelfApproval: true},
	}}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, envs)
	run, err := svc.Start(context.Background(), StartWorkflowInput{
		Workflow: wf, OrgID: "acme", ProjectID: "p1", SHA: "sha", Ref: "refs/heads/main", TriggeredBy: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(context.Background(), run.ID, "deploy", &domain.Identity{UserID: "bob"}); err == nil {
		t.Fatal("non-approver must be rejected")
	}
	if _, err := svc.Approve(context.Background(), run.ID, "deploy", &domain.Identity{UserID: "alice", GroupIDs: []string{"release-approvers"}}); err == nil {
		t.Fatal("self-approval must be rejected")
	}
	if _, err := svc.Approve(context.Background(), run.ID, "deploy", &domain.Identity{UserID: "carol", GroupIDs: []string{"release-approvers"}}); err != nil {
		t.Fatalf("approver: %v", err)
	}
}

func TestWorkflowServiceReject(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		Jobs: map[string]domain.WorkflowJob{
			"deploy": {Call: "deploy", Environment: "prod"},
			"notify": {Call: "notify", Needs: []string{"deploy"}},
		},
	}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	run, err := svc.Start(context.Background(), StartWorkflowInput{
		Workflow: wf, OrgID: "acme", ProjectID: "p1", SHA: "sha", Ref: "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := svc.Reject(context.Background(), run.ID, "deploy", &domain.Identity{UserID: "admin", Role: domain.RoleAdmin}, "no")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Jobs["deploy"].Status != "failed" || rejected.Status != "failed" {
		t.Fatalf("run = %+v", rejected)
	}
	if rejected.Jobs["notify"].Status != "skipped" {
		t.Fatalf("downstream = %+v", rejected.Jobs["notify"])
	}
}

func TestWorkflowServiceNeedsOutputs(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		Jobs: map[string]domain.WorkflowJob{
			"build":  {Call: "build", Outputs: map[string]string{"tag": "v1"}},
			"deploy": {Call: "deploy", Needs: []string{"build"}},
		},
	}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	run, err := svc.Start(context.Background(), StartWorkflowInput{
		Workflow: wf, OrgID: "acme", ProjectID: "p1", SHA: "sha", Ref: "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["build"].Outputs["tag"] != "v1" {
		t.Fatalf("outputs = %+v", run.Jobs["build"].Outputs)
	}
	run, err = svc.CompleteJob(context.Background(), run.ID, "build", map[string]string{"out": "ok"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["deploy"].Status != "running" {
		t.Fatalf("deploy = %+v", run.Jobs["deploy"])
	}
}

func TestWorkflowServiceDispatchValidation(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		On: domain.WorkflowTriggers{WorkflowDispatch: &domain.WorkflowDispatch{Inputs: map[string]domain.WorkflowInput{
			"env": {Type: "choice", Required: true, Options: []string{"prod", "staging"}},
		}}},
		Jobs: map[string]domain.WorkflowJob{"build": {Call: "build"}},
	}
	runner := &fakeWorkflowRunner{}
	svc, repo := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	repo.defs["acme/p1/release"] = &domain.WorkflowDef{OrgID: "acme", ProjectID: "p1", Workflow: *wf}
	if _, err := svc.Dispatch(context.Background(), "acme", "p1", "release", "refs/heads/main", map[string]any{"env": "nope"}, nil); err == nil {
		t.Fatal("invalid choice must fail")
	}
	if _, err := svc.Dispatch(context.Background(), "acme", "p1", "release", "refs/heads/main", map[string]any{"env": "prod"}, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestWorkflowServiceOnArtifactPublished(t *testing.T) {
	wf := &domain.Workflow{
		Name: "publish",
		On:   domain.WorkflowTriggers{Artifact: []string{"report"}},
		Jobs: map[string]domain.WorkflowJob{"build": {Call: "build"}},
	}
	runner := &fakeWorkflowRunner{}
	svc, repo := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	repo.defs["acme/p1/publish"] = &domain.WorkflowDef{OrgID: "acme", ProjectID: "p1", Workflow: *wf}
	if err := svc.OnArtifactPublished(context.Background(), "acme", "p1", "run-1", "report", "artifacts/acme/p1/run-1/report"); err != nil {
		t.Fatal(err)
	}
	if len(repo.runs) != 1 {
		t.Fatalf("runs = %d", len(repo.runs))
	}
	for _, run := range repo.runs {
		if run.TriggerArtifact == nil || run.TriggerArtifact.Name != "report" {
			t.Fatalf("trigger artifact = %+v", run.TriggerArtifact)
		}
	}
}

func TestWorkflowServiceForkRunNeverReceivesSecrets(t *testing.T) {
	wf := &domain.Workflow{
		Name: "release",
		Jobs: map[string]domain.WorkflowJob{
			"deploy": {Call: "deploy", Secrets: []string{"PROD_KEY"}},
		},
	}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	run, err := svc.Start(context.Background(), StartWorkflowInput{
		Workflow: wf, OrgID: "acme", ProjectID: "p1", SHA: "sha", Ref: "refs/pull/1/head", IsFork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["deploy"].Status != "failed" {
		t.Fatalf("fork job = %+v", run.Jobs["deploy"])
	}
	if runner.calls != 0 {
		t.Fatalf("fork job must not execute: calls=%d", runner.calls)
	}
}

func TestWorkflowServiceDisabledGatesRunCreation(t *testing.T) {
	wf := &domain.Workflow{Name: "release", Jobs: map[string]domain.WorkflowJob{"build": {Call: "build"}}}
	runner := &fakeWorkflowRunner{}
	svc, _ := newWorkflowServiceForTest(t, wf, runner, &fakeEnvRepo{envs: map[string]*domain.Environment{}})
	svc.WithEnabled(false)
	if svc.Enabled() {
		t.Fatal("Enabled must be false")
	}
	if _, err := svc.Start(context.Background(), StartWorkflowInput{Workflow: wf, OrgID: "acme", ProjectID: "p1"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Start err = %v, want ErrValidation", err)
	}
	if _, err := svc.Dispatch(context.Background(), "acme", "p1", "release", "refs/heads/main", nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Dispatch err = %v, want ErrValidation", err)
	}
	if err := svc.OnChecksSucceeded(context.Background(), &domain.CheckRunRecord{OrgID: "acme", ProjectID: "p1"}); err != nil {
		t.Fatalf("OnChecksSucceeded err = %v, want nil", err)
	}
	if err := svc.OnArtifactPublished(context.Background(), "acme", "p1", "run-1", "art", "key"); err != nil {
		t.Fatalf("OnArtifactPublished err = %v, want nil", err)
	}
	if runner.calls != 0 {
		t.Fatalf("disabled service must not dispatch: calls=%d", runner.calls)
	}
}
