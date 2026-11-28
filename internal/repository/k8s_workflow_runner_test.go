package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestK8sWorkflowRunnerRunCallCreatesJobAndSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sWorkflowRunner(cs, "resolved-ns", "runner:latest", time.Minute, "https://example.com", "", "minting-ca").
		WithImagePullSecrets([]string{"regcred"})
	spec := &domain.WorkflowCallSpec{
		RunID: "run-1", OrgID: "acme", ProjectID: "proj-1", JobName: "build",
		Call: "build", Repo: "acme/repo", SHA: "abc", Ref: "refs/heads/main",
		Token: "tok", RunToken: "run", CloneToken: "clone",
		CloneURL: "https://github.com/acme/repo.git",
	}
	if _, err := r.RunCall(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	name := "workflow-" + refHash(spec.RunID) + "-" + refHash(spec.JobName)
	job, err := cs.BatchV1().Jobs("resolved-ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("AutomountServiceAccountToken must be false")
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "regcred" {
		t.Fatalf("imagePullSecrets = %+v", pod.ImagePullSecrets)
	}
	if len(pod.Volumes) != 1 || len(pod.Volumes[0].Secret.Items) != 1 || pod.Volumes[0].Secret.Items[0].Key != "ca.crt" {
		t.Fatalf("minting-ca volume must project only ca.crt: %+v", pod.Volumes)
	}
	if !containsArg(pod.Containers[0].Args, "--clone-url", "https://github.com/acme/repo.git") {
		t.Fatalf("args missing clone-url: %v", pod.Containers[0].Args)
	}
	secret, err := cs.CoreV1().Secrets("resolved-ns").Get(context.Background(), "workflow-env-"+refHash(spec.RunID)+"-"+refHash(spec.JobName), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.StringData["DAGGER_RUN_TOKEN"] != "run" {
		t.Fatalf("secret run token = %q", secret.StringData["DAGGER_RUN_TOKEN"])
	}
}

// TestK8sWorkflowRunnerLongRunIDHashesLabel guards the K8s 63-char label-value
// cap for the workflow-run label: a raw run ID (<projectID>-<slug>-<n>) can
// exceed it for long workflow names, so the label must be hashed.
func TestK8sWorkflowRunnerLongRunIDHashesLabel(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sWorkflowRunner(cs, "ns", "img", time.Minute, "https://example.com", "", "")
	longRunID := strings.Repeat("p", 32) + "-" + strings.Repeat("w", 40) + "-1"
	if len(longRunID) <= 63 {
		t.Fatalf("fixture run id must exceed 63 chars, got %d", len(longRunID))
	}
	spec := &domain.WorkflowCallSpec{RunID: longRunID, JobName: "build", Call: "build"}
	if _, err := r.RunCall(context.Background(), spec); err != nil {
		t.Fatalf("RunCall with long run id: %v", err)
	}
	name := "workflow-" + refHash(longRunID) + "-" + refHash(spec.JobName)
	job, err := cs.BatchV1().Jobs("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job %q must exist: %v", name, err)
	}
	got := job.Labels["dagger-kubernetes.io/workflow-run"]
	if got != refHash(longRunID) {
		t.Fatalf("workflow-run label = %q, want refHash %q", got, refHash(longRunID))
	}
	if len(got) > 63 {
		t.Fatalf("workflow-run label length = %d, want <= 63", len(got))
	}
}

func TestK8sWorkflowRunnerRerunReplacesJobAndSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sWorkflowRunner(cs, "ns", "img", time.Minute, "https://example.com", "", "")
	spec := &domain.WorkflowCallSpec{RunID: "run-1", JobName: "build", Call: "build", RunToken: "first"}
	if _, err := r.RunCall(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	spec.RunToken = "second"
	if _, err := r.RunCall(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	creates, deletes := 0, 0
	for _, a := range cs.Actions() {
		if a.GetResource().Resource != "jobs" {
			continue
		}
		switch a.GetVerb() {
		case "create":
			creates++
		case "delete":
			deletes++
		}
	}
	if creates != 2 || deletes != 1 {
		t.Fatalf("job creates = %d deletes = %d, want 2/1", creates, deletes)
	}
	secret, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "workflow-env-"+refHash(spec.RunID)+"-"+refHash(spec.JobName), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.StringData["DAGGER_RUN_TOKEN"] != "second" {
		t.Fatalf("rerun secret run token = %q, want second", secret.StringData["DAGGER_RUN_TOKEN"])
	}
}

func TestK8sWorkflowRunnerNilClient(t *testing.T) {
	r := NewK8sWorkflowRunner(nil, "ns", "img", time.Minute, "https://example.com", "", "")
	if _, err := r.RunCall(context.Background(), &domain.WorkflowCallSpec{}); err == nil {
		t.Fatal("nil client must error")
	}
}

// TestWorkflowSecretCarriesSilent proves workflows.silent reaches the workflow
// Job Secret; the default leaves the variable untouched.
func TestWorkflowSecretCarriesSilent(t *testing.T) {
	for _, silent := range []bool{false, true} {
		cs := fake.NewSimpleClientset()
		r := NewK8sWorkflowRunner(cs, "resolved-ns", "runner:latest", time.Minute, "https://example.com", "", "").
			WithSilent(silent)
		spec := &domain.WorkflowCallSpec{RunID: "run-silent", OrgID: "acme", ProjectID: "proj", JobName: "build", Call: "build", Repo: "acme/repo", SHA: "abc", Ref: "refs/heads/main"}
		if _, err := r.RunCall(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		name := "workflow-env-" + refHash(spec.RunID) + "-" + refHash(spec.JobName)
		secret, err := cs.CoreV1().Secrets("resolved-ns").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if silent {
			want = "1"
		}
		if got := secret.StringData["DAGGER_SILENT"]; got != want {
			t.Fatalf("silent=%v DAGGER_SILENT = %q, want %q", silent, got, want)
		}
	}
}
