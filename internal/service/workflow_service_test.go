package service

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type fakeWorkflowParser struct{ wf *domain.Workflow }

func (f *fakeWorkflowParser) Parse(string, []byte) (*domain.Workflow, error) { return f.wf, nil }

type fakeEvaluator struct{}

func (f *fakeEvaluator) Eval(string, map[string]any) (any, error) { return true, nil }

type fakeArtifactStore struct{}

func (f *fakeArtifactStore) Put(context.Context, string, string, string, string, []byte) (string, error) {
	return "key", nil
}
func (f *fakeArtifactStore) Get(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fakeArtifactStore) List(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}
func (f *fakeArtifactStore) DeleteRun(context.Context, string, string, string) error { return nil }

type fakeWorkflowRunner struct{ calls int }

func (f *fakeWorkflowRunner) RunCall(context.Context, *domain.WorkflowCallSpec) (map[string]string, error) {
	f.calls++
	return map[string]string{"out": "ok"}, nil
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
	svc := NewWorkflowService(&fakeWorkflowParser{wf: wf}, &fakeEvaluator{}, &fakeArtifactStore{}, runner, nil, logrus.New())
	run, err := svc.Start(context.Background(), wf, "acme", "p1", "sha", "refs/heads/main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["build"].Status != "success" {
		t.Fatalf("build = %+v", run.Jobs["build"])
	}
	if run.Jobs["deploy"].Status != "waiting_approval" {
		t.Fatalf("deploy = %+v", run.Jobs["deploy"])
	}
	if runner.calls != 1 {
		t.Fatalf("calls = %d", runner.calls)
	}
	if err := svc.Approve(context.Background(), run.ID, "deploy", "admin"); err != nil {
		t.Fatal(err)
	}
}
