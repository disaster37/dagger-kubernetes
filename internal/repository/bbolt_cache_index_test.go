package repository

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBboltCacheIndexPutGetGC(t *testing.T) {
	idx, err := OpenBboltCacheIndex(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()

	if err := idx.Put("org", &CacheIndexEntry{Digest: "sha256:abc", Size: 10}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := idx.Get("org", "sha256:abc")
	if err != nil || !ok || e.Size != 10 {
		t.Fatalf("get: %+v ok=%v err=%v", e, ok, err)
	}
	count, bytes, err := idx.Stats("org")
	if err != nil || count != 1 || bytes != 10 {
		t.Fatalf("stats: %d %d %v", count, bytes, err)
	}
	evicted, err := idx.GC("org", time.Nanosecond, 0, time.Now().Add(time.Hour))
	if err != nil || len(evicted) != 1 {
		t.Fatalf("gc: %v %v", evicted, err)
	}
	if _, ok, _ := idx.Get("org", "sha256:abc"); ok {
		t.Fatal("entry not evicted")
	}
}
