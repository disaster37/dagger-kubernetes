package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// WorkerSnapshotStore pushes/pulls BuildKit worker-dir snapshots to the OCI
// registry so a fresh engine pod can warm-start its local cache from a
// sibling pod.
//
// Push/Pull (deprecated) move the whole worker dir as a single-layer OCI
// artifact (mirrors RegistryCLICache's manifest shape). PushIncremental /
// PullIncremental (v2) sync only the content-store blobs and a small metadata
// tarball, using a multi-layer manifest.
//
// Concurrent pushes are safe: blob uploads are content-addressed and the
// manifest PUT is atomic last-writer-wins — any winner is a valid
// point-in-time snapshot.
type WorkerSnapshotStore struct {
	client domain.CacheSnapshotClient
	repo   string // e.g. domain.WorkerSnapshotsRepo
	logger *logrus.Logger

	// concurrency is the PushIncremental probe/upload worker-pool size
	// (default defaultSnapshotConcurrency).
	concurrency int
}

// NewWorkerSnapshotStore returns a store backed by the supplied OCI registry
// client.
func NewWorkerSnapshotStore(client domain.CacheSnapshotClient, repo string, logger *logrus.Logger) *WorkerSnapshotStore {
	return &WorkerSnapshotStore{
		client:      client,
		repo:        repo,
		logger:      logger,
		concurrency: defaultSnapshotConcurrency,
	}
}

// WithConcurrency sets the PushIncremental probe/upload worker-pool size
// (clamped to >= 1). Returns the store for chaining.
func (s *WorkerSnapshotStore) WithConcurrency(n int) *WorkerSnapshotStore {
	if n < 1 {
		n = defaultSnapshotConcurrency
	}
	s.concurrency = n
	return s
}

// Push uploads the tarball stream (digest+size already known, computed while
// producing it) and publishes the manifest for the snapshot tag.
//
// Legacy single-layer format (v1): superseded by PushIncremental, which
// uploads only new content blobs instead of re-uploading the whole worker
// dir. Kept until all deployments are on the v2+ format (see the v3 plan,
// out-of-scope cleanups).
func (s *WorkerSnapshotStore) Push(ctx context.Context, tag string, r io.Reader, digest string, size int64) error {
	if err := s.client.UploadBlobStream(ctx, s.repo, digest, size, r); err != nil {
		return fmt.Errorf("upload snapshot blob: %w", err)
	}

	// Ensure the empty-JSON config blob referenced by the manifest exists.
	// Already-exists errors are non-fatal (the blob may be there from a
	// previous push).
	emptyJSON := strings.NewReader("{}")
	if _, _, err := s.client.UploadBlob(ctx, s.repo, emptyJSON); err != nil {
		s.logger.WithError(err).Warn("worker snapshot: upload empty config blob (may already exist)")
	}

	manifest := &domain.CLIManifest{
		SchemaVersion: 2,
		MediaType:     domain.MediaTypeOCIImageManifest,
		Config: domain.CLIManifestConfig{
			MediaType: domain.MediaTypeOCIEmptyJSON,
			Digest:    emptyJSONDigest,
			Size:      emptyJSONSize,
		},
		Layers: []domain.CLIManifestLayer{{
			MediaType: domain.MediaTypeOCILayerGzip,
			Digest:    digest,
			Size:      size,
		}},
	}

	if err := s.client.PutManifest(ctx, s.repo, tag, manifest); err != nil {
		return fmt.Errorf("put snapshot manifest %s:%s: %w", s.repo, tag, err)
	}

	s.logger.WithFields(logrus.Fields{
		"repo": s.repo, "tag": tag, "digest": digest, "size": size,
	}).Info("worker snapshot pushed")
	return nil
}

// Pull downloads the snapshot for tag and untars it into dstDir. Returns
// ok=false when no snapshot exists (or its blob vanished). Streams, never
// buffers the whole blob.
//
// Legacy single-layer format (v1): superseded by PullIncremental, which
// restores the metadata tarball and downloads only missing content blobs.
// Kept until all deployments are on the v2+ format (see the v3 plan,
// out-of-scope cleanups).
func (s *WorkerSnapshotStore) Pull(ctx context.Context, tag, dstDir string) (bool, error) {
	manifest, err := s.client.GetManifest(ctx, s.repo, tag)
	if err != nil {
		if errors.Is(err, domain.ErrManifestNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get snapshot manifest %s:%s: %w", s.repo, tag, err)
	}
	if len(manifest.Layers) == 0 {
		return false, nil
	}

	layer := manifest.Layers[0]
	rc, _, err := s.client.GetBlob(ctx, s.repo, layer.Digest)
	if err != nil {
		if errors.Is(err, domain.ErrManifestNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get snapshot blob: %w", err)
	}
	defer func() { _ = rc.Close() }()

	if err := UntarGzipDir(ctx, dstDir, rc); err != nil {
		return false, fmt.Errorf("untar snapshot: %w", err)
	}

	s.logger.WithFields(logrus.Fields{
		"repo": s.repo, "tag": tag, "digest": layer.Digest, "dst": dstDir,
	}).Info("worker snapshot restored")
	return true, nil
}

// PushIncremental performs the v2 incremental push: walk the content store,
// probe+upload missing blobs, tar the metadata (everything except
// content/blobs/) into a temp file on tmpDir, and publish a multi-layer
// manifest (layer 0 = metadata tarball, layers 1..N = raw content blobs).
// workerDir is the absolute worker directory (e.g. /var/lib/dagger/worker).
func (s *WorkerSnapshotStore) PushIncremental(ctx context.Context, tag, workerDir, tmpDir string) error {
	blobs, err := walkContentStore(workerDir)
	if err != nil {
		return err
	}

	blobLayers, err := probeAndUploadMissing(ctx, s.client, s.repo, workerDir, blobs, s.concurrency, s.logger)
	if err != nil {
		return err
	}

	// Metadata tarball: everything under the worker dir except the content
	// store (blobs are pushed individually above).
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return fmt.Errorf("prepare tmp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "worker-meta-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(tmpName) }()

	metaDigest, metaSize, err := TarGzipSubset(ctx, filepath.Dir(workerDir), filepath.Base(workerDir), tmp, contentBlobsExclude)
	if err != nil {
		return fmt.Errorf("tar %s: %w", workerDir, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp: %w", err)
	}
	if err := s.client.UploadBlobStream(ctx, s.repo, metaDigest, metaSize, tmp); err != nil {
		return fmt.Errorf("upload metadata tarball: %w", err)
	}

	manifest := buildMultiLayerManifest(metaDigest, metaSize, blobLayers)
	if err := s.client.PutManifest(ctx, s.repo, tag, manifest); err != nil {
		return fmt.Errorf("put snapshot manifest %s:%s: %w", s.repo, tag, err)
	}

	s.logger.WithFields(logrus.Fields{
		"repo": s.repo, "tag": tag, "blobs": len(blobLayers),
		"meta_digest": metaDigest, "meta_size": metaSize,
	}).Info("worker snapshot pushed (incremental)")
	return nil
}

// PullIncremental performs the v2 incremental pull: fetch the multi-layer
// manifest for tag, restore the metadata tarball (layer 0) into dstDir, then
// download only the content blobs (layers 1..N) not already present locally.
// dstDir is the base directory (e.g. /var/lib/dagger); workerSubdir is the
// worker-dir name under it (the tarball extracts "<workerSubdir>/..." and
// blobs are restored under "<dstDir>/<workerSubdir>/content/blobs/...").
// Returns ok=false when no snapshot exists.
func (s *WorkerSnapshotStore) PullIncremental(ctx context.Context, tag, dstDir, workerSubdir string) (bool, error) {
	manifest, err := s.client.GetManifest(ctx, s.repo, tag)
	if err != nil {
		if errors.Is(err, domain.ErrManifestNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get snapshot manifest %s:%s: %w", s.repo, tag, err)
	}
	if len(manifest.Layers) == 0 {
		return false, nil
	}

	metaLayer := manifest.Layers[0]
	rc, _, err := s.client.GetBlob(ctx, s.repo, metaLayer.Digest)
	if err != nil {
		if errors.Is(err, domain.ErrManifestNotFound) {
			return false, nil // metadata blob vanished (registry GC)
		}
		return false, fmt.Errorf("get metadata tarball: %w", err)
	}
	defer func() { _ = rc.Close() }()

	if err := UntarGzipDir(ctx, dstDir, rc); err != nil {
		return false, fmt.Errorf("untar snapshot: %w", err)
	}

	blobs := make([]contentBlob, 0, len(manifest.Layers)-1)
	for _, layer := range manifest.Layers[1:] {
		blobs = append(blobs, contentBlob{Digest: layer.Digest})
	}
	blobBase := filepath.Join(dstDir, workerSubdir)
	if err := downloadMissingBlobs(ctx, s.client, s.repo, blobBase, blobs, s.logger); err != nil {
		return false, err
	}

	s.logger.WithFields(logrus.Fields{
		"repo": s.repo, "tag": tag, "blobs": len(blobs), "dst": dstDir,
	}).Info("worker snapshot restored (incremental)")
	return true, nil
}
