package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// RegistryCLICache stores verified CLI tarballs as full OCI artifacts on the
// shared Dagger registry so every supervisor pod in a multi-node Raft cluster
// can serve cached binaries without re-downloading from GitHub.
type RegistryCLICache struct {
	client domain.CLIRegistryClient
	repo   string // e.g. "dagger-kubernetes/cli-cache"
	tmpDir string // for temp downloads
	logger *logrus.Logger
}

// NewRegistryCLICache returns a cache backed by the supplied OCI registry client.
func NewRegistryCLICache(client domain.CLIRegistryClient, repo, tmpDir string, logger *logrus.Logger) *RegistryCLICache {
	return &RegistryCLICache{
		client: client,
		repo:   repo,
		tmpDir: tmpDir,
		logger: logger,
	}
}

// tagFor builds an OCI-compliant tag from version/os/arch.
// E.g. "v0.21.8-linux-amd64".
func tagFor(version, osName, arch string) string {
	return fmt.Sprintf("%s-%s-%s", version, osName, arch)
}

// Has reports whether the artifact exists without a full download.
func (c *RegistryCLICache) Has(ctx context.Context, version, osName, arch string) (bool, error) {
	tag := tagFor(version, osName, arch)
	exists, err := c.client.ManifestExists(ctx, c.repo, tag)
	if err != nil {
		return false, fmt.Errorf("check manifest %s:%s: %w", c.repo, tag, err)
	}
	return exists, nil
}

// Get downloads the artifact from the registry to a temp file and returns
// the local path. Returns ("", false) when not found.
func (c *RegistryCLICache) Get(ctx context.Context, version, osName, arch string) (string, bool) {
	tag := tagFor(version, osName, arch)

	manifest, err := c.client.GetManifest(ctx, c.repo, tag)
	if err != nil {
		c.logger.WithError(err).WithFields(logrus.Fields{
			"repo": c.repo, "tag": tag,
		}).Debug("cli cache get: manifest fetch failed")
		return "", false
	}

	if len(manifest.Layers) == 0 {
		return "", false
	}

	blobDigest := manifest.Layers[0].Digest
	rc, _, err := c.client.GetBlob(ctx, c.repo, blobDigest)
	if err != nil {
		c.logger.WithError(err).WithFields(logrus.Fields{
			"repo": c.repo, "digest": blobDigest,
		}).Debug("cli cache get: blob fetch failed")
		return "", false
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp(c.tmpDir, "cli-cache-*")
	if err != nil {
		c.logger.WithError(err).Warn("cli cache get: create temp failed")
		return "", false
	}

	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		c.logger.WithError(err).Warn("cli cache get: write temp failed")
		return "", false
	}
	_ = f.Close()
	return f.Name(), true
}

// Put uploads the tarball to the registry after verifying sha256Hex.
// Returns "" (no local path) on success.
func (c *RegistryCLICache) Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (string, error) {
	// Read the entire body into memory while computing sha256.
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

	// Upload blob.
	digest, _, err := c.client.UploadBlob(ctx, c.repo, buf)
	if err != nil {
		return "", fmt.Errorf("upload blob: %w", err)
	}

	// Upload the empty JSON config blob referenced by the manifest.
	// The registry requires all referenced blobs to exist before accepting the manifest.
	// sha256("{}") = 44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a
	emptyJSON := strings.NewReader("{}")
	if _, _, err := c.client.UploadBlob(ctx, c.repo, emptyJSON); err != nil {
		// Log a warning but don't fail — the blob may already exist from a previous upload.
		c.logger.WithError(err).Warn("upload empty config blob (may already exist)")
	}

	// Build manifest with annotations.
	// The config descriptor must reference an actual empty JSON blob.
	// sha256("{}") = 44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a
	// Using the blob digest here would reference a non-existent config blob
	// and cause registry/tooling rejections (manifest integrity check).
	const emptyJSONDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	const emptyJSONSize = 2 // "{}" = 2 bytes

	filename := domain.AssetFilename(version, osName, arch)
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
		Annotations: map[string]string{
			"com.dagger-kubernetes.cli.sha256":   sha256Hex,
			"com.dagger-kubernetes.cli.version":  version,
			"com.dagger-kubernetes.cli.filename": filename,
		},
	}

	tag := tagFor(version, osName, arch)
	if err := c.client.PutManifest(ctx, c.repo, tag, manifest); err != nil {
		return "", fmt.Errorf("put manifest %s:%s: %w", c.repo, tag, err)
	}

	c.logger.WithFields(logrus.Fields{
		"repo": c.repo, "tag": tag, "digest": digest, "size": size,
	}).Info("cli cache put: uploaded to registry")
	return "", nil
}

// Dir returns "" for registry-backed cache.
func (c *RegistryCLICache) Dir() string { return "" }
