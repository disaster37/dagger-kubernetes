package domain

import (
	"context"
	"time"
)

// RegistryClient is the slice of the OCI registry stats client the service
// layer needs (probe/purge/stats operations over an OCI Distribution v2
// registry). Implemented by repository.RegistryStatsClient.
type RegistryClient interface {
	Host() string
	Ping(ctx context.Context) error
	Catalog(ctx context.Context) ([]string, error)
	Tags(ctx context.Context, repo string) ([]string, error)
	ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error)
	ManifestCreated(ctx context.Context, repo, tag string) (time.Time, error)
	ProbeManifest(ctx context.Context, repo, ref string) (bool, error)
	ProbeBlob(ctx context.Context, repo, digest string) (bool, error)
	DeleteManifest(ctx context.Context, repo, digest string) error
}

// CacheRoutesStore is the routing-table persistence slice the service layer
// needs (the object→backend routing table). Implemented by
// repository.CacheRoutesRepo.
type CacheRoutesStore interface {
	LookupManifest(ctx context.Context, repo, tag string) (CacheRoute, bool, error)
	LookupBlob(ctx context.Context, digest string) (string, bool, error)
	LookupUpload(ctx context.Context, uuid string) (CacheUploadSession, bool, error)
	RecordUpload(ctx context.Context, uuid, repo, backendID string) error
	DeleteUpload(ctx context.Context, uuid string) error
	UpsertBlob(ctx context.Context, digest, backendID string) error
	UpsertManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error
	AllCharges(ctx context.Context) (map[string]int64, error)
	DeleteManifestRoute(ctx context.Context, repo, tag string) error
}
