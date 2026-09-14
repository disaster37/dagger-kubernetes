package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// mockS3SweeperStore is an in-memory S3 object store for GC tests, implementing
// the s3SweeperAPI slice of the minio client. Not-found errors carry the
// NoSuchKey code, matching a real S3 endpoint's behavior.
type mockS3SweeperStore struct {
	mu      sync.Mutex
	objects map[string]mockSweeperObject
	nowFunc func() time.Time
}

type mockSweeperObject struct {
	data         []byte
	lastModified time.Time
}

func newMockS3SweeperStore() *mockS3SweeperStore {
	return &mockS3SweeperStore{
		objects: map[string]mockSweeperObject{},
		nowFunc: time.Now,
	}
}

func (m *mockS3SweeperStore) RemoveObject(_ context.Context, _, objectName string, _ minio.RemoveObjectOptions) error { //nolint:gocritic // hugeParam: signature must match the minio-go API
	m.mu.Lock()
	delete(m.objects, objectName)
	m.mu.Unlock()
	return nil
}

func (m *mockS3SweeperStore) ListObjects(_ context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo { //nolint:gocritic // hugeParam: signature must match the minio-go API
	ch := make(chan minio.ObjectInfo)
	go func() {
		defer close(ch)
		m.mu.Lock()
		keys := make([]string, 0, len(m.objects))
		for k := range m.objects {
			if strings.HasPrefix(k, opts.Prefix) {
				keys = append(keys, k)
			}
		}
		m.mu.Unlock()
		sort.Strings(keys)
		for _, k := range keys {
			m.mu.Lock()
			obj := m.objects[k]
			m.mu.Unlock()
			ch <- minio.ObjectInfo{Key: k, Size: int64(len(obj.data)), LastModified: obj.lastModified}
		}
	}()
	return ch
}

func (m *mockS3SweeperStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *mockS3SweeperStore) setObject(key string, data []byte, lastModified time.Time) {
	m.mu.Lock()
	m.objects[key] = mockSweeperObject{data: data, lastModified: lastModified}
	m.mu.Unlock()
}

func newTestS3CacheGC(mock *mockS3SweeperStore, gcCfg domain.GCConfig) *S3CacheGC {
	return &S3CacheGC{
		client:     mock,
		bucket:     "test-bucket",
		prefix:     domain.S3CachePrefix,
		gcCfg:      gcCfg,
		logger:     observ.NewTestLogger(),
		metricsObs: nil,
	}
}

func TestS3CacheGCRunGC(t *testing.T) {
	mock := newMockS3SweeperStore()
	now := time.Now()
	old := now.Add(-200 * time.Hour)
	mock.setObject("cache/old-layer", []byte("old"), old)
	mock.setObject("cache/fresh-layer", []byte("fresh"), now)
	mock.setObject("cache/edge-layer", []byte("edge"), now.Add(-100*time.Hour))

	gc := newTestS3CacheGC(mock, domain.GCConfig{Enabled: true, MaxAge: 168 * time.Hour, Schedule: time.Hour})
	summary, err := gc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}

	keys := mock.keys()
	if len(keys) != 2 {
		t.Fatalf("remaining keys = %v, want the fresh + edge objects", keys)
	}
	if summary.PurgedTags != 1 {
		t.Fatalf("purged = %d, want 1", summary.PurgedTags)
	}
	if summary.FreedBytes != int64(len("old")) {
		t.Fatalf("freed = %d, want %d", summary.FreedBytes, len("old"))
	}
	if summary.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", summary.Skipped)
	}
}

func TestS3CacheGCRunGCEmpty(t *testing.T) {
	gc := newTestS3CacheGC(newMockS3SweeperStore(), domain.GCConfig{Enabled: true, MaxAge: time.Hour, Schedule: time.Hour})
	summary, err := gc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC on empty prefix: %v", err)
	}
	if summary.PurgedTags != 0 || summary.Errors != 0 || summary.Skipped != 0 {
		t.Fatalf("summary = %+v, want all-zero", summary)
	}
}

func TestS3CacheGCPurge(t *testing.T) {
	mock := newMockS3SweeperStore()
	now := time.Now()
	for _, key := range []string{"cache/a", "cache/b/inner", "cache/c"} {
		mock.setObject(key, []byte(key), now)
	}
	// Objects outside the cache prefix must not be touched.
	mock.setObject("worker-snapshots/v0-20-0/meta.tar.gz", []byte("meta"), now)
	mock.setObject("cli-cache/v0.21.8/linux/amd64/x.tar.gz", []byte("cli"), now)

	result, err := newTestS3CacheGC(mock, domain.GCConfig{}).Purge(context.Background())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.Purged != 3 {
		t.Fatalf("purged = %d, want 3", result.Purged)
	}
	if got := mock.keys(); len(got) != 2 {
		t.Fatalf("remaining keys = %v, want only the non-cache prefixes", got)
	}
}

func TestS3CacheGCPurgeCap(t *testing.T) {
	mock := newMockS3SweeperStore()
	now := time.Now()
	for i := 0; i < maxPurgeAllTags+50; i++ {
		mock.setObject(fmt.Sprintf("cache/obj-%04d", i), []byte("x"), now)
	}

	result, err := newTestS3CacheGC(mock, domain.GCConfig{}).Purge(context.Background())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.Purged != maxPurgeAllTags {
		t.Fatalf("purged = %d, want the %d cap", result.Purged, maxPurgeAllTags)
	}
	if result.Message == "" {
		t.Fatal("truncation message missing")
	}
}

func TestS3CacheGCGCRules(t *testing.T) {
	gcCfg := domain.GCConfig{Enabled: true, MaxAge: 168 * time.Hour, Schedule: 2 * time.Hour}
	gc := newTestS3CacheGC(newMockS3SweeperStore(), gcCfg)

	rules := gc.GCRules()
	if rules.Enabled != gcCfg.Enabled || rules.MaxAge != gcCfg.MaxAge.String() || rules.Schedule != gcCfg.Schedule.String() {
		t.Fatalf("rules = %+v, want config %+v", rules, gcCfg)
	}
	if rules.LastRunAt != "" || rules.LastRunSummary != nil {
		t.Fatalf("no run yet, got %+v", rules.LastRunSummary)
	}

	// After a run the summary is reported.
	if _, err := gc.RunGC(context.Background()); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	rules = gc.GCRules()
	if rules.LastRunAt == "" || rules.LastRunSummary == nil {
		t.Fatalf("last run not recorded: %+v", rules)
	}
	if rules.NextRunAt == "" {
		t.Fatal("next run not scheduled")
	}
}

func TestS3CacheGCStartSweeper(t *testing.T) {
	mock := newMockS3SweeperStore()
	mock.setObject("cache/stale", []byte("old"), time.Now().Add(-1000*time.Hour))

	// A short schedule so the first tick fires quickly.
	gcCfg := domain.GCConfig{Enabled: true, MaxAge: time.Hour, Schedule: 10 * time.Millisecond}
	gc := newTestS3CacheGC(mock, gcCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := gc.StartGCSweeper(ctx)
	defer stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(mock.keys()) == 0 {
			return // the sweeper deleted the stale object
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sweeper did not run within the deadline")
}

func TestS3CacheGCSweeperDisabled(t *testing.T) {
	mock := newMockS3SweeperStore()
	mock.setObject("cache/stale", []byte("old"), time.Now().Add(-1000*time.Hour))

	gc := newTestS3CacheGC(mock, domain.GCConfig{Enabled: false, MaxAge: time.Hour, Schedule: 10 * time.Millisecond})
	stop := gc.StartGCSweeper(context.Background())
	defer stop()
	// A disabled sweeper is a no-op stop func; nothing else to assert beyond
	// it not panicking — the object stays.
	time.Sleep(30 * time.Millisecond)
	if len(mock.keys()) != 1 {
		t.Fatal("disabled sweeper must not delete anything")
	}
}
