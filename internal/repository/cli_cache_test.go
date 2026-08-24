package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFileCLICachePutGetRoundTrip(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}

	data := []byte("a fake dagger tarball")
	sum := sha256Hex(data)
	path, err := c.Put("v0.21.8", "linux", "amd64", strings.NewReader(string(data)), sum)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if path != filepath.Join(c.Dir(), domain.AssetFilename("v0.21.8", "linux", "amd64")) {
		t.Fatalf("path = %q", path)
	}
	if c.Dir() == "" {
		t.Fatal("Dir() empty")
	}

	got, ok := c.Get("v0.21.8", "linux", "amd64")
	if !ok {
		t.Fatal("Get ok=false, want true")
	}
	if got != path {
		t.Fatalf("Get = %q, want %q", got, path)
	}
}

func TestFileCLICachePutChecksumMismatch(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}

	_, err = c.Put("v0.21.8", "linux", "amd64", strings.NewReader("data"), "deadbeef")
	if !errors.Is(err, domain.ErrCLIChecksumMismatch) {
		t.Fatalf("err = %v, want ErrCLIChecksumMismatch", err)
	}

	// No final file, no sidecar, no leftover temp files.
	entries, err := os.ReadDir(c.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp-") {
			t.Fatalf("leftover temp file %q", e.Name())
		}
		if strings.Contains(e.Name(), "v0.21.8") {
			t.Fatalf("unexpected artifact file %q", e.Name())
		}
	}
}

func TestFileCLICachePutEmptyChecksum(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	_, err = c.Put("v0.21.8", "linux", "amd64", strings.NewReader("data"), "")
	if !errors.Is(err, domain.ErrCLIChecksumMismatch) {
		t.Fatalf("err = %v, want ErrCLIChecksumMismatch", err)
	}
}

func TestFileCLICacheGetMissing(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	if _, ok := c.Get("v9.9.9", "linux", "amd64"); ok {
		t.Fatal("Get ok=true for missing artifact")
	}
}

func TestFileCLICacheGetReVerifiesAndDeletesTampered(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}

	data := []byte("original")
	if _, err := c.Put("v0.21.8", "linux", "amd64", strings.NewReader(string(data)), sha256Hex(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Tamper with the file contents.
	p := filepath.Join(c.Dir(), domain.AssetFilename("v0.21.8", "linux", "amd64"))
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if _, ok := c.Get("v0.21.8", "linux", "amd64"); ok {
		t.Fatal("Get ok=true for tampered artifact")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("tampered file still present (stat err = %v)", err)
	}
	if _, err := os.Stat(p + ".sha256"); !os.IsNotExist(err) {
		t.Fatalf("tampered sidecar still present (stat err = %v)", err)
	}
}

func TestFileCLICacheGetMissingSidecar(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	// Write a file but no sidecar.
	if err := os.WriteFile(filepath.Join(c.Dir(), domain.AssetFilename("v0.21.8", "linux", "amd64")), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := c.Get("v0.21.8", "linux", "amd64"); ok {
		t.Fatal("Get ok=true with missing sidecar")
	}
}

func TestFileCLICacheCleanupTemps(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "tmp-orphan")
	keep := filepath.Join(dir, domain.AssetFilename("v0.21.8", "linux", "amd64"))
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	c, err := NewFileCLICache(dir)
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan temp file still present")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("keep file removed: %v", err)
	}
	_ = c
}

func TestFileCLICachePutOverwrites(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}

	first := []byte("first")
	if _, err := c.Put("v0.21.8", "linux", "amd64", strings.NewReader(string(first)), sha256Hex(first)); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	second := []byte("second-content")
	if _, err := c.Put("v0.21.8", "linux", "amd64", strings.NewReader(string(second)), sha256Hex(second)); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	p, ok := c.Get("v0.21.8", "linux", "amd64")
	if !ok {
		t.Fatal("Get ok=false after overwrite")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(second) {
		t.Fatalf("content = %q, want %q", got, second)
	}
}

func TestFileCLICacheNewFailsOnBadDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := NewFileCLICache(file); err == nil {
		t.Fatal("NewFileCLICache = nil error, want failure")
	}
}

func TestFileCLICacheGetSidecarWithoutFile(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	// Sidecar exists but the tarball is missing.
	if err := os.WriteFile(filepath.Join(c.Dir(), domain.AssetFilename("v0.21.8", "linux", "amd64")+".sha256"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if _, ok := c.Get("v0.21.8", "linux", "amd64"); ok {
		t.Fatal("Get ok=true with sidecar but no file")
	}
}

// errReader fails after yielding some bytes, exercising the Put copy-error path.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) {
	return 0, errors.New("boom")
}

func TestFileCLICachePutCopyError(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	_, err = c.Put("v0.21.8", "linux", "amd64", errReader{}, "deadbeef")
	if err == nil {
		t.Fatal("Put = nil, want copy error")
	}
}

// zeroReader yields zeros forever (used to overflow the size cap).
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestFileCLICachePutOversized(t *testing.T) {
	old := maxCLITarballBytes
	maxCLITarballBytes = 1024
	defer func() { maxCLITarballBytes = old }()

	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	_, err = c.Put("v0.21.8", "linux", "amd64", zeroReader{}, "deadbeef")
	if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
	}

	// No final file, no sidecar, no leftover temp files.
	entries, err := os.ReadDir(c.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp-") {
			t.Fatalf("leftover temp file %q", e.Name())
		}
		if strings.Contains(e.Name(), "v0.21.8") {
			t.Fatalf("unexpected artifact file %q", e.Name())
		}
	}
}

func TestFileCLICachePutCreateTempError(t *testing.T) {
	c := &FileCLICache{dir: "/nonexistent/cli-cache"}
	if _, err := c.Put("v0.21.8", "linux", "amd64", strings.NewReader("data"), "deadbeef"); err == nil {
		t.Fatal("Put = nil, want create-temp error")
	}
}

func TestFileCLICachePutSidecarWriteError(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	// Make the sidecar path a directory so writing it fails.
	if err := os.Mkdir(c.sidecarPath("v0.21.8", "linux", "amd64"), 0o755); err != nil {
		t.Fatalf("mkdir sidecar path: %v", err)
	}
	data := []byte("data")
	_, err = c.Put("v0.21.8", "linux", "amd64", strings.NewReader(string(data)), sha256Hex(data))
	if err == nil {
		t.Fatal("Put = nil, want sidecar write error")
	}
}

func TestFileCLICachePutRenameError(t *testing.T) {
	c, err := NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	// Make the final path a directory so the atomic rename fails.
	if err := os.Mkdir(c.path("v0.21.8", "linux", "amd64"), 0o755); err != nil {
		t.Fatalf("mkdir final path: %v", err)
	}
	data := []byte("data")
	_, err = c.Put("v0.21.8", "linux", "amd64", strings.NewReader(string(data)), sha256Hex(data))
	if err == nil {
		t.Fatal("Put = nil, want rename error")
	}
}

func TestFileCLICacheCleanupTempsSkipsDirsAndMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tmp-dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orphan := filepath.Join(dir, "tmp-file")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	if _, err := NewFileCLICache(dir); err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("tmp-file still present")
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp-dir")); err != nil {
		t.Fatal("tmp-dir removed (should be skipped)")
	}

	// Missing dir path returns silently.
	(&FileCLICache{dir: "/nonexistent/x"}).cleanupTemps()
}

func TestHashFileMissing(t *testing.T) {
	if _, err := hashFile("/nonexistent/x"); err == nil {
		t.Fatal("hashFile = nil error for missing file")
	}
}
