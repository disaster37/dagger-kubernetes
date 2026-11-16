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
	clientset kubernetes.Interface
	namespace string
	image     string
	timeout   time.Duration
	serverURL string
	caSecret  string
}

var _ domain.CheckJobRunner = (*K8sJobRunner)(nil)

// NewK8sJobRunner returns a K8sJobRunner.
func NewK8sJobRunner(clientset kubernetes.Interface, namespace, image string, timeout time.Duration, serverURL, caSecret string) *K8sJobRunner {
	if namespace == "" {
		namespace = "dagger-kubernetes"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &K8sJobRunner{clientset: clientset, namespace: namespace, image: image, timeout: timeout, serverURL: serverURL, caSecret: caSecret}
}

func refHash(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:8])
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
	env := []corev1.EnvVar{
		{Name: "DAGGER_KUBERNETES_SERVER", Value: r.serverURL},
		{Name: "DAGGER_CLOUD_URL", Value: r.serverURL},
	}
	for k, v := range spec.Secrets {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	args := []string{"check-runner", "--repo", spec.Repo, "--sha", spec.SHA, "--ref", spec.Ref, "--server", r.serverURL}
	if spec.PRNumber > 0 {
		args = append(args, "--pr", fmt.Sprintf("%d", spec.PRNumber))
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: int64Ptr(int64(r.timeout.Seconds())),
			BackoffLimit:          int32Ptr(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "check-runner",
						Image:   r.image,
						Command: []string{"/usr/local/bin/dagger-kubernetes-ci"},
						Args:    args,
						Env:     env,
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
	_, err := r.clientset.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create check job: %w", err)
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

func int64Ptr(v int64) *int64 { return &v }
func int32Ptr(v int32) *int32 { return &v }
