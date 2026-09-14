package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

func TestWalkContentStore(t *testing.T) {
	worker := t.TempDir()
	// Both engine layouts are walked: the flat content store
	// (content/blobs/sha256/<hex>, Dagger v0.19+) and the legacy sharded one
	// (content/blobs/sha256/<2>/<hex>), plus a stray non-blob file.
	flatHex := strings.Repeat("a", 64)
	shardedHex := strings.Repeat("0", 63) + "f"
	writeTestFile(t, filepath.Join(worker, "content", "blobs", "sha256", flatHex), 0o644, "flat")
	writeTestFile(t, filepath.Join(worker, "content", "blobs", "sha256", shardedHex[:2], shardedHex), 0o644, "sharded")
	writeTestFile(t, filepath.Join(worker, "content", "blobs", "sha256", "aa", "temp-file"), 0o644, "stray")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")

	blobs, err := walkContentStore(worker)
	if err != nil {
		t.Fatalf("walkContentStore: %v", err)
	}
	want := []contentBlob{
		{Digest: "sha256:" + shardedHex, RelPath: filepath.Join("content", "blobs", "sha256", shardedHex[:2], shardedHex)},
		{Digest: "sha256:" + flatHex, RelPath: filepath.Join("content", "blobs", "sha256", flatHex)},
	}
	if len(blobs) != len(want) {
		t.Fatalf("blobs = %+v, want %+v", blobs, want)
	}
	for i := range want {
		if blobs[i] != want[i] {
			t.Fatalf("blobs[%d] = %+v, want %+v (sorted by digest)", i, blobs[i], want[i])
		}
	}
}

func TestWalkContentStoreEmpty(t *testing.T) {
	worker := t.TempDir()
	blobs, err := walkContentStore(worker)
	if err != nil {
		t.Fatalf("walkContentStore (missing store): %v", err)
	}
	if len(blobs) != 0 {
		t.Fatalf("blobs = %+v, want empty", blobs)
	}
	if err := os.MkdirAll(filepath.Join(worker, "content", "blobs", "sha256"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	blobs, err = walkContentStore(worker)
	if err != nil {
		t.Fatalf("walkContentStore (empty store): %v", err)
	}
	if len(blobs) != 0 {
		t.Fatalf("blobs = %+v, want empty", blobs)
	}
}

// blobContentFile creates a content-addressed blob under the worker dir's
// content store in the real flat layout (Dagger v0.19+:
// content/blobs/sha256/<hex>) and returns its digest.
func blobContentFile(t *testing.T, worker, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	hexStr := hex.EncodeToString(sum[:])
	path := filepath.Join(worker, "content", "blobs", "sha256", hexStr)
	writeTestFile(t, path, 0o644, content)
	return "sha256:" + hexStr
}

func TestProbeAndUploadMissing(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	worker := t.TempDir()
	missing := blobContentFile(t, worker, "new-blob")
	existing := blobContentFile(t, worker, "already-there")
	stub.blobs[existing] = []byte("already-there")
	blobs, err := walkContentStore(worker)
	if err != nil {
		t.Fatalf("walkContentStore: %v", err)
	}

	layers, err := probeAndUploadMissing(context.Background(), stub, "repo", worker, blobs, 2, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("probeAndUploadMissing: %v", err)
	}
	if len(layers) != 2 {
		t.Fatalf("layers = %d, want 2 (existing + newly uploaded)", len(layers))
	}
	for _, l := range layers {
		if l.MediaType != domain.MediaTypeOCIRawBlob {
			t.Fatalf("media type = %q, want raw blob", l.MediaType)
		}
	}
	if _, ok := stub.blobs[missing]; !ok {
		t.Fatal("missing blob was not uploaded")
	}
}

func TestProbeAndUploadMissingFileVanished(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	worker := t.TempDir()
	gone := blobContentFile(t, worker, "soon-gone")
	hexGone := strings.TrimPrefix(gone, "sha256:")
	relGone := filepath.Join("content", "blobs", "sha256", hexGone)
	if err := os.Remove(filepath.Join(worker, relGone)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	layers, err := probeAndUploadMissing(context.Background(), stub, "repo", worker, []contentBlob{{Digest: gone, RelPath: relGone}}, 1, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("probeAndUploadMissing: %v (GC'd blob must be skipped silently)", err)
	}
	if len(layers) != 0 {
		t.Fatalf("layers = %v, want none", layers)
	}
}

func TestProbeAndUploadMissingProbeError(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	stub.err = errors.New("registry down")
	worker := t.TempDir()
	digest := blobContentFile(t, worker, "x1")
	blobs := []contentBlob{{Digest: digest, RelPath: filepath.Join("content", "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))}}

	if _, err := probeAndUploadMissing(context.Background(), stub, "repo", worker, blobs, 1, observ.NewTestLogger()); err == nil {
		t.Fatal("probe error must fail the whole push")
	}
}

func TestProbeAndUploadMissingUploadError(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	stub.uploadStreamErr = errors.New("upload failed")
	worker := t.TempDir()
	digest := blobContentFile(t, worker, "x2")
	blobs := []contentBlob{{Digest: digest, RelPath: filepath.Join("content", "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))}}

	layers, err := probeAndUploadMissing(context.Background(), stub, "repo", worker, blobs, 1, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("probeAndUploadMissing: %v (upload errors are best-effort skips)", err)
	}
	if len(layers) != 0 {
		t.Fatalf("layers = %v, want none (failed blob excluded)", layers)
	}
}

func TestDownloadMissingBlobs(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	dst := t.TempDir()

	present := blobContentFile(t, dst, "already-local")
	absentDigest := "sha256:" + strings.Repeat("b", 64)
	stub.blobs[absentDigest] = []byte("from-registry")
	missingDigest := "sha256:" + strings.Repeat("c", 64) // not in the registry at all
	blobs := []contentBlob{{Digest: present}, {Digest: absentDigest}, {Digest: missingDigest}}

	if err := downloadMissingBlobs(context.Background(), stub, "repo", dst, blobs, observ.NewTestLogger()); err != nil {
		t.Fatalf("downloadMissingBlobs: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "content", "blobs", "sha256", strings.TrimPrefix(present, "sha256:")))
	if err != nil {
		t.Fatalf("read pre-existing blob: %v", err)
	}
	if string(got) != "already-local" {
		t.Fatalf("pre-existing blob overwritten: %q", got)
	}
	got, err = os.ReadFile(filepath.Join(dst, "content", "blobs", "sha256", strings.Repeat("b", 64)))
	if err != nil {
		t.Fatalf("read downloaded blob: %v", err)
	}
	if string(got) != "from-registry" {
		t.Fatalf("downloaded blob = %q", got)
	}
}

func TestDownloadMissingBlobsInvalidDigest(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	blobs := []contentBlob{{Digest: "not-a-digest"}}
	if err := downloadMissingBlobs(context.Background(), stub, "repo", t.TempDir(), blobs, observ.NewTestLogger()); err != nil {
		t.Fatalf("invalid digest must be skipped, got error: %v", err)
	}
}

func TestBuildMultiLayerManifest(t *testing.T) {
	blobs := []domain.CLIManifestLayer{
		{MediaType: domain.MediaTypeOCIRawBlob, Digest: digestRepeat("a"), Size: 3},
		{MediaType: domain.MediaTypeOCIRawBlob, Digest: digestRepeat("b"), Size: 4},
	}
	m := buildMultiLayerManifest(digestRepeat("0"), 42, blobs)

	if m.SchemaVersion != 2 || m.MediaType != domain.MediaTypeOCIImageManifest {
		t.Fatalf("manifest header = %d/%s", m.SchemaVersion, m.MediaType)
	}
	if m.Config.Digest != emptyJSONDigest || m.Config.Size != emptyJSONSize {
		t.Fatalf("config = %+v", m.Config)
	}
	if len(m.Layers) != 3 {
		t.Fatalf("layers = %d, want 3", len(m.Layers))
	}
	if m.Layers[0].MediaType != domain.MediaTypeOCILayerGzip || m.Layers[0].Digest != digestRepeat("0") || m.Layers[0].Size != 42 {
		t.Fatalf("meta layer = %+v", m.Layers[0])
	}
	for i, want := range blobs {
		if m.Layers[i+1] != want {
			t.Fatalf("layer[%d] = %+v, want %+v", i+1, m.Layers[i+1], want)
		}
	}
}

// TestPushIncrementalPullIncrementalRoundTrip is the end-to-end v2 flow:
// push a worker dir with metadata + content blobs, pull into a fresh dir,
// verify tree equality.
func TestPushIncrementalPullIncrementalRoundTrip(t *testing.T) {
	stub := &stubSnapshotClient{stubCLIRegistryClient: *newStubCLIRegistryClient()}
	store := NewWorkerSnapshotStore(stub, "test-repo", observ.NewTestLogger())

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	writeTestFile(t, filepath.Join(src, "worker", "snapshots", "1"), 0o755, "snapshot")
	blobContentFile(t, filepath.Join(src, "worker"), "blob-aa")
	blobContentFile(t, filepath.Join(src, "worker"), "blob-bb")

	tag := domain.WorkerSnapshotTagV2("v0.20.0")
	if err := store.PushIncremental(context.Background(), tag, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("PushIncremental: %v", err)
	}

	dst := t.TempDir()
	ok, err := store.PullIncremental(context.Background(), tag, dst, "worker")
	if err != nil {
		t.Fatalf("PullIncremental: %v", err)
	}
	if !ok {
		t.Fatal("PullIncremental ok=false, want true")
	}
	assertTreeEqual(t, src, dst)

	// A second push uploads nothing new (all blobs probed as present).
	before := len(stub.blobs)
	if err := store.PushIncremental(context.Background(), tag, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("second PushIncremental: %v", err)
	}
	if len(stub.blobs) != before {
		t.Fatalf("second push uploaded %d new blobs, want 0", len(stub.blobs)-before)
	}
}

func TestWorkerSnapshotTagV2Format(t *testing.T) {
	if got := domain.WorkerSnapshotTagV2("v0.20.0"); got != "worker-v2-v0-20-0" {
		t.Fatalf("WorkerSnapshotTagV2 = %q, want worker-v2-v0-20-0", got)
	}
	if got := domain.WorkerSnapshotTagV2("v0-20-0"); got != "worker-v2-v0-20-0" {
		t.Fatalf("WorkerSnapshotTagV2(slug) = %q (VersionSlug is idempotent)", got)
	}
	if got := domain.WorkerSnapshotVersionSlug("worker-v2-v0-20-0"); got != "v0-20-0" {
		t.Fatalf("WorkerSnapshotVersionSlug(v2 tag) = %q", got)
	}
	if got := domain.WorkerSnapshotVersionSlug("worker-v0-20-0"); got != "v0-20-0" {
		t.Fatalf("WorkerSnapshotVersionSlug(legacy tag) = %q", got)
	}
}
