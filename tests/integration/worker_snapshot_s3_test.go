package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// newS3TestEnv starts an in-process S3 fake and returns a real minio client
// pointed at it, plus the fake (for assertions).
func newS3TestEnv(t *testing.T, bucket string) (*minio.Client, *s3FakeStore) {
	t.Helper()
	store := newS3FakeStore(bucket)
	ts := httptest.NewServer(store.handler())
	t.Cleanup(ts.Close)

	client, err := repository.NewS3Client(ts.Listener.Addr().String(), "us-east-1", "test", "test", false)
	if err != nil {
		t.Fatalf("create s3 client: %v", err)
	}
	return client, store
}

// s3RequireObjects fails the test when any of keys is missing from the fake.
func s3RequireObjects(t *testing.T, store *s3FakeStore, keys []string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, key := range keys {
		if _, ok := store.objects[key]; !ok {
			t.Fatalf("expected object %s missing (have %d objects)", key, len(store.objects))
		}
	}
}

// s3RequireCount fails the test when the objects under prefix do not match n.
func s3RequireCount(t *testing.T, store *s3FakeStore, prefix string, n int) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for key := range store.objects {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	if count != n {
		t.Fatalf("objects under %q = %d, want %d", prefix, count, n)
	}
}

// TestWorkerSnapshotS3SyncIntegration drives the full S3 worker-snapshot flow
// through a real S3 wire protocol (minio-go against an in-process fake):
// pod 1 pushes, pod 2 restores, a prune propagates via the metadata tarball,
// concurrent pushes leave idempotent blobs, and a restart self-cleans the
// version prefix.
func TestWorkerSnapshotS3SyncIntegration(t *testing.T) {
	client, store := newS3TestEnv(t, "dagger-snapshots")
	logger := observ.NewTestLogger()
	ctx := context.Background()
	version := "v0.20.0"

	newStore := func() *repository.S3SnapshotStore {
		return repository.NewS3SnapshotStore(client, "dagger-snapshots", version, "worker", logger)
	}

	// --- pod 1: build a worker dir, push it ---
	src := t.TempDir()
	writeSyncTestFile(t, filepath.Join(src, "worker", "metadata.db"), "bolt-metadata")
	writeSyncTestFile(t, filepath.Join(src, "worker", "content", "blobs", "sha256", "aa", strings.Repeat("a", 64)), "blob-aa")
	writeSyncTestFile(t, filepath.Join(src, "worker", "content", "blobs", "sha256", "bb", strings.Repeat("b", 64)), "blob-bb")
	if err := newStore().Push(ctx, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("pod1 push: %v", err)
	}

	// Storage layout: meta.tar.gz + one object per blob under blobs/sha256/.
	prefix := domain.WorkerSnapshotS3Prefix(version)
	s3RequireObjects(t, store, []string{
		prefix + domain.MetaTarballName,
		prefix + domain.BlobsPrefix + "aa/" + strings.Repeat("a", 64),
		prefix + domain.BlobsPrefix + "bb/" + strings.Repeat("b", 64),
	})

	// --- pod 2: fresh PVC pulls everything back ---
	dst := t.TempDir()
	ok, err := newStore().Pull(ctx, dst)
	if err != nil || !ok {
		t.Fatalf("pod2 pull = %v/%v, want true/nil", ok, err)
	}
	for _, tc := range []struct {
		rel  string
		want string
	}{
		{"worker/metadata.db", "bolt-metadata"},
		{"worker/content/blobs/sha256/aa/" + strings.Repeat("a", 64), "blob-aa"},
	} {
		got, err := os.ReadFile(filepath.Join(dst, tc.rel))
		if err != nil {
			t.Fatalf("read %s: %v", tc.rel, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s = %q, want %q", tc.rel, got, tc.want)
		}
	}

	// --- pod 1 prunes a local blob and re-pushes: the metadata tarball
	// (and later the self-cleanup) propagate the pruned state ---
	if err := os.Remove(filepath.Join(src, "worker", "content", "blobs", "sha256", "bb", strings.Repeat("b", 64))); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if err := newStore().Push(ctx, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("pod1 re-push: %v", err)
	}
	dst2 := t.TempDir()
	if ok, err := newStore().Pull(ctx, dst2); err != nil || !ok {
		t.Fatalf("pod2 re-pull = %v/%v", ok, err)
	}

	// --- pod 1 restarts: CleanupSelf wipes the version prefix, then re-pushes ---
	if err := newStore().CleanupSelf(ctx); err != nil {
		t.Fatalf("CleanupSelf: %v", err)
	}
	s3RequireCount(t, store, prefix, 0)
	if err := newStore().Push(ctx, filepath.Join(src, "worker"), t.TempDir()); err != nil {
		t.Fatalf("pod1 push after restart: %v", err)
	}

	// --- concurrent pushes: blobs are idempotent, meta last-writer-wins ---
	const pods = 4
	dirs := make([]string, pods)
	for i := range dirs {
		dirs[i] = t.TempDir()
		writeSyncTestFile(t, filepath.Join(dirs[i], "worker", "metadata.db"), "bolt-metadata")
		// All pods share the same content blob (same digest, same shard) and
		// each has one unique blob (distinct digest-shaped filename).
		writeSyncTestFile(t, filepath.Join(dirs[i], "worker", "content", "blobs", "sha256", "aa", strings.Repeat("a", 64)), "shared-blob")
		unique := fmt.Sprintf("%064x", i)
		writeSyncTestFile(t, filepath.Join(dirs[i], "worker", "content", "blobs", "sha256", unique[:2], unique), fmt.Sprintf("pod-%d", i))
	}
	var wg sync.WaitGroup
	errCh := make(chan error, pods)
	for i := 0; i < pods; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := newStore().Push(ctx, filepath.Join(dirs[i], "worker"), t.TempDir()); err != nil {
				errCh <- fmt.Errorf("pod%d push: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent push: %v", err)
	}

	// Shared blob stored once; one unique per pod; meta.tar.gz present.
	s3RequireCount(t, store, prefix+domain.BlobsPrefix, pods+1)
	s3RequireObjects(t, store, []string{prefix + domain.MetaTarballName})

	// The surviving metadata is intact and every listed blob restores.
	final := t.TempDir()
	if ok, err := newStore().Pull(ctx, final); err != nil || !ok {
		t.Fatalf("final pull = %v/%v", ok, err)
	}
	got, err := os.ReadFile(filepath.Join(final, "worker", "metadata.db"))
	if err != nil || string(got) != "bolt-metadata" {
		t.Fatalf("final metadata.db = %q/%v", got, err)
	}
}

// TestS3CacheGCIntegration sweeps stale BuildKit cache objects by
// LastModified through a real S3 wire protocol; the worker-snapshot prefix is
// out of scope for the GC.
func TestS3CacheGCIntegration(t *testing.T) {
	client, store := newS3TestEnv(t, "dagger-cache")
	logger := observ.NewTestLogger()

	now := time.Now()
	clientPut(t, client, "dagger-cache", "cache/old-layer", []byte("old"))
	clientPut(t, client, "dagger-cache", "cache/fresh-layer", []byte("fresh"))
	clientPut(t, client, "dagger-cache", "worker-snapshots/v0-20-0/meta.tar.gz", []byte("meta"))
	// S3 assigns LastModified server-side: age the stale objects explicitly.
	store.setLastModified("cache/old-layer", now.Add(-200*time.Hour))
	store.setLastModified("worker-snapshots/v0-20-0/meta.tar.gz", now.Add(-200*time.Hour))

	gc := service.NewS3CacheGC(client, "dagger-cache", domain.S3CachePrefix, domain.GCConfig{Enabled: true, MaxAge: 168 * time.Hour, Schedule: time.Hour}, logger, nil)
	summary, err := gc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTags != 1 || summary.Skipped != 1 {
		t.Fatalf("summary = %+v, want 1 purged + 1 skipped", summary)
	}

	s3RequireObjects(t, store, []string{
		"cache/fresh-layer",
		"worker-snapshots/v0-20-0/meta.tar.gz",
	})
	if _, ok := store.objects["cache/old-layer"]; ok {
		t.Fatal("stale cache object was not swept")
	}

	// Purge removes every remaining cache object (and only those).
	result, err := gc.Purge(context.Background())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.Purged != 1 {
		t.Fatalf("purged = %d, want 1", result.Purged)
	}
	s3RequireObjects(t, store, []string{"worker-snapshots/v0-20-0/meta.tar.gz"})
	s3RequireCount(t, store, "cache/", 0)
}

// TestS3CLICacheIntegration drives the S3 CLI cache (put → has → get) through
// a real S3 wire protocol.
func TestS3CLICacheIntegration(t *testing.T) {
	client, _ := newS3TestEnv(t, "dagger-cli-cache")
	logger := observ.NewTestLogger()
	cache := repository.NewS3CLICache(client, "dagger-cli-cache", "cli-cache", logger)
	ctx := context.Background()

	payload := []byte("cli-tarball-bytes")
	sum := sha256.Sum256(payload)
	sha256Hex := hex.EncodeToString(sum[:])

	if _, err := cache.Put(ctx, "v0.21.8", "linux", "amd64", strings.NewReader(string(payload)), sha256Hex); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ok, err := cache.Has(ctx, "v0.21.8", "linux", "amd64")
	if err != nil || !ok {
		t.Fatalf("Has = %v/%v, want true/nil", ok, err)
	}

	path, ok := cache.Get(ctx, "v0.21.8", "linux", "amd64")
	if !ok {
		t.Fatal("Get ok=false, want true")
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("Get content = %q/%v", got, err)
	}

	// Checksum mismatch fails before any upload.
	if _, err := cache.Put(ctx, "v0.21.8", "darwin", "arm64", strings.NewReader("x"), "bad"); err == nil {
		t.Fatal("checksum mismatch must fail")
	}
}

// clientPut uploads an object directly through the real minio client (S3
// assigns LastModified server-side; use s3FakeStore.setLastModified to age it).
func clientPut(t *testing.T, client *minio.Client, bucket, key string, data []byte) {
	t.Helper()
	_, err := client.PutObject(context.Background(), bucket, key, strings.NewReader(string(data)), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}
