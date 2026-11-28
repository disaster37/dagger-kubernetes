package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// CacheRootRef is one engine cache entry the service learned from telemetry: a
// result number, the recipe digest a later lookup names, and the engine cache
// identity that owns the number.
type CacheRootRef struct {
	CacheID string
	Number  uint64
	Recipe  string
}

// maxCacheRootsPerEngine bounds the telemetry-derived root list kept per engine
// cache identity, so a long-lived engine cannot grow it without limit.
const maxCacheRootsPerEngine = 20000

// exportRootsPerRequest bounds the roots asked for in one export.
const exportRootsPerRequest = 64

// RemoteCacheService implements the read-through remote cache service
// (ADR-055). It is stateless apart from the local bbolt index, the S3 store and
// the in-memory telemetry-derived root list.
type RemoteCacheService struct {
	index              *repository.BboltCacheIndex
	store              *repository.S3RemoteCacheStore
	metrics            *observ.Metrics
	logger             *logrus.Logger
	ttl                time.Duration
	maxBytes           int64
	packMaxMemberBytes int64
	gcInterval         time.Duration
	exportSem          *semaphore.Weighted
	quota              *QuotaService // nil = per-org cache_bytes quota not enforced

	rootsMu sync.Mutex
	roots   map[string]*cacheRootSet
}

// cacheRootSet is the root list learned for one engine cache identity, in
// arrival order, deduplicated by result number.
type cacheRootSet struct {
	order []CacheRootRef
	seen  map[uint64]string
}

// NewRemoteCacheService returns a RemoteCacheService.
func NewRemoteCacheService(index *repository.BboltCacheIndex, store *repository.S3RemoteCacheStore, ttl time.Duration, maxBytes int64, workers int, packMaxMemberBytes int64, gcInterval time.Duration, metrics *observ.Metrics, logger *logrus.Logger) *RemoteCacheService {
	if workers <= 0 {
		workers = 16
	}
	if packMaxMemberBytes <= 0 {
		packMaxMemberBytes = domain.DefaultPackMaxMemberBytes
	}
	if gcInterval <= 0 {
		gcInterval = 5 * time.Minute
	}
	return &RemoteCacheService{
		index: index, store: store, ttl: ttl, maxBytes: maxBytes,
		packMaxMemberBytes: packMaxMemberBytes, gcInterval: gcInterval,
		exportSem: semaphore.NewWeighted(int64(workers)), metrics: metrics, logger: logger,
		roots: make(map[string]*cacheRootSet),
	}
}

// WithCacheQuota wires per-org cache_bytes quota enforcement into the remote
// cache write path and GC. A nil quota service disables per-org enforcement
// (the configured global maximum still applies via GC).
func (s *RemoteCacheService) WithCacheQuota(q *QuotaService) *RemoteCacheService {
	s.quota = q
	return s
}

// NewConn returns a per-connection protocol state machine.
func (s *RemoteCacheService) NewConn(namespace string, readOnly bool, send func([]byte) error) *RemoteCacheConn {
	return newRemoteCacheConn(s, namespace, readOnly, send)
}

// RecordCacheRoots records the engine cache entries observed in telemetry. The
// service exports these roots after a settle, which is how values reach the
// cache without the engine sending them unprompted.
func (s *RemoteCacheService) RecordCacheRoots(refs []CacheRootRef) {
	if len(refs) == 0 {
		return
	}
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	for _, ref := range refs {
		if ref.CacheID == "" || ref.Number == 0 || ref.Recipe == "" {
			continue
		}
		set := s.roots[ref.CacheID]
		if set == nil {
			set = &cacheRootSet{seen: make(map[uint64]string)}
			s.roots[ref.CacheID] = set
		}
		if recipe, ok := set.seen[ref.Number]; ok {
			if recipe == ref.Recipe {
				continue
			}
			// The number now names another recipe: replace the stale entry.
			set.seen[ref.Number] = ref.Recipe
			for i := range set.order {
				if set.order[i].Number == ref.Number {
					set.order[i].Recipe = ref.Recipe
					break
				}
			}
			continue
		}
		if len(set.order) >= maxCacheRootsPerEngine {
			continue
		}
		set.seen[ref.Number] = ref.Recipe
		set.order = append(set.order, ref)
	}
}

// cacheRoots returns the roots learned for cacheID, in arrival order.
func (s *RemoteCacheService) cacheRoots(cacheID string) []CacheRootRef {
	if cacheID == "" {
		return nil
	}
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	set := s.roots[cacheID]
	if set == nil {
		return nil
	}
	out := make([]CacheRootRef, len(set.order))
	copy(out, set.order)
	return out
}

// forgetCacheRoots drops the exported roots from the cacheID's list.
func (s *RemoteCacheService) forgetCacheRoots(cacheID string, numbers []uint64) {
	if cacheID == "" || len(numbers) == 0 {
		return
	}
	drop := make(map[uint64]bool, len(numbers))
	for _, n := range numbers {
		drop[n] = true
	}
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	set := s.roots[cacheID]
	if set == nil {
		return
	}
	kept := set.order[:0]
	for _, ref := range set.order {
		if drop[ref.Number] {
			delete(set.seen, ref.Number)
			continue
		}
		kept = append(kept, ref)
	}
	set.order = kept
	if len(set.order) == 0 {
		delete(s.roots, cacheID)
	}
}

// cacheQuota resolves the effective soft/hard cache_bytes threshold for a
// namespace. When the namespace is not a resolvable org (e.g. "_shared" in
// deprecated single-org shared mode) or the org has no hard cap, Hard falls
// back to the configured global remote-cache maximum.
func (s *RemoteCacheService) cacheQuota(ctx context.Context, namespace string) domain.QuotaThreshold {
	th := domain.QuotaThreshold{}
	if s.quota != nil {
		if eff, err := s.quota.EffectiveQuotas(ctx, namespace); err == nil {
			th = eff.MaxCacheBytes
		}
	}
	if th.Hard <= 0 {
		th.Hard = s.maxBytes
	}
	return th
}

// cacheBytes returns the total bytes currently indexed for namespace.
func (s *RemoteCacheService) cacheBytes(namespace string) (int64, error) {
	_, size, err := s.index.Stats(namespace)
	return size, err
}

// storeBundle stores one opaque dagql ValueBundle and indexes it under its own
// content digest plus each recipe digest that led to it.
func (s *RemoteCacheService) storeBundle(ctx context.Context, namespace string, recipes []string, bundle []byte) (*repository.CacheIndexEntry, error) {
	if err := s.enforceCacheWriteQuota(ctx, namespace, int64(len(bundle))); err != nil {
		return nil, err
	}
	digest := bundleDigest(bundle)
	key, err := s.store.PutBundle(ctx, namespace, digest, bundle)
	if err != nil {
		return nil, err
	}
	entry := &repository.CacheIndexEntry{
		Digest: digest, BundleKey: key, Size: int64(len(bundle)),
		CreatedAt: time.Now().UTC(), LastAccess: time.Now().UTC(),
	}
	if err := s.index.Put(namespace, entry); err != nil {
		return nil, err
	}
	for _, recipe := range recipes {
		if recipe == "" || recipe == digest {
			continue
		}
		alias := *entry
		alias.Digest = recipe
		if err := s.index.Put(namespace, &alias); err != nil {
			return nil, err
		}
	}
	if s.metrics != nil {
		s.metrics.RemoteCacheExportedBundles.WithLabelValues(namespace).Inc()
		s.metrics.RemoteCacheStoredBytes.WithLabelValues(namespace).Set(float64(entry.Size))
	}
	return entry, nil
}

// bundleDigest returns the content digest of a stored bundle.
func bundleDigest(bundle []byte) string {
	sum := sha256.Sum256(bundle)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))
}

// handleUpload presigns a PUT for every layer blob the exported bundle carries.
// Small blobs are grouped into one dagger-pack v1 object (upstream format), the
// rest are standalone blobs. Member offsets are recorded so a later renew can
// hand out a range-addressable pack URL.
func (s *RemoteCacheService) handleUpload(ctx context.Context, namespace, exportID string, bundle json.RawMessage) (*domain.Upload, error) {
	layers := bundleLayers(bundle)
	upload := &domain.Upload{ExportID: exportID, URLs: make(map[string]string, len(layers)), Merged: true}
	var packMembers []domain.PackMember
	var packBytes int64
	addMember := func(layer domain.PackMember) bool {
		if len(packMembers) >= domain.MaxPackMembers || packBytes+layer.Size > domain.MaxPackBytes {
			return false
		}
		packMembers = append(packMembers, layer)
		packBytes += layer.Size
		return true
	}
	var standalone []domain.PackMember
	for _, layer := range layers {
		if layer.Size > 0 && layer.Size <= s.packMaxMemberBytes && addMember(layer) {
			continue
		}
		standalone = append(standalone, layer)
	}
	for _, blob := range standalone {
		url, err := s.store.PresignPut(ctx, namespace, blob.Digest)
		if err != nil {
			return nil, err
		}
		upload.URLs[blob.Digest] = url
	}
	if len(packMembers) > 0 {
		packUpload, err := s.storePresignPack(ctx, namespace, packMembers)
		if err != nil {
			return nil, err
		}
		upload.Packs = append(upload.Packs, *packUpload)
	}
	s.logger.WithFields(logrus.Fields{"namespace": namespace, "layers": len(layers), "blobs": len(upload.URLs), "packs": len(upload.Packs)}).Debug("remote cache: upload prepared")
	return upload, nil
}

// storePresignPack presigns one pack PUT and records its member offsets in the
// index so renew can serve the exact slice inside the pack.
func (s *RemoteCacheService) storePresignPack(ctx context.Context, namespace string, members []domain.PackMember) (*domain.UploadPack, error) {
	id, err := domain.BuildPackID(members)
	if err != nil {
		return nil, err
	}
	url, err := s.store.PresignPackPut(ctx, namespace, id.Digest)
	if err != nil {
		return nil, err
	}
	packKey := s.store.PackKey(namespace, id.Digest)
	headerLen := domain.PackHeaderLen(members)
	offset := headerLen
	wire := make([]domain.RCPackMember, 0, len(members))
	for _, member := range members {
		wire = append(wire, domain.RCPackMember{Digest: member.Digest, Size: member.Size})
		if err := s.index.Put(namespace, &repository.CacheIndexEntry{
			Digest: member.Digest, PackDigest: id.Digest, PackKey: packKey,
			PackOffset: offset, Size: member.Size,
		}); err != nil {
			return nil, err
		}
		offset += member.Size
	}
	return &domain.UploadPack{Pack: id.Digest, URL: url, Members: wire}, nil
}

// bundleLayers returns the unique sha256 layer blobs a dagql bundle names,
// whether it lists them once (ShareChainLayers) or carries each chain's own
// copy.
func bundleLayers(bundle json.RawMessage) []domain.PackMember {
	if len(bundle) == 0 {
		return nil
	}
	type wireBundle struct {
		Layers  []wireLayer  `json:"layers"`
		Outputs []wireOutput `json:"outputs"`
	}
	var parsed wireBundle
	if err := json.Unmarshal(bundle, &parsed); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []domain.PackMember
	add := func(items []wireLayer) {
		for _, item := range items {
			digest := item.Descriptor.Digest
			if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 || seen[digest] {
				continue
			}
			seen[digest] = true
			out = append(out, domain.PackMember{Digest: digest, Size: item.Descriptor.Size})
		}
	}
	add(parsed.Layers)
	for _, output := range parsed.Outputs {
		if output.Chain != nil {
			add(output.Chain.Layers)
		}
	}
	return out
}

// wireLayer is one layer descriptor a dagql bundle names.
type wireLayer struct {
	Descriptor struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"descriptor"`
}

// wireOutput is the subset of a dagql bundle output the service reads.
type wireOutput struct {
	Chain *struct {
		Layers []wireLayer `json:"layers"`
	} `json:"chain"`
}

// handleRenew returns a fresh presigned GET for the blob the engine needs. A
// blob stored inside a pack gets the pack object's presigned GET: the engine
// already knows the member offset and ranges inside it.
func (s *RemoteCacheService) handleRenew(ctx context.Context, namespace string, renew *domain.Renew) (*domain.Renewed, error) {
	renewed := &domain.Renewed{}
	if renew.NeededBlob == "" {
		renewed.Unavailable = true
		return renewed, nil
	}
	var url string
	var err error
	if entry, ok, getErr := s.index.Get(namespace, renew.NeededBlob); getErr == nil && ok && entry.PackKey != "" {
		url, _, err = s.store.PresignGetRange(ctx, entry.PackKey, entry.PackOffset, entry.Size)
	} else {
		url, err = s.store.PresignGet(ctx, namespace, renew.NeededBlob)
	}
	if err != nil {
		return nil, err
	}
	renewed.Addresses = map[string]domain.RCBlobAddress{
		renew.NeededBlob: {URL: url, ExpiresAtUnix: time.Now().Add(s.ttl).Unix()},
	}
	return renewed, nil
}

// lookupBundle resolves one lookup miss to a stored bundle. It returns the
// index entry and the bundle bytes, or ok=false when nothing matches.
func (s *RemoteCacheService) lookupBundle(ctx context.Context, namespace string, miss *domain.LookupMiss) ([]byte, bool) {
	candidates := make([]string, 0, 2+len(miss.Extra)+len(miss.Inputs))
	candidates = append(candidates, miss.Recipe, miss.Self)
	candidates = append(candidates, miss.Extra...)
	candidates = append(candidates, miss.Inputs...)
	for _, digest := range candidates {
		if digest == "" {
			continue
		}
		entry, ok, err := s.index.Get(namespace, digest)
		if err != nil || !ok || entry.BundleKey == "" {
			continue
		}
		bundle, err := s.store.GetBundleByKey(ctx, entry.BundleKey)
		if err != nil {
			continue
		}
		_ = s.index.Touch(namespace, digest, time.Now().UTC())
		if s.metrics != nil {
			s.metrics.RemoteCacheLookupHits.WithLabelValues(namespace).Inc()
		}
		return bundle, true
	}
	if s.metrics != nil {
		s.metrics.RemoteCacheLookupMisses.WithLabelValues(namespace).Inc()
	}
	return nil, false
}

// enforceCacheWriteQuota blocks a cache write that would push the namespace
// past its cache_bytes hard cap, after one synchronous GC to make room. Reads
// are never blocked. Soft-limit breaches admit the write and increment the
// org-quota soft-exceeded metric; a hard breach that survives GC is rejected.
func (s *RemoteCacheService) enforceCacheWriteQuota(ctx context.Context, namespace string, delta int64) error {
	th := s.cacheQuota(ctx, namespace)
	if th.Hard <= 0 {
		return nil
	}
	usage, err := s.cacheBytes(namespace)
	if err != nil {
		return err
	}
	if th.Hard > 0 && usage+delta > th.Hard {
		// One synchronous GC to make room, then re-read usage.
		if gcErr := s.GC(ctx, namespace); gcErr != nil {
			s.logger.WithError(gcErr).WithField("namespace", namespace).Warn("remote cache quota GC failed")
		}
		usage, err = s.cacheBytes(namespace)
		if err != nil {
			return err
		}
	}
	if s.quota != nil {
		_, evalErr := s.quota.evaluate(&domain.Identity{}, namespace, domain.QuotaCacheBytes, usage, delta, th)
		return evalErr
	}
	if th.Hard > 0 && usage+delta > th.Hard {
		return fmt.Errorf("organization quota exceeded: cache_bytes limit (%d) reached", th.Hard)
	}
	return nil
}

// IngestCacheEvents touches referenced digests and increments cache metrics.
func (s *RemoteCacheService) IngestCacheEvents(ctx context.Context, namespace string, events []CacheEvent) {
	now := time.Now().UTC()
	for i := range events {
		ev := &events[i]
		if ev.Key == "" {
			continue
		}
		_ = s.index.Touch(namespace, ev.Key, now)
		if s.metrics != nil {
			s.metrics.RemoteCacheLookupHits.WithLabelValues(namespace).Inc()
		}
	}
}

// Stats returns index statistics for a namespace.
func (s *RemoteCacheService) Stats(namespace string) (count int, size int64, err error) {
	return s.index.Stats(namespace)
}

// Purge removes every cached object and index entry in a namespace.
func (s *RemoteCacheService) Purge(ctx context.Context, namespace string) error {
	if err := s.store.DeleteNamespace(ctx, namespace); err != nil {
		return err
	}
	return s.index.DeleteNamespace(namespace)
}

// GC evicts expired/oversized entries and their S3 bundles. The oversized
// threshold is the namespace's effective cache_bytes hard cap (org quota,
// falling back to the configured global maximum).
func (s *RemoteCacheService) GC(ctx context.Context, namespace string) error {
	evicted, err := s.index.GC(namespace, s.ttl, s.cacheQuota(ctx, namespace).Hard, time.Now().UTC())
	if err != nil {
		return err
	}
	for i := range evicted {
		e := &evicted[i]
		if e.BundleKey == "" {
			continue
		}
		if err := s.store.DeleteBundle(ctx, namespace, e.Digest); err != nil {
			s.logger.WithError(err).WithField("digest", e.Digest).Warn("remote cache bundle delete failed")
		}
	}
	return nil
}

// Start hydrates the index from the S3 checkpoint and runs periodic GC.
func (s *RemoteCacheService) Start(ctx context.Context, isLeader func() bool) {
	if namespaces, err := s.index.Namespaces(); err == nil && len(namespaces) == 0 {
		if orgs, oErr := s.store.ListOrgPrefixes(ctx); oErr == nil {
			for _, orgID := range orgs {
				if data, gErr := s.store.GetIndexCheckpoint(ctx, orgID); gErr == nil && len(data) > 0 {
					if iErr := s.index.ImportSnapshot(orgID, data); iErr != nil {
						s.logger.WithError(iErr).WithField("org", orgID).Warn("remote cache index checkpoint import failed")
					}
				}
			}
		}
	}
	ticker := time.NewTicker(s.gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if isLeader != nil && !isLeader() {
				continue
			}
			s.runGC(ctx)
		}
	}
}

func (s *RemoteCacheService) runGC(ctx context.Context) {
	namespaces, err := s.index.Namespaces()
	if err != nil {
		return
	}
	for _, namespace := range namespaces {
		if err := s.GC(ctx, namespace); err != nil {
			s.logger.WithError(err).WithField("namespace", namespace).Warn("remote cache GC failed")
		}
	}
	for _, namespace := range namespaces {
		var buf bytes.Buffer
		if err := s.index.ExportSnapshot(&buf); err == nil {
			if err := s.store.PutIndexCheckpoint(ctx, namespace, buf.Bytes()); err != nil {
				s.logger.WithError(err).WithField("namespace", namespace).Warn("remote cache index checkpoint upload failed")
			}
		}
	}
}
