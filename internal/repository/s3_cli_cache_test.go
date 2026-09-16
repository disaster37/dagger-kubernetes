package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// sha256HexBytes returns the hex-encoded sha256 digest of b.
func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newTestS3CLICache builds an S3CLICache backed by the in-memory mock.
func newTestS3CLICache(mock *mockS3ObjectStore, prefix string) *S3CLICache {
	return &S3CLICache{
		client: mock,
		bucket: "test-bucket",
		prefix: prefix,
		logger: observ.NewTestLogger(),
	}
}

func TestS3CLICacheHasExisting(t *testing.T) {
	mock := newMockS3ObjectStore()
	cache := newTestS3CLICache(mock, "cli-cache")
	key := "cli-cache/v0.21.8/linux/amd64/" + domain.AssetFilename("v0.21.8", "linux", "amd64")
	mock.setObject(key, []byte("tarball"), time.Now())

	ok, err := cache.Has(context.Background(), "v0.21.8", "linux", "amd64")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !ok {
		t.Fatal("Has = false, want true for an existing object")
	}
}

func TestS3CLICacheHasMissing(t *testing.T) {
	cache := newTestS3CLICache(newMockS3ObjectStore(), "cli-cache")
	ok, err := cache.Has(context.Background(), "v0.21.8", "linux", "amd64")
	if err != nil {
		t.Fatalf("Has: %v (missing object must be false/nil)", err)
	}
	if ok {
		t.Fatal("Has = true, want false for a missing object")
	}
}

func TestS3CLICacheGet(t *testing.T) {
	mock := newMockS3ObjectStore()
	cache := newTestS3CLICache(mock, "cli-cache")
	key := "cli-cache/v0.21.8/linux/amd64/" + domain.AssetFilename("v0.21.8", "linux", "amd64")
	mock.setObject(key, []byte("tarball-bytes"), time.Now())

	path, ok := cache.Get(context.Background(), "v0.21.8", "linux", "amd64")
	if !ok {
		t.Fatal("Get ok=false, want true")
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if string(got) != "tarball-bytes" {
		t.Fatalf("temp file = %q", got)
	}
}

func TestS3CLICacheGetMissing(t *testing.T) {
	cache := newTestS3CLICache(newMockS3ObjectStore(), "cli-cache")
	path, ok := cache.Get(context.Background(), "v0.21.8", "linux", "amd64")
	if ok || path != "" {
		t.Fatalf("Get = %q/%v, want \"\"/false", path, ok)
	}
}

func TestS3CLICachePut(t *testing.T) {
	mock := newMockS3ObjectStore()
	cache := newTestS3CLICache(mock, "cli-cache")

	data := []byte("cli-tarball-payload")
	sum := sha256.Sum256(data)
	sha256HexStr := hex.EncodeToString(sum[:])

	path, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", strings.NewReader(string(data)), sha256HexStr)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if path != "" {
		t.Fatalf("Put path = %q, want \"\" (no local path)", path)
	}

	// The object exists under the deterministic key with the exact bytes.
	key := "cli-cache/v0.21.8/linux/amd64/" + domain.AssetFilename("v0.21.8", "linux", "amd64")
	obj, ok := mock.objects[key]
	if !ok {
		t.Fatalf("object %s not stored", key)
	}
	if !bytes.Equal(obj.data, data) {
		t.Fatalf("stored object = %q", obj.data)
	}

	// Has now reports it.
	okHas, err := cache.Has(context.Background(), "v0.21.8", "linux", "amd64")
	if err != nil || !okHas {
		t.Fatalf("Has after Put = %v/%v, want true/nil", okHas, err)
	}
}

func TestS3CLICachePutChecksumMismatch(t *testing.T) {
	mock := newMockS3ObjectStore()
	cache := newTestS3CLICache(mock, "cli-cache")

	_, err := cache.Put(context.Background(), "v0.21.8", "linux", "amd64", strings.NewReader("payload"), "deadbeef")
	if err == nil || !strings.Contains(err.Error(), domain.ErrCLIChecksumMismatch.Error()) {
		t.Fatalf("Put err = %v, want checksum mismatch", err)
	}
	if len(mock.objects) != 0 {
		t.Fatalf("checksum mismatch must upload nothing, got %d objects", len(mock.objects))
	}
}

func TestS3CLICacheKeyFormat(t *testing.T) {
	cache := newTestS3CLICache(newMockS3ObjectStore(), "cli-cache")
	want := "cli-cache/v0.21.8/linux/amd64/dagger_v0.21.8_linux_amd64.tar.gz"
	if got := cache.keyFor("v0.21.8", "linux", "amd64"); got != want {
		t.Fatalf("keyFor = %q, want %q", got, want)
	}

	// A custom prefix lands the object under <prefix>/...
	mock := newMockS3ObjectStore()
	custom := newTestS3CLICache(mock, "mirror/cli")
	if _, err := custom.Put(context.Background(), "v0.21.8", "darwin", "arm64", strings.NewReader("x"), sha256HexBytes([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	wantKey := "mirror/cli/v0.21.8/darwin/arm64/" + domain.AssetFilename("v0.21.8", "darwin", "arm64")
	if _, ok := mock.objects[wantKey]; !ok {
		t.Fatalf("object %s not stored (keys %v)", wantKey, mock.keys())
	}
}
