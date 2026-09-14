package repository

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt-metadata")
	writeTestFile(t, filepath.Join(worker, "snapshots", "1"), 0o755, "snapshot")
	digestAA := blobContentFile(t, worker, "blob-aa")
	digestBB := blobContentFile(t, worker, "blob-bb")

	ctx := context.Background()
	if err := store.Push(ctx, worker, t.TempDir()); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// Storage layout: meta.tar.gz + one flat object per blob
	// (blobs/sha256/<64-hex>) mirroring the real Dagger v0.19+ content store.
	keys := mock.keys()
	prefix := domain.WorkerSnapshotS3Prefix("v0.20.0")
	metaKey := prefix + domain.MetaTarballName
	wantBlobKeys := []string{
		prefix + domain.BlobsPrefix + strings.TrimPrefix(digestAA, "sha256:"),
		prefix + domain.BlobsPrefix + strings.TrimPrefix(digestBB, "sha256:"),
	}
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
	s3RequireMockObjects(t, mock, wantBlobKeys)

	dst := t.TempDir()
	ok, err := store.Pull(ctx, dst)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !ok {
		t.Fatal("Pull ok=false, want true")
	}
	assertTreeEqual(t, src, dst)
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

func TestS3PushVanishedBlobAbortsMeta(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	// A previous consistent snapshot is already published.
	metaKey := domain.WorkerSnapshotS3Prefix("v0.20.0") + domain.MetaTarballName
	mock.setObject(metaKey, []byte("previous-meta"), time.Now())

	src := t.TempDir()
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")
	digest := blobContentFile(t, worker, "soon-gone")
	hex := strings.TrimPrefix(digest, "sha256:")
	blobPath := filepath.Join(worker, "content", "blobs", "sha256", hex)
	key := domain.WorkerSnapshotS3Prefix("v0.20.0") + domain.BlobsPrefix + hex
	// BuildKit GC collects the blob after the walk saw it: the injected
	// client removes the local file when uploadBlob stats the object.
	store.client = &vanishOnStatS3{mockS3ObjectStore: mock, vanishKey: key, vanishPath: blobPath}

	// A blob the walk saw but BuildKit GC deleted must abort the metadata
	// push: the previous snapshot stays intact instead of a metadata-only
	// poisoning snapshot being published.
	if err := store.Push(context.Background(), worker, t.TempDir()); err == nil {
		t.Fatal("Push = nil, want error (vanished blob must abort the metadata push)")
	}
	got, ok := mock.objects[metaKey]
	if !ok {
		t.Fatal("previous meta.tar.gz disappeared")
	}
	if string(got.data) != "previous-meta" {
		t.Fatalf("meta.tar.gz = %q, want the previous snapshot untouched", got.data)
	}
}

// vanishOnStatS3 simulates BuildKit GC racing the push: when uploadBlob
// stats the configured blob key, the local file is deleted before os.Open.
type vanishOnStatS3 struct {
	*mockS3ObjectStore
	vanishKey  string
	vanishPath string
}

func (v *vanishOnStatS3) StatObject(ctx context.Context, bucket, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) { //nolint:gocritic // hugeParam: signature must match the minio-go API
	info, err := v.mockS3ObjectStore.StatObject(ctx, bucket, objectName, opts)
	if objectName == v.vanishKey {
		_ = os.Remove(v.vanishPath)
	}
	return info, err
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
	delete(mock.objects, domain.WorkerSnapshotS3Prefix("v0.20.0")+domain.BlobsPrefix+hex)

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
	if _, err := os.Stat(filepath.Join(dst, "worker", "content", "blobs", "sha256", hex)); !os.IsNotExist(err) {
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
	preCreated := filepath.Join(dst, "worker", "content", "blobs", "sha256", hex)
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

func TestS3PushBlobUploadErrorKeepsPreviousMeta(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	// A previous consistent snapshot is already published.
	metaKey := domain.WorkerSnapshotS3Prefix("v0.20.0") + domain.MetaTarballName
	mock.setObject(metaKey, []byte("previous-meta"), time.Now())

	src := t.TempDir()
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")
	blobContentFile(t, worker, "blob-aa")

	// Simulate a transient S3 failure for blob uploads: the metadata must
	// NOT be published (a metadata-only snapshot would poison restores).
	store.client = &failingPutS3{mockS3ObjectStore: mock, failFirst: 1}

	if err := store.Push(context.Background(), worker, t.TempDir()); err == nil {
		t.Fatal("Push = nil, want error (failed blob upload must abort the metadata push)")
	}
	got, ok := mock.objects[metaKey]
	if !ok {
		t.Fatal("previous meta.tar.gz disappeared")
	}
	if string(got.data) != "previous-meta" {
		t.Fatalf("meta.tar.gz = %q, want the previous snapshot untouched", got.data)
	}
}

// TestS3PushShardedLayoutRoundTrip proves the store is layout-independent:
// a worker dir using the legacy sharded content store
// (content/blobs/sha256/<2>/<hex>) pushes and pulls intact, with S3 keys
// mirroring the on-disk layout.
func TestS3PushShardedLayoutRoundTrip(t *testing.T) {
	mock := newMockS3ObjectStore()
	store := newTestS3SnapshotStore(mock, "v0.20.0")

	src := t.TempDir()
	worker := filepath.Join(src, "worker")
	writeTestFile(t, filepath.Join(worker, "metadata.db"), 0o600, "bolt")
	hexStr := strings.Repeat("c", 64)
	writeTestFile(t, filepath.Join(worker, "content", "blobs", "sha256", hexStr[:2], hexStr), 0o644, "sharded-blob")

	ctx := context.Background()
	if err := store.Push(ctx, worker, t.TempDir()); err != nil {
		t.Fatalf("Push: %v", err)
	}
	prefix := domain.WorkerSnapshotS3Prefix("v0.20.0")
	s3RequireMockObjects(t, mock, []string{prefix + domain.BlobsPrefix + hexStr[:2] + "/" + hexStr})

	dst := t.TempDir()
	ok, err := store.Pull(ctx, dst)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !ok {
		t.Fatal("Pull ok=false, want true")
	}
	assertTreeEqual(t, src, dst)
}

// staleListS3 lists one key that the underlying mock no longer holds,
// simulating a blob reclaimed between the S3 listing and the GetObject.
type staleListS3 struct {
	*mockS3ObjectStore
	staleKey string
}

func (s *staleListS3) ListObjects(ctx context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo { //nolint:gocritic // hugeParam: signature must match the minio-go API
	ch := make(chan minio.ObjectInfo)
	go func() {
		defer close(ch)
		if strings.HasPrefix(s.staleKey, opts.Prefix) {
			ch <- minio.ObjectInfo{Key: s.staleKey}
		}
		for info := range s.mockS3ObjectStore.ListObjects(ctx, bucket, opts) {
			ch <- info
		}
	}()
	return ch
}

// TestS3PullListedBlobVanishedFails proves a blob listed but deleted before
// its download fails the pull, so the caller discards the torn restore
// instead of restoring metadata that references a missing blob.
func TestS3PullListedBlobVanishedFails(t *testing.T) {
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

	key := domain.WorkerSnapshotS3Prefix("v0.20.0") + domain.BlobsPrefix + strings.TrimPrefix(digest, "sha256:")
	delete(mock.objects, key)
	store.client = &staleListS3{mockS3ObjectStore: mock, staleKey: key}

	ok, err := store.Pull(ctx, t.TempDir())
	if err == nil || ok {
		t.Fatalf("Pull = %v/%v, want false/error for a listed-then-deleted blob", ok, err)
	}
}

// s3RequireMockObjects fails the test when any key is missing from the mock.
func s3RequireMockObjects(t *testing.T, mock *mockS3ObjectStore, keys []string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := mock.objects[key]; !ok {
			t.Fatalf("expected object %s missing (have %v)", key, mock.keys())
		}
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
