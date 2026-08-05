package fleet

import (
	"context"
	"fmt"
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

	name := stsName(version)
	labelMap := p.engineLabels(version)

	sts, err := p.buildStatefulSet(ctx, name, version, image, labelMap, nil)
	if err != nil {
		return err
	}

	_, err = p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Create(ctx, sts, metav1.CreateOptions{})
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

func (p *K8sProvider) buildStatefulSet(ctx context.Context, name, version, image string, labelMap map[string]string, replicas *int32) (*appsv1.StatefulSet, error) {
	_ = ctx
	gcp := p.cfg.TerminationGraceSec
	privileged := p.cfg.Privileged

	storageSize := resource.MustParse(p.cfg.StorageSize)
	cpuRequest := resource.MustParse(p.cfg.CPURequest)
	cpuLimit := resource.MustParse(p.cfg.CPULimit)
	memRequest := resource.MustParse(p.cfg.MemoryRequest)
	memLimit := resource.MustParse(p.cfg.MemoryLimit)
	pullPolicy := p.cfg.PullPolicy

	args := []string{
		fmt.Sprintf("--addr=tcp://0.0.0.0:%d", enginePort),
		"--root=/var/lib/dagger",
	}
	args = append(args, p.cfg.ExtraArgs...)

	envVar := corev1.EnvVar{
		Name: "DAGGER_CACHE_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "engine-registry-auth"},
				Key:                  "token",
			},
		},
	}

	volMounts := []corev1.VolumeMount{
		{Name: "dagger-cache", MountPath: "/var/lib/dagger"},
		{Name: "engine-config", MountPath: "/etc/dagger"},
	}

	volumes := []corev1.Volume{
		{
			Name: "engine-config",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "engine-registry-auth",
				},
			},
		},
	}

	secCtx := &corev1.SecurityContext{Privileged: &privileged}
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(enginePort),
			},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		FailureThreshold:    3,
	}

	res := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    cpuRequest,
			corev1.ResourceMemory: memRequest,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    cpuLimit,
			corev1.ResourceMemory: memLimit,
		},
	}

	container := corev1.Container{
		Name:            "engine",
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Args:            args,
		Env:             []corev1.EnvVar{envVar},
		VolumeMounts:    volMounts,
		SecurityContext: secCtx,
		Resources:       res,
		ReadinessProbe:  probe,
	}

	podSpec := corev1.PodSpec{
		TerminationGracePeriodSeconds: &gcp,
		Containers:                    []corev1.Container{container},
		Volumes:                       volumes,
		Tolerations:                   p.cfg.Tolerations,
		NodeSelector:                  p.cfg.NodeSelector,
	}

	repCount := int32(0)
	if replicas != nil {
		repCount = *replicas
	}

	vctSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: storageSize,
			},
		},
	}
	if p.cfg.StorageClass != "" {
		vctSpec.StorageClassName = &p.cfg.StorageClass
	}

	vct := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "dagger-cache",
			Labels: labelMap,
		},
		Spec: vctSpec,
	}

	retain := appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	delet := appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	rollingUpdate := appsv1.RollingUpdateStatefulSetStrategyType
	parallel := appsv1.ParallelPodManagement
	svcName := serviceName(version)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    labelMap,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         svcName,
			Replicas:            &repCount,
			PodManagementPolicy: parallel,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: rollingUpdate,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labelMap,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labelMap},
				Spec:       podSpec,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{vct},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenScaled:  retain,
				WhenDeleted: delet,
			},
		},
	}

	return sts, nil
}

func (p *K8sProvider) DeleteStatefulSet(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := stsName(version)
	err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete statefulset %s: %w", name, err)
	}
	return nil
}

func (p *K8sProvider) EnsureService(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := serviceName(version)
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

	name := serviceName(version)
	err := p.clientset.CoreV1().Services(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	return nil
}

func (p *K8sProvider) GetReplicas(version string) ([]Replica, error) {
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

	var replicas []Replica
	for _, pod := range pods.Items {
		ordinal := p.extractOrdinal(pod.Name, version)
		startedAt := time.Now()
		if pod.Status.StartTime != nil {
			startedAt = pod.Status.StartTime.Time
		}
		replicas = append(replicas, Replica{
			Name:      pod.Name,
			Ordinal:   ordinal,
			Version:   version,
			PodIP:     pod.Status.PodIP,
			Ready:     p.isPodReady(&pod),
			StartedAt: startedAt,
		})
	}
	return replicas, nil
}

func (p *K8sProvider) ScaleUp(version string, targetReplicas int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return p.setReplicas(ctx, version, int32(targetReplicas))
}

func (p *K8sProvider) ScaleDown(version string, ordinal int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = ordinal
	name := stsName(version)
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
	name := stsName(version)
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
	for _, sts := range stsList.Items {
		if v, ok := sts.Labels[engineLabelVersion]; ok {
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
	prefix := fmt.Sprintf("dagger-engine-%s-", versionSlug(version))
	if !strings.HasPrefix(podName, prefix) {
		return -1
	}
	suffix := podName[len(prefix):]
	var ordinal int
	if _, err := fmt.Sscanf(suffix, "%d", &ordinal); err != nil {
		return -1
	}
	return ordinal
}
