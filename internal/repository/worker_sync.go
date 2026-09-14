package repository

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// contentBlobsDir is the content-store location under the BuildKit worker
// dir. Its sharded layout (sha256/<first-two-hex>/<full-hex>) mirrors the S3
// blob key layout, so the pull path needs no mapping.
const contentBlobsDir = "content/blobs/sha256"

// contentBlobsExclude is the exclude prefix for the metadata tarball: the
// whole content store is excluded (blobs are pushed individually).
const contentBlobsExclude = "content/blobs"

// defaultSnapshotConcurrency is the probe/upload worker-pool size used by
// PushIncremental when the helper does not override it.
const defaultSnapshotConcurrency = 8

// walkContentStore returns the sorted sha256 digests ("sha256:<hex>") of the
// content-store blobs under the worker dir. A blob is any regular file whose
// base name is a 64-char lowercase hex digest, at any depth under
// content/blobs/sha256/. The walk is directory traversal only (no file reads),
// so it stays fast for very large blob counts.
func walkContentStore(workerDir string) ([]string, error) {
	storeDir := filepath.Join(workerDir, contentBlobsDir)
	if _, err := os.Stat(storeDir); err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil // fresh engine: no content store yet
		}
		return nil, fmt.Errorf("stat %s: %w", storeDir, err)
	}

	var digests []string
	walkErr := filepath.WalkDir(storeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if !validContentHex(name) {
			return nil // not a content-addressed blob (stray temp file, etc.)
		}
		digests = append(digests, fmt.Sprintf("sha256:%s", name))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", storeDir, walkErr)
	}
	sort.Strings(digests)
	return digests, nil
}

// validContentHex reports whether name is a 64-char lowercase hex sha256.
func validContentHex(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// blobRelPath returns the content-store path of a blob relative to the worker
// dir: content/blobs/sha256/<first-two-hex>/<full-hex>. The digest must be
// validated ("sha256:<64 hex>") before it can reach a filesystem or object
// path (CWE-22 defense-in-depth).
func blobRelPath(digest string) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !validContentHex(hex) {
		return "", fmt.Errorf("invalid digest %q: must be sha256:<64 hex>", digest)
	}
	return filepath.Join(contentBlobsDir, hex[:2], hex), nil
}

// probeAndUploadMissing probes every digest and uploads the blobs missing
// from the registry, using a worker pool of concurrency goroutines (the
// bottleneck is the per-blob HEAD round-trip). Returns the manifest layers of
// the blobs that are present after the pass, sorted by digest for a
// deterministic manifest.
//
// Error semantics (best-effort push):
//   - ProbeBlob error (registry unreachable): fails the whole push.
//   - Local blob vanished (BuildKit GC): DEBUG skip, excluded from the manifest.
//   - UploadBlobStream error: WARN skip, excluded from the manifest (the next
//     periodic push retries).
func probeAndUploadMissing(ctx context.Context, client domain.CacheSnapshotClient, repo, workerDir string, digests []string, concurrency int, logger *logrus.Logger) ([]domain.CLIManifestLayer, error) {
	if concurrency < 1 {
		concurrency = defaultSnapshotConcurrency
	}

	var (
		mu       sync.Mutex
		firstErr error
		layers   []domain.CLIManifestLayer
		wg       sync.WaitGroup
		jobs     = make(chan string)
	)
	worker := func() {
		defer wg.Done()
		for digest := range jobs {
			layer, skip, err := probeAndUploadOne(ctx, client, repo, workerDir, digest, logger)
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if err == nil && !skip {
				layers = append(layers, layer)
			}
			mu.Unlock()
		}
	}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go worker()
	}
	for _, digest := range digests {
		jobs <- digest
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].Digest < layers[j].Digest })
	return layers, nil
}

// probeAndUploadOne probes one digest and uploads the blob when missing.
// skip=true means the blob is intentionally excluded from the manifest
// (already handled, vanished, or failed best-effort upload).
func probeAndUploadOne(ctx context.Context, client domain.CacheSnapshotClient, repo, workerDir, digest string, logger *logrus.Logger) (layer domain.CLIManifestLayer, skip bool, err error) {
	if err := ctx.Err(); err != nil {
		return domain.CLIManifestLayer{}, false, err
	}
	exists, err := client.ProbeBlob(ctx, repo, digest)
	if err != nil {
		return domain.CLIManifestLayer{}, false, fmt.Errorf("probe blob %s: %w", digest, err)
	}

	rel, err := blobRelPath(digest)
	if err != nil {
		return domain.CLIManifestLayer{}, false, err
	}
	path := filepath.Join(workerDir, rel)

	if exists {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			// The remote blob still exists but the local file was GC'd since
			// the walk: without a size we cannot describe it; the next push
			// re-evaluates.
			logger.WithField("digest", digest).Debug("content blob vanished locally; skipping")
			return domain.CLIManifestLayer{}, true, nil
		}
		if err != nil {
			return domain.CLIManifestLayer{}, false, fmt.Errorf("stat %s: %w", path, err)
		}
		return domain.CLIManifestLayer{
			MediaType: domain.MediaTypeOCIRawBlob,
			Digest:    digest,
			Size:      info.Size(),
		}, false, nil
	}

	f, err := os.Open(path) // #nosec G304 -- path is derived from the validated digest.
	if os.IsNotExist(err) {
		logger.WithField("digest", digest).Debug("content blob vanished before upload (BuildKit GC); skipping")
		return domain.CLIManifestLayer{}, true, nil
	}
	if err != nil {
		return domain.CLIManifestLayer{}, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return domain.CLIManifestLayer{}, false, fmt.Errorf("stat %s: %w", path, err)
	}

	if err := client.UploadBlobStream(ctx, repo, digest, info.Size(), f); err != nil {
		// Best-effort: exclude the blob from this push; the next periodic
		// push retries it. The metadata tarball is still pushed.
		logger.WithError(err).WithField("digest", digest).Warn("content blob upload failed; skipping")
		return domain.CLIManifestLayer{}, true, nil
	}
	return domain.CLIManifestLayer{
		MediaType: domain.MediaTypeOCIRawBlob,
		Digest:    digest,
		Size:      info.Size(),
	}, false, nil
}

// downloadMissingBlobs restores content blobs that are not already present
// locally. Blobs missing from the backend (e.g. reclaimed by a lifecycle
// policy) are skipped with a DEBUG log — the engine re-downloads them from
// the remote cache on next use.
func downloadMissingBlobs(ctx context.Context, client domain.CacheSnapshotClient, repo, dstDir string, digests []string, logger *logrus.Logger) error {
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := downloadMissingBlob(ctx, client, repo, dstDir, digest, logger); err != nil {
			return err
		}
	}
	return nil
}

// downloadMissingBlob restores one content blob unless it already exists
// locally (existence is trusted by path — re-hashing large blobs would be
// too expensive for a restore).
func downloadMissingBlob(ctx context.Context, client domain.CacheSnapshotClient, repo, dstDir, digest string, logger *logrus.Logger) error {
	rel, err := blobRelPath(digest)
	if err != nil {
		logger.WithError(err).Warn("snapshot references an invalid blob digest; skipping")
		return nil
	}
	path := filepath.Join(dstDir, rel)
	if _, err := os.Stat(path); err == nil {
		return nil // already present (e.g. a retained PVC kept a newer copy)
	}

	rc, _, err := client.GetBlob(ctx, repo, digest)
	if err != nil {
		if errors.Is(err, domain.ErrManifestNotFound) {
			// Definitively missing (404): reclaimed by registry GC or a
			// lifecycle policy; the engine re-downloads it from the remote
			// cache on next use.
			logger.WithField("digest", digest).Debug("content blob missing from registry; skipping")
			return nil
		}
		return fmt.Errorf("get blob %s: %w", digest, err)
	}
	defer func() { _ = rc.Close() }()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := writeTarFile(path, 0o600, rc); err != nil {
		return err
	}
	return nil
}

// buildMultiLayerManifest constructs the v2 snapshot manifest: layer 0 is the
// metadata tarball (gzip), layers 1..N are the raw content blobs ordered by
// digest.
func buildMultiLayerManifest(metaDigest string, metaSize int64, blobLayers []domain.CLIManifestLayer) *domain.CLIManifest {
	return &domain.CLIManifest{
		SchemaVersion: 2,
		MediaType:     domain.MediaTypeOCIImageManifest,
		Config: domain.CLIManifestConfig{
			MediaType: domain.MediaTypeOCIEmptyJSON,
			Digest:    emptyJSONDigest,
			Size:      emptyJSONSize,
		},
		Layers: append([]domain.CLIManifestLayer{{
			MediaType: domain.MediaTypeOCILayerGzip,
			Digest:    metaDigest,
			Size:      metaSize,
		}}, blobLayers...),
	}
}
