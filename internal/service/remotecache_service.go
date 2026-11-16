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
		return domain.NewRemoteCacheEnvelope(domain.RCTypeWelcome, domain.Welcome{Namespace: namespace, MaxBytes: s.maxBytes})
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
		s.metrics.RemoteCacheExportedBundles.Inc()
		s.metrics.RemoteCacheStoredBytes.Set(float64(entry.Size))
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

func (s *RemoteCacheService) handleLookup(namespace string, lookup *domain.Lookup) ([]byte, error) {
	status := make(map[string]string, len(lookup.Digests))
	now := time.Now().UTC()
	for _, digest := range lookup.Digests {
		if _, ok, err := s.index.Get(namespace, digest); err == nil && ok {
			status[digest] = "stored"
			_ = s.index.Touch(namespace, digest, now)
			if s.metrics != nil {
				s.metrics.RemoteCacheLookupHits.Inc()
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
			s.metrics.RemoteCacheLookupMisses.Inc()
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
			s.metrics.RemoteCacheLookupHits.Inc()
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

// GC evicts expired/oversized entries and their S3 bundles.
func (s *RemoteCacheService) GC(ctx context.Context, namespace string) error {
	evicted, err := s.index.GC(namespace, s.ttl, s.maxBytes, time.Now().UTC())
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
		if data, err := s.store.GetIndexCheckpoint(ctx); err == nil && len(data) > 0 {
			if err := s.index.ImportSnapshot("", data); err != nil {
				s.logger.WithError(err).Warn("remote cache index checkpoint import failed")
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
	var buf bytes.Buffer
	if err := s.index.ExportSnapshot(&buf); err == nil {
		if err := s.store.PutIndexCheckpoint(ctx, buf.Bytes()); err != nil {
			s.logger.WithError(err).Warn("remote cache index checkpoint upload failed")
		}
	}
}
