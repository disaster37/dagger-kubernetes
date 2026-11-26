package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	checkRunIDLabel      = "dagger-kubernetes.io/check-run-id"
	checkRunProjectLabel = "dagger-kubernetes.io/project"
	checkRunRefLabel     = "dagger-kubernetes.io/ref-hash"
)

// K8sJobRunner creates batch/v1 Jobs for Cloud Checks runs.
type K8sJobRunner struct {
	clientset          kubernetes.Interface
	namespace          string
	image              string
	timeout            time.Duration
	serverURL          string
	internalControlURL string
	caSecret           string
	imagePullSecrets   []string
	silent             bool
}

var _ domain.CheckJobRunner = (*K8sJobRunner)(nil)

// NewK8sJobRunner returns a K8sJobRunner.
func NewK8sJobRunner(clientset kubernetes.Interface, namespace, image string, timeout time.Duration, serverURL, internalControlURL, caSecret string) *K8sJobRunner {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &K8sJobRunner{clientset: clientset, namespace: resolvePodNamespace(namespace), image: image, timeout: timeout, serverURL: serverURL, internalControlURL: internalControlURL, caSecret: caSecret}
}

// WithImagePullSecrets attaches the given image pull secrets to every Job pod.
func (r *K8sJobRunner) WithImagePullSecrets(secrets []string) *K8sJobRunner {
	r.imagePullSecrets = secrets
	return r
}

// WithSilent runs the check-runner CLI with --silent (checks.silent). The flag
// travels in the Job Secret so nested SDK sessions inherit DAGGER_SILENT too.
func (r *K8sJobRunner) WithSilent(silent bool) *K8sJobRunner {
	r.silent = silent
	return r
}

func refHash(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:8])
}

func (r *K8sJobRunner) controlURL() string {
	if r.internalControlURL != "" {
		return r.internalControlURL
	}
	return r.serverURL
}

// Run creates a check-run Job. A Job with the same check-run ID is idempotent
// unless it is a rerun, in which case the existing Job and its env Secret are
// replaced so the fresh run token takes effect.
func (r *K8sJobRunner) Run(ctx context.Context, spec *domain.CheckRunSpec) error {
	if r.clientset == nil {
		return fmt.Errorf("check job runner: no kubernetes client")
	}
	name := fmt.Sprintf("check-%s", refHash(spec.ID))
	labels := map[string]string{
		// K8s label values are capped at 63 chars; the raw check-run ID
		// (check-<sourceID>-<sha>) exceeds that for long source IDs. The label
		// is never used as a selector, so hash it like the Job/Secret names.
		checkRunIDLabel:      refHash(spec.ID),
		checkRunProjectLabel: spec.ProjectID,
		checkRunRefLabel:     refHash(spec.Ref),
	}
	serverURL := r.controlURL()
	args, err := checkRunnerArgs(spec, serverURL)
	if err != nil {
		return err
	}
	secretName := fmt.Sprintf("check-env-%s", refHash(spec.ID))
	stringData := map[string]string{
		"DAGGER_CLOUD_TOKEN":      spec.Token,
		"DAGGER_KUBERNETES_TOKEN": spec.Token,
		"DAGGER_RUN_TOKEN":        spec.RunToken,
		"GIT_CLONE_TOKEN":         spec.CloneToken,
	}
	for k, v := range spec.Secrets {
		stringData[k] = v
	}
	if r.silent {
		stringData["DAGGER_SILENT"] = "1"
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: r.namespace, Labels: labels},
		StringData: stringData,
	}
	// A rerun must replace the terminal Job (TTLSecondsAfterFinished may have
	// left it in place) and refresh the env Secret so DAGGER_RUN_TOKEN is
	// current; a first dispatch simply creates both.
	rerun := false
	if _, err := r.clientset.BatchV1().Jobs(r.namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		rerun = true
		propagation := metav1.DeletePropagationBackground
		if err := r.clientset.BatchV1().Jobs(r.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete rerun check job: %w", err)
		}
		if err := r.clientset.CoreV1().Secrets(r.namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete rerun check env secret: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get check job: %w", err)
	}
	if _, err := r.clientset.CoreV1().Secrets(r.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create check env secret: %w", err)
		}
		existing, gerr := r.clientset.CoreV1().Secrets(r.namespace).Get(ctx, secretName, metav1.GetOptions{})
		if gerr != nil {
			return fmt.Errorf("get check env secret: %w", gerr)
		}
		existing.StringData = stringData
		if _, uerr := r.clientset.CoreV1().Secrets(r.namespace).Update(ctx, existing, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("update check env secret: %w", uerr)
		}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   int64Ptr(int64(r.timeout.Seconds())),
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					ImagePullSecrets:             imagePullSecrets(r.imagePullSecrets),
					Containers: []corev1.Container{{
						Name:    "check-runner",
						Image:   r.image,
						Command: []string{"/usr/local/bin/dagger-kubernetes-ci"},
						Args:    args,
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}},
						}},
					}},
				},
			},
		},
	}
	if r.caSecret != "" {
		job.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: "minting-ca",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: r.caSecret,
				// Project only the public CA cert; never the signing key.
				Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			}},
		}}
		job.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
			Name: "minting-ca", MountPath: "/etc/dagger-kubernetes/ca", ReadOnly: true,
		}}
		job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env,
			// Keep the system CA bundle (for `git clone https://…`) and add the
			// mounted Minting CA (for internal supervisor control/data TLS).
			// SSL_CERT_FILE would REPLACE the system bundle.
			corev1.EnvVar{Name: "SSL_CERT_DIR", Value: "/etc/ssl/certs:/etc/dagger-kubernetes/ca"})
	}
	created, err := r.createCheckJob(ctx, job, name, rerun)
	if err != nil {
		return err
	}
	if created == nil {
		// First dispatch hit an existing Job (idempotent replay): nothing to own.
		return nil
	}
	// Own the Secret by the Job so Kubernetes garbage-collects it with the Job.
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       created.Name,
		UID:        created.UID,
	}}
	if _, err := r.clientset.CoreV1().Secrets(r.namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("own check env secret: %w", err)
	}
	return nil
}

// checkRunnerArgs builds the check-runner argv: the run metadata, the optional
// clone URL/PR/release flags, and the validated typed filters (each passed as
// --filter key=value so the runner appends --key=value to `dagger check`).
func checkRunnerArgs(spec *domain.CheckRunSpec, serverURL string) ([]string, error) {
	args := []string{
		"check-runner",
		"--run-id", spec.ID,
		"--org", spec.OrgID,
		"--project", spec.ProjectID,
		"--repo", spec.Repo,
		"--sha", spec.SHA,
		"--ref", spec.Ref,
		"--event", spec.Event,
		"--default-branch", spec.DefaultBranch,
		"--target-branch", spec.TargetBranch,
		"--tag", spec.Tag,
		"--server", serverURL,
	}
	if spec.CloneURL != "" {
		args = append(args, "--clone-url", spec.CloneURL)
	}
	if spec.PRNumber > 0 {
		args = append(args, "--pr", fmt.Sprintf("%d", spec.PRNumber))
	}
	if spec.IsRelease {
		args = append(args, "--is-release")
	}
	for _, filter := range spec.Filters {
		value, err := domain.ParseCheckFilterArgv(filter)
		if err != nil {
			return nil, fmt.Errorf("check run filters: %w", err)
		}
		args = append(args, "--filter", value)
	}
	return args, nil
}

// createCheckJob creates the Job. On a rerun a transient AlreadyExists (the
// deleted Job still propagating) is retried once after a short wait; a
// persistent AlreadyExists is an explicit error rather than a silent success
// that would mask a failed replacement. A first dispatch treats AlreadyExists
// as an idempotent replay and returns (nil, nil).
func (r *K8sJobRunner) createCheckJob(ctx context.Context, job *batchv1.Job, name string, rerun bool) (*batchv1.Job, error) {
	created, err := r.clientset.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err == nil {
		return created, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create check job: %w", err)
	}
	if !rerun {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
	}
	created, err = r.clientset.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("rerun check job %s still exists after delete: %w", name, err)
		}
		return nil, fmt.Errorf("create check job: %w", err)
	}
	return created, nil
}

// CancelSuperseded deletes active Jobs for the same project/ref.
func (r *K8sJobRunner) CancelSuperseded(ctx context.Context, projectID, ref string) error {
	if r.clientset == nil {
		return nil
	}
	selector := fmt.Sprintf("%s=%s,%s=%s", checkRunProjectLabel, projectID, checkRunRefLabel, refHash(ref))
	jobs, err := r.clientset.BatchV1().Jobs(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list check jobs: %w", err)
	}
	propagation := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}
		if err := r.clientset.BatchV1().Jobs(r.namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete superseded check job %s: %w", job.Name, err)
		}
	}
	return nil
}

// JobStatus reports the observed state of the Job for a check run.
func (r *K8sJobRunner) JobStatus(ctx context.Context, runID string) (domain.JobState, error) {
	if r.clientset == nil {
		return domain.JobStateUnknown, nil
	}
	name := fmt.Sprintf("check-%s", refHash(runID))
	job, err := r.clientset.BatchV1().Jobs(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return domain.JobStateUnknown, nil
		}
		return domain.JobStateUnknown, fmt.Errorf("get check job: %w", err)
	}
	if job.Status.Succeeded > 0 {
		return domain.JobStateSucceeded, nil
	}
	if job.Status.Failed > 0 {
		return domain.JobStateFailed, nil
	}
	return domain.JobStateActive, nil
}

func int64Ptr(v int64) *int64 { return &v }
func int32Ptr(v int32) *int32 { return &v }
func boolPtr(v bool) *bool    { return &v }

// imagePullSecrets converts secret names into the PodSpec reference list.
func imagePullSecrets(names []string) []corev1.LocalObjectReference {
	if len(names) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		refs = append(refs, corev1.LocalObjectReference{Name: name})
	}
	return refs
}
