package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// FileCLICache is a sha256-verified, atomic filesystem cache for Dagger CLI
// tarballs. Key = <version>_<os>_<arch>.tar.gz, with a <key>.sha256 sidecar.
type FileCLICache struct {
	dir string
}

// maxCLITarballBytes bounds how many compressed tarball bytes Put will stream to
// disk. Real Dagger CLI tarballs are ~20 MB; 1 GiB is far above any legitimate
// asset but prevents a misbehaving mirror from filling the cache volume with an
// unbounded response (CWE-400/CWE-409). A var (not const) so tests can lower it.
var maxCLITarballBytes int64 = 1 << 30

// NewFileCLICache creates the cache directory (0755) and removes any leftover
// tmp-* files from a previous crash.
func NewFileCLICache(dir string) (*FileCLICache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cli cache dir: %w", err)
	}
	c := &FileCLICache{dir: dir}
	c.cleanupTemps()
	return c, nil
}

// Dir returns the cache root directory.
func (c *FileCLICache) Dir() string {
	return c.dir
}

func (c *FileCLICache) path(version, osName, arch string) string {
	return filepath.Join(c.dir, domain.AssetFilename(version, osName, arch))
}

func (c *FileCLICache) sidecarPath(version, osName, arch string) string {
	return fmt.Sprintf("%s.sha256", c.path(version, osName, arch))
}

// Get returns the cached tarball path after re-verifying its sha256 sidecar.
// A missing sidecar or a mismatch deletes the file and reports ok=false.
func (c *FileCLICache) Get(version, osName, arch string) (string, bool) {
	p := c.path(version, osName, arch)
	sidecar, err := os.ReadFile(c.sidecarPath(version, osName, arch))
	if err != nil {
		return "", false
	}
	expected := strings.TrimSpace(string(sidecar))
	if expected == "" {
		return "", false
	}

	actual, err := hashFile(p)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(actual, expected) {
		_ = os.Remove(p)
		_ = os.Remove(c.sidecarPath(version, osName, arch))
		return "", false
	}
	return p, true
}

// Put streams r to a temp file while hashing, verifies sha256Hex, writes the
// sidecar, and atomically renames into place. On any failure the temp file is
// removed and the final file is never created.
func (c *FileCLICache) Put(version, osName, arch string, r io.Reader, sha256Hex string) (string, error) {
	if sha256Hex == "" {
		return "", fmt.Errorf("%w: empty checksum", domain.ErrCLIChecksumMismatch)
	}

	p := c.path(version, osName, arch)
	tmp, err := os.CreateTemp(c.dir, "tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	if n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxCLITarballBytes+1)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write cache temp: %w", err)
	} else if n > maxCLITarballBytes {
		_ = tmp.Close()
		return "", fmt.Errorf("%w: tarball exceeds %d bytes", domain.ErrCLIUpstreamUnavailable, maxCLITarballBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close cache temp: %w", err)
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, sha256Hex) {
		return "", fmt.Errorf("%w: %s", domain.ErrCLIChecksumMismatch, sha256Hex)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("chmod cache temp: %w", err)
	}
	if err := os.WriteFile(c.sidecarPath(version, osName, arch), []byte(fmt.Sprintf("%s\n", sha256Hex)), 0o644); err != nil {
		return "", fmt.Errorf("write sidecar: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(c.sidecarPath(version, osName, arch))
		return "", fmt.Errorf("rename cache file: %w", err)
	}
	ok = true
	return p, nil
}

// cleanupTemps removes leftover tmp-* files in the cache directory.
func (c *FileCLICache) cleanupTemps() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "tmp-") {
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
		}
	}
}

// hashFile returns the hex sha256 digest of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
