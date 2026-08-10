package repository

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	engineLabelApp     = "app"
	engineLabelValue   = "dagger-engine"
	engineLabelVersion = "version"
	enginePort         = 9999
)

type K8sProviderConfig struct {
	Namespace           string
	ImageRegistry       string
	StorageClass        string
	StorageSize         string
	CPURequest          string
	CPULimit            string
	MemoryRequest       string
	MemoryLimit         string
	TerminationGraceSec int64
	NodeSelector        map[string]string
	Tolerations         []corev1.Toleration
	ExtraArgs           []string
	PullPolicy          corev1.PullPolicy
	Privileged          bool
}

type K8sProvider struct {
	clientset kubernetes.Interface
	cfg       K8sProviderConfig
}

var _ domain.FleetProvider = (*K8sProvider)(nil)

// NewK8sProvider returns a provider for the given cluster. The config is taken
// by value on purpose: defaults are applied to a local copy so the caller's
// struct is never mutated (exported API, kept stable).
//
//nolint:gocritic // hugeParam: value param preserved for API stability
func NewK8sProvider(clientset kubernetes.Interface, cfg K8sProviderConfig) *K8sProvider {
	if cfg.Namespace == "" {
		cfg.Namespace = "dagger-cache"
	}
	if cfg.ImageRegistry == "" {
		cfg.ImageRegistry = "registry.dagger.io/engine"
	}
	if cfg.StorageSize == "" {
		cfg.StorageSize = "50Gi"
	}
	if cfg.CPURequest == "" {
		cfg.CPURequest = "500m"
	}
	if cfg.CPULimit == "" {
		cfg.CPULimit = "2000m"
	}
	if cfg.MemoryRequest == "" {
		cfg.MemoryRequest = "1Gi"
	}
	if cfg.MemoryLimit == "" {
		cfg.MemoryLimit = "8Gi"
	}
	if cfg.TerminationGraceSec == 0 {
		cfg.TerminationGraceSec = 120
	}
	if cfg.PullPolicy == "" {
		cfg.PullPolicy = corev1.PullIfNotPresent
	}
	return &K8sProvider{
		clientset: clientset,
		cfg:       cfg,
	}
}

func (p *K8sProvider) engineLabels(version string) map[string]string {
	return map[string]string{
		engineLabelApp:     engineLabelValue,
		engineLabelVersion: version,
	}
}

func (p *K8sProvider) EnsureStatefulSet(version, image string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := domain.StsName(version)
	labelMap := p.engineLabels(version)

	sts := p.buildStatefulSet(name, version, image, labelMap)

	_, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Create(ctx, sts, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		existing, getErr := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing statefulset %s: %w", name, getErr)
		}
		sts.ResourceVersion = existing.ResourceVersion
		_, err = p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("create/update statefulset %s: %w", name, err)
	}
	return nil
}

// buildStatefulSet renders the engine StatefulSet for a version. It always
// starts at zero replicas; scaling is handled separately by setReplicas.
func (p *K8sProvider) buildStatefulSet(name, version, image string, labelMap map[string]string) *appsv1.StatefulSet {
	graceSec := p.cfg.TerminationGraceSec
	privileged := p.cfg.Privileged
	replicas := int32(0)

	args := make([]string, 0, 2+len(p.cfg.ExtraArgs))
	args = append(args,
		fmt.Sprintf("--addr=tcp://0.0.0.0:%d", enginePort),
		"--root=/var/lib/dagger",
	)
	args = append(args, p.cfg.ExtraArgs...)

	container := corev1.Container{
		Name:            "engine",
		Image:           image,
		ImagePullPolicy: p.cfg.PullPolicy,
		Args:            args,
		Env: []corev1.EnvVar{
			{
				Name: "DAGGER_CACHE_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "engine-registry-auth"},
						Key:                  "token",
					},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "dagger-cache", MountPath: "/var/lib/dagger"},
			{Name: "engine-config", MountPath: "/etc/dagger"},
		},
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(p.cfg.CPURequest),
				corev1.ResourceMemory: resource.MustParse(p.cfg.MemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(p.cfg.CPULimit),
				corev1.ResourceMemory: resource.MustParse(p.cfg.MemoryLimit),
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(enginePort),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
	}

	vctSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(p.cfg.StorageSize),
			},
		},
	}
	if p.cfg.StorageClass != "" {
		vctSpec.StorageClassName = &p.cfg.StorageClass
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    labelMap,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         domain.ServiceName(version),
			Replicas:            &replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labelMap,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelMap},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &graceSec,
					Containers:                    []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: "engine-config",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "engine-registry-auth",
								},
							},
						},
					},
					Tolerations:  p.cfg.Tolerations,
					NodeSelector: p.cfg.NodeSelector,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "dagger-cache",
						Labels: labelMap,
					},
					Spec: vctSpec,
				},
			},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
}

func (p *K8sProvider) DeleteStatefulSet(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := domain.StsName(version)
	err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete statefulset %s: %w", name, err)
	}
	return nil
}

func (p *K8sProvider) EnsureService(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := domain.ServiceName(version)
	labelMap := p.engineLabels(version)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    labelMap,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  labelMap,
			Ports: []corev1.ServicePort{
				{
					Name:       "engine",
					Port:       enginePort,
					TargetPort: intstr.FromInt32(enginePort),
				},
			},
		},
	}

	_, err := p.clientset.CoreV1().Services(p.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		existing, getErr := p.clientset.CoreV1().Services(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing service %s: %w", name, getErr)
		}
		svc.ResourceVersion = existing.ResourceVersion
		svc.Spec.ClusterIP = existing.Spec.ClusterIP
		_, err = p.clientset.CoreV1().Services(p.cfg.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("create/update service %s: %w", name, err)
	}
	return nil
}

func (p *K8sProvider) DeleteService(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := domain.ServiceName(version)
	err := p.clientset.CoreV1().Services(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	return nil
}

func (p *K8sProvider) GetReplicas(version string) ([]domain.Replica, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	labelMap := p.engineLabels(version)
	selector := labels.SelectorFromSet(labelMap).String()

	pods, err := p.clientset.CoreV1().Pods(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	var replicas []domain.Replica
	for i := range pods.Items {
		pod := &pods.Items[i]
		ordinal := p.extractOrdinal(pod.Name, version)
		startedAt := time.Now()
		if pod.Status.StartTime != nil {
			startedAt = pod.Status.StartTime.Time
		}
		replicas = append(replicas, domain.Replica{
			Name:      pod.Name,
			Ordinal:   ordinal,
			Version:   version,
			PodIP:     pod.Status.PodIP,
			Ready:     p.isPodReady(pod),
			StartedAt: startedAt,
		})
	}
	return replicas, nil
}

func (p *K8sProvider) ScaleUp(version string, targetReplicas int) error {
	if targetReplicas < 0 || targetReplicas > math.MaxInt32 {
		return fmt.Errorf("target replicas %d out of range", targetReplicas)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return p.setReplicas(ctx, version, int32(targetReplicas))
}

// ScaleDown removes one replica from the version's StatefulSet. The ordinal
// is part of the FleetProvider contract; Kubernetes scales the whole set, so
// only the current count matters here.
func (p *K8sProvider) ScaleDown(version string, _ int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := domain.StsName(version)
	sts, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get statefulset %s: %w", name, err)
	}

	current := int32(0)
	if sts.Spec.Replicas != nil {
		current = *sts.Spec.Replicas
	}
	if current <= 0 {
		return nil
	}

	newReplicas := current - 1
	return p.setReplicas(ctx, version, newReplicas)
}

func (p *K8sProvider) setReplicas(ctx context.Context, version string, replicas int32) error {
	name := domain.StsName(version)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		sts, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get statefulset %s: %w", name, err)
		}

		sts.Spec.Replicas = &replicas
		_, err = p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if apierrors.IsConflict(err) {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return fmt.Errorf("scale statefulset %s: %w", name, err)
	}
	return fmt.Errorf("scale statefulset %s after retries: %w", name, lastErr)
}

func (p *K8sProvider) GetReadyReplicaIP(version, podName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pod, err := p.clientset.CoreV1().Pods(p.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get pod %s: %w", podName, err)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s has no IP", podName)
	}
	return pod.Status.PodIP, nil
}

func (p *K8sProvider) WaitForReady(version, podName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pod %s to become ready", podName)
		default:
		}

		pod, err := p.clientset.CoreV1().Pods(p.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				time.Sleep(2 * time.Second)
				continue
			}
			return fmt.Errorf("get pod %s: %w", podName, err)
		}

		if p.isPodReady(pod) && pod.Status.PodIP != "" {
			return nil
		}

		time.Sleep(2 * time.Second)
	}
}

func (p *K8sProvider) GetEngineImage(version string) string {
	return fmt.Sprintf("%s:%s", p.cfg.ImageRegistry, version)
}

func (p *K8sProvider) AllVersions() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	selector := labels.SelectorFromSet(map[string]string{
		engineLabelApp: engineLabelValue,
	}).String()

	stsList, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}

	var versions []string
	for i := range stsList.Items {
		if v, ok := stsList.Items[i].Labels[engineLabelVersion]; ok {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

func (p *K8sProvider) isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (p *K8sProvider) extractOrdinal(podName, version string) int {
	prefix := fmt.Sprintf("dagger-engine-%s-", domain.VersionSlug(version))
	suffix, ok := strings.CutPrefix(podName, prefix)
	if !ok {
		return -1
	}
	ordinal, err := strconv.Atoi(suffix)
	if err != nil {
		return -1
	}
	return ordinal
}
