package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// RemoteCacheService implements the read-through remote cache protocol
// (ADR-055). It is stateless apart from the local bbolt index and S3 store.
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

	mu       sync.Mutex
	inflight map[string]time.Time
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
		inflight: make(map[string]time.Time),
	}
}

// WithCacheQuota wires per-org cache_bytes quota enforcement into the remote
// cache write path and GC. A nil quota service disables per-org enforcement
// (the configured global maximum still applies via GC).
func (s *RemoteCacheService) WithCacheQuota(q *QuotaService) *RemoteCacheService {
	s.quota = q
	return s
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

// Handle processes one protocol message and returns the response envelope.
//
//nolint:gocyclo // protocol message dispatch
func (s *RemoteCacheService) Handle(ctx context.Context, namespace string, readOnly bool, raw []byte) ([]byte, error) {
	var env domain.RemoteCacheEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return s.errorEnvelope("invalid envelope")
	}
	switch env.Type {
	case domain.RCTypeHello:
		var hello domain.Hello
		_ = json.Unmarshal(env.Data, &hello)
		return domain.NewRemoteCacheEnvelope(domain.RCTypeWelcome, domain.Welcome{Namespace: namespace, MaxBytes: s.cacheQuota(ctx, namespace).Hard})
	case domain.RCTypeOpen:
		var open domain.Open
		_ = json.Unmarshal(env.Data, &open)
		return domain.NewRemoteCacheEnvelope(domain.RCTypeOpened, domain.Opened{SessionID: open.SessionID})
	case domain.RCTypeClaims:
		var claims domain.Claims
		_ = json.Unmarshal(env.Data, &claims)
		return domain.NewRemoteCacheEnvelope(domain.RCTypeClaimed, domain.Claimed{SessionID: claims.SessionID})
	case domain.RCTypeSettle:
		var settle domain.Settle
		_ = json.Unmarshal(env.Data, &settle)
		return domain.NewRemoteCacheEnvelope(domain.RCTypeSettled, domain.Settled{Root: settle.Root})
	case domain.RCTypeExport:
		var export domain.Export
		if err := json.Unmarshal(env.Data, &export); err != nil {
			return s.errorEnvelope("invalid export payload")
		}
		if readOnly {
			return domain.NewRemoteCacheEnvelope(domain.RCTypeExported, domain.Exported{Root: export.Root})
		}
		return s.handleExport(ctx, namespace, &export)
	case domain.RCTypeUpload:
		var upload domain.Upload
		_ = json.Unmarshal(env.Data, &upload)
		if readOnly {
			return domain.NewRemoteCacheEnvelope(domain.RCTypeUploaded, domain.Uploaded{URLs: map[string]string{}})
		}
		return s.handleUpload(ctx, namespace, &upload)
	case domain.RCTypeLookup:
		var lookup domain.Lookup
		_ = json.Unmarshal(env.Data, &lookup)
		return s.handleLookup(namespace, &lookup)
	case domain.RCTypeMerge:
		var merge domain.Merge
		_ = json.Unmarshal(env.Data, &merge)
		return s.handleGet(ctx, namespace, merge.Digests, domain.RCTypeMerged)
	case domain.RCTypeOffer:
		var offer domain.Offer
		_ = json.Unmarshal(env.Data, &offer)
		return domain.NewRemoteCacheEnvelope(domain.RCTypeOffered, domain.Offered{Root: offer.Root})
	case domain.RCTypeRenew:
		var renew domain.Renew
		_ = json.Unmarshal(env.Data, &renew)
		return s.handleGet(ctx, namespace, renew.Digests, domain.RCTypeRenewed)
	default:
		return s.errorEnvelope(fmt.Sprintf("unsupported message type %q", env.Type))
	}
}

func (s *RemoteCacheService) handleExport(ctx context.Context, namespace string, export *domain.Export) ([]byte, error) {
	if err := s.exportSem.Acquire(ctx, 1); err != nil {
		return s.errorEnvelope("export saturated")
	}
	defer s.exportSem.Release(1)
	if err := s.enforceCacheWriteQuota(ctx, namespace, int64(len(export.Bundle))); err != nil {
		return s.errorEnvelope(err.Error())
	}
	key, err := s.store.PutBundle(ctx, namespace, export.Digest, export.Bundle)
	if err != nil {
		return s.errorEnvelope(err.Error())
	}
	entry := &repository.CacheIndexEntry{
		Digest: export.Digest, Root: export.Root, BundleKey: key, Size: int64(len(export.Bundle)),
	}
	var bundle domain.ValueBundle
	if err := json.Unmarshal(export.Bundle, &bundle); err == nil {
		entry.Aliases = bundleAliases(&bundle)
	}
	if err := s.index.Put(namespace, entry); err != nil {
		return s.errorEnvelope(err.Error())
	}
	for _, alias := range entry.Aliases {
		aliasEntry := *entry
		aliasEntry.Digest = alias
		_ = s.index.Put(namespace, &aliasEntry)
	}
	if s.metrics != nil {
		s.metrics.RemoteCacheExportedBundles.WithLabelValues(namespace).Inc()
		s.metrics.RemoteCacheStoredBytes.WithLabelValues(namespace).Set(float64(entry.Size))
	}
	return domain.NewRemoteCacheEnvelope(domain.RCTypeExported, domain.Exported{Root: export.Root, Key: key})
}

func bundleAliases(b *domain.ValueBundle) []string {
	seen := map[string]bool{b.Digest: true}
	var out []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	add(b.RecipeDigest)
	add(b.SelfDigest)
	for _, d := range b.ExtraDigests {
		add(d)
	}
	for _, d := range b.InputDigests {
		add(d)
	}
	for _, d := range b.Layers {
		add(d)
	}
	return out
}

func (s *RemoteCacheService) handleUpload(ctx context.Context, namespace string, upload *domain.Upload) ([]byte, error) {
	if err := s.enforceCacheWriteQuota(ctx, namespace, uploadDelta(upload)); err != nil {
		return s.errorEnvelope(err.Error())
	}
	urls := make(map[string]string, len(upload.Digests))
	now := time.Now().UTC()
	s.mu.Lock()
	for _, digest := range upload.Digests {
		s.inflight[namespace+"/"+digest] = now
	}
	s.mu.Unlock()

	var packMembers []PackMemberSpec
	var packDigests []string
	for _, blob := range upload.Blobs {
		if blob.Size > 0 && blob.Size <= s.packMaxMemberBytes {
			packMembers = append(packMembers, PackMemberSpec{Digest: blob.Digest, Size: blob.Size})
			packDigests = append(packDigests, blob.Digest)
			continue
		}
		u, err := s.store.PresignPut(ctx, namespace, blob.Digest)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		urls[blob.Digest] = u
	}
	for _, digest := range upload.Digests {
		if _, ok := urls[digest]; ok {
			continue
		}
		u, err := s.store.PresignPut(ctx, namespace, digest)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		urls[digest] = u
	}
	for _, manifest := range upload.Packs {
		packKey := s.store.PackKey(namespace, manifest.ID.Digest)
		for _, member := range manifest.Members {
			_ = s.index.Put(namespace, &repository.CacheIndexEntry{
				Digest: member.Digest, PackDigest: manifest.ID.Digest, PackKey: packKey,
				PackOffset: member.Offset, Size: member.Size,
			})
		}
	}
	var packs []domain.UploadPack
	if len(packMembers) > 0 {
		id := domain.BuildPackID(packDigests)
		u, err := s.store.PresignPackPut(ctx, namespace, id.Digest)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		packKey := s.store.PackKey(namespace, id.Digest)
		headerLen := PackHeaderLen(id, packMembers)
		members := make([]domain.PackMember, 0, len(packMembers))
		var rel int64
		for _, m := range packMembers {
			abs := headerLen + rel
			members = append(members, domain.PackMember{Digest: m.Digest, Offset: abs, Size: m.Size})
			// Pre-record the member descriptor so a later Merge/Renew can serve
			// the exact member slice via a signed HTTP Range (206) URL.
			_ = s.index.Put(namespace, &repository.CacheIndexEntry{
				Digest: m.Digest, PackDigest: id.Digest, PackKey: packKey,
				PackOffset: abs, Size: m.Size,
			})
			rel += m.Size
		}
		packs = append(packs, domain.UploadPack{PackID: id, URL: u, Members: members})
	}
	return domain.NewRemoteCacheEnvelope(domain.RCTypeUploaded, domain.Uploaded{URLs: urls, Packs: packs})
}

// uploadDelta estimates the bytes a cache upload will add to the namespace. It
// sums the declared blob sizes and pack totals only; opaque digests without a
// declared size are not counted, and a digest that only appears in
// upload.Digests contributes nothing. The result is a deliberately cheap
// estimate that can under-count an upload, unlike the single-bundle Export path
// which is enforced on the exact bundle length.
func uploadDelta(upload *domain.Upload) int64 {
	var delta int64
	for _, b := range upload.Blobs {
		delta += b.Size
	}
	for _, p := range upload.Packs {
		delta += p.Total
	}
	return delta
}

// enforceCacheWriteQuota blocks a cache write that would push the namespace
// past its cache_bytes hard cap, after one synchronous GC to make room. Reads
// (Lookup/Merge/Renew) are never blocked. Soft-limit breaches admit the write
// and increment the org-quota soft-exceeded metric; a hard breach that survives
// GC is rejected with the standard quota error envelope.
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

func (s *RemoteCacheService) handleLookup(namespace string, lookup *domain.Lookup) ([]byte, error) {
	status := make(map[string]string, len(lookup.Digests))
	now := time.Now().UTC()
	for _, digest := range lookup.Digests {
		if _, ok, err := s.index.Get(namespace, digest); err == nil && ok {
			status[digest] = "stored"
			_ = s.index.Touch(namespace, digest, now)
			if s.metrics != nil {
				s.metrics.RemoteCacheLookupHits.WithLabelValues(namespace).Inc()
			}
			continue
		}
		s.mu.Lock()
		_, merging := s.inflight[namespace+"/"+digest]
		s.mu.Unlock()
		if merging {
			status[digest] = "merging"
			continue
		}
		status[digest] = "notStored"
		if s.metrics != nil {
			s.metrics.RemoteCacheLookupMisses.WithLabelValues(namespace).Inc()
		}
	}
	return domain.NewRemoteCacheEnvelope(domain.RCTypeLookedUp, domain.LookedUp{Status: status})
}

func (s *RemoteCacheService) handleGet(ctx context.Context, namespace string, digests []string, responseType string) ([]byte, error) {
	urls := make(map[string]string, len(digests))
	locations := make(map[string]domain.BlobLocation, len(digests))
	bundleURLs := make(map[string]string, len(digests))
	for _, digest := range digests {
		entry, ok, err := s.index.Get(namespace, digest)
		if err != nil || !ok {
			continue
		}
		if entry.PackKey != "" && entry.Size > 0 {
			u, rangeHdr, err := s.store.PresignGetRange(ctx, entry.PackKey, entry.PackOffset, entry.Size)
			if err != nil {
				return s.errorEnvelope(err.Error())
			}
			urls[digest] = u
			locations[digest] = domain.BlobLocation{
				URL: u, PackDigest: entry.PackDigest, Offset: entry.PackOffset, Size: entry.Size,
				Headers: map[string]string{"Range": rangeHdr},
			}
		} else {
			u, err := s.store.PresignGet(ctx, namespace, digest)
			if err != nil {
				return s.errorEnvelope(err.Error())
			}
			urls[digest] = u
			locations[digest] = domain.BlobLocation{URL: u, Size: entry.Size}
		}
		if entry.BundleKey != "" {
			if u, err := s.store.PresignBundleGet(ctx, namespace, digest); err == nil {
				bundleURLs[digest] = u
			}
		}
	}
	if responseType == domain.RCTypeRenewed {
		return domain.NewRemoteCacheEnvelope(domain.RCTypeRenewed, domain.Renewed{URLs: urls, Locations: locations, BundleURLs: bundleURLs})
	}
	return domain.NewRemoteCacheEnvelope(domain.RCTypeMerged, domain.Merged{URLs: urls, Locations: locations, BundleURLs: bundleURLs})
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

func (s *RemoteCacheService) errorEnvelope(msg string) ([]byte, error) {
	return domain.NewRemoteCacheEnvelope(domain.RCTypeError, domain.RemoteCacheError{Message: msg})
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
