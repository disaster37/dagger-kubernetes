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
}

var _ domain.CheckJobRunner = (*K8sJobRunner)(nil)

// NewK8sJobRunner returns a K8sJobRunner.
func NewK8sJobRunner(clientset kubernetes.Interface, namespace, image string, timeout time.Duration, serverURL, internalControlURL, caSecret string) *K8sJobRunner {
	if namespace == "" {
		namespace = "dagger-kubernetes"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &K8sJobRunner{clientset: clientset, namespace: namespace, image: image, timeout: timeout, serverURL: serverURL, internalControlURL: internalControlURL, caSecret: caSecret}
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

// Run creates a check-run Job. A Job with the same check-run ID is idempotent.
func (r *K8sJobRunner) Run(ctx context.Context, spec *domain.CheckRunSpec) error {
	if r.clientset == nil {
		return fmt.Errorf("check job runner: no kubernetes client")
	}
	name := fmt.Sprintf("check-%s", refHash(spec.ID))
	labels := map[string]string{
		checkRunIDLabel:      spec.ID,
		checkRunProjectLabel: spec.ProjectID,
		checkRunRefLabel:     refHash(spec.Ref),
	}
	serverURL := r.controlURL()
	secretName := fmt.Sprintf("check-env-%s", refHash(spec.ID))
	stringData := map[string]string{
		"DAGGER_CLOUD_TOKEN":      spec.Token,
		"DAGGER_KUBERNETES_TOKEN": spec.Token,
		"GIT_CLONE_TOKEN":         spec.CloneToken,
	}
	for k, v := range spec.Secrets {
		stringData[k] = v
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: r.namespace, Labels: labels},
		StringData: stringData,
	}
	if _, err := r.clientset.CoreV1().Secrets(r.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create check env secret: %w", err)
	}

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
	if spec.PRNumber > 0 {
		args = append(args, "--pr", fmt.Sprintf("%d", spec.PRNumber))
	}
	if spec.IsRelease {
		args = append(args, "--is-release")
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
					RestartPolicy: corev1.RestartPolicyNever,
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
			Name:         "minting-ca",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: r.caSecret}},
		}}
		job.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
			Name: "minting-ca", MountPath: "/etc/dagger-kubernetes/ca", ReadOnly: true,
		}}
		job.Spec.Template.Spec.Containers[0].Env = append(job.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "SSL_CERT_FILE", Value: "/etc/dagger-kubernetes/ca/ca.crt"})
	}
	created, err := r.clientset.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create check job: %w", err)
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
