package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// s3ObjectAPI is the slice of the minio-go client the S3 stores need. An
// interface (instead of the concrete *minio.Client) so tests can supply an
// in-memory object store; *minio.Client satisfies it via minioS3API.
type s3ObjectAPI interface {
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error)
	StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
	RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error
	ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
}

// minioS3API adapts *minio.Client to s3ObjectAPI: GetObject's *minio.Object
// return narrows to io.ReadCloser, which the interface requires.
type minioS3API struct{ client *minio.Client }

func (m minioS3API) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

func (m minioS3API) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.GetObject(ctx, bucketName, objectName, opts)
}

func (m minioS3API) StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.StatObject(ctx, bucketName, objectName, opts)
}

func (m minioS3API) RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.RemoveObject(ctx, bucketName, objectName, opts)
}

func (m minioS3API) ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo { //nolint:gocritic // hugeParam: signature must match the minio-go API
	return m.client.ListObjects(ctx, bucketName, opts)
}

// NewS3Client returns a minio client for an S3-compatible endpoint
// (self-hosted MinIO/SeaweedFS/Garage/Ceph RGW or AWS S3). Empty credentials
// fall back to the standard AWS env chain (AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY), matching minio-go's default credential lookup.
func NewS3Client(endpoint, region, accessKey, secretKey string, useSSL bool) (*minio.Client, error) {
	opts := &minio.Options{
		Secure: useSSL,
		Region: region,
	}
	if accessKey != "" || secretKey != "" {
		opts.Creds = credentials.NewStaticV4(accessKey, secretKey, "")
	} else {
		opts.Creds = credentials.NewEnvAWS()
	}
	return minio.New(endpoint, opts)
}

// isNoSuchKey reports whether err is an S3 NoSuchKey error (object missing).
func isNoSuchKey(err error) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	return resp.Code == "NoSuchKey"
}

// S3SnapshotStore pushes/pulls BuildKit worker-dir snapshots to/from an
// S3-compatible object store. All pods in the same StatefulSet (same engine
// version) share one snapshot prefix; concurrent pushes are safe because blob
// uploads are idempotent (content-addressed keys) and the metadata tarball is
// an atomic last-writer-wins object overwrite.
//
// Storage layout (see domain.WorkerSnapshotS3Prefix):
//
//	<bucket>/<prefix>/meta.tar.gz                       metadata tarball
//	<bucket>/<prefix>/blobs/sha256/<2-hex>/<64-hex>     content blobs
type S3SnapshotStore struct {
	client s3ObjectAPI
	bucket string
	prefix string // e.g. "worker-snapshots/v0-20-0/"

	// workerSubdir is the worker-dir name under the base dir (the tarball
	// entry prefix, e.g. "worker").
	workerSubdir string
	logger       *logrus.Logger
}

// NewS3SnapshotStore returns a store for version's snapshot prefix, backed by
// the supplied S3 client.
func NewS3SnapshotStore(client *minio.Client, bucket, version, workerSubdir string, logger *logrus.Logger) *S3SnapshotStore {
	return &S3SnapshotStore{
		client:       minioS3API{client},
		bucket:       bucket,
		prefix:       domain.WorkerSnapshotS3Prefix(version),
		workerSubdir: workerSubdir,
		logger:       logger,
	}
}

// metaKey returns the S3 key of the version's metadata tarball.
func (s *S3SnapshotStore) metaKey() string {
	return s.prefix + domain.MetaTarballName
}

// blobKey returns the S3 key of a content blob ("sha256:<hex>" digest):
// <prefix>/blobs/sha256/<first-two-hex>/<full-hex>, mirroring the on-disk
// content-store layout without the "content/" component.
func (s *S3SnapshotStore) blobKey(digest string) (string, error) {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !validContentHex(hex) {
		return "", fmt.Errorf("invalid digest %q: must be sha256:<64 hex>", digest)
	}
	return s.prefix + domain.BlobsPrefix + hex[:2] + "/" + hex, nil
}

// Push walks the content store, uploads missing blobs to S3, creates the
// metadata tarball (everything in the worker dir except content/blobs/), and
// uploads it as meta.tar.gz. Blob-level failures are non-fatal (WARN + skip;
// the next periodic push retries) — only a metadata-tarball failure fails the
// push, leaving the previous meta.tar.gz intact (S3 overwrite is atomic).
func (s *S3SnapshotStore) Push(ctx context.Context, workerDir, tmpDir string) error {
	if _, err := os.Stat(workerDir); err != nil {
		return fmt.Errorf("stat %s: %w", workerDir, err)
	}

	digests, err := walkContentStore(workerDir)
	if err != nil {
		return err
	}
	uploaded, skipped := 0, 0
	for _, digest := range digests {
		if err := s.uploadBlob(ctx, workerDir, digest); err != nil {
			s.logger.WithError(err).WithField("digest", digest).Warn("worker snapshot: blob upload failed; skipping")
			skipped++
			continue
		}
		uploaded++
	}

	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return fmt.Errorf("prepare tmp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "worker-meta-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(tmpName) }()

	metaDigest, metaSize, err := TarGzipSubset(ctx, filepath.Dir(workerDir), s.workerSubdir, tmp, contentBlobsExclude)
	if err != nil {
		return fmt.Errorf("tar %s: %w", workerDir, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp: %w", err)
	}

	key := s.metaKey()
	if _, err := s.client.PutObject(ctx, s.bucket, key, tmp, metaSize, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}

	s.logger.WithFields(logrus.Fields{
		"bucket": s.bucket, "prefix": s.prefix, "meta_digest": metaDigest,
		"blobs_uploaded": uploaded, "blobs_skipped": skipped, "meta_size": metaSize,
	}).Info("worker snapshot pushed to s3")
	return nil
}

// uploadBlob uploads one content blob unless the object already exists
// (content-addressed keys make uploads idempotent). A blob deleted by BuildKit
// GC between the walk and the upload is skipped silently (DEBUG).
func (s *S3SnapshotStore) uploadBlob(ctx context.Context, workerDir, digest string) error {
	key, err := s.blobKey(digest)
	if err != nil {
		return err
	}
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return nil // already present
	} else if !isNoSuchKey(err) {
		return fmt.Errorf("stat %s: %w", key, err)
	}

	rel, err := blobRelPath(digest)
	if err != nil {
		return err
	}
	path := filepath.Join(workerDir, rel)
	f, err := os.Open(path) // #nosec G304 -- path is derived from the validated digest.
	if os.IsNotExist(err) {
		s.logger.WithField("digest", digest).Debug("worker snapshot: blob vanished before upload (BuildKit GC); skipping")
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, f, info.Size(), minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Pull downloads the metadata tarball and the missing content blobs from the
// version prefix into dstDir (the base dir: the tarball extracts
// "<workerSubdir>/..." under it). Returns ok=false when no snapshot exists
// (first boot, or meta.tar.gz reclaimed by a lifecycle policy).
func (s *S3SnapshotStore) Pull(ctx context.Context, dstDir string) (bool, error) {
	key := s.metaKey()
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNoSuchKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", key, err)
	}

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return false, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()

	if err := UntarGzipDir(ctx, dstDir, obj); err != nil {
		return false, fmt.Errorf("untar snapshot: %w", err)
	}

	// Blobs live under the worker dir (the metadata tarball carries the
	// workerSubdir prefix itself, the blob keys do not).
	if err := s.downloadMissingBlobs(ctx, filepath.Join(dstDir, s.workerSubdir)); err != nil {
		return false, err
	}

	s.logger.WithFields(logrus.Fields{
		"bucket": s.bucket, "prefix": s.prefix, "dst": dstDir,
	}).Info("worker snapshot restored from s3")
	return true, nil
}

// downloadMissingBlobs lists the version prefix's content blobs and downloads
// those not already present locally. workerDir is the absolute worker dir the
// content store lives under. Objects missing from S3 (reclaimed by a
// lifecycle policy) are skipped with a DEBUG log — the engine re-downloads
// them from the remote cache on next use.
func (s *S3SnapshotStore) downloadMissingBlobs(ctx context.Context, workerDir string) error {
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    s.prefix + domain.BlobsPrefix,
		Recursive: true,
	}) {
		if info.Err != nil {
			return fmt.Errorf("list blobs: %w", info.Err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.downloadBlob(ctx, info.Key, workerDir); err != nil {
			return err
		}
	}
	return nil
}

// downloadBlob restores one content blob object unless a file already exists
// at the expected local path (existence is trusted by path — re-hashing large
// blobs would be too expensive for a restore). Blob keys have the shape
// <prefix>/blobs/sha256/<first-two-hex>/<full-hex>.
func (s *S3SnapshotStore) downloadBlob(ctx context.Context, key, workerDir string) error {
	relKey, ok := strings.CutPrefix(key, s.prefix+domain.BlobsPrefix)
	if !ok {
		s.logger.WithField("key", key).Debug("worker snapshot: unexpected blob key; skipping")
		return nil
	}
	shard, hex, ok := strings.Cut(relKey, "/")
	if !ok || !validContentHex(hex) || shard != hex[:2] {
		s.logger.WithField("key", key).Debug("worker snapshot: unexpected blob key; skipping")
		return nil
	}
	digest := fmt.Sprintf("sha256:%s", hex)
	rel, err := blobRelPath(digest)
	if err != nil {
		s.logger.WithField("key", key).Debug("worker snapshot: unexpected blob key; skipping")
		return nil
	}
	path := filepath.Join(workerDir, rel)
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			s.logger.WithField("digest", digest).Debug("worker snapshot: blob missing from s3; skipping")
			return nil
		}
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := writeTarFile(path, 0o600, obj); err != nil {
		return err
	}
	return nil
}

// CleanupSelf deletes all objects under this version's prefix. Called on
// sidecar startup before the first push so old blobs from previous pushes do
// not accumulate across restarts (D5).
func (s *S3SnapshotStore) CleanupSelf(ctx context.Context) error {
	removed := 0
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    s.prefix,
		Recursive: true,
	}) {
		if info.Err != nil {
			return fmt.Errorf("list %s: %w", s.prefix, info.Err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.client.RemoveObject(ctx, s.bucket, info.Key, minio.RemoveObjectOptions{}); err != nil {
			return fmt.Errorf("remove %s: %w", info.Key, err)
		}
		removed++
	}
	if removed > 0 {
		s.logger.WithFields(logrus.Fields{
			"bucket": s.bucket, "prefix": s.prefix, "removed": removed,
		}).Info("worker snapshot: startup self-cleanup removed stale objects")
	}
	return nil
}
