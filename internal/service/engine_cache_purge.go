package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	pruneConcurrency = 8                // bounded concurrent per-pod prune jobs
	prunePodTimeout  = 10 * time.Minute // dagql prune on a large cache can be slow
)

// Purge states recorded in EngineCachePurgeResult.State.
const (
	purgeStateRunning   = "running"
	purgeStateCompleted = "completed"
	purgeStateFailed    = "failed"
)

// EngineCachePurgeService orchestrates a per-version local-cache purge:
// concurrently prune every pod via the engine dagql API (in-process). Status
// is in-memory only.
type EngineCachePurgeService struct {
	provider domain.FleetProvider
	pruner   domain.EnginePruner
	logger   *logrus.Logger

	mu     sync.Mutex
	active map[string]bool
	status map[string]*domain.EngineCachePurgeResult
}

var _ domain.EngineCachePurger = (*EngineCachePurgeService)(nil)

// NewEngineCachePurgeService builds the per-version purge orchestrator.
func NewEngineCachePurgeService(provider domain.FleetProvider, pruner domain.EnginePruner, logger *logrus.Logger) *EngineCachePurgeService {
	return &EngineCachePurgeService{
		provider: provider,
		pruner:   pruner,
		logger:   logger,
		active:   make(map[string]bool),
		status:   make(map[string]*domain.EngineCachePurgeResult),
	}
}

// Status returns a copy of the last recorded result for version, or ok=false
// when no purge has been recorded.
func (s *EngineCachePurgeService) Status(version string) (*domain.EngineCachePurgeResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.status[version]
	if !ok {
		return nil, false
	}
	cp := *r
	cp.Pods = append([]domain.EnginePodPurgeResult(nil), r.Pods...)
	return &cp, true
}

// Purge prunes every running pod of version's StatefulSet. It is serialized
// per version: a concurrent purge of the same version returns
// domain.ErrPurgeInProgress. Per-pod failures are reported in the result
// (state "completed"); only precondition failures (missing fleet, unconfigured
// collaborators) yield state "failed" + an error.
func (s *EngineCachePurgeService) Purge(ctx context.Context, version string) (*domain.EngineCachePurgeResult, error) {
	startedAt := time.Now()

	s.mu.Lock()
	if s.active[version] {
		s.mu.Unlock()
		return nil, domain.ErrPurgeInProgress
	}
	s.active[version] = true
	s.status[version] = &domain.EngineCachePurgeResult{
		Version:   version,
		State:     purgeStateRunning,
		StartedAt: rfc3339(startedAt),
		Pods:      []domain.EnginePodPurgeResult{},
	}
	s.mu.Unlock()

	var (
		result *domain.EngineCachePurgeResult
		err    error
	)
	// finish even when runPurge panics: a wedged active flag would 409 every
	// later purge of this version until the next supervisor restart.
	defer func() { s.finish(version, result, startedAt) }()
	result, err = s.runPurge(ctx, version, startedAt)
	return result, err
}

// runPurge performs the orchestration and returns the completed result. The
// result is not published to the status map until finish runs.
func (s *EngineCachePurgeService) runPurge(ctx context.Context, version string, startedAt time.Time) (*domain.EngineCachePurgeResult, error) {
	result := &domain.EngineCachePurgeResult{
		Version:   version,
		State:     purgeStateCompleted,
		StartedAt: rfc3339(startedAt),
		Pods:      []domain.EnginePodPurgeResult{},
	}

	if s.pruner == nil {
		return failedPurge(result, "engine cache purge not configured"), fmt.Errorf("engine cache purge not configured")
	}

	versions, err := s.provider.AllVersions()
	if err != nil {
		return failedPurge(result, "list engine fleets failed"), fmt.Errorf("list engine fleets: %w", err)
	}
	if !slices.Contains(versions, version) {
		return failedPurge(result, domain.ErrEngineFleetNotFound.Error()), domain.ErrEngineFleetNotFound
	}

	replicas, err := s.provider.GetReplicas(version)
	if err != nil {
		return failedPurge(result, "list engine pods failed"), fmt.Errorf("list engine pods: %w", err)
	}
	result.Replicas = len(replicas)
	if len(replicas) == 0 {
		result.Message = "no running engine pods; nothing to prune"
		return result, nil
	}

	pods := s.pruneAll(ctx, version, replicas)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Ordinal < pods[j].Ordinal })
	result.Pods = pods
	result.Message = purgeSummary(pods)
	return result, nil
}

// pruneAll runs one prune job per replica, bounded by pruneConcurrency.
// Each goroutine writes only its own slice element, so no lock is needed.
func (s *EngineCachePurgeService) pruneAll(ctx context.Context, version string, replicas []domain.Replica) []domain.EnginePodPurgeResult {
	results := make([]domain.EnginePodPurgeResult, len(replicas))
	sem := make(chan struct{}, pruneConcurrency)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.prunePod(ctx, version, &replicas[i])
		}(i)
	}
	wg.Wait()
	return results
}

// prunePod prunes one pod's local cache. A failed prune leaves the pod's
// cache untouched and is reported per pod.
func (s *EngineCachePurgeService) prunePod(ctx context.Context, version string, r *domain.Replica) domain.EnginePodPurgeResult {
	out := domain.EnginePodPurgeResult{PodName: r.Name, Ordinal: r.Ordinal}

	pruneCtx, cancel := context.WithTimeout(ctx, prunePodTimeout)
	err := s.pruner.PruneLocalCache(pruneCtx, r.PodIP, version)
	cancel()
	if err != nil {
		out.Error = err.Error()
		s.logger.WithError(err).WithFields(logrus.Fields{
			"pod":     r.Name,
			"pod_ip":  r.PodIP,
			"version": version,
		}).Warn("engine local-cache prune failed")
		return out
	}
	out.Pruned = true
	return out
}

// finish publishes the completed result and clears the in-flight marker. It
// also runs when runPurge panicked (result == nil), so the version's active
// flag can never wedge.
func (s *EngineCachePurgeService) finish(version string, result *domain.EngineCachePurgeResult, startedAt time.Time) {
	if result == nil {
		result = &domain.EngineCachePurgeResult{
			Version:   version,
			State:     purgeStateFailed,
			StartedAt: rfc3339(startedAt),
			Pods:      []domain.EnginePodPurgeResult{},
			Message:   "purge panicked",
		}
	}
	result.FinishedAt = rfc3339(time.Now())
	s.mu.Lock()
	s.status[version] = result
	delete(s.active, version)
	s.mu.Unlock()
}

// failedPurge stamps a precondition failure onto result.
func failedPurge(result *domain.EngineCachePurgeResult, message string) *domain.EngineCachePurgeResult {
	result.State = purgeStateFailed
	result.Message = message
	return result
}

// purgeSummary renders the per-pod outcome for the result message.
func purgeSummary(pods []domain.EnginePodPurgeResult) string {
	pruned := 0
	for _, p := range pods {
		if p.Pruned {
			pruned++
		}
	}
	if failed := len(pods) - pruned; failed > 0 {
		if failed == 1 {
			return fmt.Sprintf("%d of %d pods pruned; 1 pod failed", pruned, len(pods))
		}
		return fmt.Sprintf("%d of %d pods pruned; %d pods failed", pruned, len(pods), failed)
	}
	return fmt.Sprintf("pruned %d of %d pods", pruned, len(pods))
}
