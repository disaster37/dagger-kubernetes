package repository

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// K8sWorkflowRunner executes workflow call: jobs as batch/v1 Jobs.
type K8sWorkflowRunner struct {
	clientset kubernetes.Interface
	namespace string
	image     string
	timeout   time.Duration
	serverURL string
	caSecret  string
}

var _ domain.WorkflowJobRunner = (*K8sWorkflowRunner)(nil)

// NewK8sWorkflowRunner returns a K8sWorkflowRunner.
func NewK8sWorkflowRunner(clientset kubernetes.Interface, namespace, image string, timeout time.Duration, serverURL, caSecret string) *K8sWorkflowRunner {
	if namespace == "" {
		namespace = "dagger-kubernetes"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &K8sWorkflowRunner{clientset: clientset, namespace: namespace, image: image, timeout: timeout, serverURL: serverURL, caSecret: caSecret}
}

// RunCall creates a workflow Job and returns immediately (outputs are captured
// by the job's logs in a full deployment; the leader records success).
func (r *K8sWorkflowRunner) RunCall(ctx context.Context, spec *domain.WorkflowCallSpec) (map[string]string, error) {
	if r.clientset == nil {
		return nil, fmt.Errorf("workflow runner: no kubernetes client")
	}
	name := fmt.Sprintf("workflow-%s-%s", refHash(spec.RunID), refHash(spec.JobName))
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	labels := map[string]string{
		"dagger-kubernetes.io/workflow-run": spec.RunID,
		"dagger-kubernetes.io/workflow-job": refHash(spec.JobName),
	}
	secretName := fmt.Sprintf("workflow-env-%s-%s", refHash(spec.RunID), refHash(spec.JobName))
	stringData := map[string]string{
		"DAGGER_CLOUD_TOKEN":      spec.Token,
		"DAGGER_KUBERNETES_TOKEN": spec.Token,
		"DAGGER_RUN_TOKEN":        spec.RunToken,
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
		return nil, fmt.Errorf("create workflow env secret: %w", err)
	}
	args := make([]string, 0, 19+len(spec.Flags))
	args = append(args,
		"workflow-runner",
		"--run-id", spec.RunID,
		"--org", spec.OrgID,
		"--project", spec.ProjectID,
		"--job", spec.JobName,
		"--call", spec.Call,
		"--repo", spec.Repo,
		"--sha", spec.SHA,
		"--ref", spec.Ref,
		"--server", r.serverURL,
	)
	args = append(args, spec.Flags...)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   int64Ptr(int64(timeout.Seconds())),
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "workflow-job",
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
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("create workflow job: %w", err)
	}
	secret.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       created.Name,
		UID:        created.UID,
	}}
	if _, err := r.clientset.CoreV1().Secrets(r.namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("own workflow env secret: %w", err)
	}
	return map[string]string{}, nil
}
