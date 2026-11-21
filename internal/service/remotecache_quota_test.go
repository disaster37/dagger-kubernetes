package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// newCacheQuotaService builds a QuotaService with a single org whose
// cache_bytes hard cap is `hard` (0 = unlimited).
func newCacheQuotaService(t *testing.T, orgID string, hard int64) *QuotaService {
	t.Helper()
	orgRepo := newFakeOrgRepo()
	if err := orgRepo.Create(context.Background(), &domain.Org{
		ID:     orgID,
		Name:   "acme",
		Quotas: domain.OrgQuotas{MaxCacheBytes: domain.QuotaThreshold{Hard: hard}},
	}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	return NewQuotaService(&stubSessionStore{}, nil, testLogger()).WithOrgRepository(orgRepo, nil)
}

// newCacheIndexService builds a RemoteCacheService with a real temp bbolt index
// and no S3 store (sufficient for quota admission/rejection, which run before
// any S3 write and only evict S3 objects when GC actually evicts entries).
//
//nolint:unparam // test helper: orgID is a fixed test fixture
func newCacheIndexService(t *testing.T, orgID string, hard, maxBytes int64) (*RemoteCacheService, *repository.BboltCacheIndex) {
	t.Helper()
	index, err := repository.OpenBboltCacheIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	svc := NewRemoteCacheService(index, nil, 0, maxBytes, 4, 4<<20, time.Minute, nil, testLogger())
	if hard > 0 {
		svc.WithCacheQuota(newCacheQuotaService(t, orgID, hard))
	}
	return svc, index
}

//nolint:unparam // test helper: namespace is always the fixed test org
func seedCacheEntry(t *testing.T, index *repository.BboltCacheIndex, namespace, digest string, size int64) {
	t.Helper()
	if err := index.Put(namespace, &repository.CacheIndexEntry{Digest: digest, Size: size}); err != nil {
		t.Fatalf("seed index: %v", err)
	}
}

func TestEnforceCacheWriteQuotaUnderHard(t *testing.T) {
	const orgID = "org-aaaaaaaaaaaaaaaa"
	svc, index := newCacheIndexService(t, orgID, 100, 1<<30)
	seedCacheEntry(t, index, orgID, "sha256:existing", 40)

	if err := svc.enforceCacheWriteQuota(context.Background(), orgID, 20); err != nil {
		t.Fatalf("under hard (40+20=60 <= 100): %v", err)
	}
}

func TestEnforceCacheWriteQuotaRejectsOverHard(t *testing.T) {
	const orgID = "org-aaaaaaaaaaaaaaaa"
	svc, index := newCacheIndexService(t, orgID, 100, 1<<30)
	seedCacheEntry(t, index, orgID, "sha256:existing", 90)

	err := svc.enforceCacheWriteQuota(context.Background(), orgID, 20)
	if err == nil {
		t.Fatal("over hard (90+20=110 > 100) should reject")
	}
	if !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("error = %v, want wrapping ErrOrgQuotaExceeded", err)
	}
	if got, want := err.Error(), "organization quota exceeded: cache_bytes limit (100) reached"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestEnforceCacheWriteQuotaUnlimited(t *testing.T) {
	const orgID = "org-aaaaaaaaaaaaaaaa"
	// hard = 0 (no quota service wired): the global maxBytes is huge.
	svc, index := newCacheIndexService(t, orgID, 0, 1<<30)
	seedCacheEntry(t, index, orgID, "sha256:existing", 90)

	if err := svc.enforceCacheWriteQuota(context.Background(), orgID, 20); err != nil {
		t.Fatalf("unlimited quota should admit: %v", err)
	}
}

func TestCacheWelcomeUsesOrgHardLimit(t *testing.T) {
	const orgID = "org-aaaaaaaaaaaaaaaa"
	svc, _ := newCacheIndexService(t, orgID, 100, 1<<30)

	raw, err := domain.NewRemoteCacheEnvelope(domain.RCTypeHello, domain.Hello{})
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	resp, err := svc.Handle(context.Background(), orgID, false, raw)
	if err != nil {
		t.Fatalf("handle hello: %v", err)
	}
	var welcome domain.Welcome
	if typ, err := domain.DecodeRemoteCacheEnvelope(resp, &welcome); err != nil {
		t.Fatalf("decode welcome: %v", err)
	} else if typ != domain.RCTypeWelcome {
		t.Fatalf("type = %q, want welcome", typ)
	}
	if welcome.MaxBytes != 100 {
		t.Fatalf("welcome MaxBytes = %d, want 100 (org hard limit)", welcome.MaxBytes)
	}
}

func TestEnforceCacheWriteQuotaSoftExceededAdmits(t *testing.T) {
	const orgID = "org-aaaaaaaaaaaaaaaa"
	orgRepo := newFakeOrgRepo()
	if err := orgRepo.Create(context.Background(), &domain.Org{
		ID:     orgID,
		Name:   "acme",
		Quotas: domain.OrgQuotas{MaxCacheBytes: domain.QuotaThreshold{Soft: 50, Hard: 100}},
	}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	index, err := repository.OpenBboltCacheIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	svc := NewRemoteCacheService(index, nil, 0, 1<<30, 4, 4<<20, time.Minute, nil, testLogger())
	svc.WithCacheQuota(NewQuotaService(&stubSessionStore{}, nil, testLogger()).WithOrgRepository(orgRepo, nil))
	seedCacheEntry(t, index, orgID, "sha256:existing", 40)

	// 40 + 20 = 60 > soft(50) but <= hard(100): admit (soft breach only).
	if err := svc.enforceCacheWriteQuota(context.Background(), orgID, 20); err != nil {
		t.Fatalf("soft breach must admit: %v", err)
	}
}
