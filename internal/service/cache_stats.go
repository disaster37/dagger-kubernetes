package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

const (
	cacheStatsTTL    = 15 * time.Second
	cacheProbeBudget = 30 * time.Second
	maxCacheVersions = 200
	maxPurgeAllTags  = 1000

	s3UnsupportedMessage = "s3 cache stats not supported in this release"
	registryDownMessage  = "registry unreachable"
	catalogDisabledMsg   = "catalog disabled"
	truncatedMsg         = "truncated"
)

var tagRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// CacheStatsService implements domain.CacheStatsProvider + domain.CachePurger.
// It probes the OCI registry (catalog/tags/manifests), VictoriaMetrics hit
// counters, and the fleet to assemble the rich cache payload, and it owns the
// manual purge + background GC sweeper.
type CacheStatsService struct {
	cache      *Cache
	registry   *repository.RegistryStatsClient
	metrics    *repository.MetricsClient // may be nil
	fleet      domain.FleetProvider      // may be nil
	gcCfg      domain.GCConfig
	logger     *logrus.Logger
	metricsObs *observ.Metrics // may be nil

	mu       sync.Mutex
	cached   *domain.CacheStats
	cachedAt time.Time

	purgeMu  sync.Mutex // serializes purge / purge-all / GC
	gcMu     sync.Mutex // guards lastGC / lastGCAt / nextGCAt (short critical section)
	lastGC   *domain.GCRunSummary
	lastGCAt time.Time
	nextGCAt time.Time
}

func NewCacheStatsService(
	cache *Cache,
	registry *repository.RegistryStatsClient,
	metricsClient *repository.MetricsClient,
	fleet domain.FleetProvider,
	gcCfg domain.GCConfig,
	logger *logrus.Logger,
	obs *observ.Metrics,
) *CacheStatsService {
	return &CacheStatsService{
		cache:      cache,
		registry:   registry,
		metrics:    metricsClient,
		fleet:      fleet,
		gcCfg:      gcCfg,
		logger:     logger,
		metricsObs: obs,
	}
}

// cacheEntry is one manifest discovered in the registry catalog.
type cacheEntry struct {
	repo   string
	tag    string
	digest string
	size   int64
	layers int64
}

func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// registryHost returns the host portion of cache.Registry ("cache.reg" for
// "cache.reg/dagger-cache").
func (s *CacheStatsService) registryHost() string {
	host, _, ok := strings.Cut(s.cache.Registry, "/")
	if !ok {
		return s.cache.Registry
	}
	return host
}

// repo returns the repository portion of cache.Registry ("dagger-cache" for
// "cache.reg/dagger-cache").
func (s *CacheStatsService) repo() string {
	_, rest, ok := strings.Cut(s.cache.Registry, "/")
	if !ok {
		return s.cache.Registry
	}
	return rest
}

// parseVersionTag reverses a version slug into a *Version ("v0-21-4" ->
// "v0.21.4"). ok is false for non-version tags.
func parseVersionTag(tag string) (*domain.Version, bool) {
	v, err := domain.Parse(strings.ReplaceAll(tag, "-", "."))
	if err != nil {
		return nil, false
	}
	return v, true
}

// activeVersions returns the set of versions with active (ready) fleet
// replicas. unknown=true means the fleet state could not be determined, in
// which case callers must treat every version as protected (fail safe).
func (s *CacheStatsService) activeVersions() (active map[string]bool, unknown bool) {
	if s.fleet == nil {
		return nil, false
	}
	versions, err := s.fleet.AllVersions()
	if err != nil {
		return nil, true
	}
	active = make(map[string]bool)
	for _, v := range versions {
		replicas, err := s.fleet.GetReplicas(v)
		if err != nil {
			continue
		}
		for _, r := range replicas {
			if r.Ready {
				active[v] = true
				break
			}
		}
	}
	return active, false
}

// isProtected reports whether the version must not be auto-purged.
func isProtected(version string, active map[string]bool, unknown bool) bool {
	if unknown {
		return true
	}
	return active[version]
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
		Versions:    []domain.CacheVersionRef{},
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

	if s.registry == nil {
		stats.Message = registryDownMessage
		return stats
	}

	if err := s.registry.Ping(ctx); err != nil {
		stats.Message = registryDownMessage
		return stats
	}
	stats.Running = true
	stats.Reachable = true

	repos, err := s.registry.Catalog(ctx)
	if err != nil {
		if errors.Is(err, repository.ErrRegistryCatalogDisabled) {
			stats.Message = catalogDisabledMsg
			return stats
		}
		stats.Message = registryDownMessage
		return stats
	}

	entries, timedOut := s.collectEntries(ctx, repos)

	active, unknown := s.activeVersions()

	var totalSize int64
	var objectCount int64
	refs := make([]domain.CacheVersionRef, 0, len(entries))
	for _, e := range entries {
		if e.size >= 0 {
			totalSize += e.size
		}
		objectCount += e.layers

		v, ok := parseVersionTag(e.tag)
		if !ok {
			continue
		}
		refs = append(refs, domain.CacheVersionRef{
			Version:    v.String(),
			Tag:        e.tag,
			Ref:        fmt.Sprintf("%s/%s:%s", s.registryHost(), e.repo, e.tag),
			Size:       e.size,
			LayerCount: e.layers,
			Digest:     e.digest,
			Protected:  isProtected(v.String(), active, unknown),
		})
	}

	sort.SliceStable(refs, func(i, j int) bool {
		vi, oki := domain.Parse(refs[i].Version)
		vj, okj := domain.Parse(refs[j].Version)
		if oki != nil || okj != nil {
			return oki == nil // parseable versions first
		}
		return vi.Compare(vj) > 0
	})

	truncated := false
	if len(refs) > maxCacheVersions {
		refs = refs[:maxCacheVersions]
		truncated = true
	}

	stats.TotalSize = totalSize
	stats.ObjectCount = objectCount
	stats.Versions = refs

	s.attachHitRate(ctx, stats)

	if timedOut {
		stats.Message = "partial: probe timed out"
	} else if truncated {
		stats.Message = truncatedMsg
	}
	return stats
}

// collectEntries walks every repo's tags and collects manifest metadata.
// Returns entries plus a timedOut flag for partial (context-cancelled) walks.
func (s *CacheStatsService) collectEntries(ctx context.Context, repos []string) ([]cacheEntry, bool) {
	var out []cacheEntry
	for _, repo := range repos {
		if ctx.Err() != nil {
			return out, true
		}
		tags, err := s.registry.Tags(ctx, repo)
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
			digest, size, layers, err := s.registry.ManifestSize(ctx, repo, tag)
			if err != nil {
				if errors.Is(err, repository.ErrManifestNotFound) {
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
		Enabled:               s.gcCfg.Enabled,
		MaxAge:                s.gcCfg.MaxAge.String(),
		Schedule:              s.gcCfg.Schedule.String(),
		MinRefsToKeep:         s.gcCfg.MinRefsToKeep,
		ProtectActiveVersions: s.gcCfg.ProtectActiveVersions,
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

// Purge implements domain.CachePurger. It validates the version, derives the
// tag, deletes the manifest, and is idempotent: a missing tag counts as
// already_purged (no error).
func (s *CacheStatsService) Purge(ctx context.Context, req domain.PurgeRequest) (*domain.PurgeResult, error) {
	parsed, err := domain.Parse(req.Version)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid version", domain.ErrValidation)
	}
	tag := req.Tag
	if tag == "" {
		tag = parsed.Slug()
	} else if !tagRe.MatchString(tag) {
		return nil, fmt.Errorf("%w: invalid tag", domain.ErrValidation)
	}

	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	if s.registry == nil {
		return nil, fmt.Errorf("registry not configured")
	}

	repo := s.repo()
	digest, size, _, err := s.registry.ManifestSize(ctx, repo, tag)
	if errors.Is(err, repository.ErrManifestNotFound) {
		return &domain.PurgeResult{
			AlreadyPurged: 1,
			Versions:      []string{parsed.String()},
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("manifest lookup: %w", err)
	}

	if err := s.registry.DeleteManifest(ctx, repo, digest); err != nil {
		if errors.Is(err, repository.ErrManifestNotFound) {
			return &domain.PurgeResult{
				AlreadyPurged: 1,
				Versions:      []string{parsed.String()},
			}, nil
		}
		return nil, fmt.Errorf("delete manifest: %w", err)
	}

	s.invalidateCache()
	if s.metricsObs != nil {
		s.metricsObs.CachePurgeTotal.Inc()
	}
	return &domain.PurgeResult{
		Purged:     1,
		FreedBytes: size,
		Versions:   []string{parsed.String()},
	}, nil
}

// PurgeAll implements domain.CachePurger. It purges every tag in every catalog
// repo, capped at maxPurgeAllTags entries.
func (s *CacheStatsService) PurgeAll(ctx context.Context) (*domain.PurgeResult, error) {
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()

	if s.registry == nil {
		return nil, fmt.Errorf("registry not configured")
	}

	repos, err := s.registry.Catalog(ctx)
	if err != nil {
		if errors.Is(err, repository.ErrRegistryCatalogDisabled) {
			return nil, fmt.Errorf("%w", domain.ErrRegistryDeleteDisabled)
		}
		return nil, fmt.Errorf("catalog: %w", err)
	}

	result := &domain.PurgeResult{}
	truncated := false
	for _, repo := range repos {
		tags, err := s.registry.Tags(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("tags: %w", err)
		}
		for _, tag := range tags {
			if result.Purged+result.AlreadyPurged >= maxPurgeAllTags {
				truncated = true
				break
			}
			digest, size, _, err := s.registry.ManifestSize(ctx, repo, tag)
			if errors.Is(err, repository.ErrManifestNotFound) {
				result.AlreadyPurged++
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("manifest lookup: %w", err)
			}
			if err := s.registry.DeleteManifest(ctx, repo, digest); err != nil {
				if errors.Is(err, repository.ErrManifestNotFound) {
					result.AlreadyPurged++
					continue
				}
				return nil, fmt.Errorf("delete manifest: %w", err)
			}
			result.Purged++
			result.FreedBytes += size
			if s.metricsObs != nil {
				s.metricsObs.CachePurgeTotal.Inc()
			}
		}
		if truncated {
			break
		}
	}

	if truncated {
		result.Message = "truncated at 1000 tags"
	}
	s.invalidateCache()
	return result, nil
}

// RunGC is the GC sweeper entry point. It purges tags older than MaxAge that
// are not protected, always keeping the newest MinRefsToKeep tags per minor
// version line.
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

	if s.registry == nil {
		return finish("registry not configured", fmt.Errorf("registry not configured"))
	}

	probeCtx, cancel := context.WithTimeout(ctx, cacheProbeBudget)
	defer cancel()

	repos, err := s.registry.Catalog(probeCtx)
	if err != nil {
		if errors.Is(err, repository.ErrRegistryCatalogDisabled) {
			return finish("catalog disabled", err)
		}
		return finish("catalog failed", err)
	}

	entries, _ := s.collectEntries(probeCtx, repos)

	active, unknown := s.activeVersions()

	// Group parseable version tags by minor key, newest first.
	groups := map[string][]cacheEntry{}
	for _, e := range entries {
		v, ok := parseVersionTag(e.tag)
		if !ok {
			continue
		}
		groups[v.MinorKey()] = append(groups[v.MinorKey()], e)
	}

	for _, group := range groups {
		sort.SliceStable(group, func(i, j int) bool {
			vi, oki := parseVersionTag(group[i].tag)
			vj, okj := parseVersionTag(group[j].tag)
			if !oki || !okj {
				return oki
			}
			return vi.Compare(vj) > 0
		})

		keep := s.gcCfg.MinRefsToKeep
		if keep < 0 {
			keep = 0
		}
		for idx, e := range group {
			if idx < keep {
				continue // keep newest min_refs_to_keep
			}
			v, _ := parseVersionTag(e.tag)
			ver := v.String()
			if s.gcCfg.ProtectActiveVersions && isProtected(ver, active, unknown) {
				summary.Skipped++
				continue
			}
			created, err := s.registry.ManifestCreated(probeCtx, e.repo, e.tag)
			if err != nil || created.IsZero() {
				// Unknown age: never purge (conservative).
				summary.Skipped++
				continue
			}
			if time.Since(created) < s.gcCfg.MaxAge {
				summary.Skipped++
				continue
			}
			if err := s.registry.DeleteManifest(probeCtx, e.repo, e.digest); err != nil {
				if errors.Is(err, domain.ErrRegistryDeleteDisabled) {
					return finish("registry delete not enabled", err)
				}
				summary.Errors++
				continue
			}
			summary.PurgedTags++
			summary.FreedBytes += e.size
			if s.metricsObs != nil {
				s.metricsObs.CachePurgeTotal.Inc()
			}
		}
	}

	s.invalidateCache()
	return finish("", nil)
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
