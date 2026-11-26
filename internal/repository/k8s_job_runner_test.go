package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestResolvePodNamespace(t *testing.T) {
	t.Run("configured wins", func(t *testing.T) {
		t.Setenv(podNamespaceEnv, "env-ns")
		if got := resolvePodNamespace("  configured  "); got != "configured" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv(podNamespaceEnv, "env-ns")
		if got := resolvePodNamespace(""); got != "env-ns" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("file fallback", func(t *testing.T) {
		t.Setenv(podNamespaceEnv, "")
		dir := t.TempDir()
		path := filepath.Join(dir, "namespace")
		if err := os.WriteFile(path, []byte("file-ns\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		orig := podNamespaceFile
		podNamespaceFile = path
		t.Cleanup(func() { podNamespaceFile = orig })
		if got := resolvePodNamespace(""); got != "file-ns" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("default fallback", func(t *testing.T) {
		t.Setenv(podNamespaceEnv, "")
		orig := podNamespaceFile
		podNamespaceFile = filepath.Join(t.TempDir(), "missing")
		t.Cleanup(func() { podNamespaceFile = orig })
		if got := resolvePodNamespace(""); got != defaultRunnerNamespace {
			t.Fatalf("got %q", got)
		}
	})
}

func TestK8sJobRunnerRunCreatesJobAndSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "resolved-ns", "runner:latest", time.Minute, "https://example.com", "", "minting-ca").
		WithImagePullSecrets([]string{"regcred"})
	spec := &domain.CheckRunSpec{
		ID: "check-1", OrgID: "acme", ProjectID: "proj-1", SourceID: "src-1",
		Provider: domain.ForgeGitHub, Repo: "acme/repo", SHA: "abc", Ref: "refs/heads/main",
		Event: "push", Token: "tok", RunToken: "run", CloneToken: "clone",
		CloneURL: "https://github.com/acme/repo.git",
	}
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	name := "check-" + refHash(spec.ID)
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
	args := pod.Containers[0].Args
	if !containsArg(args, "--clone-url", "https://github.com/acme/repo.git") {
		t.Fatalf("args missing clone-url: %v", args)
	}
	secret, err := cs.CoreV1().Secrets("resolved-ns").Get(context.Background(), "check-env-"+refHash(spec.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.StringData["DAGGER_RUN_TOKEN"] != "run" {
		t.Fatalf("secret run token = %q", secret.StringData["DAGGER_RUN_TOKEN"])
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" {
		t.Fatalf("secret must be owned by the Job: %+v", secret.OwnerReferences)
	}
}

// TestK8sJobRunnerLongIDHashesLabel guards the K8s 63-char label-value cap: a
// raw check-run ID (check-<sourceID>-<sha>) exceeds it for long source IDs, so
// the label must be hashed (and the Job/Secret names use the hashed form).
func TestK8sJobRunnerLongIDHashesLabel(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "ns", "runner:latest", time.Minute, "https://example.com", "", "")
	longID := "check-" + strings.Repeat("s", 50) + "-" + strings.Repeat("a", 40)
	if len(longID) <= 63 {
		t.Fatalf("fixture id must exceed 63 chars, got %d", len(longID))
	}
	spec := &domain.CheckRunSpec{ID: longID, ProjectID: "proj-1", Ref: "refs/heads/main"}
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run with long id: %v", err)
	}
	name := "check-" + refHash(longID)
	job, err := cs.BatchV1().Jobs("ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("job %q must exist: %v", name, err)
	}
	got := job.Labels[checkRunIDLabel]
	if got != refHash(longID) {
		t.Fatalf("check-run-id label = %q, want refHash %q", got, refHash(longID))
	}
	if len(got) > 63 {
		t.Fatalf("check-run-id label length = %d, want <= 63", len(got))
	}
	// Secret name must be the hashed form too.
	if _, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "check-env-"+refHash(longID), metav1.GetOptions{}); err != nil {
		t.Fatalf("hashed secret name missing: %v", err)
	}
}

func TestK8sJobRunnerRerunReplacesJobAndSecret(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "ns", "runner:latest", time.Minute, "https://example.com", "", "")
	spec := &domain.CheckRunSpec{ID: "check-1", ProjectID: "proj-1", Ref: "refs/heads/main", RunToken: "first"}
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	name := "check-" + refHash(spec.ID)
	spec.RunToken = "second"
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// The rerun must delete the existing Job and create a fresh one.
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
	if _, err := cs.BatchV1().Jobs("ns").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("job must exist after rerun: %v", err)
	}
	secret, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "check-env-"+refHash(spec.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if secret.StringData["DAGGER_RUN_TOKEN"] != "second" {
		t.Fatalf("rerun secret run token = %q, want second", secret.StringData["DAGGER_RUN_TOKEN"])
	}
}

// TestK8sJobRunnerRerunRetriesTransientAlreadyExists covers the delete-
// propagation race: the first create after a rerun delete returns AlreadyExists
// once, and the runner retries rather than silently succeeding.
func TestK8sJobRunnerRerunRetriesTransientAlreadyExists(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "ns", "img", time.Minute, "https://example.com", "", "")
	spec := &domain.CheckRunSpec{ID: "check-1", ProjectID: "proj-1", Ref: "refs/heads/main", RunToken: "first"}
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// Fail the first create after the rerun delete once with AlreadyExists.
	injected := false
	cs.PrependReactor("create", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if injected {
			return false, nil, nil
		}
		injected = true
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "jobs"}, "check-x")
	})
	spec.RunToken = "second"
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatalf("rerun must retry the transient AlreadyExists: %v", err)
	}
	if !injected {
		t.Fatal("reactor did not inject AlreadyExists")
	}
	name := "check-" + refHash(spec.ID)
	if _, err := cs.BatchV1().Jobs("ns").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("job must exist after retry: %v", err)
	}
}

// TestK8sJobRunnerRerunPersistentAlreadyExistsErrors covers a replacement that
// never succeeds: the runner returns a descriptive error instead of masking it.
func TestK8sJobRunnerRerunPersistentAlreadyExistsErrors(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "ns", "img", time.Minute, "https://example.com", "", "")
	spec := &domain.CheckRunSpec{ID: "check-1", ProjectID: "proj-1", Ref: "refs/heads/main", RunToken: "first"}
	if err := r.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	cs.PrependReactor("create", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "jobs"}, "check-x")
	})
	spec.RunToken = "second"
	if err := r.Run(context.Background(), spec); err == nil {
		t.Fatal("persistent AlreadyExists must return an error, not silent success")
	}
}

func TestK8sJobRunnerJobStatus(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "ns", "img", time.Minute, "https://example.com", "", "")
	state, err := r.JobStatus(context.Background(), "missing")
	if err != nil || state != domain.JobStateUnknown {
		t.Fatalf("state = %v err = %v", state, err)
	}
	name := "check-" + refHash("check-1")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	job.Status.Succeeded = 1
	if _, err := cs.BatchV1().Jobs("ns").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	state, err = r.JobStatus(context.Background(), "check-1")
	if err != nil || state != domain.JobStateSucceeded {
		t.Fatalf("state = %v err = %v", state, err)
	}
}

func TestK8sJobRunnerCancelSuperseded(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := NewK8sJobRunner(cs, "ns", "img", time.Minute, "https://example.com", "", "")
	labels := map[string]string{checkRunProjectLabel: "proj-1", checkRunRefLabel: refHash("refs/heads/main")}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "ns", Labels: labels}}
	if _, err := cs.BatchV1().Jobs("ns").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.CancelSuperseded(context.Background(), "proj-1", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BatchV1().Jobs("ns").Get(context.Background(), "old", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("superseded job must be deleted, err = %v", err)
	}
}

func TestK8sJobRunnerNilClient(t *testing.T) {
	r := NewK8sJobRunner(nil, "ns", "img", time.Minute, "https://example.com", "", "")
	if err := r.Run(context.Background(), &domain.CheckRunSpec{}); err == nil {
		t.Fatal("nil client must error")
	}
	if err := r.CancelSuperseded(context.Background(), "p", "r"); err != nil {
		t.Fatalf("nil client cancel must be a no-op: %v", err)
	}
}

func containsArg(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestJobSecretCarriesSilent proves checks.silent reaches the Job Secret (and
// therefore the runner process and every nested CLI child), and that the
// default leaves the variable untouched.
func TestJobSecretCarriesSilent(t *testing.T) {
	cases := []struct {
		name   string
		silent bool
		want   string // "" = absent
	}{
		{name: "default off", silent: false, want: ""},
		{name: "configured on", silent: true, want: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			r := NewK8sJobRunner(cs, "resolved-ns", "runner:latest", time.Minute, "https://example.com", "", "").
				WithSilent(tc.silent)
			spec := &domain.CheckRunSpec{ID: "check-silent", OrgID: "acme", ProjectID: "proj", Repo: "acme/repo", SHA: "abc", Ref: "refs/heads/main"}
			if err := r.Run(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			secret, err := cs.CoreV1().Secrets("resolved-ns").Get(context.Background(), "check-env-"+refHash(spec.ID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := secret.StringData["DAGGER_SILENT"]
			if got != tc.want {
				t.Fatalf("DAGGER_SILENT = %q, want %q", got, tc.want)
			}
		})
	}
}
