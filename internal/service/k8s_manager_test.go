package service

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func TestK8sProviderWithManagerIntegration(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := repository.NewK8sProvider(cs, repository.K8sProviderConfig{
		Namespace:           "dagger-kubernetes",
		ImageRegistry:       "registry.dagger.io/engine",
		StorageSize:         "10Gi",
		CPURequest:          "200m",
		CPULimit:            "1",
		MemoryRequest:       "256Mi",
		MemoryLimit:         "1Gi",
		TerminationGraceSec: 120,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	sessions := NewStore(5 * time.Minute)
	manager := NewManager(p, sessions, ManagerConfig{
		MaxReplicasPerVersion: 2,
		MaxSessionsPerReplica: 2,
		ReplicaIdleTTL:        5 * time.Minute,
	}, logger, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errChan := make(chan *domain.AcquireResult, 1)
	errChan2 := make(chan error, 1)

	go func() {
		result, err := manager.Acquire(ctx, "v0.20.0")
		errChan2 <- err
		if err == nil {
			errChan <- result
		}
	}()

	time.Sleep(100 * time.Millisecond)

	labels := map[string]string{"app": "dagger-engine", "version": "v0.20.0"}
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dagger-engine-v0-20-0-0",
			Namespace: "dagger-kubernetes",
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.5",
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			StartTime: &now,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "engine", Image: "e"}},
		},
	}
	cs.CoreV1().Pods("dagger-kubernetes").Create(context.Background(), pod, metav1.CreateOptions{})

	err := <-errChan2
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	result := <-errChan
	if result.Version != "v0.20.0" {
		t.Errorf("expected version v0.20.0, got %s", result.Version)
	}
	expectedPod := "dagger-engine-v0-20-0-0"
	if result.PodName != expectedPod {
		t.Errorf("expected pod %q, got %q", expectedPod, result.PodName)
	}
	if result.PodIP != "10.0.0.5" {
		t.Errorf("expected IP 10.0.0.5, got %s", result.PodIP)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-kubernetes").Get(context.Background(), domain.StsName("v0.20.0"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas < 1 {
		t.Error("expected statefulset to be scaled up to at least 1 replica")
	}

	_, err = cs.CoreV1().Services("dagger-kubernetes").Get(context.Background(), domain.ServiceName("v0.20.0"), metav1.GetOptions{})
	if err != nil {
		t.Errorf("expected service to exist: %v", err)
	}
}

func newK8sGCManager(t *testing.T) (*Manager, *repository.K8sProvider, *fake.Clientset, *Store) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	p := repository.NewK8sProvider(cs, repository.K8sProviderConfig{
		Namespace:           "dagger-kubernetes",
		ImageRegistry:       "registry.dagger.io/engine",
		StorageSize:         "10Gi",
		CPURequest:          "200m",
		CPULimit:            "1",
		MemoryRequest:       "256Mi",
		MemoryLimit:         "1Gi",
		TerminationGraceSec: 120,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	sessions := NewStore(5 * time.Minute)
	manager := NewManager(p, sessions, ManagerConfig{
		MaxReplicasPerVersion: 2,
		MaxSessionsPerReplica: 2,
		ReplicaIdleTTL:        5 * time.Minute,
		VersionRetention:      30 * time.Minute,
	}, logger, nil)
	return manager, p, cs, sessions
}

func TestK8sManagerSweepGCDeletesIdleVersion(t *testing.T) {
	manager, p, cs, _ := newK8sGCManager(t)

	if err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0"); err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}
	if err := p.EnsureService("v0.20.0"); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}
	if err := p.SetVersionIdleSince("v0.20.0", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetVersionIdleSince: %v", err)
	}

	if err := manager.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	_, err := cs.AppsV1().StatefulSets("dagger-kubernetes").Get(context.Background(), domain.StsName("v0.20.0"), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("statefulset Get err = %v, want IsNotFound", err)
	}
	_, err = cs.CoreV1().Services("dagger-kubernetes").Get(context.Background(), domain.ServiceName("v0.20.0"), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("service Get err = %v, want IsNotFound", err)
	}
}

func TestK8sManagerSweepGCMalformedAnnotationResets(t *testing.T) {
	manager, p, cs, _ := newK8sGCManager(t)

	if err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0"); err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-kubernetes").Get(context.Background(), domain.StsName("v0.20.0"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	sts.Annotations["dagger-kubernetes.io/idle-since"] = "garbage"
	if _, err := cs.AppsV1().StatefulSets("dagger-kubernetes").Update(context.Background(), sts, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update statefulset: %v", err)
	}

	if err := manager.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	sts, err = cs.AppsV1().StatefulSets("dagger-kubernetes").Get(context.Background(), domain.StsName("v0.20.0"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("statefulset should still exist after malformed annotation reset: %v", err)
	}
	raw := sts.Annotations["dagger-kubernetes.io/idle-since"]
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("annotation %q does not parse as RFC3339 after reset: %v", raw, err)
	}
	if d := time.Since(parsed); d < 0 || d > time.Minute {
		t.Fatalf("annotation %q is not within a minute of now (delta %v)", raw, d)
	}
}
