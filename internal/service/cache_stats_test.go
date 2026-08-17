package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// --- fake registry ---------------------------------------------------------

type fakeRegistry struct {
	mu             sync.Mutex
	catalogStatus  int
	repos          []string
	tags           map[string][]string
	tagsStatus     map[string]int
	manifestBody   map[string]string // "repo:tag" -> raw manifest JSON
	manifestDigest map[string]string // "repo:tag" -> digest header
	deleteStatus   int
	deleted        []string
}

func (f *fakeRegistry) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/_catalog":
			status := f.catalogStatus
			if status == 0 {
				status = http.StatusOK
			}
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"repositories":[%s]}`, strings.Join(quoteEach(f.repos), ","))))
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
			if st := f.tagsStatus[repo]; st != 0 {
				w.WriteHeader(st)
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"name":%q,"tags":[%s]}`, repo, strings.Join(quoteEach(f.tags[repo]), ","))))
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(r.URL.Path, "/manifests/"):
			key := manifestKey(r.URL.Path)
			body, ok := f.manifestBody[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if d := f.manifestDigest[key]; d != "" {
				w.Header().Set("Docker-Content-Digest", d)
			}
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/manifests/"):
			status := f.deleteStatus
			if status == 0 {
				status = http.StatusAccepted
			}
			if status != http.StatusAccepted && status != http.StatusOK && status != http.StatusNoContent {
				w.WriteHeader(status)
				return
			}
			f.mu.Lock()
			f.deleted = append(f.deleted, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func manifestKey(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 4 {
		return ""
	}
	return parts[len(parts)-3] + ":" + parts[len(parts)-1]
}

func quoteEach(items []string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf("%q", it)
	}
	return out
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		repos:          []string{"dagger-cache"},
		tags:           map[string][]string{},
		tagsStatus:     map[string]int{},
		manifestBody:   map[string]string{},
		manifestDigest: map[string]string{},
	}
}

func manifestJSON(digest string, size int64, layers int64, created string) string {
	var layerParts []string
	for i := int64(0); i < layers; i++ {
		layerParts = append(layerParts, fmt.Sprintf(`{"digest":"sha256:layer%d","size":%d}`, i, size/layers))
	}
	annotation := ""
	if created != "" {
		annotation = fmt.Sprintf(`,"annotations":{"org.opencontainers.image.created":%q}`, created)
	}
	return fmt.Sprintf(`{"config":{"digest":%q,"size":0},"layers":[%s]%s}`, digest+"-cfg", strings.Join(layerParts, ","), annotation)
}

// digestStr returns a valid sha256:<64 hex> digest built by repeating c, so
// the fake registry serves realistic digest shapes (the registry client now
// validates digest shape before placing it in a DELETE path).
func digestStr(c string) string {
	return "sha256:" + strings.Repeat(c, 64)
}

func newTestRouter(t *testing.T, backends ...domain.RegistryBackend) *RegistryRouter {
	t.Helper()
	return NewRegistryRouter(backends, repository.NewCacheRoutesRepo(newServiceStore(t)), observ.NewTestLogger())
}

func newStatsService(t *testing.T, reg *fakeRegistry, metricsURL string, fleet domain.FleetProvider, gc domain.GCConfig) (*CacheStatsService, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(reg.handler())
	t.Cleanup(ts.Close)

	var mc *repository.MetricsClient
	if metricsURL != "" {
		mc = repository.NewMetricsClient(metricsURL)
	}
	router := newTestRouter(t, domain.RegistryBackend{ID: "default", InternalAddr: ts.Listener.Addr().String()})
	return NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.supv.example.com"},
		router,
		mc,
		fleet,
		gc,
		observ.NewTestLogger(),
		observ.NewMetrics(nil),
	), ts
}

// --- fleet stub ------------------------------------------------------------

type stubFleetProvider struct {
	versions []string
	replicas map[string][]domain.Replica
	allErr   error
}

func (p *stubFleetProvider) EnsureStatefulSet(string, string) error { return nil }
func (p *stubFleetProvider) DeleteStatefulSet(string) error         { return nil }
func (p *stubFleetProvider) EnsureService(string) error             { return nil }
func (p *stubFleetProvider) DeleteService(string) error             { return nil }
func (p *stubFleetProvider) GetReplicas(v string) ([]domain.Replica, error) {
	return p.replicas[v], nil
}
func (p *stubFleetProvider) ScaleUp(string, int) error                        { return nil }
func (p *stubFleetProvider) ScaleDown(string, int) error                      { return nil }
func (p *stubFleetProvider) GetReadyReplicaIP(string, string) (string, error) { return "", nil }
func (p *stubFleetProvider) WaitForReady(string, string) error                { return nil }
func (p *stubFleetProvider) GetEngineImage(v string) string                   { return v }
func (p *stubFleetProvider) AllVersions() ([]string, error)                   { return p.versions, p.allErr }

var _ domain.FleetProvider = (*stubFleetProvider)(nil)

func defaultGC() domain.GCConfig {
	return domain.GCConfig{
		Enabled:               false,
		MaxAge:                168 * time.Hour,
		Schedule:              time.Hour,
		MinRefsToKeep:         3,
		ProtectActiveVersions: true,
	}
}

// --- tests -----------------------------------------------------------------

func TestCacheStatsRegistryOK(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4", "v0-20-0"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 120, 3, "")
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")
	reg.manifestBody["dagger-cache:v0-20-0"] = manifestJSON("sha256:b", 60, 2, "")
	reg.manifestDigest["dagger-cache:v0-20-0"] = digestStr("b")

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.Running || !stats.Reachable {
		t.Fatalf("running=%v reachable=%v", stats.Running, stats.Reachable)
	}
	if stats.TotalSize != 180 {
		t.Fatalf("total_size = %d, want 180", stats.TotalSize)
	}
	if stats.ObjectCount != 5 {
		t.Fatalf("object_count = %d, want 5", stats.ObjectCount)
	}
	if len(stats.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(stats.Versions))
	}
	if stats.Versions[0].Version != "v0.21.4" {
		t.Fatalf("versions[0].version = %q (want newest first)", stats.Versions[0].Version)
	}
	if stats.Versions[0].Ref != "cache.supv.example.com/dagger-cache:v0-21-4" {
		t.Fatalf("ref = %q", stats.Versions[0].Ref)
	}
	if stats.HitRate != nil {
		t.Fatalf("hit_rate = %v, want nil", *stats.HitRate)
	}
}

func TestCacheStatsCacheHitAndExpiry(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	first, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	second, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if first != second {
		t.Fatal("second Stats() should return the cached pointer")
	}

	// Force expiry and re-probe.
	svc.cachedAt = time.Now().Add(-2 * cacheStatsTTL)
	third, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if third == first {
		t.Fatal("third Stats() should re-probe after TTL expiry")
	}
}

func TestCacheStatsRegistryUnreachable(t *testing.T) {
	reg := newFakeRegistry()
	svc, ts := newStatsService(t, reg, "", nil, defaultGC())
	ts.Close()

	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Running || stats.Reachable {
		t.Fatal("expected running=false reachable=false")
	}
	if stats.TotalSize != -1 || stats.ObjectCount != -1 {
		t.Fatalf("sizes = %d/%d, want -1/-1", stats.TotalSize, stats.ObjectCount)
	}
	if stats.Message != registryDownMessage {
		t.Fatalf("message = %q", stats.Message)
	}
}

func TestCacheStatsCatalogDisabled(t *testing.T) {
	reg := newFakeRegistry()
	reg.catalogStatus = http.StatusNotFound

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.Running {
		t.Fatal("running should be true when ping succeeds but catalog is disabled")
	}
	if stats.TotalSize != -1 {
		t.Fatalf("total_size = %d, want -1", stats.TotalSize)
	}
	if stats.Message != catalogDisabledMsg {
		t.Fatalf("message = %q", stats.Message)
	}
}

func TestCacheStatsS3Unsupported(t *testing.T) {
	svc := NewCacheStatsService(
		&Cache{Type: "s3", Registry: "my-bucket", S3: domain.S3Ref{Bucket: "my-bucket"}},
		nil, nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.Running {
		t.Fatal("s3 running should be true when bucket configured")
	}
	if stats.Message != s3UnsupportedMessage {
		t.Fatalf("message = %q", stats.Message)
	}
	if len(stats.Versions) != 0 {
		t.Fatalf("versions = %d, want 0", len(stats.Versions))
	}
}

func TestCacheStatsHitRate(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")

	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		val := "0"
		if strings.Contains(q, "hits_total") {
			val = "8"
		} else if strings.Contains(q, "misses_total") {
			val = "2"
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"success","data":{"result":[{"metric":{},"value":[1700000000,%q]}]}}`, val)))
	}))
	defer ms.Close()

	svc, _ := newStatsService(t, reg, ms.URL, nil, defaultGC())
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.HitRate == nil {
		t.Fatal("hit_rate should be set")
	}
	if *stats.HitRate != 0.8 {
		t.Fatalf("hit_rate = %v, want 0.8", *stats.HitRate)
	}
	if stats.HitCount != 8 || stats.MissCount != 2 {
		t.Fatalf("counts = %d/%d", stats.HitCount, stats.MissCount)
	}
}

func TestCacheStatsHitRateNoData(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")

	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer ms.Close()

	svc, _ := newStatsService(t, reg, ms.URL, nil, defaultGC())
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.HitRate != nil {
		t.Fatalf("hit_rate = %v, want nil", *stats.HitRate)
	}
}

func TestPurgeInvalidVersion(t *testing.T) {
	svc, _ := newStatsService(t, newFakeRegistry(), "", nil, defaultGC())
	_, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "not-a-version"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestPurgeInvalidTag(t *testing.T) {
	svc, _ := newStatsService(t, newFakeRegistry(), "", nil, defaultGC())
	_, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "v0.21.4", Tag: "bad tag!"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestPurgeSuccess(t *testing.T) {
	reg := newFakeRegistry()
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 55, 1, "")
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	res, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "v0.21.4"})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Purged != 1 || res.FreedBytes != 55 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Versions) != 1 || res.Versions[0] != "v0.21.4" {
		t.Fatalf("versions = %v", res.Versions)
	}
	if len(reg.deleted) != 1 || !strings.Contains(reg.deleted[0], digestStr("a")) {
		t.Fatalf("deleted = %v", reg.deleted)
	}
}

func TestPurgeAlreadyPurged(t *testing.T) {
	reg := newFakeRegistry() // no manifest for v0-21-4 → 404

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	res, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "v0.21.4"})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.AlreadyPurged != 1 || res.Purged != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestPurgeDeleteDisabled(t *testing.T) {
	reg := newFakeRegistry()
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 55, 2, "")
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")
	reg.deleteStatus = http.StatusMethodNotAllowed

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	_, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "v0.21.4"})
	if !errors.Is(err, domain.ErrRegistryDeleteDisabled) {
		t.Fatalf("err = %v, want ErrRegistryDeleteDisabled", err)
	}
}

func TestPurgeAll(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4", "v0-20-0"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 40, 2, "")
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")
	reg.manifestBody["dagger-cache:v0-20-0"] = manifestJSON("sha256:b", 20, 1, "")
	reg.manifestDigest["dagger-cache:v0-20-0"] = digestStr("b")

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	res, err := svc.PurgeAll(context.Background())
	if err != nil {
		t.Fatalf("PurgeAll: %v", err)
	}
	if res.Purged != 2 || res.FreedBytes != 60 {
		t.Fatalf("result = %+v", res)
	}
}

func TestPurgeAllTruncated(t *testing.T) {
	reg := newFakeRegistry()
	var tags []string
	for i := 0; i < 1005; i++ {
		tag := fmt.Sprintf("v0-%d-%d", 21, i)
		tags = append(tags, tag)
		reg.manifestBody["dagger-cache:"+tag] = manifestJSON(fmt.Sprintf("sha256:%d", i), 1, 1, "")
		reg.manifestDigest["dagger-cache:"+tag] = fmt.Sprintf("sha256:%064d", i)
	}
	reg.tags["dagger-cache"] = tags

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	res, err := svc.PurgeAll(context.Background())
	if err != nil {
		t.Fatalf("PurgeAll: %v", err)
	}
	if res.Purged+res.AlreadyPurged != maxPurgeAllTags {
		t.Fatalf("processed = %d, want %d", res.Purged+res.AlreadyPurged, maxPurgeAllTags)
	}
	if res.Message != "truncated at 1000 tags" {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestRunGCPurgesOldTags(t *testing.T) {
	reg := newFakeRegistry()
	gc := defaultGC()
	gc.Enabled = true
	gc.MaxAge = 1 * time.Hour
	gc.MinRefsToKeep = 3

	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	var tags []string
	for i := 0; i < 5; i++ {
		tag := fmt.Sprintf("v0-21-%d", i)
		tags = append(tags, tag)
		reg.manifestBody["dagger-cache:"+tag] = manifestJSON(fmt.Sprintf("sha256:%d", i), 10, 1, old)
		reg.manifestDigest["dagger-cache:"+tag] = fmt.Sprintf("sha256:%064d", i)
	}
	reg.tags["dagger-cache"] = tags

	svc, _ := newStatsService(t, reg, "", nil, gc)
	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	// 5 tags, keep 3 newest → purge 2 oldest.
	if summary.PurgedTags != 2 {
		t.Fatalf("purged_tags = %d, want 2", summary.PurgedTags)
	}
	if summary.FreedBytes != 20 {
		t.Fatalf("freed_bytes = %d, want 20", summary.FreedBytes)
	}
}

func TestRunGCProtectsActiveVersion(t *testing.T) {
	reg := newFakeRegistry()
	gc := defaultGC()
	gc.Enabled = true
	gc.MaxAge = 1 * time.Hour
	gc.MinRefsToKeep = 3
	gc.ProtectActiveVersions = true

	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	var tags []string
	for i := 0; i < 5; i++ {
		tag := fmt.Sprintf("v0-21-%d", i)
		tags = append(tags, tag)
		reg.manifestBody["dagger-cache:"+tag] = manifestJSON(fmt.Sprintf("sha256:%d", i), 10, 1, old)
		reg.manifestDigest["dagger-cache:"+tag] = fmt.Sprintf("sha256:%064d", i)
	}
	reg.tags["dagger-cache"] = tags

	fleet := &stubFleetProvider{
		versions: []string{"v0.21.0"},
		replicas: map[string][]domain.Replica{
			"v0.21.0": {{Name: "p0", Version: "v0.21.0", Ready: true}},
		},
	}

	svc, _ := newStatsService(t, reg, "", fleet, gc)
	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	// v0-21-0 is oldest but active → skipped; v0-21-1 (old, inactive) purged.
	if summary.PurgedTags != 1 {
		t.Fatalf("purged_tags = %d, want 1 (v0-21-0 protected)", summary.PurgedTags)
	}
	if summary.Skipped < 1 {
		t.Fatalf("skipped = %d, want >= 1", summary.Skipped)
	}
}

func TestRunGCUnknownAgeSkips(t *testing.T) {
	reg := newFakeRegistry()
	gc := defaultGC()
	gc.Enabled = true
	gc.MaxAge = 1 * time.Hour
	gc.MinRefsToKeep = 0

	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	// No created annotation → unknown age.
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")

	svc, _ := newStatsService(t, reg, "", nil, gc)
	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTags != 0 || summary.Skipped != 1 {
		t.Fatalf("summary = %+v, want 0 purged / 1 skipped", summary)
	}
}

func TestGCRulesReflectConfigAndLastRun(t *testing.T) {
	reg := newFakeRegistry()
	gc := defaultGC()
	gc.Enabled = true
	gc.MaxAge = 2 * time.Hour
	gc.Schedule = 30 * time.Minute

	reg.tags["dagger-cache"] = []string{"v0-21-4", "v0-21-3"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")
	reg.manifestBody["dagger-cache:v0-21-3"] = manifestJSON("sha256:b", 10, 1, "")

	svc, _ := newStatsService(t, reg, "", nil, gc)

	rules := svc.GCRules()
	if !rules.Enabled || rules.MaxAge != "2h0m0s" || rules.Schedule != "30m0s" || rules.MinRefsToKeep != 3 || !rules.ProtectActiveVersions {
		t.Fatalf("rules = %+v", rules)
	}
	if rules.LastRunAt != "" {
		t.Fatalf("last_run_at should be empty before first run, got %q", rules.LastRunAt)
	}

	if _, err := svc.RunGC(context.Background()); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	rules = svc.GCRules()
	if rules.LastRunAt == "" || rules.LastRunSummary == nil {
		t.Fatalf("rules after run = %+v", rules)
	}
	if rules.NextRunAt == "" {
		t.Fatal("next_run_at should be set")
	}
}

func TestStartGCSweeperDisabled(t *testing.T) {
	svc, _ := newStatsService(t, newFakeRegistry(), "", nil, defaultGC())
	stop := svc.StartGCSweeper(context.Background())
	stop() // no-op; must not panic
}

func TestStartGCSweeperEnabled(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")

	gc := defaultGC()
	gc.Enabled = true
	gc.Schedule = 10 * time.Millisecond

	svc, _ := newStatsService(t, reg, "", nil, gc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := svc.StartGCSweeper(ctx)
	defer stop()

	// Wait for at least one tick to record a GC run.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if rules := svc.GCRules(); rules.LastRunAt != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not run within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCacheStatsRegistryNil(t *testing.T) {
	svc := NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache"},
		nil, nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Running || stats.Reachable {
		t.Fatal("expected running=false reachable=false for nil registry")
	}
	if stats.Message != registryDownMessage {
		t.Fatalf("message = %q", stats.Message)
	}
}

func TestCacheStatsSkipsMissingManifestAndBadTags(t *testing.T) {
	reg := newFakeRegistry()
	reg.repos = []string{"dagger-cache", "broken"}
	reg.tags["dagger-cache"] = []string{"v0-21-4", "missing"}
	reg.tags["broken"] = []string{"v0-20-0"}
	reg.tagsStatus["broken"] = http.StatusInternalServerError
	// "missing" has no manifest body → 404 → skipped.
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 30, 1, "")
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalSize != 30 {
		t.Fatalf("total_size = %d, want 30", stats.TotalSize)
	}
	if len(stats.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(stats.Versions))
	}
}

func TestPurgeRegistryNil(t *testing.T) {
	svc := NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache"},
		nil, nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)
	_, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "v0.21.4"})
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	_, err = svc.PurgeAll(context.Background())
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestPurgeAllCatalogDisabled(t *testing.T) {
	reg := newFakeRegistry()
	reg.catalogStatus = http.StatusNotFound

	svc, _ := newStatsService(t, reg, "", nil, defaultGC())
	_, err := svc.PurgeAll(context.Background())
	if !errors.Is(err, domain.ErrRegistryCatalogDisabled) {
		t.Fatalf("err = %v, want ErrRegistryCatalogDisabled", err)
	}
}

func TestRunGCRegistryNil(t *testing.T) {
	svc := NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache"},
		nil, nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)
	summary, err := svc.RunGC(context.Background())
	if err == nil || summary == nil {
		t.Fatalf("summary=%v err=%v, want error", summary, err)
	}
}

func TestRunGCDeleteDisabled(t *testing.T) {
	reg := newFakeRegistry()
	gc := defaultGC()
	gc.Enabled = true
	gc.MaxAge = 1 * time.Hour
	gc.MinRefsToKeep = 0

	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, old)
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")
	reg.deleteStatus = http.StatusMethodNotAllowed

	svc, _ := newStatsService(t, reg, "", nil, gc)
	_, err := svc.RunGC(context.Background())
	if !errors.Is(err, domain.ErrRegistryDeleteDisabled) {
		t.Fatalf("err = %v, want ErrRegistryDeleteDisabled", err)
	}
}

// TestStatsPurgeNoDeadlock guards against the lock-ordering (ABBA) deadlock
// between Stats() (holds mu, wants purgeMu via GCRules) and Purge()/RunGC()
// (holds purgeMu, wants mu via invalidateCache). A manifest fetch is blocked
// so both callers deterministically sit inside their first critical section,
// then released; with a consistent lock order both complete, otherwise the
// test times out.
func TestStatsPurgeNoDeadlock(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()

	blocked := make(chan struct{}, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v2/_catalog":
			_, _ = w.Write([]byte(`{"repositories":["dagger-cache"]}`))
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			_, _ = w.Write([]byte(`{"name":"dagger-cache","tags":["v0-21-4"]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/"):
			blocked <- struct{}{}
			<-release
			w.Header().Set("Docker-Content-Digest", digestStr("a"))
			_, _ = w.Write([]byte(manifestJSON(digestStr("a"), 10, 1, "")))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	router := newTestRouter(t, domain.RegistryBackend{ID: "default", InternalAddr: ts.Listener.Addr().String()})
	if err := router.RecordManifest(context.Background(), "dagger-cache", "v0-21-4", digestStr("a"), "default", 0); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	svc := NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache"},
		router,
		nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)

	// Purge acquires purgeMu then blocks inside ManifestSize.
	purgeDone := make(chan error, 1)
	go func() {
		_, err := svc.Purge(context.Background(), domain.PurgeRequest{Version: "v0.21.4"})
		purgeDone <- err
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("purge never reached the manifest fetch")
	}

	// Stats acquires mu; without consistent lock ordering it blocks on
	// purgeMu inside GCRules, with the fix it reaches its own manifest fetch.
	statsDone := make(chan error, 1)
	go func() {
		_, err := svc.Stats(context.Background())
		statsDone <- err
	}()

	// Wait until Stats is holding its mutex (blocked either way).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !svc.mu.TryLock() {
			break
		}
		svc.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("Stats never acquired its mutex")
		}
		time.Sleep(time.Millisecond)
	}

	// Release both manifest fetches; both callers must now finish.
	releaseFn()

	select {
	case err := <-statsDone:
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stats() deadlocked (mu -> purgeMu)")
	}
	select {
	case err := <-purgeDone:
		if err != nil {
			t.Fatalf("Purge: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Purge() deadlocked (purgeMu -> mu)")
	}
}

func TestFleetErrorTreatsAllProtected(t *testing.T) {
	reg := newFakeRegistry()
	gc := defaultGC()
	gc.Enabled = true
	gc.MaxAge = 1 * time.Hour
	gc.MinRefsToKeep = 0

	old := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, old)

	fleet := &stubFleetProvider{allErr: errors.New("k8s down")}
	svc, _ := newStatsService(t, reg, "", fleet, gc)
	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTags != 0 {
		t.Fatalf("purged_tags = %d, want 0 (unknown fleet → all protected)", summary.PurgedTags)
	}
}

func TestCacheStatsMultiBackend(t *testing.T) {
	reg1 := newFakeRegistry()
	reg1.tags["dagger-cache"] = []string{"v0-21-4"}
	reg1.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 120, 3, "")
	reg1.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")

	reg2 := newFakeRegistry()
	reg2.tags["dagger-cache"] = []string{"v0-20-0"}
	reg2.manifestBody["dagger-cache:v0-20-0"] = manifestJSON("sha256:b", 60, 2, "")
	reg2.manifestDigest["dagger-cache:v0-20-0"] = digestStr("b")

	ts1 := httptest.NewServer(reg1.handler())
	t.Cleanup(ts1.Close)
	ts2 := httptest.NewServer(reg2.handler())
	t.Cleanup(ts2.Close)

	router := newTestRouter(t,
		domain.RegistryBackend{ID: "reg-1", InternalAddr: ts1.Listener.Addr().String()},
		domain.RegistryBackend{ID: "reg-2", InternalAddr: ts2.Listener.Addr().String()},
	)
	svc := NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.supv.example.com"},
		router, nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)

	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.Running || !stats.Reachable {
		t.Fatalf("running=%v reachable=%v", stats.Running, stats.Reachable)
	}
	if stats.TotalSize != 180 {
		t.Fatalf("total_size = %d, want 180", stats.TotalSize)
	}
	if stats.ObjectCount != 5 {
		t.Fatalf("object_count = %d, want 5", stats.ObjectCount)
	}
	if len(stats.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(stats.Versions))
	}

	router.mu.RLock()
	charge1 := router.charges["reg-1"]
	charge2 := router.charges["reg-2"]
	router.mu.RUnlock()
	if charge1 != 120 || charge2 != 60 {
		t.Fatalf("charges = reg-1:%d reg-2:%d, want 120/60", charge1, charge2)
	}
}

func TestCacheStatsMarkDownFailingBackend(t *testing.T) {
	reg := newFakeRegistry()
	reg.tags["dagger-cache"] = []string{"v0-21-4"}
	reg.manifestBody["dagger-cache:v0-21-4"] = manifestJSON("sha256:a", 10, 1, "")
	reg.manifestDigest["dagger-cache:v0-21-4"] = digestStr("a")

	ts := httptest.NewServer(reg.handler())
	t.Cleanup(ts.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	badAddr := ln.Addr().String()
	_ = ln.Close()

	router := newTestRouter(t,
		domain.RegistryBackend{ID: "good", InternalAddr: ts.Listener.Addr().String()},
		domain.RegistryBackend{ID: "bad", InternalAddr: badAddr},
	)
	svc := NewCacheStatsService(
		&Cache{Type: "registry", Registry: "cache.reg/dagger-cache", PublicHost: "cache.supv.example.com"},
		router, nil, nil, defaultGC(), observ.NewTestLogger(), observ.NewMetrics(nil),
	)

	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.Running || !stats.Reachable {
		t.Fatal("running should be true when at least one backend is reachable")
	}

	router.mu.RLock()
	down := router.down["bad"]
	up := !router.down["good"]
	router.mu.RUnlock()
	if !down {
		t.Fatal("failing backend should be marked down")
	}
	if !up {
		t.Fatal("healthy backend should not be marked down")
	}
}
