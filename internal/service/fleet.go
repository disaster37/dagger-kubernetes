package service

import (
	"context"
	"fmt"
	"sort"
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
	minReplicasPerVersion int
	logger                *logrus.Logger
}

type ManagerConfig struct {
	MaxReplicasPerVersion int
	MaxSessionsPerReplica int
	ReplicaIdleTTL        time.Duration
	VersionRetention      time.Duration
	MinReplicasPerVersion int
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
		minReplicasPerVersion: cfg.MinReplicasPerVersion,
		logger:                logger,
	}
}

func (m *Manager) Acquire(ctx context.Context, version string) (*domain.AcquireResult, error) {
	image := m.provider.GetEngineImage(version)
	if err := m.provider.EnsureStatefulSet(version, image); err != nil {
		return nil, fmt.Errorf("ensure statefulset: %w", err)
	}
	if err := m.provider.EnsureService(version); err != nil {
		return nil, fmt.Errorf("ensure service: %w", err)
	}

	replicas, err := m.provider.GetReplicas(version)
	if err != nil {
		return nil, fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(version, replicas)

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
	maxReplicas := m.maxReplicasPerVersion
	if currentCount >= maxReplicas {
		return nil, fmt.Errorf("at capacity for version %s (%d/%d replicas)", version, currentCount, maxReplicas)
	}

	targetReplicas := currentCount + 1
	m.logger.WithFields(logrus.Fields{
		"version": version,
		"target":  targetReplicas,
	}).Info("scaling up")

	if err := m.provider.ScaleUp(version, targetReplicas); err != nil {
		return nil, fmt.Errorf("scale up: %w", err)
	}

	newPodName := domain.PodName(version, currentCount)
	if err := m.provider.WaitForReady(version, newPodName); err != nil {
		return nil, fmt.Errorf("wait for ready: %w", err)
	}

	ip, err := m.provider.GetReadyReplicaIP(version, newPodName)
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
	replicas, err := m.provider.GetReplicas(version)
	if err != nil {
		return nil, fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(version, replicas)

	info := &domain.FleetInfo{
		Version:  version,
		STSName:  domain.StsName(version),
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

func (m *Manager) Sweep(ctx context.Context) error {
	versions, err := m.provider.AllVersions()
	if err != nil {
		return fmt.Errorf("all versions: %w", err)
	}

	for _, version := range versions {
		if err := m.sweepVersion(version); err != nil {
			m.logger.WithField("version", version).WithError(err).Error("sweep version error")
		}
	}
	return nil
}

func (m *Manager) sweepVersion(version string) error {
	replicas, err := m.provider.GetReplicas(version)
	if err != nil {
		return fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(version, replicas)

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

		if err := m.provider.ScaleDown(version, r.Ordinal); err != nil {
			return fmt.Errorf("scale down %s: %w", r.Name, err)
		}
		break
	}

	return nil
}

func (m *Manager) replicaHasActiveSessions(podName string) bool {
	return m.sessions.PinnedSessionsOnReplica(podName) > 0
}

func sortDescendingOrdinal(replicas []domain.Replica) {
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].Ordinal > replicas[j].Ordinal
	})
}

func (m *Manager) ScaleToZero(version string) error {
	replicas, err := m.provider.GetReplicas(version)
	if err != nil {
		return fmt.Errorf("get replicas: %w", err)
	}
	m.observeReplicas(version, replicas)

	sortDescendingOrdinal(replicas)

	for _, r := range replicas {
		if m.replicaHasActiveSessions(r.Name) {
			continue
		}
		if err := m.provider.ScaleDown(version, r.Ordinal); err != nil {
			return fmt.Errorf("scale down %s: %w", r.Name, err)
		}
	}
	return nil
}

func (m *Manager) AllFleetInfo() ([]domain.FleetInfo, error) {
	versions, err := m.provider.AllVersions()
	if err != nil {
		return nil, fmt.Errorf("all versions: %w", err)
	}

	var infos []domain.FleetInfo
	for _, v := range versions {
		info, err := m.GetVersionFleet(v)
		if err != nil {
			m.logger.WithField("version", v).WithError(err).Error("get version fleet")
			continue
		}
		infos = append(infos, *info)
	}
	return infos, nil
}

// observeReplicas publishes the per-version replica count. It is safe to call
// when no Metrics were injected.
func (m *Manager) observeReplicas(version string, replicas []domain.Replica) {
	if m.metrics == nil || m.metrics.ActiveReplicas == nil {
		return
	}
	m.metrics.ActiveReplicas.WithLabelValues(version).Set(float64(len(replicas)))
}
