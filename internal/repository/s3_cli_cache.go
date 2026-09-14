package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// S3CLICache stores verified CLI tarballs on an S3-compatible object store so
// every supervisor pod in a multi-node Raft cluster can serve cached binaries
// without re-downloading from upstream.
//
// The S3 key for a CLI artifact is deterministic and immutable:
//
//	<prefix>/<version>/<os>/<arch>/<filename>
//	e.g. cli-cache/v0.21.8/linux/amd64/dagger_v0.21.8_linux_amd64.tar.gz
type S3CLICache struct {
	client s3ObjectAPI
	bucket string
	prefix string // e.g. "cli-cache"
	logger *logrus.Logger
}

// NewS3CLICache returns a cache backed by the supplied S3 client.
func NewS3CLICache(client *minio.Client, bucket, prefix string, logger *logrus.Logger) *S3CLICache {
	return &S3CLICache{
		client: minioS3API{client},
		bucket: bucket,
		prefix: prefix,
		logger: logger,
	}
}

// keyFor builds the deterministic S3 key for a CLI tarball.
func (c *S3CLICache) keyFor(version, osName, arch string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", c.prefix, version, osName, arch, domain.AssetFilename(version, osName, arch))
}

// Has reports whether the artifact exists (S3 StatObject, no download).
func (c *S3CLICache) Has(ctx context.Context, version, osName, arch string) (bool, error) {
	key := c.keyFor(version, osName, arch)
	if _, err := c.client.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNoSuchKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("check object %s: %w", key, err)
	}
	return true, nil
}

// Get downloads the artifact from S3 to a temp file and returns the local
// path. Returns ("", false) when not found or the download fails.
func (c *S3CLICache) Get(ctx context.Context, version, osName, arch string) (string, bool) {
	key := c.keyFor(version, osName, arch)
	obj, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		c.logger.WithError(err).WithFields(logrus.Fields{
			"bucket": c.bucket, "key": key,
		}).Debug("cli cache get: object fetch failed")
		return "", false
	}
	defer func() { _ = obj.Close() }()

	f, err := os.CreateTemp("", "cli-cache-*")
	if err != nil {
		c.logger.WithError(err).Warn("cli cache get: create temp failed")
		return "", false
	}
	if _, err := io.Copy(f, obj); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		c.logger.WithError(err).Warn("cli cache get: write temp failed")
		return "", false
	}
	_ = f.Close()
	return f.Name(), true
}

// Put uploads the tarball to S3 after verifying sha256Hex. The tarball is
// read into memory (CLI tarballs are ~50 MB, acceptable) so the checksum is
// verified before any upload. Returns "" (no local path) on success.
func (c *S3CLICache) Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (string, error) {
	buf := new(bytes.Buffer)
	size, err := io.Copy(buf, r)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	sum := sha256.Sum256(buf.Bytes())
	computed := hex.EncodeToString(sum[:])
	if computed != sha256Hex {
		return "", fmt.Errorf("%w: expected %s, got %s", domain.ErrCLIChecksumMismatch, sha256Hex, computed)
	}

	key := c.keyFor(version, osName, arch)
	if _, err := c.client.PutObject(ctx, c.bucket, key, bytes.NewReader(buf.Bytes()), size, minio.PutObjectOptions{}); err != nil {
		return "", fmt.Errorf("put %s: %w", key, err)
	}

	c.logger.WithFields(logrus.Fields{
		"bucket": c.bucket, "key": key, "size": size,
	}).Info("cli cache put: uploaded to s3")
	return "", nil
}

// Dir returns "" for S3-backed cache (remote; no local cache directory).
func (c *S3CLICache) Dir() string { return "" }
