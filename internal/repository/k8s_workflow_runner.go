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
}

var _ domain.WorkflowJobRunner = (*K8sWorkflowRunner)(nil)

// NewK8sWorkflowRunner returns a K8sWorkflowRunner.
func NewK8sWorkflowRunner(clientset kubernetes.Interface, namespace, image string, timeout time.Duration, serverURL string) *K8sWorkflowRunner {
	if namespace == "" {
		namespace = "dagger-kubernetes"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	return &K8sWorkflowRunner{clientset: clientset, namespace: namespace, image: image, timeout: timeout, serverURL: serverURL}
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
	env := []corev1.EnvVar{
		{Name: "DAGGER_CLOUD_URL", Value: r.serverURL},
		{Name: "DAGGER_ENGINE", Value: "cloud"},
	}
	for k, v := range spec.Secrets {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: int64Ptr(int64(timeout.Seconds())),
			BackoffLimit:          int32Ptr(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "workflow-job",
						Image:   r.image,
						Command: []string{"dagger"},
						Args:    []string{"call", spec.Call},
						Env:     env,
					}},
				},
			},
		},
	}
	_, err := r.clientset.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create workflow job: %w", err)
	}
	return map[string]string{}, nil
}
