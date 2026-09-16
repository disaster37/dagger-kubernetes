package domain

import (
	"context"
	"errors"
	"regexp"
)

// Sentinel errors for the OCI Distribution v2 client. They live in domain so
// the HTTP handler can map them to statuses without importing the repository
// layer. Moved here from the removed domain/cache.go; the mirror-facing client
// surface (domain.DistributionClient and the image-cache payloads) lives here
// too.

// ErrRegistryDeleteDisabled indicates the registry does not allow manifest
// deletion.
var ErrRegistryDeleteDisabled = errors.New("registry delete not enabled")

// ErrRegistryCatalogDisabled indicates the registry does not expose the
// /v2/_catalog endpoint, so tags cannot be enumerated.
var ErrRegistryCatalogDisabled = errors.New("registry catalog disabled")

// ErrManifestNotFound indicates the registry does not have the requested
// repo:tag manifest (a definitive 404).
var ErrManifestNotFound = errors.New("manifest not found")

// Image-cache request sentinels surfaced by the service so the handler can map
// them to 400/404/502 without importing the repository layer.
var (
	// ErrImageCacheMirrorNotFound indicates the requested mirror id is not in
	// image_cache.mirrors.
	ErrImageCacheMirrorNotFound = errors.New("image cache mirror not found")
	// ErrImageCacheInvalidRef indicates a malformed prune ref (missing
	// repository, neither tag nor digest, both, or a malformed digest).
	ErrImageCacheInvalidRef = errors.New("invalid image cache ref")
	// ErrImageCacheUnreachable indicates the mirror's OCI API could not be
	// dialed or returned a transport failure.
	ErrImageCacheUnreachable = errors.New("image cache mirror unreachable")
)

// DistributionClient is the mirror-facing OCI Distribution v2 slice (Zot
// serves this API). It is implemented by repository.DistributionClient and
// injected into the image-cache service via a factory.
type DistributionClient interface {
	Host() string
	Ping(ctx context.Context) error
	Catalog(ctx context.Context) ([]string, error)
	Tags(ctx context.Context, repo string) ([]string, error)
	// ManifestSize returns (digest, sizeBytes, layerCount). digest is used by
	// the prune path for tag→digest resolution; size/layerCount by listing.
	ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error)
	DeleteManifest(ctx context.Context, repo, digest string) error
}

// ImageCacheService lists and prunes the local image mirrors over their OCI
// Distribution v2 API. Implemented by service.ImageCacheService.
type ImageCacheService interface {
	List(ctx context.Context) (*ImageCacheInfo, error)
	Prune(ctx context.Context, mirrorID string, refs []ImageCachePruneRef) (*ImageCachePruneResult, error)
	PruneAll(ctx context.Context, mirrorID string) (*ImageCachePruneAllResult, error)
}

// digestRe constrains sha256 digests accepted from API clients before they are
// interpolated into a DELETE request path. It mirrors the repository client's
// validation (defense-in-depth) so the service can reject a malformed digest
// with a 400 instead of surfacing it as a per-item failure.
var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ValidDigest reports whether d has the sha256:<hex> shape required before it
// is used as a manifest reference.
func ValidDigest(d string) bool {
	return digestRe.MatchString(d)
}

// --- API payloads ---

// ImageCacheTag is one cached tag of a repository.
type ImageCacheTag struct {
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`      // sha256:...
	SizeBytes  int64  `json:"size_bytes"`  // -1 unknown
	LayerCount int64  `json:"layer_count"` // -1 unknown
}

// ImageCacheRepository groups a repository with its cached tags.
type ImageCacheRepository struct {
	Repository string          `json:"repository"`
	Tags       []ImageCacheTag `json:"tags"`
}

// ImageCacheMirrorInfo is one mirror's listing result. Error carries a
// per-mirror failure or a truncation note; the request itself is still 200.
type ImageCacheMirrorInfo struct {
	ID           string                 `json:"id"`
	Host         string                 `json:"host"`
	Upstream     string                 `json:"upstream"`
	Backend      string                 `json:"backend"`
	Reachable    bool                   `json:"reachable"`
	Repositories []ImageCacheRepository `json:"repositories"`
	Error        string                 `json:"error,omitempty"`
}

// ImageCacheInfo is the GET /api/v1/image-cache response body.
type ImageCacheInfo struct {
	Mirrors     []ImageCacheMirrorInfo `json:"mirrors"`
	CollectedAt string                 `json:"collected_at"` // RFC3339 UTC
}

// ImageCachePruneRef identifies one manifest to prune by tag or digest.
type ImageCachePruneRef struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"` // sha256:...; alternative to tag
}

// ImageCachePruneRequest is the POST /api/v1/image-cache/prune body.
type ImageCachePruneRequest struct {
	MirrorID string               `json:"mirror_id"`
	Refs     []ImageCachePruneRef `json:"refs"`
}

// ImageCachePruneItem is one ref's prune outcome.
type ImageCachePruneItem struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Pruned     bool   `json:"pruned"`
	Error      string `json:"error,omitempty"`
}

// ImageCachePruneResult is the POST /api/v1/image-cache/prune response body.
type ImageCachePruneResult struct {
	MirrorID string                `json:"mirror_id"`
	Items    []ImageCachePruneItem `json:"items"`
	Pruned   int                   `json:"pruned"`
	Errors   int                   `json:"errors"`
	Message  string                `json:"message,omitempty"`
}

// ImageCachePruneAllRequest is the POST /api/v1/image-cache/prune-all body.
type ImageCachePruneAllRequest struct {
	MirrorID string `json:"mirror_id"` // empty = all mirrors
}

// ImageCachePruneAllItem is one failed (or unlinkable) manifest during a
// prune-all.
type ImageCachePruneAllItem struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Pruned     bool   `json:"pruned"`
	Error      string `json:"error,omitempty"`
}

// ImageCachePruneAllMirror aggregates one mirror's prune-all outcome.
type ImageCachePruneAllMirror struct {
	MirrorID              string                   `json:"mirror_id"`
	ManifestsPruned       int                      `json:"manifests_pruned"`
	RepositoriesProcessed int                      `json:"repositories_processed"`
	Errors                int                      `json:"errors"`
	Failed                []ImageCachePruneAllItem `json:"failed,omitempty"`  // per-item failures
	Message               string                   `json:"message,omitempty"` // truncation/untagged caveat
	Error                 string                   `json:"error,omitempty"`   // whole-mirror failure (unreachable/409)
}

// ImageCachePruneAllResult is the POST /api/v1/image-cache/prune-all response
// body.
type ImageCachePruneAllResult struct {
	Mirrors []ImageCachePruneAllMirror `json:"mirrors"`
	Message string                     `json:"message,omitempty"`
}
