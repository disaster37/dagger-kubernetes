package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// stubSnapshotClient wraps stubCLIRegistryClient with the streaming upload the
// WorkerSnapshotStore needs. Like a real registry, it stores the body at the
// announced digest and rejects size/digest mismatches. Safe for the concurrent
// probe/upload worker pool.
type stubSnapshotClient struct {
	stubCLIRegistryClient
	mu              sync.Mutex
	uploadStreamErr error
	putManifestErr  error
}

func (s *stubSnapshotClient) UploadBlobStream(ctx context.Context, repo, digest string, size int64, body io.Reader) error {
	if s.err != nil {
		return s.err
	}
	if s.uploadStreamErr != nil {
		return s.uploadStreamErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("size mismatch: got %d, want %d", len(data), size)
	}
	if got := "sha256:" + sha256HexBytes(data); got != digest {
		return fmt.Errorf("digest mismatch: got %s, want %s", got, digest)
	}
	s.mu.Lock()
	s.blobs[digest] = data
	s.mu.Unlock()
	return nil
}

func (s *stubSnapshotClient) ProbeBlob(ctx context.Context, repo, digest string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.blobs[digest]
	return ok, nil
}

func (s *stubSnapshotClient) PutManifest(ctx context.Context, repo, tag string, manifest *domain.CLIManifest) error {
	if s.putManifestErr != nil {
		return s.putManifestErr
	}
	return s.stubCLIRegistryClient.PutManifest(ctx, repo, tag, manifest)
}

var _ domain.CacheSnapshotClient = (*stubSnapshotClient)(nil)

// pushTestSnapshot tars src/worker and pushes it via the store.
func pushTestSnapshot(t *testing.T, store *WorkerSnapshotStore, src, tag string) {
	t.Helper()
	digest, size, compressed := tarGzipDirInto(t, src)
	if err := store.Push(context.Background(), tag, bytes.NewReader(compressed), digest, size); err != nil {
		t.Fatalf("Push: %v", err)
	}
}

// assertTreeEqual compares every file (path → content) under both dirs.
func assertTreeEqual(t *testing.T, want, got, subDir string) {
	t.Helper()
	wantFiles := map[string]string{}
	if err := filepath.WalkDir(filepath.Join(want, subDir), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(want, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		wantFiles[rel] = string(b)
		return nil
	}); err != nil {
		t.Fatalf("walk want: %v", err)
	}

	gotFiles := map[string]string{}
	if err := filepath.WalkDir(filepath.Join(got, subDir), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(got, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		gotFiles[rel] = string(b)
		return nil
	}); err != nil {
		t.Fatalf("walk got: %v", err)
	}

	if len(gotFiles) != len(wantFiles) {
		t.Fatalf("file count = %d, want %d", len(gotFiles), len(wantFiles))
	}
	for rel, wantContent := range wantFiles {
		if gotFiles[rel] != wantContent {
			t.Fatalf("%s = %q, want %q", rel, gotFiles[rel], wantContent)
		}
	}
}

func TestWorkerSnapshotPushPullRoundTrip(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	writeTestFile(t, filepath.Join(src, "worker", "content", "abc"), 0o644, "blob-bytes")

	tag := domain.WorkerSnapshotTag("v0.20.0")
	pushTestSnapshot(t, store, src, tag)

	dst := t.TempDir()
	ok, err := store.Pull(context.Background(), tag, dst)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !ok {
		t.Fatal("Pull returned ok=false, want true")
	}
	assertTreeEqual(t, src, dst, "worker")

	// Manifest shape mirrors the CLI cache: empty-JSON config + one gzip layer.
	m, okM := stub.manifests[tag]
	if !okM {
		t.Fatal("manifest not stored")
	}
	if m.Config.MediaType != domain.MediaTypeOCIEmptyJSON || m.Config.Digest != emptyJSONDigest || m.Config.Size != emptyJSONSize {
		t.Fatalf("config = %+v, want the canonical empty-JSON descriptor", m.Config)
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != domain.MediaTypeOCILayerGzip {
		t.Fatalf("layers = %+v, want one gzip layer", m.Layers)
	}
}

func TestWorkerSnapshotPullMissing(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	ok, err := store.Pull(context.Background(), domain.WorkerSnapshotTag("v0.20.0"), t.TempDir())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if ok {
		t.Fatal("Pull returned ok=true, want false for a missing snapshot")
	}
}

func TestWorkerSnapshotPushUploadError(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	stub.err = errors.New("registry down")
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "a"), 0o600, "x")
	digest, size, compressed := tarGzipDirInto(t, src)

	if err := store.Push(context.Background(), "worker-v0-20-0", bytes.NewReader(compressed), digest, size); err == nil {
		t.Fatal("Push = nil, want error")
	}
	if len(stub.manifests) != 0 {
		t.Fatalf("expected no manifests, got %d", len(stub.manifests))
	}
}

func TestWorkerSnapshotPushManifestError(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	stub.putManifestErr = errors.New("manifest put failed")
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "a"), 0o600, "x")
	digest, size, compressed := tarGzipDirInto(t, src)

	if err := store.Push(context.Background(), "worker-v0-20-0", bytes.NewReader(compressed), digest, size); err == nil {
		t.Fatal("Push = nil, want error")
	}
}

func TestWorkerSnapshotPushDigestMismatch(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "a"), 0o600, "x")

	// Announce a wrong digest: the (stub) registry must reject it.
	if err := store.Push(context.Background(), "worker-v0-20-0", bytes.NewReader([]byte("compressed")), digestRepeat("0"), 11); err == nil {
		t.Fatal("Push = nil, want digest-mismatch error")
	}
	if len(stub.manifests) != 0 {
		t.Fatalf("expected no manifests, got %d", len(stub.manifests))
	}
}

func TestWorkerSnapshotTagFormat(t *testing.T) {
	if got := domain.WorkerSnapshotTag("v0.20.0"); got != "worker-v0-20-0" {
		t.Fatalf("WorkerSnapshotTag = %q, want worker-v0-20-0", got)
	}
	if got := domain.WorkerSnapshotTag("v0-20-0"); got != "worker-v0-20-0" {
		t.Fatalf("WorkerSnapshotTag(slug) = %q, want worker-v0-20-0 (VersionSlug is idempotent)", got)
	}
	if domain.WorkerSnapshotsRepo != "dagger-cache/worker-snapshots" {
		t.Fatalf("WorkerSnapshotsRepo = %q", domain.WorkerSnapshotsRepo)
	}
}

// TestPushIncrementalMultiLayer verifies the v2 manifest shape: layer 0 is the
// gzip metadata tarball (worker dir minus content/blobs/), layers 1..N are the
// raw content blobs sorted by digest.
func TestPushIncrementalMultiLayer(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	digestA := blobContentFile(t, filepath.Join(src, "worker"), "blob-aa")
	digestB := blobContentFile(t, filepath.Join(src, "worker"), "blob-bb")

	tag := domain.WorkerSnapshotTagV2("v0.20.0")
	if err := store.PushIncremental(context.Background(), tag, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("PushIncremental: %v", err)
	}

	m, ok := stub.manifests[tag]
	if !ok {
		t.Fatal("manifest not stored")
	}
	if m.Config.MediaType != domain.MediaTypeOCIEmptyJSON || m.Config.Digest != emptyJSONDigest {
		t.Fatalf("config = %+v, want the canonical empty-JSON descriptor", m.Config)
	}
	if len(m.Layers) != 3 {
		t.Fatalf("layers = %d, want 3 (meta + 2 blobs)", len(m.Layers))
	}
	if m.Layers[0].MediaType != domain.MediaTypeOCILayerGzip {
		t.Fatalf("layer0 media type = %q, want gzip", m.Layers[0].MediaType)
	}
	wantOrder := []string{digestA, digestB}
	if digestA > digestB {
		wantOrder = []string{digestB, digestA}
	}
	if m.Layers[1].Digest != wantOrder[0] || m.Layers[2].Digest != wantOrder[1] {
		t.Fatalf("blob layers not sorted by digest: %+v", m.Layers[1:])
	}
	for _, l := range m.Layers[1:] {
		if l.MediaType != domain.MediaTypeOCIRawBlob {
			t.Fatalf("blob media type = %q, want %q", l.MediaType, domain.MediaTypeOCIRawBlob)
		}
		if len(stub.blobs[l.Digest]) != int(l.Size) {
			t.Fatalf("layer %s size = %d, want %d", l.Digest, l.Size, len(stub.blobs[l.Digest]))
		}
	}
	// The metadata tarball must exclude the content store.
	if _, ok := stub.blobs[m.Layers[0].Digest]; !ok {
		t.Fatal("metadata tarball not uploaded")
	}
}

// TestPullIncrementalSkipsExistingBlobs verifies the pull only downloads the
// content blobs that are missing locally.
func TestPullIncrementalSkipsExistingBlobs(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	digestA := blobContentFile(t, filepath.Join(src, "worker"), "blob-aa")
	digestB := blobContentFile(t, filepath.Join(src, "worker"), "blob-bb")

	tag := domain.WorkerSnapshotTagV2("v0.20.0")
	if err := store.PushIncremental(context.Background(), tag, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("PushIncremental: %v", err)
	}

	// Pre-create one blob in the destination: it must not be re-downloaded.
	dst := t.TempDir()
	hexA := strings.TrimPrefix(digestA, "sha256:")
	preCreated := filepath.Join(dst, "worker", "content", "blobs", "sha256", hexA[:2], hexA)
	writeTestFile(t, preCreated, 0o600, "locally-newer-content")

	// Remove the other blob from the registry too: the pull must still
	// succeed (the missing blob is skipped with a DEBUG log).
	delete(stub.blobs, digestB)

	ok, err := store.PullIncremental(context.Background(), tag, dst, "worker")
	if err != nil {
		t.Fatalf("PullIncremental: %v", err)
	}
	if !ok {
		t.Fatal("PullIncremental ok=false, want true")
	}
	got, err := os.ReadFile(preCreated)
	if err != nil {
		t.Fatalf("read pre-created blob: %v", err)
	}
	if string(got) != "locally-newer-content" {
		t.Fatalf("pre-created blob was overwritten: %q", got)
	}
	restored, err := os.ReadFile(filepath.Join(dst, "worker", "metadata.db"))
	if err != nil {
		t.Fatalf("read restored metadata: %v", err)
	}
	if string(restored) != "bolt-metadata" {
		t.Fatalf("metadata = %q", restored)
	}
	hexB := strings.TrimPrefix(digestB, "sha256:")
	if _, err := os.Stat(filepath.Join(dst, "worker", "content", "blobs", "sha256", hexB[:2], hexB)); !os.IsNotExist(err) {
		t.Fatalf("missing blob should be skipped, not created: %v", err)
	}
}
