package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// RemoteCacheService implements the read-through remote cache protocol
// (ADR-055). It is stateless apart from the local bbolt index and S3 store.
type RemoteCacheService struct {
	index    *repository.BboltCacheIndex
	store    *repository.S3RemoteCacheStore
	metrics  *observ.Metrics
	logger   *logrus.Logger
	ttl      time.Duration
	maxBytes int64
}

// NewRemoteCacheService returns a RemoteCacheService.
func NewRemoteCacheService(index *repository.BboltCacheIndex, store *repository.S3RemoteCacheStore, ttl time.Duration, maxBytes int64, metrics *observ.Metrics, logger *logrus.Logger) *RemoteCacheService {
	return &RemoteCacheService{index: index, store: store, ttl: ttl, maxBytes: maxBytes, metrics: metrics, logger: logger}
}

// Handle processes one protocol message and returns the response envelope.
//
//nolint:gocyclo // protocol message dispatch
func (s *RemoteCacheService) Handle(ctx context.Context, namespace string, raw []byte) ([]byte, error) {
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
		key, err := s.store.PutBundle(ctx, namespace, export.Digest, export.Bundle)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		if err := s.index.Put(namespace, &repository.CacheIndexEntry{
			Digest: export.Digest, Root: export.Root, BundleKey: key, Size: int64(len(export.Bundle)),
		}); err != nil {
			return s.errorEnvelope(err.Error())
		}
		if s.metrics != nil {
			s.metrics.RemoteCacheExportedBundles.Inc()
		}
		return domain.NewRemoteCacheEnvelope(domain.RCTypeExported, domain.Exported{Root: export.Root, Key: key})
	case domain.RCTypeUpload:
		var upload domain.Upload
		_ = json.Unmarshal(env.Data, &upload)
		urls, err := s.presign(ctx, namespace, upload.Digests, true)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		return domain.NewRemoteCacheEnvelope(domain.RCTypeUploaded, domain.Uploaded{URLs: urls})
	case domain.RCTypeLookup:
		var lookup domain.Lookup
		_ = json.Unmarshal(env.Data, &lookup)
		status := make(map[string]string, len(lookup.Digests))
		for _, digest := range lookup.Digests {
			if _, ok, err := s.index.Get(namespace, digest); err == nil && ok {
				status[digest] = "merging"
				if s.metrics != nil {
					s.metrics.RemoteCacheLookupHits.Inc()
				}
			} else {
				status[digest] = "notStored"
				if s.metrics != nil {
					s.metrics.RemoteCacheLookupMisses.Inc()
				}
			}
		}
		return domain.NewRemoteCacheEnvelope(domain.RCTypeLookedUp, domain.LookedUp{Status: status})
	case domain.RCTypeMerge:
		var merge domain.Merge
		_ = json.Unmarshal(env.Data, &merge)
		urls, err := s.presign(ctx, namespace, merge.Digests, false)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		return domain.NewRemoteCacheEnvelope(domain.RCTypeMerged, domain.Merged{URLs: urls})
	case domain.RCTypeOffer:
		var offer domain.Offer
		_ = json.Unmarshal(env.Data, &offer)
		return domain.NewRemoteCacheEnvelope(domain.RCTypeOffered, domain.Offered{Root: offer.Root})
	case domain.RCTypeRenew:
		var renew domain.Renew
		_ = json.Unmarshal(env.Data, &renew)
		urls, err := s.presign(ctx, namespace, renew.Digests, false)
		if err != nil {
			return s.errorEnvelope(err.Error())
		}
		return domain.NewRemoteCacheEnvelope(domain.RCTypeRenewed, domain.Renewed{URLs: urls})
	default:
		return s.errorEnvelope(fmt.Sprintf("unsupported message type %q", env.Type))
	}
}

func (s *RemoteCacheService) presign(ctx context.Context, namespace string, digests []string, put bool) (map[string]string, error) {
	urls := make(map[string]string, len(digests))
	for _, digest := range digests {
		var (
			u   string
			err error
		)
		if put {
			u, err = s.store.PresignPut(ctx, namespace, digest)
		} else {
			u, err = s.store.PresignGet(ctx, namespace, digest)
		}
		if err != nil {
			return nil, err
		}
		urls[digest] = u
	}
	return urls, nil
}

func (s *RemoteCacheService) errorEnvelope(msg string) ([]byte, error) {
	return domain.NewRemoteCacheEnvelope(domain.RCTypeError, domain.RemoteCacheError{Message: msg})
}

// Stats returns index statistics for a namespace.
func (s *RemoteCacheService) Stats(namespace string) (count int, bytes int64, err error) {
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
	for _, e := range evicted {
		if err := s.store.DeleteBundle(ctx, namespace, e.Digest); err != nil {
			s.logger.WithError(err).WithField("digest", e.Digest).Warn("remote cache bundle delete failed")
		}
	}
	return nil
}
