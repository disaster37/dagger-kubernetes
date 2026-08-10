package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func defaultK8sProvider(extra ...func(*K8sProviderConfig)) (*K8sProvider, *fake.Clientset) {
	cfg := K8sProviderConfig{
		Namespace:           "dagger-cache",
		ImageRegistry:       "registry.dagger.io/engine",
		StorageClass:        "fast-ssd",
		StorageSize:         "50Gi",
		CPURequest:          "500m",
		CPULimit:            "2000m",
		MemoryRequest:       "1Gi",
		MemoryLimit:         "8Gi",
		TerminationGraceSec: 120,
		Privileged:          true,
		PullPolicy:          corev1.PullIfNotPresent,
	}
	for _, fn := range extra {
		fn(&cfg)
	}
	cs := fake.NewSimpleClientset()
	return NewK8sProvider(cs, cfg), cs
}

const (
	testEngineVersion = "v0.20.0"
	testEngineImage   = "registry.dagger.io/engine:v0.20.0"
)

// ensureEngineSet runs EnsureStatefulSet for the standard test engine version
// and returns the resulting StatefulSet.
func ensureEngineSet(t *testing.T, p *K8sProvider, cs *fake.Clientset) *appsv1.StatefulSet {
	t.Helper()
	if err := p.EnsureStatefulSet(testEngineVersion, testEngineImage); err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}
	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), domain.StsName(testEngineVersion), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}
	return sts
}

// engineConfigMap fetches the fleet-wide engine ConfigMap or fails the test.
func engineConfigMap(t *testing.T, cs *fake.Clientset) *corev1.ConfigMap {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps("dagger-cache").Get(context.Background(), engineConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	return cm
}

// requireEnvNames asserts that the env var names appear in exactly the given
// order (and count).
func requireEnvNames(t *testing.T, envs []corev1.EnvVar, want []string) {
	t.Helper()
	if len(envs) != len(want) {
		names := make([]string, len(envs))
		for i, env := range envs {
			names[i] = env.Name
		}
		t.Fatalf("expected %d env vars, got %d: %v", len(want), len(envs), names)
	}
	for i, name := range want {
		if envs[i].Name != name {
			t.Errorf("env[%d] = %q, want %q", i, envs[i].Name, name)
		}
	}
}

// envByName returns the env var with the given name, or nil.
func envByName(envs []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i]
		}
	}
	return nil
}

// volumeByName returns the pod volume with the given name, or nil.
func volumeByName(sts *appsv1.StatefulSet, name string) *corev1.Volume {
	for i := range sts.Spec.Template.Spec.Volumes {
		if sts.Spec.Template.Spec.Volumes[i].Name == name {
			return &sts.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

// mountByName returns the container volume mount with the given name, or nil.
func mountByName(container *corev1.Container, name string) *corev1.VolumeMount {
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == name {
			return &container.VolumeMounts[i]
		}
	}
	return nil
}

func TestK8sEnsureStatefulSet(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset failed: %v", err)
	}

	if sts.Spec.ServiceName != "dagger-engine-v0-20-0" {
		t.Errorf("expected serviceName 'dagger-engine-v0-20-0', got %q", sts.Spec.ServiceName)
	}

	if sts.Spec.Template.Spec.TerminationGracePeriodSeconds == nil || *sts.Spec.Template.Spec.TerminationGracePeriodSeconds != 120 {
		t.Errorf("expected termination grace 120, got %v", sts.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}

	if len(sts.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("expected at least one container")
	}

	container := sts.Spec.Template.Spec.Containers[0]
	if container.Image != "registry.dagger.io/engine:v0.20.0" {
		t.Errorf("expected image 'registry.dagger.io/engine:v0.20.0', got %q", container.Image)
	}

	if container.SecurityContext == nil || !*container.SecurityContext.Privileged {
		t.Error("expected privileged security context")
	}

	cpuReq := container.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("expected CPU request 500m, got %s", cpuReq.String())
	}

	memLim := container.Resources.Limits[corev1.ResourceMemory]
	if memLim.String() != "8Gi" {
		t.Errorf("expected memory limit 8Gi, got %s", memLim.String())
	}

	found := false
	for _, arg := range container.Args {
		if strings.Contains(arg, "--addr=tcp://0.0.0.0:9999") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --addr=tcp://0.0.0.0:9999 in args, got %v", container.Args)
	}

	if container.ReadinessProbe == nil || container.ReadinessProbe.TCPSocket.Port.String() != "9999" {
		t.Error("expected readiness probe on port 9999")
	}

	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 volume claim template, got %d", len(sts.Spec.VolumeClaimTemplates))
	}

	vct := sts.Spec.VolumeClaimTemplates[0]
	if vct.Name != "dagger-cache" {
		t.Errorf("expected PVC name 'dagger-cache', got %q", vct.Name)
	}
	if vct.Spec.StorageClassName == nil || *vct.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("expected storage class 'fast-ssd', got %v", vct.Spec.StorageClassName)
	}

	storageReq := vct.Spec.Resources.Requests[corev1.ResourceStorage]
	if storageReq.String() != "50Gi" {
		t.Errorf("expected storage request 50Gi, got %s", storageReq.String())
	}

	if sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		t.Fatal("expected PVC retention policy")
	}

	if sts.Labels[engineLabelApp] != engineLabelValue {
		t.Errorf("expected label app=dagger-engine, got %v", sts.Labels[engineLabelApp])
	}
	if sts.Labels[engineLabelVersion] != "v0.20.0" {
		t.Errorf("expected label version=v0.20.0, got %v", sts.Labels[engineLabelVersion])
	}
}

func TestK8sEnsureStatefulSetWithTolerations(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.Tolerations = []corev1.Toleration{
			{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "dagger", Effect: corev1.TaintEffectNoSchedule},
		}
	})

	err := p.EnsureStatefulSet("v0.21.0", "registry.dagger.io/engine:v0.21.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-21-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if len(sts.Spec.Template.Spec.Tolerations) != 1 {
		t.Fatalf("expected 1 toleration, got %d", len(sts.Spec.Template.Spec.Tolerations))
	}

	tol := sts.Spec.Template.Spec.Tolerations[0]
	if tol.Key != "dedicated" || tol.Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("unexpected toleration: %+v", tol)
	}
}

func TestK8sEnsureStatefulSetWithNodeSelector(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.NodeSelector = map[string]string{
			"node-type": "dagger",
		}
	})

	err := p.EnsureStatefulSet("v0.21.0", "registry.dagger.io/engine:v0.21.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-21-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if v, ok := sts.Spec.Template.Spec.NodeSelector["node-type"]; !ok || v != "dagger" {
		t.Errorf("expected node-selector node-type=dagger, got %v", sts.Spec.Template.Spec.NodeSelector)
	}
}

func TestK8sEnsureStatefulSetWithoutStorageClass(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.StorageClass = ""
	})

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	vct := sts.Spec.VolumeClaimTemplates[0]
	if vct.Spec.StorageClassName != nil && *vct.Spec.StorageClassName != "" {
		t.Errorf("expected empty storage class, got %q", *vct.Spec.StorageClassName)
	}
}

func TestK8sEnsureService(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureService("v0.20.0")
	if err != nil {
		t.Fatalf("EnsureService failed: %v", err)
	}

	svc, err := cs.CoreV1().Services("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service failed: %v", err)
	}

	if svc.Spec.ClusterIP != "None" {
		t.Errorf("expected headless service (ClusterIP=None), got %q", svc.Spec.ClusterIP)
	}

	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(svc.Spec.Ports))
	}

	if svc.Spec.Ports[0].Port != 9999 {
		t.Errorf("expected port 9999, got %d", svc.Spec.Ports[0].Port)
	}
}

func TestK8sGetReplicas(t *testing.T) {
	p, cs := defaultK8sProvider()

	labels := p.engineLabels("v0.20.0")
	now := metav1.Now()

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dagger-engine-v0-20-0-0",
				Namespace: "dagger-cache",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "e"}}},
			Status: corev1.PodStatus{
				PodIP: "10.0.0.1",
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
				StartTime: &now,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dagger-engine-v0-20-0-1",
				Namespace: "dagger-cache",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "e"}}},
			Status: corev1.PodStatus{
				PodIP: "10.0.0.2",
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				},
			},
		},
	}

	for _, pod := range pods {
		_, err := cs.CoreV1().Pods("dagger-cache").Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}

	replicas, err := p.GetReplicas("v0.20.0")
	if err != nil {
		t.Fatalf("GetReplicas failed: %v", err)
	}

	if len(replicas) != 2 {
		t.Fatalf("expected 2 replicas, got %d", len(replicas))
	}

	if replicas[0].Ordinal == 0 {
		if !replicas[0].Ready {
			t.Error("expected replica 0 to be ready")
		}
		if replicas[0].PodIP != "10.0.0.1" {
			t.Errorf("expected IP 10.0.0.1, got %s", replicas[0].PodIP)
		}
	}

	if replicas[1].Ordinal == 1 {
		if replicas[1].Ready {
			t.Error("expected replica 1 to be not ready")
		}
	}
}

func TestK8sScaleUp(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	err = p.ScaleUp("v0.20.0", 3)
	if err != nil {
		t.Fatalf("ScaleUp failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %v", sts.Spec.Replicas)
	}
}

func TestK8sScaleDown(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	err = p.ScaleUp("v0.20.0", 3)
	if err != nil {
		t.Fatalf("ScaleUp failed: %v", err)
	}

	err = p.ScaleDown("v0.20.0", 2)
	if err != nil {
		t.Fatalf("ScaleDown failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 {
		t.Errorf("expected 2 replicas after scale-down, got %v", sts.Spec.Replicas)
	}
}

func TestK8sScaleDownToZero(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	err = p.ScaleUp("v0.20.0", 1)
	if err != nil {
		t.Fatalf("ScaleUp failed: %v", err)
	}

	err = p.ScaleDown("v0.20.0", 0)
	if err != nil {
		t.Fatalf("ScaleDown failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Spec.Replicas != nil && *sts.Spec.Replicas != 0 {
		t.Errorf("expected 0 replicas, got %v", sts.Spec.Replicas)
	}
}

func TestK8sScaleDownAlreadyZero(t *testing.T) {
	p, _ := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	err = p.ScaleDown("v0.20.0", 0)
	if err != nil {
		t.Fatalf("ScaleDown below zero should not error: %v", err)
	}
}

func TestK8sWaitForReadyTimesOutOnMissingPod(t *testing.T) {
	p, _ := defaultK8sProvider()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- p.WaitForReady("v0.20.0", "nonexistent-pod")
	}()

	select {
	case err := <-errCh:
		t.Fatalf("WaitForReady returned unexpectedly: %v", err)
	case <-ctx.Done():
		// expected — WaitForReady loops for 5 min internally; our 1s context just proves it doesn't crash
	}
}

func TestK8sGetReadyReplicaIP(t *testing.T) {
	p, cs := defaultK8sProvider()

	labels := p.engineLabels("v0.20.0")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dagger-engine-v0-20-0-0",
			Namespace: "dagger-cache",
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.10",
		},
	}

	_, err := cs.CoreV1().Pods("dagger-cache").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}

	ip, err := p.GetReadyReplicaIP("v0.20.0", "dagger-engine-v0-20-0-0")
	if err != nil {
		t.Fatalf("GetReadyReplicaIP failed: %v", err)
	}

	if ip != "10.0.0.10" {
		t.Errorf("expected IP 10.0.0.10, got %s", ip)
	}
}

func TestK8sGetEngineImage(t *testing.T) {
	p, _ := defaultK8sProvider()

	img := p.GetEngineImage("v0.20.0")
	if img != "registry.dagger.io/engine:v0.20.0" {
		t.Errorf("expected 'registry.dagger.io/engine:v0.20.0', got %q", img)
	}

	p2, _ := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.ImageRegistry = "my-registry.io/dagger"
	})
	img2 := p2.GetEngineImage("v0.21.0")
	if img2 != "my-registry.io/dagger:v0.21.0" {
		t.Errorf("expected 'my-registry.io/dagger:v0.21.0', got %q", img2)
	}
}

func TestK8sAllVersions(t *testing.T) {
	p, cs := defaultK8sProvider()

	for _, v := range []string{"v0.20.0", "v0.21.0", "v0.22.0"} {
		labels := p.engineLabels(v)
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      domain.StsName(v),
				Namespace: "dagger-cache",
				Labels:    labels,
			},
		}
		_, err := cs.AppsV1().StatefulSets("dagger-cache").Create(context.Background(), sts, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create sts: %v", err)
		}
	}

	versions, err := p.AllVersions()
	if err != nil {
		t.Fatalf("AllVersions failed: %v", err)
	}

	if len(versions) != 3 {
		t.Fatalf("expected 3 versions, got %d: %v", len(versions), versions)
	}

	found := map[string]bool{}
	for _, v := range versions {
		found[v] = true
	}
	for _, expected := range []string{"v0.20.0", "v0.21.0", "v0.22.0"} {
		if !found[expected] {
			t.Errorf("expected version %s not found", expected)
		}
	}
}

func TestK8sAllVersionsEmpty(t *testing.T) {
	p, _ := defaultK8sProvider()

	versions, err := p.AllVersions()
	if err != nil {
		t.Fatalf("AllVersions failed: %v", err)
	}

	if len(versions) != 0 {
		t.Errorf("expected 0 versions, got %d", len(versions))
	}
}

func TestK8sDeleteStatefulSet(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	err = p.DeleteStatefulSet("v0.20.0")
	if err != nil {
		t.Fatalf("DeleteStatefulSet failed: %v", err)
	}

	_, err = cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected statefulset to be deleted")
	}
}

func TestK8sDeleteService(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureService("v0.20.0")
	if err != nil {
		t.Fatalf("EnsureService failed: %v", err)
	}

	err = p.DeleteService("v0.20.0")
	if err != nil {
		t.Fatalf("DeleteService failed: %v", err)
	}

	_, err = cs.CoreV1().Services("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected service to be deleted")
	}
}

func TestK8sExtraArgs(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.ExtraArgs = []string{"--debug", "--cache-exporter=type=registry,ref=cache.reg/dagger:latest"}
	})

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet failed: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	container := sts.Spec.Template.Spec.Containers[0]
	hasDebug := false
	hasCache := false
	for _, arg := range container.Args {
		if arg == "--debug" {
			hasDebug = true
		}
		if strings.Contains(arg, "--cache-exporter") {
			hasCache = true
		}
	}

	if !hasDebug {
		t.Error("expected --debug in args")
	}
	if !hasCache {
		t.Error("expected --cache-exporter in args")
	}
}

func TestK8sProviderDefaults(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := NewK8sProvider(cs, K8sProviderConfig{})

	if p.cfg.Namespace != "dagger-cache" {
		t.Errorf("expected default namespace 'dagger-cache', got %q", p.cfg.Namespace)
	}
	if p.cfg.StorageSize != "50Gi" {
		t.Errorf("expected default storage '50Gi', got %q", p.cfg.StorageSize)
	}
	if p.cfg.CPURequest != "500m" {
		t.Errorf("expected default CPU request '500m', got %q", p.cfg.CPURequest)
	}
	if p.cfg.MemoryLimit != "8Gi" {
		t.Errorf("expected default memory limit '8Gi', got %q", p.cfg.MemoryLimit)
	}
	if p.cfg.TerminationGraceSec != 120 {
		t.Errorf("expected default termination grace 120, got %d", p.cfg.TerminationGraceSec)
	}
	if p.cfg.PullPolicy != corev1.PullIfNotPresent {
		t.Errorf("expected default pull policy IfNotPresent, got %v", p.cfg.PullPolicy)
	}
}

func TestK8sEnsureStatefulSetIdempotent(t *testing.T) {
	p, _ := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("first EnsureStatefulSet: %v", err)
	}

	err = p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("second EnsureStatefulSet should be idempotent: %v", err)
	}
}

func TestVersionSlug(t *testing.T) {
	tests := []struct {
		version string
		slug    string
	}{
		{"v0.20.0", "v0-20-0"},
		{"v0.21.4", "v0-21-4"},
		{"1.2.3", "1-2-3"},
		{"latest", "latest"},
	}
	for _, tc := range tests {
		if got := domain.VersionSlug(tc.version); got != tc.slug {
			t.Errorf("VersionSlug(%q) = %q, want %q", tc.version, got, tc.slug)
		}
	}
}

func TestStsName(t *testing.T) {
	name := domain.StsName("v0.20.0")
	if name != "dagger-engine-v0-20-0" {
		t.Errorf("expected 'dagger-engine-v0-20-0', got %q", name)
	}
}

func TestPodName(t *testing.T) {
	name := domain.PodName("v0.20.0", 3)
	if name != "dagger-engine-v0-20-0-3" {
		t.Errorf("expected 'dagger-engine-v0-20-0-3', got %q", name)
	}
}

func TestK8sExtractOrdinal(t *testing.T) {
	p, _ := defaultK8sProvider()

	tests := []struct {
		podName string
		want    int
	}{
		{"dagger-engine-v0-20-0-0", 0},
		{"dagger-engine-v0-20-0-5", 5},
		{"dagger-engine-v0-20-0-99", 99},
		{"wrong-prefix-0", -1},
		{"dagger-engine-v0-20-0-abc", -1},
	}

	for _, tc := range tests {
		got := p.extractOrdinal(tc.podName, "v0.20.0")
		if got != tc.want {
			t.Errorf("extractOrdinal(%q, v0.20.0) = %d, want %d", tc.podName, got, tc.want)
		}
	}
}

func TestK8sGetReplicasFiltersOtherVersions(t *testing.T) {
	p, cs := defaultK8sProvider()

	labelsV20 := p.engineLabels("v0.20.0")
	labelsV21 := p.engineLabels("v0.21.0")

	now := metav1.Now()
	podV20 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dagger-engine-v0-20-0-0",
			Namespace: "dagger-cache",
			Labels:    labelsV20,
		},
		Status: corev1.PodStatus{
			PodIP:     "10.0.0.1",
			Phase:     corev1.PodRunning,
			StartTime: &now,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	podV21 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dagger-engine-v0-21-0-0",
			Namespace: "dagger-cache",
			Labels:    labelsV21,
		},
		Status: corev1.PodStatus{
			PodIP:     "10.0.0.2",
			Phase:     corev1.PodRunning,
			StartTime: &now,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	_, _ = cs.CoreV1().Pods("dagger-cache").Create(context.Background(), podV20, metav1.CreateOptions{})
	_, _ = cs.CoreV1().Pods("dagger-cache").Create(context.Background(), podV21, metav1.CreateOptions{})

	replicas, err := p.GetReplicas("v0.20.0")
	if err != nil {
		t.Fatalf("GetReplicas: %v", err)
	}

	if len(replicas) != 1 {
		t.Fatalf("expected 1 replica for v0.20.0, got %d", len(replicas))
	}
	if replicas[0].Name != "dagger-engine-v0-20-0-0" {
		t.Errorf("expected pod 'dagger-engine-v0-20-0-0', got %q", replicas[0].Name)
	}
}

func TestK8sSetReplicas(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	err = p.setReplicas(context.Background(), "v0.20.0", 5)
	if err != nil {
		t.Fatalf("setReplicas: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 5 {
		t.Errorf("expected 5 replicas, got %v", sts.Spec.Replicas)
	}
}

func TestK8sEnsureStatefulSetWithCustomResourceRequirements(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.CPURequest = "100m"
		cfg.CPULimit = "500m"
		cfg.MemoryRequest = "128Mi"
		cfg.MemoryLimit = "512Mi"
	})

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	container := sts.Spec.Template.Spec.Containers[0]

	cpuReq := container.Resources.Requests[corev1.ResourceCPU]
	memReq := container.Resources.Requests[corev1.ResourceMemory]
	cpuLim := container.Resources.Limits[corev1.ResourceCPU]
	memLim := container.Resources.Limits[corev1.ResourceMemory]

	if cpuReq.String() != "100m" {
		t.Errorf("CPU request: expected 100m, got %s", cpuReq.String())
	}
	if cpuLim.String() != "500m" {
		t.Errorf("CPU limit: expected 500m, got %s", cpuLim.String())
	}
	if memReq.String() != "128Mi" {
		t.Errorf("memory request: expected 128Mi, got %s", memReq.String())
	}
	if memLim.String() != "512Mi" {
		t.Errorf("memory limit: expected 512Mi, got %s", memLim.String())
	}
}

func TestK8sVolumeClaimTemplatesRetentionPolicy(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	if sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		t.Fatal("expected retention policy to be set")
	}

	policy := sts.Spec.PersistentVolumeClaimRetentionPolicy
	if policy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Errorf("expected WhenScaled=Retain, got %v", policy.WhenScaled)
	}
	if policy.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		t.Errorf("expected WhenDeleted=Delete, got %v", policy.WhenDeleted)
	}
}

func TestK8sEnsureServiceIdempotent(t *testing.T) {
	p, _ := defaultK8sProvider()

	err := p.EnsureService("v0.20.0")
	if err != nil {
		t.Fatalf("first EnsureService: %v", err)
	}

	err = p.EnsureService("v0.20.0")
	if err != nil {
		t.Fatalf("second EnsureService should be idempotent: %v", err)
	}
}

func TestK8sEngineEnvironVariables(t *testing.T) {
	p, cs := defaultK8sProvider()

	err := p.EnsureStatefulSet("v0.20.0", "registry.dagger.io/engine:v0.20.0")
	if err != nil {
		t.Fatalf("EnsureStatefulSet: %v", err)
	}

	sts, err := cs.AppsV1().StatefulSets("dagger-cache").Get(context.Background(), "dagger-engine-v0-20-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get statefulset: %v", err)
	}

	container := sts.Spec.Template.Spec.Containers[0]

	hasTokenEnv := false
	for _, env := range container.Env {
		if env.Name == "DAGGER_CACHE_TOKEN" {
			hasTokenEnv = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Name != "engine-registry-auth" {
				t.Error("DAGGER_CACHE_TOKEN should reference secret engine-registry-auth")
			}
		}
	}
	if !hasTokenEnv {
		t.Error("expected DAGGER_CACHE_TOKEN environment variable")
	}

	hasConfigMount := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "engine-config" {
			hasConfigMount = true
		}
	}
	if !hasConfigMount {
		t.Error("expected engine-config volume mount")
	}
}

func TestK8sEngineExtraEnv(t *testing.T) {
	const proxyURL = "http://proxy.corp.example:3128"
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.ExtraEnv = map[string]string{
			"HTTPS_PROXY": proxyURL,
			"https_proxy": proxyURL,
		}
	})

	container := ensureEngineSet(t, p, cs).Spec.Template.Spec.Containers[0]

	// Token first, then the literal operator env sorted by name
	// (HTTPS_PROXY before https_proxy: uppercase sorts first).
	requireEnvNames(t, container.Env, []string{"DAGGER_CACHE_TOKEN", "HTTPS_PROXY", "https_proxy"})
	if token := container.Env[0]; token.ValueFrom == nil || token.ValueFrom.SecretKeyRef == nil {
		t.Error("DAGGER_CACHE_TOKEN must be secret-sourced")
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if env := envByName(container.Env, name); env == nil || env.Value != proxyURL {
			t.Errorf("%s = %+v, want literal value %q", name, env, proxyURL)
		}
	}
}

func TestK8sEngineExtraEnvFrom(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.ExtraEnvFrom = map[string]domain.EnvVarSource{
			"HTTP_PROXY":  {SecretName: "proxy-credentials", Key: "http_proxy"},
			"HTTPS_PROXY": {SecretName: "proxy-credentials", Key: "https_proxy"},
		}
	})

	container := ensureEngineSet(t, p, cs).Spec.Template.Spec.Containers[0]

	// Token first, then the secret-sourced env sorted by name
	// (HTTPS_PROXY before HTTP_PROXY: "S" 0x53 < "_" 0x5f).
	requireEnvNames(t, container.Env, []string{"DAGGER_CACHE_TOKEN", "HTTPS_PROXY", "HTTP_PROXY"})
	for name, wantKey := range map[string]string{"HTTP_PROXY": "http_proxy", "HTTPS_PROXY": "https_proxy"} {
		env := envByName(container.Env, name)
		if env == nil {
			t.Fatalf("missing env %s", name)
		}
		if env.Value != "" {
			t.Errorf("%s Value should be empty, got %q", name, env.Value)
		}
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s must be secret-sourced", name)
		}
		if ref := env.ValueFrom.SecretKeyRef; ref.Name != "proxy-credentials" || ref.Key != wantKey {
			t.Errorf("%s ref = %+v, want secret proxy-credentials key %s", name, ref, wantKey)
		}
	}
}

func TestK8sEngineExtraEnvCombinedOrder(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.ExtraEnv = map[string]string{
			"NO_PROXY": "localhost,127.0.0.1",
		}
		cfg.ExtraEnvFrom = map[string]domain.EnvVarSource{
			"HTTP_PROXY": {SecretName: "proxy-credentials", Key: "http_proxy"},
		}
		cfg.CASecret = "custom-ca-bundle"
	})

	container := ensureEngineSet(t, p, cs).Spec.Template.Spec.Containers[0]

	// Deterministic order: token → sorted literal envs → sorted secret-sourced
	// envs → CA envs.
	requireEnvNames(t, container.Env, []string{
		"DAGGER_CACHE_TOKEN",
		"NO_PROXY",
		"HTTP_PROXY",
		"SSL_CERT_FILE",
		"NODE_EXTRA_CA_CERTS",
	})
}

func TestK8sEngineCAInjection(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.CASecret = "custom-ca-bundle"
		cfg.CAKey = "tls-ca.crt"
	})

	sts := ensureEngineSet(t, p, cs)

	// Volume ca-bundle with secret name and Items tls-ca.crt→ca.crt.
	caVol := volumeByName(sts, volumeCABundle)
	if caVol == nil {
		t.Fatalf("expected %s volume, got %+v", volumeCABundle, sts.Spec.Template.Spec.Volumes)
	}
	if caVol.Secret == nil || caVol.Secret.SecretName != "custom-ca-bundle" {
		t.Errorf("expected secret custom-ca-bundle, got %+v", caVol.Secret)
	}
	if len(caVol.Secret.Items) != 1 || caVol.Secret.Items[0].Key != "tls-ca.crt" || caVol.Secret.Items[0].Path != "ca.crt" {
		t.Errorf("expected Items tls-ca.crt→ca.crt, got %+v", caVol.Secret.Items)
	}

	// Mounted read-only at the well-known path, with env vars pointing at it.
	container := sts.Spec.Template.Spec.Containers[0]
	caMount := mountByName(&container, volumeCABundle)
	if caMount == nil {
		t.Fatalf("expected %s mount, got %+v", volumeCABundle, container.VolumeMounts)
	}
	if caMount.MountPath != engineCAMountPath || caMount.SubPath != "ca.crt" || !caMount.ReadOnly {
		t.Errorf("CA mount = %+v, want path %s subPath ca.crt readOnly", caMount, engineCAMountPath)
	}
	for _, name := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if env := envByName(container.Env, name); env == nil || env.Value != engineCAMountPath {
			t.Errorf("%s = %+v, want value %s", name, env, engineCAMountPath)
		}
	}
}

func TestK8sEngineCAInjectionDisabled(t *testing.T) {
	p, cs := defaultK8sProvider()
	sts := ensureEngineSet(t, p, cs)

	if vol := volumeByName(sts, volumeCABundle); vol != nil {
		t.Errorf("expected no %s volume, got %+v", volumeCABundle, vol)
	}
	container := sts.Spec.Template.Spec.Containers[0]
	if mount := mountByName(&container, volumeCABundle); mount != nil {
		t.Errorf("expected no %s mount, got %+v", volumeCABundle, mount)
	}
	if env := envByName(container.Env, "SSL_CERT_FILE"); env != nil {
		t.Errorf("expected no SSL_CERT_FILE env, got %+v", env)
	}
}

func TestK8sEngineTOMLConfigMap(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.Debug = true
		cfg.LogFormat = "json"
		cfg.RegistryMirrors = map[string][]string{
			"docker.io": {"mirror.gcr.io"},
		}
	})

	sts := ensureEngineSet(t, p, cs)

	wantTOML := "debug = true\n\n[log]\n  format = \"json\"\n\n[registry.\"docker.io\"]\n  mirrors = [\"mirror.gcr.io\"]\n"
	if got := engineConfigMap(t, cs).Data[engineTOMLKey]; got != wantTOML {
		t.Errorf("configmap %s = %q, want %q", engineTOMLKey, got, wantTOML)
	}

	// STS has volume dagger-config (ConfigMap source).
	cfgVol := volumeByName(sts, volumeDaggerConfig)
	if cfgVol == nil {
		t.Fatalf("expected %s volume, got %+v", volumeDaggerConfig, sts.Spec.Template.Spec.Volumes)
	}
	if cfgVol.ConfigMap == nil || cfgVol.ConfigMap.Name != engineConfigMapName {
		t.Errorf("expected ConfigMap %s, got %+v", engineConfigMapName, cfgVol.ConfigMap)
	}

	// Mounted read-only at /etc/dagger/engine.toml via subPath.
	container := sts.Spec.Template.Spec.Containers[0]
	tomlMount := mountByName(&container, volumeDaggerConfig)
	if tomlMount == nil {
		t.Fatalf("expected %s mount, got %+v", volumeDaggerConfig, container.VolumeMounts)
	}
	if tomlMount.MountPath != engineTOMLPath || tomlMount.SubPath != engineTOMLKey || !tomlMount.ReadOnly {
		t.Errorf("engine.toml mount = %+v, want path %s subPath %s readOnly", tomlMount, engineTOMLPath, engineTOMLKey)
	}
}

func TestK8sEngineTOMLDefaultLogFormat(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.LogFormat = "json"
	})
	ensureEngineSet(t, p, cs)

	wantTOML := "[log]\n  format = \"json\"\n"
	if got := engineConfigMap(t, cs).Data[engineTOMLKey]; got != wantTOML {
		t.Errorf("configmap %s = %q, want %q", engineTOMLKey, got, wantTOML)
	}
}

func TestK8sEngineTOMLEmpty(t *testing.T) {
	p, cs := defaultK8sProvider()

	// Pre-create a stale ConfigMap to verify deletion.
	_, err := cs.CoreV1().ConfigMaps("dagger-cache").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      engineConfigMapName,
			Namespace: "dagger-cache",
		},
		Data: map[string]string{engineTOMLKey: "stale"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("pre-create stale configmap: %v", err)
	}

	sts := ensureEngineSet(t, p, cs)

	// ConfigMap should be deleted.
	if _, err := cs.CoreV1().ConfigMaps("dagger-cache").Get(context.Background(), engineConfigMapName, metav1.GetOptions{}); err == nil {
		t.Error("expected stale configmap to be deleted")
	}

	// No dagger-config volume/mount.
	if vol := volumeByName(sts, volumeDaggerConfig); vol != nil {
		t.Errorf("expected no %s volume, got %+v", volumeDaggerConfig, vol)
	}
	container := sts.Spec.Template.Spec.Containers[0]
	if mount := mountByName(&container, volumeDaggerConfig); mount != nil {
		t.Errorf("expected no %s mount, got %+v", volumeDaggerConfig, mount)
	}
}

func TestK8sEngineConfigMapIdempotent(t *testing.T) {
	p, cs := defaultK8sProvider(func(cfg *K8sProviderConfig) {
		cfg.LogFormat = "json"
	})

	ensureEngineSet(t, p, cs)
	ensureEngineSet(t, p, cs) // second ensure takes the update path

	wantTOML := "[log]\n  format = \"json\"\n"
	if got := engineConfigMap(t, cs).Data[engineTOMLKey]; got != wantTOML {
		t.Errorf("configmap %s = %q, want %q", engineTOMLKey, got, wantTOML)
	}
}
