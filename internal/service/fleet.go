package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

type Manager struct {
	provider              domain.FleetProvider
	sessions              domain.SessionStore
	metrics               *observ.Metrics
	maxReplicasPerVersion int
	maxSessionsPerReplica int
	replicaIdleTTL        time.Duration
	versionRetention      time.Duration
	isolation             string
	engineCap             func(ctx context.Context, orgID string) (int64, error)
	logger                *logrus.Logger
}

type ManagerConfig struct {
	MaxReplicasPerVersion int
	MaxSessionsPerReplica int
	ReplicaIdleTTL        time.Duration
	VersionRetention      time.Duration
	// Isolation is "per_org" (default) or "shared".
	Isolation string
}

func NewManager(provider domain.FleetProvider, sessions domain.SessionStore, cfg ManagerConfig, logger *logrus.Logger, metrics *observ.Metrics) *Manager {
	return &Manager{
		provider:              provider,
		sessions:              sessions,
		metrics:               metrics,
		maxReplicasPerVersion: cfg.MaxReplicasPerVersion,
		maxSessionsPerReplica: cfg.MaxSessionsPerReplica,
		replicaIdleTTL:        cfg.ReplicaIdleTTL,
		versionRetention:      cfg.VersionRetention,
		isolation:             cfg.Isolation,
		logger:                logger,
	}
}

// WithEngineQuota wires the org engine hard cap evaluated only when a new
// replica must be scaled up.
func (m *Manager) WithEngineQuota(fn func(ctx context.Context, orgID string) (int64, error)) *Manager {
	m.engineCap = fn
	return m
}

// providerForOrg returns the provider scoped to orgID. Scoping is applied
// whenever orgID is non-empty (regardless of the current isolation mode) so
// the sweeper can address leftover per-org StatefulSets even after the
// isolation setting changed.
func (m *Manager) providerForOrg(orgID string) domain.FleetProvider {
	if orgID == "" {
		return m.provider
	}
	if scoped, ok := m.provider.(domain.ScopedFleetProvider); ok {
		return scoped.WithScope(orgID)
	}
	return m.provider
}

// effectiveOrg maps a requested org to the org actually used for provisioning
// under the current isolation mode: per_org keeps the org, shared collapses to
// "" (a single fleet).
func (m *Manager) effectiveOrg(orgID string) string {
	if m.isolation == "per_org" {
		return orgID
	}
	return ""
}

func (m *Manager) Acquire(ctx context.Context, version string) (*domain.AcquireResult, error) {
	return m.AcquireForOrg(ctx, "", version)
}

// AcquireForOrg provisions/acquires an engine replica scoped to orgID when
// fleet.isolation=per_org.
func (m *Manager) AcquireForOrg(ctx context.Context, orgID, version string) (*domain.AcquireResult, error) {
	effOrg := m.effectiveOrg(orgID)
	provider := m.providerForOrg(effOrg)
	image := provider.GetEngineImage(version)
	if err := provider.EnsureStatefulSet(version, image); err != nil {
		return nil, fmt.Errorf("ensure statefulset: %w", err)
	}
	if err := provider.EnsureService(version); err != nil {
		return nil, fmt.Errorf("ensure service: %w", err)
	}

	replicas, err := provider.GetReplicas(version)
	if err != nil {
		return nil, fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(orgID, version, replicas)

	var bestMatch *domain.Replica
	bestPinned := m.maxSessionsPerReplica + 1

	for i := range replicas {
		r := &replicas[i]
		if !r.Ready {
			continue
		}
		pinned := m.sessions.PinnedSessionsOnReplica(r.Name)
		if pinned < m.maxSessionsPerReplica && pinned < bestPinned {
			bestMatch = r
			bestPinned = pinned
		}
	}

	if bestMatch != nil {
		m.logger.WithFields(logrus.Fields{
			"version": version,
			"pod":     bestMatch.Name,
			"pinned":  bestPinned,
		}).Info("acquired existing replica")
		return &domain.AcquireResult{
			PodName: bestMatch.Name,
			PodIP:   bestMatch.PodIP,
			Version: version,
			Image:   image,
		}, nil
	}

	currentCount := len(replicas)
	if m.engineCap != nil {
		if hard, capErr := m.engineCap(ctx, effOrg); capErr != nil {
			return nil, capErr
		} else if hard > 0 && int64(currentCount+1) > hard {
			return nil, fmt.Errorf("%w: %s limit (%d) reached", domain.ErrOrgQuotaExceeded, domain.QuotaEngines, hard)
		}
	}
	maxReplicas := m.maxReplicasPerVersion
	if currentCount >= maxReplicas {
		return nil, fmt.Errorf("at capacity for version %s (%d/%d replicas)", version, currentCount, maxReplicas)
	}

	targetReplicas := currentCount + 1
	m.logger.WithFields(logrus.Fields{
		"version": version,
		"target":  targetReplicas,
	}).Info("scaling up")

	if err := provider.ScaleUp(version, targetReplicas); err != nil {
		return nil, fmt.Errorf("scale up: %w", err)
	}

	newPodName := domain.EnginePodName(effOrg, version, currentCount)
	if err := provider.WaitForReady(version, newPodName); err != nil {
		return nil, fmt.Errorf("wait for ready: %w", err)
	}

	ip, err := provider.GetReadyReplicaIP(version, newPodName)
	if err != nil {
		return nil, fmt.Errorf("get replica IP: %w", err)
	}

	return &domain.AcquireResult{
		PodName: newPodName,
		PodIP:   ip,
		Version: version,
		Image:   image,
	}, nil
}

func (m *Manager) Unpin(certFP string) {
	m.sessions.Remove(certFP)
}

func (m *Manager) GetVersionFleet(version string) (*domain.FleetInfo, error) {
	return m.GetVersionFleetForOrg("", version)
}

// GetVersionFleetForOrg returns fleet info scoped to orgID.
func (m *Manager) GetVersionFleetForOrg(orgID, version string) (*domain.FleetInfo, error) {
	provider := m.providerForOrg(orgID)
	replicas, err := provider.GetReplicas(version)
	if err != nil {
		return nil, fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(orgID, version, replicas)

	info := &domain.FleetInfo{
		Version:  version,
		STSName:  domain.EngineStsName(orgID, version),
		Org:      orgID,
		Replicas: len(replicas),
	}

	for _, r := range replicas {
		r.PinnedSessions = m.sessions.PinnedSessionsOnReplica(r.Name)
		if r.Ready {
			info.ReadyReplicas++
		}
		info.Ordinals = append(info.Ordinals, r)
	}

	return info, nil
}

// Sweep reconciles every deployed engine fleet. When the provider can
// enumerate scopes it sweeps each (org, version) pair so per-org StatefulSets
// are addressed by their real names; otherwise it falls back to an unscoped
// sweep.
func (m *Manager) Sweep(ctx context.Context) error {
	if scoped, ok := m.provider.(domain.ScopedFleetProvider); ok {
		scopes, err := scoped.AllScopes()
		if err != nil {
			return fmt.Errorf("all scopes: %w", err)
		}
		seen := make(map[domain.FleetScope]bool, len(scopes))
		var errs []error
		for _, fs := range scopes {
			if seen[fs] {
				continue
			}
			seen[fs] = true
			if err := m.sweepVersion(fs.OrgID, fs.Version); err != nil {
				m.logger.WithFields(logrus.Fields{"org": fs.OrgID, "version": fs.Version}).WithError(err).Error("sweep version error")
				errs = append(errs, fmt.Errorf("sweep %s/%s: %w", fs.OrgID, fs.Version, err))
			}
		}
		return errors.Join(errs...)
	}
	return m.SweepForOrg(ctx, "")
}

// SweepForOrg sweeps the fleet scoped to orgID.
func (m *Manager) SweepForOrg(ctx context.Context, orgID string) error {
	provider := m.providerForOrg(orgID)
	versions, err := provider.AllVersions()
	if err != nil {
		return fmt.Errorf("all versions: %w", err)
	}

	var errs []error
	for _, version := range versions {
		if err := m.sweepVersion(orgID, version); err != nil {
			m.logger.WithFields(logrus.Fields{"org": orgID, "version": version}).WithError(err).Error("sweep version error")
			errs = append(errs, fmt.Errorf("sweep %s/%s: %w", orgID, version, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) sweepVersion(orgID, version string) error {
	provider := m.providerForOrg(orgID)
	replicas, err := provider.GetReplicas(version)
	if err != nil {
		return fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(orgID, version, replicas)

	sortDescendingOrdinal(replicas)

	for _, r := range replicas {
		if m.replicaHasActiveSessions(r.Name) {
			continue
		}
		idle := time.Since(r.StartedAt)
		if idle < m.replicaIdleTTL {
			continue
		}

		m.logger.WithFields(logrus.Fields{
			"version": version,
			"pod":     r.Name,
			"idle":    idle,
		}).Info("scaling down idle replica")

		if err := provider.ScaleDown(version, r.Ordinal); err != nil {
			return fmt.Errorf("scale down %s: %w", r.Name, err)
		}
		break
	}

	if m.versionRetention <= 0 {
		// Feature disabled: run only the scale-down loop above. No idle-since
		// reads/writes and no GC (semantics table §1).
		return nil
	}

	if len(replicas) > 0 {
		// Version is active (has pods); clear any stale idle-since stamp.
		return m.clearIdleSinceIfPresent(orgID, version)
	}

	// Zero replicas: run idle tracking + GC.
	return m.gcIdleVersion(orgID, version)
}

// clearIdleSinceIfPresent removes the idle-since annotation when it exists.
func (m *Manager) clearIdleSinceIfPresent(orgID, version string) error {
	provider := m.providerForOrg(orgID)
	_, ok, err := provider.VersionIdleSince(version)
	if err != nil {
		return fmt.Errorf("get idle since: %w", err)
	}
	if !ok {
		return nil
	}
	if err := provider.SetVersionIdleSince(version, time.Time{}); err != nil {
		return fmt.Errorf("clear idle since: %w", err)
	}
	return nil
}

// gcIdleVersion evaluates idle-since tracking and, once version_retention
// elapses, deletes the idle version's Service then StatefulSet.
func (m *Manager) gcIdleVersion(orgID, version string) error {
	provider := m.providerForOrg(orgID)
	if m.versionRetention <= 0 {
		return nil // feature disabled
	}

	if m.versionHasPinnedSessions(orgID, version) {
		// Stale sessions still pin this (dead) version; be conservative and
		// restart the idle clock so GC does not fire prematurely once the
		// sessions are reaped.
		return m.clearIdleSinceIfPresent(orgID, version)
	}

	idleSince, ok, err := provider.VersionIdleSince(version)
	if err != nil {
		if ok {
			// Malformed annotation: log + reset to now, do not crash.
			m.logger.WithField("version", version).WithError(err).Warn("resetting malformed idle-since annotation")
			if setErr := provider.SetVersionIdleSince(version, time.Now()); setErr != nil {
				return fmt.Errorf("reset idle since: %w", setErr)
			}
			return nil
		}
		return fmt.Errorf("get idle since: %w", err)
	}

	now := time.Now()
	if !ok {
		// First idle observation: stamp and wait for version_retention.
		if err := provider.SetVersionIdleSince(version, now); err != nil {
			return fmt.Errorf("set idle since: %w", err)
		}
		return nil
	}

	if now.Sub(idleSince) < m.versionRetention {
		return nil
	}

	// Delete the Service first (self-healing: if the STS delete fails, the
	// next sweep still lists the STS and retries), then the StatefulSet.
	if err := provider.DeleteService(version); err != nil {
		return fmt.Errorf("delete service %s: %w", version, err)
	}
	if err := provider.DeleteStatefulSet(version); err != nil {
		return fmt.Errorf("delete statefulset %s: %w", version, err)
	}
	m.logger.WithField("version", version).Info("deleted idle engine fleet version")
	return nil
}

// versionHasPinnedSessions reports whether any session still pins a replica of
// the given version. When orgID is set, only sessions whose replica pod
// belongs to that org's StatefulSet count, so an active session in org A does
// not block idle GC of org B's fleet.
func (m *Manager) versionHasPinnedSessions(orgID, version string) bool {
	for _, l := range m.sessions.List() {
		if l.Version != version {
			continue
		}
		if orgID == "" {
			return true
		}
		if strings.HasPrefix(l.ReplicaPod, fmt.Sprintf("%s-", domain.EngineStsName(orgID, version))) {
			return true
		}
	}
	return false
}

func (m *Manager) replicaHasActiveSessions(podName string) bool {
	return m.sessions.PinnedSessionsOnReplica(podName) > 0
}

func sortDescendingOrdinal(replicas []domain.Replica) {
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].Ordinal > replicas[j].Ordinal
	})
}

func (m *Manager) AllFleetInfo() ([]domain.FleetInfo, error) {
	// Prefer enumerating real scopes so per-org fleets are reported with their
	// actual STS names and pod ordinals.
	if scoped, ok := m.provider.(domain.ScopedFleetProvider); ok {
		scopes, err := scoped.AllScopes()
		if err != nil {
			return nil, fmt.Errorf("all scopes: %w", err)
		}
		infos := make([]domain.FleetInfo, 0, len(scopes))
		for _, fs := range scopes {
			info, err := m.GetVersionFleetForOrg(fs.OrgID, fs.Version)
			if err != nil {
				m.logger.WithFields(logrus.Fields{"org": fs.OrgID, "version": fs.Version}).WithError(err).Error("get version fleet")
				continue
			}
			infos = append(infos, *info)
		}
		return infos, nil
	}
	return m.AllFleetInfoForOrg("")
}

// AllFleetInfoForOrg returns fleet info for every version scoped to orgID.
func (m *Manager) AllFleetInfoForOrg(orgID string) ([]domain.FleetInfo, error) {
	versions, err := m.providerForOrg(orgID).AllVersions()
	if err != nil {
		return nil, fmt.Errorf("all versions: %w", err)
	}

	infos := make([]domain.FleetInfo, 0, len(versions))
	for _, v := range versions {
		info, err := m.GetVersionFleetForOrg(orgID, v)
		if err != nil {
			m.logger.WithField("version", v).WithError(err).Error("get version fleet")
			continue
		}
		infos = append(infos, *info)
	}
	return infos, nil
}

// DeleteOrgFleets removes every fleet resource (StatefulSets, Services, PVCs,
// token Secret) belonging to orgID when the provider supports it.
func (m *Manager) DeleteOrgFleets(ctx context.Context, orgID string) error {
	type orgFleetDeleter interface {
		DeleteOrgFleets(ctx context.Context, orgID string) error
	}
	if d, ok := m.provider.(orgFleetDeleter); ok {
		return d.DeleteOrgFleets(ctx, orgID)
	}
	return nil
}

// ResolvePodIP returns the pod IP for a lease's replica pod, searching every
// deployed scope for the pod's version. It replaces unscoped
// GetVersionFleet lookups that mis-resolve per-org pod names.
func (m *Manager) ResolvePodIP(orgID, version, podName string) (string, error) {
	if orgID != "" {
		// A lease must point at a pod of its own org's StatefulSet.
		if !strings.HasPrefix(podName, fmt.Sprintf("%s-", domain.EngineStsName(orgID, version))) {
			return "", fmt.Errorf("pod %s does not belong to org %s: %w", podName, orgID, domain.ErrForbidden)
		}
		return m.providerForOrg(orgID).GetReadyReplicaIP(version, podName)
	}
	// Shared isolation: unscoped lookup.
	return m.provider.GetReadyReplicaIP(version, podName)
}

// observeReplicas publishes the per-version replica count. It is safe to call
// when no Metrics were injected.
func (m *Manager) observeReplicas(orgID, version string, replicas []domain.Replica) {
	if m.metrics == nil || m.metrics.ActiveReplicas == nil {
		return
	}
	m.metrics.ActiveReplicas.WithLabelValues(orgID, version).Set(float64(len(replicas)))
}
