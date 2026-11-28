package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
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
	engineLabelOrg     = "dagger-kubernetes.io/org"
	enginePort         = 9999

	engineConfigMapName = "dagger-engine-config"
	// engineConfigDir is the engine container's config directory and the mount
	// target of the projected volume carrying the image-pull auth secret plus
	// (when configured) the engine.json ConfigMap. Projecting the ConfigMap
	// key engineConfigKey here lands it at /etc/dagger/engine.json, the path
	// the engine's config.LoadDefault() reads; the file is fatal to parse, so
	// it is rendered per engine version. The image already ships a placeholder
	// /etc/dagger/engine.json; the projected volume replaces the whole
	// directory instead of bind-mounting a file under the secret mount (the
	// latter is rejected by runc on this cluster with "not a directory").
	engineConfigDir           = "/etc/dagger"
	engineCAMountPath         = "/usr/local/share/ca-certificates" // Dagger auto-detects CAs here on startup
	engineImageAuthSecretName = "engine-image-auth"                // holds engine image-pull auth (.dockerconfigjson); projected into engineConfigDir
	volumeDaggerKubernetes    = "dagger-kubernetes"
	volumeEngineConfig        = "engine-config" // projected secret + engine.json ConfigMap, mounted at engineConfigDir
	volumeCABundle            = "ca-bundle"
	volumeCASecret            = "ca-secret" // K8s Secret volume (read-only); mounted only in init container

	engineIdleSinceAnnotation = "dagger-kubernetes.io/idle-since"
)

type K8sProviderConfig struct {
	Namespace           string
	ImageRegistry       string
	StorageClass        string
	StorageSize         string
	PVCLabels           map[string]string // extra labels added to engine PVCs
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
	ExtraEnv            map[string]string              // literal env vars added to the engine container
	ExtraEnvFrom        map[string]domain.EnvVarSource // env vars sourced from Secret keys (proxy credentials)
	CASecret            string                         // Secret with the custom CA PEM bundle; "" = disabled
	CAKey               string                         // key inside CASecret (default applied in NewK8sProvider)
	Debug               bool                           // engine.json: logLevel = "debug"
	LogFormat           string                         // supervisor-only; the engine hardcodes its text log formatter, so this is not rendered
	RegistryMirrors     map[string][]string            // engine.json: registries["<host>"].mirrors
	MirrorHTTP          []string                       // engine.json: registries["<mirror>"].http = true
	ImageCacheMirrors   []domain.ImageCacheMirror      // engine-image routing through TLS image-cache mirrors
	RemoteCacheURL      string                         // engine.json remoteCache.url (>= v1.0.0-beta.28 only)
	RemoteCacheToken    string                         // legacy shared DAGGER_CLOUD_TOKEN (single-org shared mode)
	OrgTokenProvider    func(ctx context.Context, orgID string) (string, error)
}

type K8sProvider struct {
	clientset kubernetes.Interface
	cfg       K8sProviderConfig
	scope     string // org slug when fleet.isolation=per_org
}

var _ domain.FleetProvider = (*K8sProvider)(nil)

// NewK8sProvider returns a provider for the given cluster. The config is taken
// by value on purpose: defaults are applied to a local copy so the caller's
// struct is never mutated (exported API, kept stable).
//
//nolint:gocritic // hugeParam: value param preserved for API stability
func NewK8sProvider(clientset kubernetes.Interface, cfg K8sProviderConfig) *K8sProvider {
	if cfg.Namespace == "" {
		cfg.Namespace = "dagger-kubernetes"
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
	if cfg.CAKey == "" {
		cfg.CAKey = "ca.crt"
	}
	return &K8sProvider{
		clientset: clientset,
		cfg:       cfg,
	}
}

// SetRemoteCache configures the engine.json remoteCache.url (rendered only for
// engines >= v1.0.0-beta.28) and the legacy shared DAGGER_CLOUD_TOKEN injected
// into unscoped engine pods.
func (p *K8sProvider) SetRemoteCache(url, token string) {
	p.cfg.RemoteCacheURL = url
	p.cfg.RemoteCacheToken = token
}

// SetOrgTokenProvider wires on-demand per-org engine token minting.
func (p *K8sProvider) SetOrgTokenProvider(fn func(ctx context.Context, orgID string) (string, error)) {
	p.cfg.OrgTokenProvider = fn
}

// WithScope returns a shallow copy of the provider scoped to orgSlug. An empty
// slug returns the shared (unscoped) provider.
func (p *K8sProvider) WithScope(orgSlug string) domain.FleetProvider {
	cp := *p
	cp.scope = orgSlug
	return &cp
}

func (p *K8sProvider) engineLabels(version string) map[string]string {
	labelMap := map[string]string{
		engineLabelApp:     engineLabelValue,
		engineLabelVersion: version,
	}
	if p.scope != "" {
		labelMap[engineLabelOrg] = p.scope
	}
	return labelMap
}

func (p *K8sProvider) stsName(version string) string {
	return domain.OrgStsName(p.scope, version)
}

func (p *K8sProvider) serviceName(version string) string {
	return domain.OrgServiceName(p.scope, version)
}

// renderEngineConfig renders the fleet-wide Dagger engine configuration for a
// specific engine version. The remote cache key is version-gated because the
// engine rejects unknown config keys fatally.
func (p *K8sProvider) renderEngineConfig(version string) ([]byte, error) {
	return buildEngineConfig(&p.cfg, version).render()
}

func (p *K8sProvider) EnsureStatefulSet(version, image string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Rendered once so the ConfigMap content and the pod spec always agree.
	engineConfig, err := p.renderEngineConfig(version)
	if err != nil {
		return fmt.Errorf("render engine config: %w", err)
	}
	if err := p.ensureEngineConfigMap(ctx, engineConfig); err != nil {
		return fmt.Errorf("ensure engine config map: %w", err)
	}

	if err := p.ensureEngineTokenSecret(ctx); err != nil {
		return fmt.Errorf("ensure engine token secret: %w", err)
	}

	name := p.stsName(version)
	labelMap := p.engineLabels(version)

	sts := p.buildStatefulSet(name, version, image, labelMap, engineConfig)

	_, err = p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Create(ctx, sts, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		existing, getErr := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing statefulset %s: %w", name, getErr)
		}
		sts.ResourceVersion = existing.ResourceVersion
		// buildStatefulSet always starts at 0 replicas; on update, preserve the
		// live replica count so we don't scale down (kill) the running engine pod.
		if existing.Spec.Replicas != nil {
			sts.Spec.Replicas = existing.Spec.Replicas
		}
		_, err = p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("create/update statefulset %s: %w", name, err)
	}
	return nil
}

// buildStatefulSet renders the engine StatefulSet for a version. It always
// starts at zero replicas; scaling is handled separately by setReplicas.
// engineConfig is the rendered engine.json content (empty omits the ConfigMap
// volume and mount).
func (p *K8sProvider) buildStatefulSet(name, version, image string, labelMap map[string]string, engineConfig []byte) *appsv1.StatefulSet {
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
		Env:             p.engineEnv(),
		VolumeMounts:    p.engineVolumeMounts(),
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

	containers := []corev1.Container{container}
	volumes := p.podVolumes(engineConfig)
	initContainers := p.initContainers(image)

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

	pvcLabels := make(map[string]string, len(labelMap)+len(p.cfg.PVCLabels))
	for key, value := range p.cfg.PVCLabels {
		pvcLabels[key] = value
	}
	for key, value := range labelMap {
		pvcLabels[key] = value
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    labelMap,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         p.serviceName(version),
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
					InitContainers:                initContainers,
					Containers:                    containers,
					Volumes:                       volumes,
					Tolerations:                   p.cfg.Tolerations,
					NodeSelector:                  p.cfg.NodeSelector,
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   volumeDaggerKubernetes, // must match the container volume mount name
						Labels: pvcLabels,
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

// engineEnv returns the engine container environment: operator-supplied
// literal and secret-sourced vars (each group sorted by name for deterministic
// specs).
func (p *K8sProvider) engineEnv() []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(p.cfg.ExtraEnv)+len(p.cfg.ExtraEnvFrom)+1)
	switch {
	case p.scope != "" && p.cfg.OrgTokenProvider != nil:
		env = append(env, corev1.EnvVar{
			Name: "DAGGER_CLOUD_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: engineTokenSecretName(p.scope)},
					Key:                  "token",
				},
			},
		})
	case p.cfg.RemoteCacheToken != "":
		env = append(env, corev1.EnvVar{Name: "DAGGER_CLOUD_TOKEN", Value: p.cfg.RemoteCacheToken})
	}
	for _, name := range sortedKeys(p.cfg.ExtraEnv) {
		env = append(env, corev1.EnvVar{Name: name, Value: p.cfg.ExtraEnv[name]})
	}
	for _, name := range sortedKeys(p.cfg.ExtraEnvFrom) {
		src := p.cfg.ExtraEnvFrom[name]
		env = append(env, secretEnvVar(name, src.SecretName, src.Key))
	}
	return env
}

// secretEnvVar returns an env var whose value is one key of a Secret. The
// reference is deliberately not Optional: a missing credential must keep the
// pod from starting.
func secretEnvVar(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

// engineVolumeMounts returns the engine container mounts: the data dir and
// the engine-config volume (image-pull auth secret plus the projected
// engine.json) at the engine's config directory, plus the CA bundle when
// enabled.
func (p *K8sProvider) engineVolumeMounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: volumeDaggerKubernetes, MountPath: "/var/lib/dagger"},
		{Name: volumeEngineConfig, MountPath: engineConfigDir},
	}
	if p.cfg.CASecret != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeCABundle,
			MountPath: engineCAMountPath,
		})
	}
	return mounts
}

// podVolumes returns the pod volumes: the engine image-pull auth secret,
// projected together with the engine.json ConfigMap in one volume when a
// config is rendered (empty engineConfig keeps the secret-only volume), plus
// the CA bundle volumes when a CA secret is configured.
func (p *K8sProvider) podVolumes(engineConfig []byte) []corev1.Volume {
	engineConfigVolume := corev1.Volume{
		Name: volumeEngineConfig,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: engineImageAuthSecretName},
		},
	}
	if len(engineConfig) > 0 {
		engineConfigVolume.VolumeSource = corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{
						Secret: &corev1.SecretProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: engineImageAuthSecretName},
						},
					},
					{
						ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: engineConfigMapName},
							Items: []corev1.KeyToPath{
								{Key: engineConfigKey, Path: engineConfigKey},
							},
						},
					},
				},
			},
		}
	}
	volumes := []corev1.Volume{engineConfigVolume}
	if p.cfg.CASecret != "" {
		volumes = append(volumes,
			corev1.Volume{
				Name: volumeCABundle,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
			corev1.Volume{
				Name: volumeCASecret,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: p.cfg.CASecret,
					},
				},
			},
		)
	}
	return volumes
}

// initContainers returns init containers for the engine pod. When a custom CA
// secret is configured, it copies the CA files from the read-only secret volume
// into a writable emptyDir so Dagger can rehash and install them at startup.
func (p *K8sProvider) initContainers(image string) []corev1.Container {
	if p.cfg.CASecret == "" {
		return nil
	}
	return []corev1.Container{
		{
			Name:            "ca-init",
			Image:           image,
			ImagePullPolicy: p.cfg.PullPolicy,
			Command:         []string{"/bin/sh", "-c"},
			Args:            []string{"cp /tmp/ca-secret/* /usr/local/share/ca-certificates/"},
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
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      volumeCASecret,
					MountPath: "/tmp/ca-secret",
					ReadOnly:  true,
				},
				{
					Name:      volumeCABundle,
					MountPath: "/usr/local/share/ca-certificates",
				},
			},
		},
	}
}

// sortedKeys returns the keys of m in ascending order so map-driven pod specs
// are deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// engineTokenSecretName returns the per-org engine token Secret name.
func engineTokenSecretName(orgID string) string {
	return fmt.Sprintf("dagger-engine-token-%s", orgID)
}

// ensureEngineTokenSecret mints (or refreshes) the org's engine token and
// writes it to Secret/dagger-engine-token-<orgID>.
func (p *K8sProvider) ensureEngineTokenSecret(ctx context.Context) error {
	if p.scope == "" || p.cfg.OrgTokenProvider == nil {
		return nil
	}
	token, err := p.cfg.OrgTokenProvider(ctx, p.scope)
	if err != nil {
		return err
	}
	return p.EnsureOrgTokenSecret(ctx, p.scope, token)
}

// EnsureOrgTokenSecret creates or updates the per-org engine token Secret.
func (p *K8sProvider) EnsureOrgTokenSecret(ctx context.Context, orgID, token string) error {
	if orgID == "" || token == "" {
		return nil
	}
	secLabels := map[string]string{
		engineLabelApp: engineLabelValue,
		engineLabelOrg: orgID,
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      engineTokenSecretName(orgID),
			Namespace: p.cfg.Namespace,
			Labels:    secLabels,
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"token": token},
	}
	_, err := p.clientset.CoreV1().Secrets(p.cfg.Namespace).Create(ctx, sec, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		existing, getErr := p.clientset.CoreV1().Secrets(p.cfg.Namespace).Get(ctx, sec.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing secret %s: %w", sec.Name, getErr)
		}
		sec.ResourceVersion = existing.ResourceVersion
		_, err = p.clientset.CoreV1().Secrets(p.cfg.Namespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("create/update secret %s: %w", sec.Name, err)
	}
	return nil
}

// DeleteOrgTokenSecret removes the per-org engine token Secret.
func (p *K8sProvider) DeleteOrgTokenSecret(ctx context.Context, orgID string) error {
	err := p.clientset.CoreV1().Secrets(p.cfg.Namespace).Delete(ctx, engineTokenSecretName(orgID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete secret: %w", err)
	}
	return nil
}

// DeleteOrgFleets deletes every engine StatefulSet, Service, retained PVC, and
// token Secret belonging to orgID.
func (p *K8sProvider) DeleteOrgFleets(ctx context.Context, orgID string) error {
	scoped, ok := p.WithScope(orgID).(*K8sProvider)
	if !ok {
		return fmt.Errorf("delete org fleets: provider is not a K8sProvider")
	}
	versions, err := scoped.AllVersions()
	if err != nil {
		return err
	}
	var errs []error
	for _, version := range versions {
		if pvcs, err := scoped.ListPVCs(version); err == nil {
			for _, pvc := range pvcs {
				_ = scoped.DeletePVC(pvc.Name)
			}
		}
		_ = scoped.DeleteStatefulSet(version)
		_ = scoped.DeleteService(version)
	}
	_ = p.clientset.CoreV1().Secrets(p.cfg.Namespace).Delete(ctx, engineTokenSecretName(orgID), metav1.DeleteOptions{})
	return errors.Join(errs...)
}

// ensureEngineConfigMap creates or updates the fleet-wide ConfigMap holding
// engine.json. When the rendered config is empty, a stale ConfigMap is deleted
// best-effort so pods are not pinned to a removed volume source.
func (p *K8sProvider) ensureEngineConfigMap(ctx context.Context, engineConfig []byte) error {
	if len(engineConfig) == 0 {
		err := p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Delete(ctx, engineConfigMapName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete configmap %s: %w", engineConfigMapName, err)
		}
		return nil
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      engineConfigMapName,
			Namespace: p.cfg.Namespace,
			Labels:    map[string]string{engineLabelApp: engineLabelValue},
		},
		Data: map[string]string{engineConfigKey: string(engineConfig)},
	}
	_, err := p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		existing, getErr := p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Get(ctx, engineConfigMapName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("get existing configmap %s: %w", engineConfigMapName, getErr)
		}
		cm.ResourceVersion = existing.ResourceVersion
		_, err = p.clientset.CoreV1().ConfigMaps(p.cfg.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("create/update configmap %s: %w", engineConfigMapName, err)
	}
	return nil
}

func (p *K8sProvider) DeleteStatefulSet(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := p.stsName(version)
	err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete statefulset %s: %w", name, err)
	}
	return nil
}

func (p *K8sProvider) EnsureService(version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := p.serviceName(version)
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

	name := p.serviceName(version)
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
		if pod.DeletionTimestamp != nil {
			continue
		}
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

// ListPVCs returns the per-pod PVCs of the version's StatefulSet, discovered
// via the label selector app=dagger-engine,version=<v>. Each entry carries the
// PVC name and its ordinal (extracted from the StatefulSet volume-claim naming
// convention). PVCs whose ordinal cannot be parsed are silently omitted (they
// do not match the naming convention and should not exist under the label
// selector).
func (p *K8sProvider) ListPVCs(version string) ([]domain.PVCInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	labelMap := p.engineLabels(version)
	selector := labels.SelectorFromSet(labelMap).String()

	pvcList, err := p.clientset.CoreV1().PersistentVolumeClaims(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list PVCs: %w", err)
	}

	var out []domain.PVCInfo
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		ordinal := extractScopedPVCOrdinal(pvc.Name, p.stsName(version))
		if ordinal < 0 {
			continue
		}
		out = append(out, domain.PVCInfo{Name: pvc.Name, Ordinal: ordinal})
	}
	return out, nil
}

// DeletePVC deletes a single PVC by name. NotFound is treated as success
// (idempotent — the PVC is already gone).
func (p *K8sProvider) DeletePVC(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.clientset.CoreV1().PersistentVolumeClaims(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete PVC %s: %w", name, err)
	}
	return nil
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

	name := p.stsName(version)
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
	name := p.stsName(version)
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
	registry := domain.EngineImageRegistryViaMirror(p.cfg.ImageRegistry, p.cfg.ImageCacheMirrors)
	return fmt.Sprintf("%s:%s", registry, version)
}

// AllScopes returns one FleetScope per deployed engine StatefulSet, reading the
// org and version from the StatefulSet labels. Unlike AllVersions it preserves
// the org label so the sweeper can address each per-org fleet by its real name.
// The listing is constrained to the provider's scope when set.
func (p *K8sProvider) AllScopes() ([]domain.FleetScope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	selectorMap := map[string]string{engineLabelApp: engineLabelValue}
	if p.scope != "" {
		selectorMap[engineLabelOrg] = p.scope
	}
	selector := labels.SelectorFromSet(selectorMap).String()

	stsList, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}

	scopes := make([]domain.FleetScope, 0, len(stsList.Items))
	for i := range stsList.Items {
		version, ok := stsList.Items[i].Labels[engineLabelVersion]
		if !ok {
			continue
		}
		scopes = append(scopes, domain.FleetScope{
			OrgID:   stsList.Items[i].Labels[engineLabelOrg],
			Version: version,
		})
	}
	return scopes, nil
}

func (p *K8sProvider) AllVersions() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	selectorMap := map[string]string{engineLabelApp: engineLabelValue}
	if p.scope != "" {
		selectorMap[engineLabelOrg] = p.scope
	}
	selector := labels.SelectorFromSet(selectorMap).String()

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

// VersionIdleSince returns the idle-since annotation value. ok is true when the
// annotation exists (including a malformed value, which returns ok=true plus a
// parse error); ok is false when the StatefulSet is missing or has no annotation.
func (p *K8sProvider) VersionIdleSince(version string) (time.Time, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	name := p.stsName(version)
	sts, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("get statefulset %s: %w", name, err)
	}

	raw, ok := sts.Annotations[engineIdleSinceAnnotation]
	if !ok || raw == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("parse idle-since annotation %q: %w", raw, err)
	}
	return t, true, nil
}

// SetVersionIdleSince writes (or, when idleSince is zero, clears) the
// idle-since annotation. A missing StatefulSet is a no-op (already deleted).
func (p *K8sProvider) SetVersionIdleSince(version string, idleSince time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := p.stsName(version)
	sts, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get statefulset %s: %w", name, err)
	}

	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	if idleSince.IsZero() {
		delete(sts.Annotations, engineIdleSinceAnnotation)
	} else {
		sts.Annotations[engineIdleSinceAnnotation] = idleSince.UTC().Format(time.RFC3339)
	}

	if _, err := p.clientset.AppsV1().StatefulSets(p.cfg.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update statefulset %s: %w", name, err)
	}
	return nil
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
	prefix := fmt.Sprintf("%s-", p.stsName(version))
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

// extractPVCOrdinal extracts the ordinal from a StatefulSet PVC name
// (<volume-claim-template>-<stsName>-<ordinal>). Returns -1 when the name does
// not follow the convention.
func extractPVCOrdinal(pvcName, version string) int {
	return extractScopedPVCOrdinal(pvcName, domain.StsName(version))
}

func extractScopedPVCOrdinal(pvcName, stsName string) int {
	prefix := fmt.Sprintf("%s-%s-", volumeDaggerKubernetes, stsName)
	suffix, ok := strings.CutPrefix(pvcName, prefix)
	if !ok {
		return -1
	}
	ordinal, err := strconv.Atoi(suffix)
	if err != nil {
		return -1
	}
	return ordinal
}
