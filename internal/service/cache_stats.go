package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

const (
	cacheStatsTTL    = 15 * time.Second
	cacheProbeBudget = 30 * time.Second
	maxPurgeAllTags  = 1000

	s3UnsupportedMessage = "s3 cache stats not supported in this release"
	registryDownMessage  = "registry unreachable"
	catalogDisabledMsg   = "catalog disabled"
)

// CacheStatsService implements domain.CacheStatsProvider + domain.CachePurger.
// It probes the OCI registry (catalog/tags/manifests), VictoriaMetrics hit
// counters, and assembles the rich cache payload, and it owns the
// manual purge + background GC sweeper.
type CacheStatsService struct {
	cache      *Cache
	router     *RegistryRouter           // may be nil (s3 backend)
	metrics    domain.CacheMetricsClient // may be nil
	gcCfg      domain.GCConfig
	logger     *logrus.Logger
	metricsObs *observ.Metrics // may be nil

	mu       sync.Mutex
	cached   *domain.CacheStats
	cachedAt time.Time

	purgeMu  sync.Mutex // serializes purge / GC
	gcMu     sync.Mutex // guards lastGC / lastGCAt / nextGCAt (short critical section)
	lastGC   *domain.GCRunSummary
	lastGCAt time.Time
	nextGCAt time.Time
}

func NewCacheStatsService(
	cache *Cache,
	router *RegistryRouter,
	metricsClient domain.CacheMetricsClient,
	gcCfg domain.GCConfig,
	logger *logrus.Logger,
	obs *observ.Metrics,
) *CacheStatsService {
	return &CacheStatsService{
		cache:      cache,
		router:     router,
		metrics:    metricsClient,
		gcCfg:      gcCfg,
		logger:     logger,
		metricsObs: obs,
	}
}

// cacheEntry is one manifest discovered in the registry catalog.
type cacheEntry struct {
	repo      string
	tag       string
	digest    string
	size      int64
	layers    int64
	backendID string
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// deleteManifestRoute removes a repo+tag routing row. It no-ops when the
// router (s3 backend) or its routing table is unavailable.
func (s *CacheStatsService) deleteManifestRoute(ctx context.Context, repo, tag string) {
	if s.router == nil || s.router.routes == nil {
		return
	}
	_ = s.router.routes.DeleteManifestRoute(ctx, repo, tag)
}

// Stats implements domain.CacheStatsProvider. Returns the cached payload when
// fresh, else re-probes the registry + metrics with a bounded budget.
func (s *CacheStatsService) Stats(ctx context.Context) (*domain.CacheStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && time.Since(s.cachedAt) < cacheStatsTTL {
		return s.cached, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, cacheProbeBudget)
	defer cancel()

	stats := s.probe(probeCtx)
	s.cached = stats
	s.cachedAt = time.Now()
	return stats, nil
}

// probe assembles the uncached CacheStats payload. It never fails hard: an
// unreachable registry yields running:false with size -1 (HTTP 200 in the
// handler).
func (s *CacheStatsService) probe(ctx context.Context) *domain.CacheStats {
	stats := &domain.CacheStats{
		Backend:     s.cache.Type,
		Registry:    s.cache.Registry,
		Running:     false,
		Reachable:   false,
		TotalSize:   -1,
		ObjectCount: -1,
		Ref:         nil,
		CollectedAt: rfc3339(time.Now()),
		GC:          s.GCRules(),
	}
	defer s.publishCacheGauges(stats)

	if s.cache.Type == "s3" {
		stats.Running = s.cache.S3.Bucket != ""
		stats.Reachable = stats.Running
		stats.Message = s3UnsupportedMessage
		return stats
	}

	if s.router == nil || len(s.router.Backends()) == 0 {
		stats.Message = registryDownMessage
		return stats
	}

	entries, charges, anyReachable, catalogDisabled, timedOut := s.probeBackends(ctx)
	s.router.SetCharges(charges)

	stats.Running = anyReachable
	stats.Reachable = anyReachable
	if !anyReachable {
		stats.Message = registryDownMessage
		return stats
	}
	if len(entries) == 0 && catalogDisabled > 0 {
		stats.Message = catalogDisabledMsg
		return stats
	}

	ref, totalSize, objectCount := s.buildCacheRef(ctx, entries)

	stats.Ref = ref
	stats.TotalSize = totalSize
	stats.ObjectCount = objectCount

	s.attachHitRate(ctx, stats)

	if timedOut {
		stats.Message = "partial: probe timed out"
	}
	return stats
}

// probeBackends pings every configured backend and collects manifest entries
// from the reachable ones, updating their charge counters along the way.
func (s *CacheStatsService) probeBackends(ctx context.Context) (entries []cacheEntry, charges map[string]int64, anyReachable bool, catalogDisabled int, timedOut bool) {
	charges = make(map[string]int64)
	for _, b := range s.router.Backends() {
		client, ok := s.router.ClientByID(b.ID)
		if !ok {
			s.router.MarkDown(b.ID)
			continue
		}
		if err := client.Ping(ctx); err != nil {
			s.router.MarkDown(b.ID)
			continue
		}
		s.router.MarkUp(b.ID)
		anyReachable = true

		repos, err := client.Catalog(ctx)
		if err != nil {
			if errors.Is(err, domain.ErrRegistryCatalogDisabled) {
				catalogDisabled++
			}
			continue
		}

		es, partial := s.collectEntries(ctx, client, repos)
		if partial {
			timedOut = true
		}
		var backendCharge int64
		for _, e := range es {
			if e.size >= 0 {
				backendCharge += e.size
			}
			e.backendID = b.ID
			entries = append(entries, e)
		}
		charges[b.ID] = backendCharge
	}
	return entries, charges, anyReachable, catalogDisabled, timedOut
}

// buildCacheRef sums totalSize/objectCount across all entries and builds the
// single *CacheRef from the first entry whose tag == cacheTag.
func (s *CacheStatsService) buildCacheRef(ctx context.Context, entries []cacheEntry) (ref *domain.CacheRef, totalSize, objectCount int64) {
	for _, e := range entries {
		if e.size >= 0 {
			totalSize += e.size
		}
		objectCount += e.layers
	}

	for _, e := range entries {
		if e.tag != cacheTag {
			continue
		}
		lastUsed := s.lastUsedAt(ctx, &e)
		ref = &domain.CacheRef{
			Ref:        s.cache.CacheRef(),
			Tag:        cacheTag,
			Size:       e.size,
			LayerCount: e.layers,
			Digest:     e.digest,
			LastUsedAt: rfc3339(lastUsed),
		}
		break
	}
	return ref, totalSize, objectCount
}

// lastUsedAt returns the shared staleness signal for both stats and GC:
// 1. routing-table LastSeenAt; 2. manifest creation annotation; 3. zero time.
func (s *CacheStatsService) lastUsedAt(ctx context.Context, e *cacheEntry) time.Time {
	// 1. Routing table LastSeenAt.
	if s.router != nil && s.router.routes != nil {
		if route, ok, err := s.router.routes.LookupManifest(ctx, e.repo, e.tag); ok && err == nil {
			if t, err := time.Parse(time.RFC3339, route.LastSeenAt); err == nil && !t.IsZero() {
				return t
			}
		}
	}
	// 2. Manifest creation annotation.
	if client, ok := s.router.ClientByID(e.backendID); ok {
		if t, err := client.ManifestCreated(ctx, e.repo, e.tag); err == nil && !t.IsZero() {
			return t
		}
	}
	// 3. Unknown.
	return time.Time{}
}

// collectEntries walks every repo's tags and collects manifest metadata.
// Returns entries plus a timedOut flag for partial (context-cancelled) walks.
func (s *CacheStatsService) collectEntries(ctx context.Context, client domain.RegistryClient, repos []string) ([]cacheEntry, bool) {
	var out []cacheEntry
	for _, repo := range repos {
		if ctx.Err() != nil {
			return out, true
		}
		tags, err := client.Tags(ctx, repo)
		if err != nil {
			if ctx.Err() != nil {
				return out, true
			}
			s.logger.WithField("repo", repo).WithError(err).Warn("list tags failed")
			continue
		}
		for _, tag := range tags {
			if ctx.Err() != nil {
				return out, true
			}
			digest, size, layers, err := client.ManifestSize(ctx, repo, tag)
			if err != nil {
				if errors.Is(err, domain.ErrManifestNotFound) {
					continue
				}
				if ctx.Err() != nil {
					return out, true
				}
				s.logger.WithFields(logrus.Fields{"repo": repo, "tag": tag}).WithError(err).Warn("manifest size failed")
				continue
			}
			out = append(out, cacheEntry{repo: repo, tag: tag, digest: digest, size: size, layers: layers})
		}
	}
	return out, false
}

// attachHitRate fills the hit-rate fields from VictoriaMetrics when available.
func (s *CacheStatsService) attachHitRate(ctx context.Context, stats *domain.CacheStats) {
	if s.metrics == nil {
		return
	}
	hit, miss, err := s.metrics.CacheHitRate(ctx)
	if err != nil {
		return // hit_rate stays nil; counts stay 0
	}
	stats.HitCount = int64(hit)
	stats.MissCount = int64(miss)
	if hit+miss > 0 {
		rate := hit / (hit + miss)
		stats.HitRate = &rate
	}
}

// publishCacheGauges syncs the observable cache size/object gauges.
func (s *CacheStatsService) publishCacheGauges(stats *domain.CacheStats) {
	if s.metricsObs == nil {
		return
	}
	size := float64(stats.TotalSize)
	if stats.TotalSize < 0 {
		size = 0
	}
	objects := float64(stats.ObjectCount)
	if stats.ObjectCount < 0 {
		objects = 0
	}
	s.metricsObs.CacheSizeBytes.Set(size)
	s.metricsObs.CacheObjectCount.Set(objects)
}

// GCRules implements domain.CacheStatsProvider.
func (s *CacheStatsService) GCRules() domain.GCRules {
	rules := domain.GCRules{
		Enabled:  s.gcCfg.Enabled,
		MaxAge:   s.gcCfg.MaxAge.String(),
		Schedule: s.gcCfg.Schedule.String(),
	}

	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if !s.lastGCAt.IsZero() {
		rules.LastRunAt = rfc3339(s.lastGCAt)
		rules.LastRunSummary = s.lastGC
	}
	if s.gcCfg.Enabled && !s.nextGCAt.IsZero() {
		rules.NextRunAt = rfc3339(s.nextGCAt)
	}
	return rules
}

// Purge implements domain.CachePurger. It purges every tag in every catalog
// repo across all backends, capped at maxPurgeAllTags entries.
func (s *CacheStatsService) Purge(ctx context.Context) (*domain.PurgeResult, error) {
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	if s.router == nil || len(s.router.Backends()) == 0 {
		return nil, fmt.Errorf("registry not configured")
	}

	result := &domain.PurgeResult{}
	truncated := false
	catalogOK := false
	catalogDisabled := false
	for _, b := range s.router.Backends() {
		client, ok := s.router.ClientByID(b.ID)
		if !ok {
			continue
		}
		repos, err := client.Catalog(ctx)
		if err != nil {
			if errors.Is(err, domain.ErrRegistryCatalogDisabled) {
				catalogDisabled = true
				continue
			}
			return nil, fmt.Errorf("catalog: %w", err)
		}
		catalogOK = true
		backendTruncated, err := s.purgeBackend(ctx, client, repos, result)
		if err != nil {
			return nil, err
		}
		if backendTruncated {
			truncated = true
			break
		}
	}

	if truncated {
		result.Message = "truncated at 1000 tags"
	}
	if !catalogOK && catalogDisabled {
		return nil, fmt.Errorf("%w: catalog disabled; cannot enumerate tags for purge", domain.ErrRegistryCatalogDisabled)
	}
	s.invalidateCache()
	return result, nil
}

// purgeBackend purges every tag of every repo on one backend. Returns
// truncated=true when the global maxPurgeAllTags cap was reached.
func (s *CacheStatsService) purgeBackend(ctx context.Context, client domain.RegistryClient, repos []string, result *domain.PurgeResult) (bool, error) {
	for _, repo := range repos {
		tags, err := client.Tags(ctx, repo)
		if err != nil {
			return false, fmt.Errorf("tags: %w", err)
		}
		for _, tag := range tags {
			if result.Purged+result.AlreadyPurged >= maxPurgeAllTags {
				return true, nil
			}
			digest, size, _, err := client.ManifestSize(ctx, repo, tag)
			if errors.Is(err, domain.ErrManifestNotFound) {
				result.AlreadyPurged++
				s.deleteManifestRoute(ctx, repo, tag)
				continue
			}
			if err != nil {
				return false, fmt.Errorf("manifest lookup: %w", err)
			}
			if err := client.DeleteManifest(ctx, repo, digest); err != nil {
				if errors.Is(err, domain.ErrManifestNotFound) {
					result.AlreadyPurged++
					s.deleteManifestRoute(ctx, repo, tag)
					continue
				}
				return false, fmt.Errorf("delete manifest: %w", err)
			}
			result.Purged++
			result.FreedBytes += size
			result.Tags = append(result.Tags, tag)
			s.deleteManifestRoute(ctx, repo, tag)
			if s.metricsObs != nil {
				s.metricsObs.CachePurgeTotal.Inc()
			}
		}
	}
	return false, nil
}

// RunGC is the GC sweeper entry point. It purges tags older than MaxAge that
// have not been observed recently.
func (s *CacheStatsService) RunGC(ctx context.Context) (*domain.GCRunSummary, error) {
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	summary := &domain.GCRunSummary{StartedAt: rfc3339(time.Now())}
	finish := func(msg string, err error) (*domain.GCRunSummary, error) {
		summary.FinishedAt = rfc3339(time.Now())
		summary.Message = msg
		s.recordGC(summary, err)
		return summary, err
	}

	if s.router == nil || len(s.router.Backends()) == 0 {
		return finish("registry not configured", fmt.Errorf("registry not configured"))
	}

	probeCtx, cancel := context.WithTimeout(ctx, cacheProbeBudget)
	defer cancel()

	entries := s.gcCollectEntries(probeCtx)
	if err := s.gcSweepEntries(probeCtx, ctx, entries, summary); err != nil {
		return finish("registry delete not enabled", err)
	}
	s.invalidateCache()
	return finish("", nil)
}

// gcCollectEntries catalogs every backend and collects manifest entries.
func (s *CacheStatsService) gcCollectEntries(ctx context.Context) []cacheEntry {
	var entries []cacheEntry
	for _, b := range s.router.Backends() {
		client, ok := s.router.ClientByID(b.ID)
		if !ok {
			continue
		}
		repos, err := client.Catalog(ctx)
		if err != nil {
			if !errors.Is(err, domain.ErrRegistryCatalogDisabled) {
				s.logger.WithField("backend", b.ID).WithError(err).Warn("gc catalog failed")
			}
			continue
		}
		es, _ := s.collectEntries(ctx, client, repos)
		for _, e := range es {
			e.backendID = b.ID
			entries = append(entries, e)
		}
	}
	return entries
}

// gcSweepEntries iterates over all entries and deletes those whose last-used
// time is older than MaxAge. Returns ErrRegistryDeleteDisabled when the
// backend refuses deletes.
func (s *CacheStatsService) gcSweepEntries(probeCtx, ctx context.Context, entries []cacheEntry, summary *domain.GCRunSummary) error {
	for _, e := range entries {
		used := s.lastUsedAt(probeCtx, &e)
		if used.IsZero() {
			summary.Skipped++
			continue // never observed → keep
		}
		if time.Since(used) < s.gcCfg.MaxAge {
			summary.Skipped++
			continue
		}
		client, ok := s.router.ClientByID(e.backendID)
		if !ok {
			summary.Skipped++
			continue
		}
		if err := client.DeleteManifest(probeCtx, e.repo, e.digest); err != nil {
			if errors.Is(err, domain.ErrRegistryDeleteDisabled) {
				return err
			}
			if errors.Is(err, domain.ErrManifestNotFound) {
				summary.Skipped++
				continue
			}
			summary.Errors++
			continue
		}
		summary.PurgedTags++
		summary.FreedBytes += e.size
		s.deleteManifestRoute(ctx, e.repo, e.tag)
		if s.metricsObs != nil {
			s.metricsObs.CachePurgeTotal.Inc()
		}
	}
	return nil
}

// recordGC stores the last run summary and bumps the GC run counter.
func (s *CacheStatsService) recordGC(summary *domain.GCRunSummary, runErr error) {
	s.gcMu.Lock()
	s.lastGC = summary
	s.lastGCAt = time.Now()
	s.nextGCAt = s.lastGCAt.Add(s.gcCfg.Schedule)
	s.gcMu.Unlock()

	if s.metricsObs != nil {
		status := "success"
		if runErr != nil {
			status = "error"
		}
		s.metricsObs.GCRunTotal.WithLabelValues(status).Inc()
	}
}

// invalidateCache clears the stats cache so the next Stats() re-probes.
func (s *CacheStatsService) invalidateCache() {
	s.mu.Lock()
	s.cached = nil
	s.cachedAt = time.Time{}
	s.mu.Unlock()
}

// StartGCSweeper launches the background ticker goroutine and returns a stop
// func. No-op when gcCfg.Enabled is false.
func (s *CacheStatsService) StartGCSweeper(ctx context.Context) (stop func()) {
	if !s.gcCfg.Enabled {
		return func() {}
	}
	if s.gcCfg.Schedule <= 0 {
		s.logger.Warn("cache gc schedule is non-positive; sweeper disabled")
		return func() {}
	}
	ticker := time.NewTicker(s.gcCfg.Schedule)
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if _, err := s.RunGC(ctx); err != nil {
					s.logger.WithError(err).Error("cache gc run failed")
				}
			}
		}
	}()
	return func() { close(done) }
}
