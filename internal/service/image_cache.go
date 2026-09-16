package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	// Per-mirror operation budgets. The client applies its own 10s per-request
	// timeout; these bound the whole mirror walk so a slow/hung mirror cannot
	// stall the admin request indefinitely (partial results are returned).
	imageCacheListTimeout     = 30 * time.Second
	imageCachePruneTimeout    = 30 * time.Second
	imageCachePruneAllTimeout = 10 * time.Minute

	// Enumeration caps bound the per-mirror walk the way a registry's
	// catalog max-entries setting would; truncation is noted in the result.
	imageCacheRepoCap = 1000
	imageCacheTagCap  = 1000

	// imageCachePruneConcurrency bounds concurrent manifest deletes per mirror.
	imageCachePruneConcurrency = 4
)

// ImageCacheService lists and prunes the local image mirrors over their OCI
// Distribution v2 API. The mirror client is constructed by an injected factory
// so the service layer stays free of the repository import.
type ImageCacheService struct {
	mirrors   []domain.ImageCacheMirror
	byID      map[string]domain.ImageCacheMirror
	newClient func(addr string) domain.DistributionClient
	logger    *logrus.Logger
}

var _ domain.ImageCacheService = (*ImageCacheService)(nil)

// NewImageCacheService builds the image-cache service. newClient is wired to
// repository.NewDistributionClient in main; mirrors come from
// image_cache.mirrors (chart-rendered).
func NewImageCacheService(
	mirrors []domain.ImageCacheMirror,
	newClient func(addr string) domain.DistributionClient,
	logger *logrus.Logger,
) *ImageCacheService {
	byID := make(map[string]domain.ImageCacheMirror, len(mirrors))
	for _, m := range mirrors {
		byID[m.ID] = m
	}
	return &ImageCacheService{mirrors: mirrors, byID: byID, newClient: newClient, logger: logger}
}

// List probes every mirror and enumerates its repositories/tags/manifests.
// Errors are per-mirror (never a hard failure): an unreachable or
// catalog-disabled mirror yields reachable:false + an Error string in the 200
// body, and an empty mirror yields reachable:true with no repositories.
func (s *ImageCacheService) List(ctx context.Context) (*domain.ImageCacheInfo, error) {
	info := &domain.ImageCacheInfo{
		Mirrors:     make([]domain.ImageCacheMirrorInfo, len(s.mirrors)),
		CollectedAt: rfc3339(time.Now()),
	}
	// Mirrors are probed concurrently (bounded by the configured mirror
	// count): each walk carries its own 30s budget, so the whole list
	// returns in roughly one budget instead of mirrors×30s. Each goroutine
	// writes only its own slot.
	var wg sync.WaitGroup
	for i := range s.mirrors {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			info.Mirrors[i] = s.listMirror(ctx, &s.mirrors[i])
		}(i)
	}
	wg.Wait()
	return info, nil
}

func (s *ImageCacheService) listMirror(ctx context.Context, m *domain.ImageCacheMirror) domain.ImageCacheMirrorInfo {
	out := domain.ImageCacheMirrorInfo{
		ID:           m.ID,
		Host:         m.Host,
		Upstream:     m.Upstream,
		Backend:      m.Backend,
		Repositories: []domain.ImageCacheRepository{},
	}
	c, opCtx, cancel, err := s.dialMirror(ctx, m, imageCacheListTimeout)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer cancel()
	out.Reachable = true

	repos, err := c.Catalog(opCtx)
	if err != nil {
		out.Error = err.Error()
		s.logger.WithError(err).WithField("mirror", m.ID).Warn("image cache catalog failed")
		return out
	}
	if len(repos) > imageCacheRepoCap {
		repos = repos[:imageCacheRepoCap]
		out.Error = mergeNote(out.Error, truncationNote("repository", imageCacheRepoCap))
	}

	for _, repo := range repos {
		if opCtx.Err() != nil {
			out.Error = mergeNote(out.Error, "listing cancelled before completion")
			break
		}
		tags, err := c.Tags(opCtx, repo)
		if err != nil {
			out.Error = mergeNote(out.Error, fmt.Sprintf("tags %s: %v", repo, err))
			continue
		}
		if len(tags) > imageCacheTagCap {
			tags = tags[:imageCacheTagCap]
			out.Error = mergeNote(out.Error, truncationNote("tag", imageCacheTagCap))
		}
		repoOut := domain.ImageCacheRepository{
			Repository: repo,
			Tags:       make([]domain.ImageCacheTag, 0, len(tags)),
		}
		for _, tag := range tags {
			digest, size, layers, err := c.ManifestSize(opCtx, repo, tag)
			if err != nil {
				out.Error = mergeNote(out.Error, fmt.Sprintf("manifest %s:%s: %v", repo, tag, err))
				continue
			}
			repoOut.Tags = append(repoOut.Tags, domain.ImageCacheTag{
				Tag:        tag,
				Digest:     digest,
				SizeBytes:  size,
				LayerCount: layers,
			})
		}
		if len(repoOut.Tags) > 0 {
			out.Repositories = append(out.Repositories, repoOut)
		}
	}
	return out
}

// Prune deletes the requested refs from one mirror. The tag→digest step uses
// ManifestSize; a supplied digest is validated and used directly. Failures are
// per-item (the request stays valid), except a delete-disabled mirror, which is
// a mirror-wide condition and is surfaced so the handler can answer 409.
func (s *ImageCacheService) Prune(ctx context.Context, mirrorID string, refs []domain.ImageCachePruneRef) (*domain.ImageCachePruneResult, error) {
	m, ok := s.byID[mirrorID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrImageCacheMirrorNotFound, mirrorID)
	}
	if err := validateImageCacheRefs(refs); err != nil {
		return nil, err
	}

	c, opCtx, cancel, err := s.dialMirror(ctx, &m, imageCachePruneTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrImageCacheUnreachable, err)
	}
	defer cancel()

	result := &domain.ImageCachePruneResult{
		MirrorID: mirrorID,
		Items:    make([]domain.ImageCachePruneItem, 0, len(refs)),
	}
	deleteDisabled := false
	for _, o := range s.pruneRefs(opCtx, c, refs) {
		result.Items = append(result.Items, o.item)
		if o.item.Pruned {
			result.Pruned++
		}
		if o.item.Error != "" {
			result.Errors++
		}
		if errors.Is(o.err, domain.ErrRegistryDeleteDisabled) {
			deleteDisabled = true
		}
	}
	if deleteDisabled {
		return nil, domain.ErrRegistryDeleteDisabled
	}
	result.Message = pruneResultMessage(result)
	return result, nil
}

// PruneAll enumerates and deletes every manifest reachable by tag on the
// selected mirror(s). mirrorID empty = all mirrors. It issues only OCI
// Distribution v2 API calls against the mirror Service — no workload scaling,
// PVC deletion, storage wipe, or rollout restart.
func (s *ImageCacheService) PruneAll(ctx context.Context, mirrorID string) (*domain.ImageCachePruneAllResult, error) {
	mirrors, err := s.selectMirrors(mirrorID)
	if err != nil {
		return nil, err
	}
	result := &domain.ImageCachePruneAllResult{
		Mirrors: make([]domain.ImageCachePruneAllMirror, 0, len(mirrors)),
	}
	for i := range mirrors {
		result.Mirrors = append(result.Mirrors, s.pruneAllMirror(ctx, &mirrors[i]))
	}
	result.Message = pruneAllResultMessage(result)
	return result, nil
}

// selectMirrors resolves the requested mirror set. An empty id means every
// configured mirror.
func (s *ImageCacheService) selectMirrors(mirrorID string) ([]domain.ImageCacheMirror, error) {
	if mirrorID == "" {
		return s.mirrors, nil
	}
	m, ok := s.byID[mirrorID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrImageCacheMirrorNotFound, mirrorID)
	}
	return []domain.ImageCacheMirror{m}, nil
}

func (s *ImageCacheService) pruneAllMirror(ctx context.Context, m *domain.ImageCacheMirror) domain.ImageCachePruneAllMirror {
	out := domain.ImageCachePruneAllMirror{MirrorID: m.ID}
	c, opCtx, cancel, err := s.dialMirror(ctx, m, imageCachePruneAllTimeout)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer cancel()

	repos, err := c.Catalog(opCtx)
	if err != nil {
		out.Error = err.Error()
		s.logger.WithError(err).WithField("mirror", m.ID).Warn("image cache prune-all catalog failed")
		return out
	}
	truncated := false
	if len(repos) > imageCacheRepoCap {
		repos = repos[:imageCacheRepoCap]
		truncated = true
	}

	var refs []domain.ImageCachePruneRef
	for _, repo := range repos {
		if opCtx.Err() != nil {
			truncated = true
			break
		}
		tags, err := c.Tags(opCtx, repo)
		if err != nil {
			out.Errors++
			out.Failed = append(out.Failed, domain.ImageCachePruneAllItem{Repository: repo, Error: err.Error()})
			continue
		}
		out.RepositoriesProcessed++
		if len(tags) > imageCacheTagCap {
			tags = tags[:imageCacheTagCap]
			truncated = true
		}
		for _, tag := range tags {
			refs = append(refs, domain.ImageCachePruneRef{Repository: repo, Tag: tag})
		}
	}

	for _, o := range s.pruneRefs(opCtx, c, refs) {
		if o.item.Pruned {
			out.ManifestsPruned++
			continue
		}
		if o.item.Error != "" {
			out.Errors++
			out.Failed = append(out.Failed, domain.ImageCachePruneAllItem{
				Repository: o.item.Repository,
				Tag:        o.item.Tag,
				Digest:     o.item.Digest,
				Error:      o.item.Error,
			})
		}
		if errors.Is(o.err, domain.ErrRegistryDeleteDisabled) {
			out.Error = domain.ErrRegistryDeleteDisabled.Error()
		}
	}
	if opCtx.Err() != nil {
		truncated = true
	}
	out.Message = pruneAllMirrorMessage(&out, truncated)
	return out
}

// imageCachePruneOutcome is one prune attempt: its API item plus the raw error
// (used to detect mirror-wide conditions like delete-disabled).
type imageCachePruneOutcome struct {
	item domain.ImageCachePruneItem
	err  error
}

// pruneRefs runs pruneRef over refs with a fixed worker pool, preserving
// input order (each worker writes only its own outcome slot). A pool — rather
// than a goroutine per ref — keeps memory flat: a prune-all at the 1000×1000
// enumeration caps yields a million refs, and a million parked goroutines
// would exceed the supervisor's 1Gi limit (CWE-400).
func (s *ImageCacheService) pruneRefs(ctx context.Context, c domain.DistributionClient, refs []domain.ImageCachePruneRef) []imageCachePruneOutcome {
	outcomes := make([]imageCachePruneOutcome, len(refs))
	indices := make(chan int)
	var wg sync.WaitGroup
	wg.Add(imageCachePruneConcurrency)
	for w := 0; w < imageCachePruneConcurrency; w++ {
		go func() {
			defer wg.Done()
			for i := range indices {
				outcomes[i] = s.pruneRef(ctx, c, refs[i])
			}
		}()
	}
	for i := range refs {
		indices <- i
	}
	close(indices)
	wg.Wait()
	return outcomes
}

// pruneRef resolves a ref's digest (tag → ManifestSize, or a validated digest)
// and deletes the manifest. A manifest already absent counts as already pruned
// (idempotent), not an error.
func (s *ImageCacheService) pruneRef(ctx context.Context, c domain.DistributionClient, ref domain.ImageCachePruneRef) imageCachePruneOutcome {
	item := domain.ImageCachePruneItem{Repository: ref.Repository, Tag: ref.Tag, Digest: ref.Digest}

	digest := ref.Digest
	if digest == "" {
		resolved, _, _, err := c.ManifestSize(ctx, ref.Repository, ref.Tag)
		if err != nil {
			if errors.Is(err, domain.ErrManifestNotFound) {
				item.Pruned = true
				return imageCachePruneOutcome{item: item}
			}
			item.Error = err.Error()
			return imageCachePruneOutcome{item: item, err: err}
		}
		digest = resolved
		item.Digest = resolved
	}

	if err := c.DeleteManifest(ctx, ref.Repository, digest); err != nil {
		if errors.Is(err, domain.ErrManifestNotFound) {
			item.Pruned = true
			return imageCachePruneOutcome{item: item}
		}
		item.Error = err.Error()
		return imageCachePruneOutcome{item: item, err: err}
	}
	item.Pruned = true
	return imageCachePruneOutcome{item: item}
}

// dialMirror builds the client for m and pings it under a fresh per-mirror
// timeout. The returned cancel must always be deferred by the caller.
func (s *ImageCacheService) dialMirror(ctx context.Context, m *domain.ImageCacheMirror, timeout time.Duration) (domain.DistributionClient, context.Context, context.CancelFunc, error) {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	if s.newClient == nil {
		return nil, opCtx, cancel, errors.New("image cache client not configured")
	}
	c := s.newClient(m.InternalAddr)
	if err := c.Ping(opCtx); err != nil {
		return nil, opCtx, cancel, err
	}
	return c, opCtx, cancel, nil
}

// validateImageCacheRefs rejects a malformed prune request (mapped to 400).
func validateImageCacheRefs(refs []domain.ImageCachePruneRef) error {
	if len(refs) == 0 {
		return fmt.Errorf("%w: at least one ref is required", domain.ErrImageCacheInvalidRef)
	}
	for i, ref := range refs {
		if strings.TrimSpace(ref.Repository) == "" {
			return fmt.Errorf("%w: refs[%d].repository must not be empty", domain.ErrImageCacheInvalidRef, i)
		}
		switch {
		case ref.Tag == "" && ref.Digest == "":
			return fmt.Errorf("%w: refs[%d] must set tag or digest", domain.ErrImageCacheInvalidRef, i)
		case ref.Tag != "" && ref.Digest != "":
			return fmt.Errorf("%w: refs[%d] must not set both tag and digest", domain.ErrImageCacheInvalidRef, i)
		case ref.Digest != "" && !domain.ValidDigest(ref.Digest):
			return fmt.Errorf("%w: refs[%d].digest must be sha256:<hex>", domain.ErrImageCacheInvalidRef, i)
		}
	}
	return nil
}

func truncationNote(kind string, limit int) string {
	return fmt.Sprintf("%s cap (%d) reached; results truncated", kind, limit)
}

// mergeNote appends note to existing with a separator, keeping the first
// (root-cause) error first.
func mergeNote(existing, note string) string {
	if existing == "" {
		return note
	}
	return fmt.Sprintf("%s; %s", existing, note)
}

func pruneResultMessage(r *domain.ImageCachePruneResult) string {
	switch {
	case r.Errors > 0:
		return fmt.Sprintf("pruned %d of %d refs; %d failed", r.Pruned, len(r.Items), r.Errors)
	case r.Pruned > 0:
		return "manifests unlinked; blob bytes are reclaimed automatically by Zot GC after gcDelay"
	default:
		return ""
	}
}

func pruneAllMirrorMessage(out *domain.ImageCachePruneAllMirror, truncated bool) string {
	var notes []string
	if truncated {
		notes = append(notes, "repository/tag cap reached; enumeration truncated")
	}
	if out.Error == "" && out.ManifestsPruned > 0 {
		notes = append(notes, "untagged/orphaned manifests are not enumerable by tag; Zot GC reclaims their bytes automatically")
	}
	return strings.Join(notes, "; ")
}

func pruneAllResultMessage(r *domain.ImageCachePruneAllResult) string {
	total, errs := 0, 0
	for _, m := range r.Mirrors {
		total += m.ManifestsPruned
		errs += m.Errors
	}
	switch {
	case errs > 0:
		return fmt.Sprintf("pruned %d manifests; %d errors", total, errs)
	case total > 0:
		return fmt.Sprintf("pruned %d manifests; blob bytes are reclaimed automatically by Zot GC after gcDelay", total)
	default:
		return ""
	}
}
