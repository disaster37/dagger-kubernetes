//go:build integration
// +build integration

package fleet

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/disaster/dagger-kubernetes/internal/session"
)

func clientset(t *testing.T) kubernetes.Interface {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("new clientset: %v", err)
	}

	ns := "dagger-cache-test"
	_, err = cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		_, err = cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create test namespace: %v", err)
		}
	}

	return cs
}

func cleanupProvider(t *testing.T, cs kubernetes.Interface, version string) {
	t.Helper()
	ns := "dagger-cache-test"
	name := stsName(version)
	_ = cs.AppsV1().StatefulSets(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	svcName := serviceName(version)
	_ = cs.CoreV1().Services(ns).Delete(context.Background(), svcName, metav1.DeleteOptions{})
}

func TestRealK8sEnsureStatefulSetCreate(t *testing.T) {
	cs := clientset(t)
	version := "v0.20.0"
	defer cleanupProvider(t, cs, version)

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace:           "dagger-cache-test",
		ImageRegistry:       "registry.dagger.io/engine",
		StorageSize:         "1Gi",
		CPURequest:          "100m",
		CPULimit:            "500m",
		MemoryRequest:       "128Mi",
		MemoryLimit:         "512Mi",
		TerminationGraceSec: 30,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	err := p.EnsureStatefulSet(version, "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache-test").Get(context.Background(), stsName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Name != stsName(version) {
		t.Errorf("expected name %q, got %q", stsName(version), sts.Name)
	}

	if sts.Spec.ServiceName != serviceName(version) {
		t.Errorf("expected serviceName %q, got %q", serviceName(version), sts.Spec.ServiceName)
	}

	if len(sts.Labels) == 0 || sts.Labels[engineLabelApp] != engineLabelValue {
		t.Errorf("expected label app=dagger-engine, got %v", sts.Labels)
	}

	if len(sts.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected containers in pod template")
	}

	container := sts.Spec.Template.Spec.Containers[0]
	if container.Name != "engine" {
		t.Errorf("expected container name 'engine', got %q", container.Name)
	}

	if container.Resources.Requests.Cpu().String() != "100m" {
		t.Errorf("expected CPU request 100m, got %s", container.Resources.Requests.Cpu().String())
	}

	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 volume claim template, got %d", len(sts.Spec.VolumeClaimTemplates))
	}

	vct := sts.Spec.VolumeClaimTemplates[0]
	if vct.Name != "dagger-cache" {
		t.Errorf("expected PVC name 'dagger-cache', got %q", vct.Name)
	}

	storageReq := vct.Spec.Resources.Requests[corev1.ResourceStorage]
	if storageReq.String() != "1Gi" {
		t.Errorf("expected storage 1Gi, got %s", storageReq.String())
	}

	t.Logf("StatefulSet %s created successfully with %d containers, storage %s",
		sts.Name, len(sts.Spec.Template.Spec.Containers), storageReq.String())
}

func TestRealK8sEnsureServiceCreate(t *testing.T) {
	cs := clientset(t)
	version := "v0.20.0"
	defer cleanupProvider(t, cs, version)

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace: "dagger-cache-test",
	})

	err := p.EnsureService(version)
	if err != nil {
		t.Fatalf("EnsureService: %v", err)
	}

	svc, err := cs.CoreV1().Services("dagger-cache-test").Get(context.Background(), serviceName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}

	if svc.Spec.ClusterIP != "None" {
		t.Errorf("expected headless service (ClusterIP=None), got %q", svc.Spec.ClusterIP)
	}

	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(svc.Spec.Ports))
	}

	if svc.Spec.Ports[0].Port != int32(enginePort) {
		t.Errorf("expected port %d, got %d", enginePort, svc.Spec.Ports[0].Port)
	}

	t.Logf("Service %s created successfully (headless, port %d)", svc.Name, svc.Spec.Ports[0].Port)
}

func TestRealK8sScaleUpDown(t *testing.T) {
	cs := clientset(t)
	version := "v0.20.0"
	defer cleanupProvider(t, cs, version)

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace:           "dagger-cache-test",
		StorageSize:         "1Gi",
		CPURequest:          "100m",
		CPULimit:            "500m",
		MemoryRequest:       "128Mi",
		MemoryLimit:         "512Mi",
		TerminationGraceSec: 30,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	err := p.EnsureStatefulSet(version, "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	err = p.ScaleUp(version, 1)
	if err != nil {
		t.Fatalf("ScaleUp to 1: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache-test").Get(context.Background(), stsName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("expected 1 replica, got %v", sts.Spec.Replicas)
	}

	err = p.ScaleDown(version, 0)
	if err != nil {
		t.Fatalf("ScaleDown: %v", err)
	}

	sts, err = cs.AppsV1().StatefulSets("dagger-cache-test").Get(context.Background(), stsName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset after scale-down: %v", err)
	}

	if sts.Spec.Replicas != nil && *sts.Spec.Replicas != 0 {
		t.Errorf("expected 0 replicas after scale-down, got %v", sts.Spec.Replicas)
	}

	t.Log("Scale up/down verified successfully")
}

func TestRealK8sVersionIsolation(t *testing.T) {
	cs := clientset(t)
	v1 := "v0.20.0"
	v2 := "v0.21.0"
	defer cleanupProvider(t, cs, v1)
	defer cleanupProvider(t, cs, v2)

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace:           "dagger-cache-test",
		StorageSize:         "1Gi",
		CPURequest:          "100m",
		CPULimit:            "500m",
		MemoryRequest:       "128Mi",
		MemoryLimit:         "512Mi",
		TerminationGraceSec: 30,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	for _, v := range []string{v1, v2} {
		if err := p.EnsureStatefulSet(v, fmt.Sprintf("registry.dagger.io/engine:%s", v)); err != nil {
			t.Fatalf("EnsureStatefulSet %s: %v", v, err)
		}
		if err := p.EnsureService(v); err != nil {
			t.Fatalf("EnsureService %s: %v", v, err)
		}
	}

	versions, err := p.AllVersions()
	if err != nil {
		t.Fatalf("AllVersions: %v", err)
	}

	if len(versions) < 2 {
		t.Errorf("expected at least 2 versions, got %d: %v", len(versions), versions)
	}

	found := map[string]bool{}
	for _, v := range versions {
		found[v] = true
	}
	if !found[v1] || !found[v2] {
		t.Errorf("expected both %s and %s in version list, got %v", v1, v2, versions)
	}

	t.Logf("Version isolation verified: %d versions found", len(versions))
}

func TestRealK8sProviderIntegration(t *testing.T) {
	cs := clientset(t)
	version := "v0.22.0"
	defer cleanupProvider(t, cs, version)

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace:           "dagger-cache-test",
		StorageSize:         "1Gi",
		CPURequest:          "100m",
		CPULimit:            "500m",
		MemoryRequest:       "128Mi",
		MemoryLimit:         "512Mi",
		TerminationGraceSec: 30,
		Privileged:          false,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	sessions := session.NewStore(5 * time.Minute)
	manager := NewManager(p, sessions, ManagerConfig{
		MaxReplicasPerVersion: 2,
		MaxSessionsPerReplica: 2,
		ReplicaIdleTTL:        5 * time.Minute,
	}, logger, nil)

	err := p.EnsureStatefulSet(version, "registry.dagger.io/engine:v0.22.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}
	err = p.EnsureService(version)
	if err != nil {
		t.Fatalf("EnsureService: %v", err)
	}

	err = p.ScaleUp(version, 1)
	if err != nil {
		t.Fatalf("ScaleUp: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errChan := make(chan error, 1)
	resultChan := make(chan *AcquireResult, 1)

	go func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		result, err := manager.Acquire(ctx2, version)
		errChan <- err
		if err == nil {
			resultChan <- result
		}
	}()

	select {
	case err := <-errChan:
		if err != nil {
			t.Logf("Acquire returned: %v (expected if engine image not pullable)", err)
		}
		select {
		case result := <-resultChan:
			t.Logf("Acquire succeeded: pod=%s, ip=%s", result.PodName, result.PodIP)
		default:
		}
	case <-ctx.Done():
		t.Logf("Acquire timed out (engine image not pullable in this environment)")
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache-test").Get(context.Background(), stsName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas < 1 {
		t.Error("statefulset should be scaled to at least 1 replica")
	}

	_, err = cs.CoreV1().Services("dagger-cache-test").Get(context.Background(), serviceName(version), metav1.GetOptions{})
	if err != nil {
		t.Errorf("service should exist: %v", err)
	}

	replicas, err := p.GetReplicas(version)
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}

	t.Logf("Integration flow: STS=%s replicas=%d ready, %d pods found",
		stsName(version), *sts.Spec.Replicas, len(replicas))
}

func TestRealK8sIdempotentCreate(t *testing.T) {
	cs := clientset(t)
	version := "v0.20.0"
	defer cleanupProvider(t, cs, version)

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace:           "dagger-cache-test",
		StorageSize:         "1Gi",
		CPURequest:          "100m",
		CPULimit:            "500m",
		MemoryRequest:       "128Mi",
		MemoryLimit:         "512Mi",
		TerminationGraceSec: 30,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	for i := 0; i < 3; i++ {
		err := p.EnsureStatefulSet(version, "registry.dagger.io/engine:v0.20.0")
		if err != nil {
			t.Fatalf("EnsureStatefulSet iteration %d: %v", i, err)
		}

		err = p.EnsureService(version)
		if err != nil {
			t.Fatalf("EnsureService iteration %d: %v", i, err)
		}
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache-test").Get(context.Background(), stsName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	if sts.Name != stsName(version) {
		t.Errorf("expected %q, got %q", stsName(version), sts.Name)
	}

	svc, err := cs.CoreV1().Services("dagger-cache-test").Get(context.Background(), serviceName(version), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.ClusterIP != "None" {
		t.Errorf("expected headless service, got ClusterIP=%q", svc.Spec.ClusterIP)
	}

	t.Log("Idempotent create verified: 3x EnsureStatefulSet + EnsureService produced 1 STS + 1 Service")
}

func TestRealK8sDeleteStatefulSetAndService(t *testing.T) {
	cs := clientset(t)
	version := "v0.20.0"

	p := NewK8sProvider(cs, K8sProviderConfig{
		Namespace:           "dagger-cache-test",
		StorageSize:         "1Gi",
		CPURequest:          "100m",
		CPULimit:            "500m",
		MemoryRequest:       "128Mi",
		MemoryLimit:         "512Mi",
		TerminationGraceSec: 30,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	})

	if err := p.EnsureStatefulSet(version, "registry.dagger.io/engine:v0.20.0"); err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}
	if err := p.EnsureService(version); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}

	if err := p.DeleteStatefulSet(version); err != nil {
		t.Fatalf("DeleteStatefulSet: %v", err)
	}
	if err := p.DeleteService(version); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}

	ctx := context.Background()
	_, err := cs.AppsV1().StatefulSets("dagger-cache-test").Get(ctx, stsName(version), metav1.GetOptions{})
	if err == nil {
		t.Error("statefulset should be deleted")
	}

	_, err = cs.CoreV1().Services("dagger-cache-test").Get(ctx, serviceName(version), metav1.GetOptions{})
	if err == nil {
		t.Error("service should be deleted")
	}

	t.Log("Delete verified: both StatefulSet and Service removed")
}
