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
// concurrently prune every pod via the engine dagql API (in-process), then
// delete the PVCs of runners that are not running (retained by the
// StatefulSet's WhenScaled:Retain policy). Status is in-memory only.
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
	// Copy into fresh non-nil slices: the documented JSON shape is
	// pods[]/pvcs[], never null.
	cp.Pods = append([]domain.EnginePodPurgeResult{}, r.Pods...)
	cp.PVCs = append([]domain.EnginePVCDeleteResult{}, r.PVCs...)
	return &cp, true
}

// Purge prunes every running pod of version's StatefulSet, then deletes the
// PVCs of the version's runners that are not running. It is serialized per
// version: a concurrent purge of the same version returns
// domain.ErrPurgeInProgress. Per-pod and per-PVC failures are reported in the
// result (state "completed"); only precondition failures (missing fleet,
// unconfigured collaborators) yield state "failed" + an error.
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
		PVCs:      []domain.EnginePVCDeleteResult{},
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
		PVCs:      []domain.EnginePVCDeleteResult{},
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

	// 1. Prune the running pods (an empty step when the version has zero
	// replicas).
	pods := s.pruneAll(ctx, version, replicas)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Ordinal < pods[j].Ordinal })
	result.Pods = pods

	// 2. Delete the PVCs of the runners that are not running.
	pvcs, err := s.deleteOrphanedPVCs(version, replicas)
	if err != nil {
		return failedPurge(result, "list engine PVCs failed"), fmt.Errorf("list engine PVCs: %w", err)
	}
	sort.Slice(pvcs, func(i, j int) bool { return pvcs[i].Ordinal < pvcs[j].Ordinal })
	result.PVCs = pvcs

	result.Message = purgeMessage(pods, pvcs)
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

// deleteOrphanedPVCs deletes the version's PVCs whose ordinal has no running
// pod. The running set is built from GetReplicas (non-terminating pods), so
// every mounted PVC — ready, pending or starting pods alike — is protected.
// Per-PVC failures are logged and recorded; the aggregate error is nil unless
// listing PVCs itself fails.
func (s *EngineCachePurgeService) deleteOrphanedPVCs(version string, replicas []domain.Replica) ([]domain.EnginePVCDeleteResult, error) {
	pvcs, err := s.provider.ListPVCs(version)
	if err != nil {
		return nil, err
	}

	running := make(map[int]bool, len(replicas))
	for _, r := range replicas {
		running[r.Ordinal] = true
	}

	results := make([]domain.EnginePVCDeleteResult, 0, len(pvcs))
	for _, pvc := range pvcs {
		if pvc.Ordinal < 0 {
			s.logger.WithField("pvc", pvc.Name).Warn("skip PVC: cannot parse ordinal from name")
			continue
		}
		if running[pvc.Ordinal] {
			continue // keep the PVC of every running runner
		}
		out := domain.EnginePVCDeleteResult{PVCName: pvc.Name, Ordinal: pvc.Ordinal}
		if err := s.provider.DeletePVC(pvc.Name); err != nil {
			out.Error = err.Error()
			s.logger.WithError(err).WithFields(logrus.Fields{
				"pvc":     pvc.Name,
				"ordinal": pvc.Ordinal,
				"version": version,
			}).Warn("engine PVC delete failed")
		} else {
			out.Deleted = true
		}
		results = append(results, out)
	}
	return results, nil
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
			PVCs:      []domain.EnginePVCDeleteResult{},
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

// purgeMessage combines the per-pod prune summary with the orphaned-PVC
// deletion summary (running pods first, then PVCs).
func purgeMessage(pods []domain.EnginePodPurgeResult, pvcs []domain.EnginePVCDeleteResult) string {
	if len(pods) == 0 && len(pvcs) == 0 {
		return "no running engine pods; nothing to prune"
	}
	msg := purgeSummary(pods)
	if extra := pvcSummary(pvcs); extra != "" {
		msg = fmt.Sprintf("%s; %s", msg, extra)
	}
	return msg
}

// pvcSummary renders the per-PVC outcome for the result message.
func pvcSummary(pvcs []domain.EnginePVCDeleteResult) string {
	if len(pvcs) == 0 {
		return ""
	}
	deleted := 0
	for _, p := range pvcs {
		if p.Deleted {
			deleted++
		}
	}
	if failed := len(pvcs) - deleted; failed > 0 {
		if failed == 1 {
			return fmt.Sprintf("deleted %d of %d orphaned PVCs; 1 PVC failed", deleted, len(pvcs))
		}
		return fmt.Sprintf("deleted %d of %d orphaned PVCs; %d PVCs failed", deleted, len(pvcs), failed)
	}
	return fmt.Sprintf("deleted %d orphaned PVC(s)", deleted)
}
