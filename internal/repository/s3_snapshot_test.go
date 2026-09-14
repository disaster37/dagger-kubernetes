package repository

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// newTestS3SnapshotStore builds an S3SnapshotStore backed by the in-memory
// mock (the constructor's *minio.Client wrapper is exercised by the
// integration test against a real S3 endpoint).
func newTestS3SnapshotStore(mock *mockS3ObjectStore, version string) *S3SnapshotStore {
	return &S3SnapshotStore{
		client:       mock,
		bucket:       "test-bucket",
		prefix:       domain.WorkerSnapshotS3Prefix(version),
		workerSubdir: "worker",
		logger:       observ.NewTestLogger(),
	}
}

func TestS3PushPullRoundTrip(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt-metadata")
	writeTestFile(t, filepath.Join(src, "worker", "snapshots", "1"), 0o755, "snapshot")
	blobContentFile(t, filepath.Join(src, "worker"), "blob-aa")
	blobContentFile(t, filepath.Join(src, "worker"), "blob-bb")

	ctx := context.Background()
	if err := store.Push(ctx, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Storage layout: meta.tar.gz + one object per blob under blobs/sha256/.
	keys := mock.keys()
	prefix := domain.WorkerSnapshotS3Prefix("v0.20.0")
	metaKey := prefix + domain.MetaTarballName
	foundMeta, foundBlobs := false, 0
	for _, k := range keys {
		switch {
		case k == metaKey:
			foundMeta = true
		case strings.HasPrefix(k, prefix+domain.BlobsPrefix):
			foundBlobs++
		default:
			t.Fatalf("unexpected object key %q", k)
		}
	}
	if !foundMeta || foundBlobs != 2 {
		t.Fatalf("layout: meta=%v blobs=%d (keys %v)", foundMeta, foundBlobs, keys)
	}

	dst := t.TempDir()
	ok, err := store.Pull(ctx, dst)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !ok {
		t.Fatal("Pull ok=false, want true")
	}
	assertTreeEqual(t, src, dst, "worker")
}

func TestS3PullNoSnapshots(t *testing.T) {
	// A version prefix no pod ever pushed to (also exercises prefix slugging).
	store := newTestS3SnapshotStore(newMockS3ObjectStore(), "v9.9.9")
	ok, err := store.Pull(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if ok {
		t.Fatal("Pull ok=true, want false for an empty bucket")
	}
}

func TestS3CleanupSelf(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt")
	blobContentFile(t, filepath.Join(src, "worker"), "blob-aa")
	if err := store.Push(context.Background(), filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(mock.keys()) == 0 {
		t.Fatal("push stored nothing")
	}

	if err := store.CleanupSelf(context.Background()); err != nil {
		t.Fatalf("CleanupSelf: %v", err)
	}
	if got := mock.keys(); len(got) != 0 {
		t.Fatalf("prefix not empty after CleanupSelf: %v", got)
	}
}

func TestS3PushSkipsGCdBlob(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "worker", "metadata.db"), 0o600, "bolt")
	worker := filepath.Join(src, "worker")
	digest := blobContentFile(t, worker, "soon-gone")
	hex := strings.TrimPrefix(digest, "sha256:")
	if err := os.Remove(filepath.Join(worker, "content", "blobs", "sha256", hex[:2], hex)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// The vanished blob must not fail the push: meta.tar.gz still lands.
	if err := store.Push(context.Background(), worker, t.TempDir()); err != nil {
		t.Fatalf("Push: %v (GC'd blob must be skipped silently)", err)
	}
	metaKey := domain.WorkerSnapshotS3Prefix("v0.20.0") + domain.MetaTarballName
	if _, ok := mock.objects[metaKey]; !ok {
		t.Fatal("meta.tar.gz not pushed")
	}
}

func TestS3PullSkipsMissingBlob(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")
	digest := blobContentFile(t, worker, "blob-aa")

	ctx := context.Background()
	if err := store.Push(ctx, worker, t.TempDir()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// The lifecycle policy reclaimed the blob; pull must skip it.
	hex := strings.TrimPrefix(digest, "sha256:")
	delete(mock.objects, domain.WorkerSnapshotS3Prefix("v0.20.0")+domain.BlobsPrefix+hex[:2]+"/"+hex)

	dst := t.TempDir()
	ok, err := store.Pull(ctx, dst)
	if err != nil {
		t.Fatalf("Pull: %v (missing blob must be skipped)", err)
	}
	if !ok {
		t.Fatal("Pull ok=false, want true")
	}
	got, err := os.ReadFile(filepath.Join(dst, "worker", "metadata.db"))
	if err != nil || string(got) != "bolt" {
		t.Fatalf("metadata.db = %q/%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "worker", "content", "blobs", "sha256", hex[:2], hex)); !os.IsNotExist(err) {
		t.Fatalf("missing blob must not be created: %v", err)
	}
}

func TestS3PullSkipsExistingBlob(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")
	digest := blobContentFile(t, worker, "blob-aa")
	if err := store.Push(context.Background(), worker, t.TempDir()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	dst := t.TempDir()
	hex := strings.TrimPrefix(digest, "sha256:")
	preCreated := filepath.Join(dst, "worker", "content", "blobs", "sha256", hex[:2], hex)
	writeTestFile(t, preCreated, 0o600, "locally-newer-content")

	if ok, err := store.Pull(context.Background(), dst); err != nil || !ok {
		t.Fatalf("Pull = %v/%v, want true/nil", ok, err)
	}
	got, err := os.ReadFile(preCreated)
	if err != nil {
		t.Fatalf("read pre-created blob: %v", err)
	}
	if string(got) != "locally-newer-content" {
		t.Fatalf("pre-created blob was re-downloaded: %q", got)
	}
}

func TestS3ConcurrentPushIdempotent(t *testing.T) {
	// Two pods push to the same version prefix concurrently: blob uploads
	// are idempotent (content-addressed keys) and meta.tar.gz is a valid
	// last-writer-wins object.
	mock := newMockS3ObjectStore()
	const pods = 4
	dirs := make([]string, pods)
	for i := range dirs {
		dirs[i] = t.TempDir()
		writeTestFile(t, filepath.Join(dirs[i], "worker", "metadata.db"), 0o600, "bolt")
		blobContentFile(t, filepath.Join(dirs[i], "worker"), "shared-blob")
		blobContentFile(t, filepath.Join(dirs[i], "worker"), "pod-blob-"+strings.Repeat("a", i+1))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, pods)
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := newTestS3SnapshotStore(mock, "v0.20.0")
			if err := store.Push(context.Background(), filepath.Join(dirs[i], "worker"), t.TempDir()); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent push: %v", err)
	}

	// Shared blob stored once; metadata tarball + one per-pod blob + shared.
	prefix := domain.WorkerSnapshotS3Prefix("v0.20.0")
	blobs := 0
	for _, k := range mock.keys() {
		if strings.HasPrefix(k, prefix+domain.BlobsPrefix) {
			blobs++
		}
	}
	if blobs != pods+1 {
		t.Fatalf("blob objects = %d, want %d (1 shared + %d unique, content-addressed idempotency)", blobs, pods+1, pods)
	}

	// Any winner is a valid, self-consistent metadata tarball; with the S3
	// prefix-listing layout the pull restores EVERY blob under the version
	// prefix (they are all part of the shared per-version cache), so all
	// pods+1 distinct blobs come back intact.
	dst := t.TempDir()
	if ok, err := newTestS3SnapshotStore(mock, "v0.20.0").Pull(context.Background(), dst); err != nil || !ok {
		t.Fatalf("final pull = %v/%v, want true/nil", ok, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "worker", "metadata.db")); err != nil || string(got) != "bolt" {
		t.Fatalf("restored metadata.db = %q/%v", got, err)
	}
	count, err := countFiles(filepath.Join(dst, "worker", "content", "blobs", "sha256"))
	if err != nil {
		t.Fatalf("read restored content store: %v", err)
	}
	if count != pods+1 {
		t.Fatalf("restored blob count = %d, want %d (all blobs under the shared prefix)", count, pods+1)
	}
}

// countFiles counts the regular files under dir recursively.
func countFiles(dir string) (int, error) {
	count := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			count++
		}
		return nil
	})
	return count, err
}

func TestS3PushBlobUploadErrorStillPushesMeta(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")
	blobContentFile(t, worker, "blob-aa")

	// Simulate a transient S3 failure for blob uploads only (the meta PUT
	// happens after the blob loop, so it succeeds).
	store.client = &failingPutS3{mockS3ObjectStore: mock, failFirst: 1}

	if err := store.Push(context.Background(), worker, t.TempDir()); err != nil {
		t.Fatalf("Push: %v (blob upload failures must not fail the push)", err)
	}
	metaKey := domain.WorkerSnapshotS3Prefix("v0.20.0") + domain.MetaTarballName
	if _, ok := mock.objects[metaKey]; !ok {
		t.Fatal("meta.tar.gz not pushed despite the blob upload failure")
	}
}

// failingPutS3 fails the first N PutObject calls (e.g. blob uploads) and then
// delegates to the underlying store (so the meta.tar.gz PUT succeeds).
type failingPutS3 struct {
	*mockS3ObjectStore
	mu        sync.Mutex
	failFirst int
}

func (f *failingPutS3) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	f.mu.Lock()
	if f.failFirst > 0 {
		f.failFirst--
		f.mu.Unlock()
		return minio.UploadInfo{}, errUploadFailed
	}
	f.mu.Unlock()
	return f.mockS3ObjectStore.PutObject(ctx, bucketName, objectName, reader, objectSize, opts)
}

var errUploadFailed = errMockUploadFailed{}

type errMockUploadFailed struct{}

func (errMockUploadFailed) Error() string { return "upload failed" }
