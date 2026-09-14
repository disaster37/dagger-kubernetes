package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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

// errBlobVanished marks a content blob that the walk saw but that BuildKit GC
// deleted before its upload. Push treats it like a failed upload: the
// metadata tarball is not published, so the previous consistent snapshot is
// kept.
var errBlobVanished = errors.New("content blob vanished before upload")

// s3BlobsDir is the S3 key prefix under each version prefix that holds the
// content blobs. It is deliberately broader than domain.BlobsPrefix
// ("blobs/sha256/") so the listing covers both engine layouts: flat
// (blobs/sha256/<64-hex>) and sharded (blobs/sha256/<2-hex>/<64-hex>).
const s3BlobsDir = "blobs/"

// S3SnapshotStore pushes/pulls BuildKit worker-dir snapshots to/from an
// S3-compatible object store. All pods in the same StatefulSet (same engine
// version) share one snapshot prefix; concurrent pushes are safe because blob
// uploads are idempotent (content-addressed keys) and the metadata tarball is
// an atomic last-writer-wins object overwrite. The store never deletes from
// the shared prefix: orphaned blobs (no longer referenced by the current
// metadata) are harmless and are reclaimed by a bucket lifecycle policy.
//
// Storage layout (see domain.WorkerSnapshotS3Prefix):
//
//	<bucket>/<prefix>/meta.tar.gz      metadata tarball
//	<bucket>/<prefix>/blobs/sha256/... content blobs, mirroring the engine's
//	                                   on-disk content store (flat
//	                                   <64-hex> files, or <2-hex>/<64-hex>
//	                                   shards on engines that shard)
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

// Push syncs the worker dir to S3: it tars the metadata first, walks the
// content store, uploads missing blobs, and uploads meta.tar.gz. The metadata
// is only published when every walked blob could be uploaded (or was already
// present): a failed or vanished blob aborts the push with an error, leaving
// the previous consistent snapshot in place (S3 overwrite is atomic). Blob
// keys mirror the engine's on-disk content-store layout, so both the flat and
// the sharded layouts work without a mapping table.
func (s *S3SnapshotStore) Push(ctx context.Context, workerDir, tmpDir string) error {
	if _, err := os.Stat(workerDir); err != nil {
		return fmt.Errorf("stat %s: %w", workerDir, err)
	}

	// Tar the metadata FIRST: the captured metadata can only reference blobs
	// that already exist, so the walk below is guaranteed to see every blob
	// the metadata references. Tarring after the walk would let the engine
	// create a referenced blob mid-push that the walk never uploaded, which
	// would poison the next restore.
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

	blobs, err := walkContentStore(workerDir)
	if err != nil {
		return err
	}

	uploaded, skipped, failed := 0, 0, 0
	for _, blob := range blobs {
		up, err := s.uploadBlob(ctx, workerDir, blob)
		if err != nil {
			s.logger.WithError(err).WithField("digest", blob.Digest).Warn("worker snapshot: blob upload failed; not publishing metadata")
			failed++
			continue
		}
		if up {
			uploaded++
		} else {
			skipped++
		}
	}

	// Safety property: never publish a metadata tarball whose blobs are not
	// all in S3. A failed/vanished blob keeps the previous snapshot (and its
	// blobs) consistent; the next periodic push retries.
	if failed > 0 {
		s.logger.WithFields(logrus.Fields{
			"bucket": s.bucket, "prefix": s.prefix,
			"blobs_total": len(blobs), "blobs_uploaded": uploaded,
			"blobs_skipped": skipped, "blobs_failed": failed,
		}).Warn("worker snapshot: metadata not published; previous snapshot kept")
		return fmt.Errorf("worker snapshot: %d of %d content blobs unavailable; metadata not pushed", failed, len(blobs))
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
		"blobs_uploaded": uploaded, "blobs_skipped": skipped, "blobs_failed": failed,
		"meta_size": metaSize,
	}).Info("worker snapshot pushed to s3")
	return nil
}

// uploadBlob uploads one content blob unless the object already exists
// (content-addressed keys make uploads idempotent). A blob deleted by BuildKit
// GC between the walk and the upload returns errBlobVanished so Push refuses
// to publish metadata that references it.
func (s *S3SnapshotStore) uploadBlob(ctx context.Context, workerDir string, blob contentBlob) (uploaded bool, err error) {
	// Mirror the on-disk layout under the prefix, minus the "content/"
	// component: blobs/sha256/<hex> (flat) or blobs/sha256/<2>/<hex>
	// (sharded).
	key := s.prefix + strings.TrimPrefix(blob.RelPath, "content/")
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return false, nil // already present
	} else if !isNoSuchKey(err) {
		return false, fmt.Errorf("stat %s: %w", key, err)
	}

	blobPath := filepath.Join(workerDir, blob.RelPath)
	f, err := os.Open(blobPath) // #nosec G304 -- path is a blob path recorded by walkContentStore under the worker dir.
	if os.IsNotExist(err) {
		s.logger.WithField("digest", blob.Digest).Debug("worker snapshot: blob vanished before upload (BuildKit GC)")
		return false, fmt.Errorf("%w: %s", errBlobVanished, blob.Digest)
	}
	if err != nil {
		return false, fmt.Errorf("open %s: %w", blobPath, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", blobPath, err)
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, f, info.Size(), minio.PutObjectOptions{}); err != nil {
		return false, fmt.Errorf("put %s: %w", key, err)
	}
	return true, nil
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
// content store lives under. The listing prefix is "blobs/" so both the flat
// and sharded layouts are covered: every object key maps 1:1 to the local
// path it mirrors under content/. A blob that was listed but is gone by the
// time it is fetched fails the pull, so the caller discards the torn restore
// instead of restoring metadata that references a missing blob.
func (s *S3SnapshotStore) downloadMissingBlobs(ctx context.Context, workerDir string) error {
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    s.prefix + s3BlobsDir,
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
// at its mirrored local path (existence is trusted by path — re-hashing large
// blobs would be too expensive for a restore). The S3 key mirrors the
// on-disk content store under a "blobs/" listing prefix, so the local path is
// workerDir/content/<key minus the version prefix> for both layouts.
func (s *S3SnapshotStore) downloadBlob(ctx context.Context, key, workerDir string) error {
	relKey, ok := strings.CutPrefix(key, s.prefix)
	if !ok {
		s.logger.WithField("key", key).Debug("worker snapshot: unexpected blob key; skipping")
		return nil
	}
	if !validContentHex(path.Base(relKey)) {
		s.logger.WithField("key", key).Debug("worker snapshot: unexpected blob key; skipping")
		return nil
	}
	localPath := filepath.Join(workerDir, "content", filepath.FromSlash(relKey))
	if _, err := os.Stat(localPath); err == nil {
		return nil
	}

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return fmt.Errorf("blob %s vanished from s3 after listing: %w", key, err)
		}
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()

	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(localPath), err)
	}
	if err := writeTarFile(localPath, 0o600, obj); err != nil {
		return err
	}
	return nil
}
