package domain

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Well-known OCI coordinates of the BuildKit worker-dir snapshots. Constants
// (not config keys) so the helper, the pod renderer, and the stats/GC skip
// cannot drift apart — mirroring the fixed `cache` tag of the remote BuildKit
// cache.
const (
	// WorkerSnapshotsRepo is the OCI repository holding the per-version
	// BuildKit worker-dir snapshots (local cache warm-start).
	WorkerSnapshotsRepo = "dagger-cache/worker-snapshots"

	// workerSnapshotTagPrefix prefixes every legacy (v1) snapshot tag.
	workerSnapshotTagPrefix = "worker-"

	// workerSnapshotTagV2Prefix prefixes every v2 snapshot tag (multi-layer
	// incremental format). It distinguishes the new manifests from the legacy
	// single-layer format so old and new pods can coexist.
	workerSnapshotTagV2Prefix = "worker-v2-"
)

// S3 key prefixes inside the shared cache bucket. Constants (not config keys)
// so the snapshot store, the CLI cache, and the GC sweeper cannot drift apart.
const (
	// WorkerSnapshotsPrefix is the S3 key prefix for worker snapshots:
	// worker-snapshots/<version-slug>/{meta.tar.gz,blobs/...}.
	WorkerSnapshotsPrefix = "worker-snapshots"

	// MetaTarballName is the metadata tarball object name under each version
	// prefix (everything in the worker dir except the content store).
	MetaTarballName = "meta.tar.gz"

	// BlobsPrefix is the S3 key prefix for content blobs under each version
	// prefix; it mirrors the on-disk content-store layout.
	BlobsPrefix = "blobs/sha256/"

	// S3CachePrefix is the S3 key prefix holding the BuildKit remote-cache
	// refs. It is the only prefix the S3 GC sweeper touches (worker snapshots
	// and the CLI cache have their own cleanup mechanisms).
	S3CachePrefix = "cache"
)

// WorkerSnapshotTag returns the per-version legacy snapshot tag, e.g.
// "worker-v0-20-0". VersionSlug is idempotent, so both "v0.20.0" and the slug
// "v0-20-0" produce the same tag.
func WorkerSnapshotTag(version string) string {
	return workerSnapshotTagPrefix + VersionSlug(version)
}

// WorkerSnapshotTagV2 returns the per-version v2 snapshot tag, e.g.
// "worker-v2-v0-20-0". The "v2" prefix distinguishes the multi-layer
// incremental-sync format from the legacy single-layer format.
func WorkerSnapshotTagV2(version string) string {
	return workerSnapshotTagV2Prefix + VersionSlug(version)
}

// WorkerSnapshotVersionSlug extracts the version slug from a snapshot tag
// ("worker-v2-<slug>" or the legacy "worker-<slug>"), so the S3 snapshot
// prefix can be derived from the rendered tag. VersionSlug is idempotent, so
// an already-slugged value passes through unchanged.
func WorkerSnapshotVersionSlug(tag string) string {
	if slug, ok := strings.CutPrefix(tag, workerSnapshotTagV2Prefix); ok {
		return slug
	}
	slug, _ := strings.CutPrefix(tag, workerSnapshotTagPrefix)
	return slug
}

// WorkerSnapshotS3Prefix returns the S3 key prefix for a version's snapshots,
// e.g. "worker-snapshots/v0-20-0/".
func WorkerSnapshotS3Prefix(version string) string {
	return fmt.Sprintf("%s/%s/", WorkerSnapshotsPrefix, VersionSlug(version))
}

// CacheSnapshotClient is the slice of the OCI Distribution v2 client the
// worker-snapshot store needs (implemented by repository.RegistryStatsClient).
// It reuses CLIRegistryClient (and its CLIManifest single-layer artifact
// shape) plus the streaming blob upload, which avoids buffering multi-GB
// snapshots in memory.
type CacheSnapshotClient interface {
	CLIRegistryClient

	// ProbeBlob checks whether a blob exists in the repository (HEAD request).
	// Returns (true, nil) for 200, (false, nil) for 404/405.
	ProbeBlob(ctx context.Context, repo, digest string) (bool, error)

	// UploadBlobStream uploads a blob whose sha256 digest and byte size are
	// known up front, streaming body in a single monolithic PUT.
	UploadBlobStream(ctx context.Context, repo, digest string, size int64, body io.Reader) error
}
