package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// stubCLIRegistryClient is an in-memory OCI registry client for tests.
type stubCLIRegistryClient struct {
	blobs    map[string][]byte          // digest -> raw bytes
	manifests map[string]*domain.CLIManifest // tag -> manifest
	putBlobFn func(repo string, body io.Reader) (digest string, size int64, err error)
	getBlobFn func(repo, digest string) (io.ReadCloser, int64, error)
	err      error
}

func newStubCLIRegistryClient() *stubCLIRegistryClient {
	return &stubCLIRegistryClient{
		blobs:     make(map[string][]byte),
		manifests: make(map[string]*domain.CLIManifest),
	}
}

func (s *stubCLIRegistryClient) ManifestExists(ctx context.Context, repo, tag string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	_, ok := s.manifests[tag]
	return ok, nil
}

func (s *stubCLIRegistryClient) GetManifest(ctx context.Context, repo, tag string) (*domain.CLIManifest, error) {
	if s.err != nil {
		return nil, s.err
	}
	m, ok := s.manifests[tag]
	if !ok {
		return nil, domain.ErrManifestNotFound
	}
	return m, nil
}

func (s *stubCLIRegistryClient) GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error) {
	if s.getBlobFn != nil {
		return s.getBlobFn(repo, digest)
	}
	if s.err != nil {
		return nil, 0, s.err
	}
	data, ok := s.blobs[digest]
	if !ok {
		return nil, 0, domain.ErrManifestNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (s *stubCLIRegistryClient) UploadBlob(ctx context.Context, repo string, body io.Reader) (string, int64, error) {
	if s.putBlobFn != nil {
		return s.putBlobFn(repo, body)
	}
	if s.err != nil {
		return "", 0, s.err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", 0, err
	}
	sum := sha256HexBytes(data)
	digest := "sha256:" + sum
	s.blobs[digest] = data
	return digest, int64(len(data)), nil
}

func (s *stubCLIRegistryClient) PutManifest(ctx context.Context, repo, tag string, manifest *domain.CLIManifest) error {
	if s.err != nil {
		return s.err
	}
	s.manifests[tag] = manifest
	return nil
}

func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sha256Hex is an alias for sha256HexBytes, matching the name used in other test files.
var sha256Hex = sha256HexBytes

func TestRegistryCLICachePutGetRoundTrip(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	data := []byte("tarball-content-for-testing")
	rc := io.NopCloser(bytes.NewReader(data))
	path, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", rc, sha256Hex(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if path != "" {
		t.Fatalf("Put returned path=%q, want empty", path)
	}

	ok, err := cache.Has(context.Background(), "v0.21.8", "linux", "amd64")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !ok {
		t.Fatal("Has = false, want true")
	}

	gotPath, gotOK := cache.Get(context.Background(), "v0.21.8", "linux", "amd64")
	if !gotOK {
		t.Fatal("Get returned ok=false, want true")
	}
	defer func() { _ = os.Remove(gotPath) }()

	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("body = %q, want %q", got, data)
	}
}

func TestRegistryCLICachePutChecksumMismatch(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	data := []byte("tarball-content")
	rc := io.NopCloser(bytes.NewReader(data))
	_, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", rc, "deadbeef")
	if !errors.Is(err, domain.ErrCLIChecksumMismatch) {
		t.Fatalf("err = %v, want ErrCLIChecksumMismatch", err)
	}
	// No manifest should have been created.
	if len(stub.manifests) != 0 {
		t.Fatalf("expected no manifests, got %d", len(stub.manifests))
	}
}

func TestRegistryCLICacheHasMissing(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	ok, err := cache.Has(context.Background(), "v0.21.8", "linux", "amd64")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if ok {
		t.Fatal("Has = true, want false")
	}
}

func TestRegistryCLICacheGetMissing(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	_, ok := cache.Get(context.Background(), "v0.21.8", "linux", "amd64")
	if ok {
		t.Fatal("Get returned ok=true, want false")
	}
}

func TestRegistryCLICacheHasError(t *testing.T) {
	stub := newStubCLIRegistryClient()
	stub.err = errors.New("registry down")
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	_, err := cache.Has(context.Background(), "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("Has = nil, want error")
	}
}

func TestRegistryCLICacheGetError(t *testing.T) {
	stub := newStubCLIRegistryClient()
	stub.err = errors.New("registry down")
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	_, ok := cache.Get(context.Background(), "v0.21.8", "linux", "amd64")
	if ok {
		t.Fatal("Get returned ok=true, want false on error")
	}
}

func TestRegistryCLICachePutUploadError(t *testing.T) {
	stub := newStubCLIRegistryClient()
	stub.err = errors.New("upload failed")
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	data := []byte("tarball-content")
	rc := io.NopCloser(bytes.NewReader(data))
	_, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", rc, sha256Hex(data))
	if err == nil {
		t.Fatal("Put = nil, want error")
	}
}

func TestRegistryCLICachePutManifestError(t *testing.T) {
	stub := newStubCLIRegistryClient()
	// Upload succeeds but manifest put fails.
	stub.err = errors.New("manifest put failed")
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	data := []byte("tarball-content")
	rc := io.NopCloser(bytes.NewReader(data))
	_, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", rc, sha256Hex(data))
	if err == nil {
		t.Fatal("Put = nil, want error")
	}
}

func TestRegistryCLICacheDir(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", "/tmp", observ.NewTestLogger())
	if got := cache.Dir(); got != "" {
		t.Fatalf("Dir() = %q, want empty", got)
	}
}

func TestRegistryCLICacheTagFormat(t *testing.T) {
	if got := tagFor("v0.21.8", "linux", "amd64"); got != "v0.21.8-linux-amd64" {
		t.Fatalf("tagFor = %q, want v0.21.8-linux-amd64", got)
	}
	if got := tagFor("v0.21.0", "darwin", "arm64"); got != "v0.21.0-darwin-arm64" {
		t.Fatalf("tagFor = %q, want v0.21.0-darwin-arm64", got)
	}
}

func TestRegistryCLICacheGetReturnsCorrectContent(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	// Manually populate the stub to simulate a pre-existing artifact.
	data := []byte("pre-existing-tarball-data")
	sum := sha256HexBytes(data)
	digest := "sha256:" + sum
	stub.blobs[digest] = data
	tag := tagFor("v0.21.8", "linux", "amd64")
	stub.manifests[tag] = &domain.CLIManifest{
		SchemaVersion: 2,
		MediaType:     domain.MediaTypeOCIImageManifest,
		Layers: []domain.CLIManifestLayer{{
			MediaType: domain.MediaTypeOCILayerGzip,
			Digest:    digest,
			Size:      int64(len(data)),
		}},
	}

	gotPath, ok := cache.Get(context.Background(), "v0.21.8", "linux", "amd64")
	if !ok {
		t.Fatal("Get returned ok=false, want true")
	}
	defer func() { _ = os.Remove(gotPath) }()

	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("body = %q, want %q", got, data)
	}
}

func TestRegistryCLICachePutAnnotations(t *testing.T) {
	stub := newStubCLIRegistryClient()
	cache := NewRegistryCLICache(stub, "test-repo", t.TempDir(), observ.NewTestLogger())

	data := []byte("tarball-content")
	rc := io.NopCloser(bytes.NewReader(data))
	_, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", rc, sha256Hex(data))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	tag := tagFor("v0.21.8", "linux", "amd64")
	m, ok := stub.manifests[tag]
	if !ok {
		t.Fatal("manifest not found in stub")
	}
	if m.Annotations == nil {
		t.Fatal("annotations nil")
	}
	if m.Annotations["com.dagger-kubernetes.cli.sha256"] != sha256Hex(data) {
		t.Fatalf("sha256 annotation = %q", m.Annotations["com.dagger-kubernetes.cli.sha256"])
	}
	if m.Annotations["com.dagger-kubernetes.cli.version"] != "v0.21.8" {
		t.Fatalf("version annotation = %q", m.Annotations["com.dagger-kubernetes.cli.version"])
	}
	if m.Annotations["com.dagger-kubernetes.cli.filename"] != "dagger_v0.21.8_linux_amd64.tar.gz" {
		t.Fatalf("filename annotation = %q", m.Annotations["com.dagger-kubernetes.cli.filename"])
	}

	// Config descriptor must reference the canonical empty JSON blob.
	const emptyJSONDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if m.Config.Digest != emptyJSONDigest {
		t.Fatalf("config digest = %q, want empty JSON digest", m.Config.Digest)
	}
	if m.Config.Size != 2 {
		t.Fatalf("config size = %d, want 2", m.Config.Size)
	}
	if m.Config.MediaType != domain.MediaTypeOCIEmptyJSON {
		t.Fatalf("config media type = %q", m.Config.MediaType)
	}

	// Layer descriptor must reference the blob digest (not empty JSON).
	if len(m.Layers) == 0 {
		t.Fatal("no layers")
	}
	if m.Layers[0].MediaType != domain.MediaTypeOCILayerGzip {
		t.Fatalf("layer media type = %q", m.Layers[0].MediaType)
	}
	expectedBlobDigest := "sha256:" + sha256Hex(data)
	if m.Layers[0].Digest != expectedBlobDigest {
		t.Fatalf("layer digest = %q, want %q", m.Layers[0].Digest, expectedBlobDigest)
	}
}
