package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// fakeImageCacheClient is a configurable domain.DistributionClient used by the
// service tests. It records deletes and lets each call fail independently so
// partial-failure behaviour can be asserted.
type fakeImageCacheClient struct {
	host        string
	pingErr     error
	catalog     []string
	catalogErr  error
	tags        map[string][]string
	tagsErr     map[string]error
	manifests   map[string]fakeImageCacheManifest
	manifestErr map[string]error
	deleteErr   map[string]error

	mu      sync.Mutex
	deleted []string
}

type fakeImageCacheManifest struct {
	digest string
	size   int64
	layers int64
}

func (c *fakeImageCacheClient) Host() string { return c.host }

func (c *fakeImageCacheClient) Ping(context.Context) error { return c.pingErr }

func (c *fakeImageCacheClient) Catalog(context.Context) ([]string, error) {
	return c.catalog, c.catalogErr
}

func (c *fakeImageCacheClient) Tags(_ context.Context, repo string) ([]string, error) {
	if err := c.tagsErr[repo]; err != nil {
		return nil, err
	}
	return c.tags[repo], nil
}

func (c *fakeImageCacheClient) ManifestSize(_ context.Context, repo, tag string) (dg string, sizeBytes, layers int64, err error) {
	if err := c.manifestErr[repo+"@"+tag]; err != nil {
		return "", 0, 0, err
	}
	m, ok := c.manifests[repo+"@"+tag]
	if !ok {
		return "", 0, 0, domain.ErrManifestNotFound
	}
	return m.digest, m.size, m.layers, nil
}

func (c *fakeImageCacheClient) DeleteManifest(_ context.Context, repo, digest string) error {
	key := repo + "@" + digest
	if err := c.deleteErr[key]; err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, key)
	return nil
}

func (c *fakeImageCacheClient) deletedSorted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]string(nil), c.deleted...)
	sort.Strings(out)
	return out
}

func digest(ch byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = ch
	}
	return "sha256:" + string(b)
}

// newFakeImageCacheService wires the service to a static mirror→client map.
func newFakeImageCacheService(mirrors []domain.ImageCacheMirror, clients map[string]*fakeImageCacheClient) *ImageCacheService {
	return NewImageCacheService(mirrors, func(addr string) domain.DistributionClient {
		return clients[addr]
	}, testLogger())
}

var imageCacheTestMirrors = []domain.ImageCacheMirror{
	{ID: "docker-io", Host: "docker.io", Upstream: "https://registry-1.docker.io", InternalAddr: "mirror-docker:5000", Backend: "s3"},
	{ID: "ghcr-io", Host: "ghcr.io", Upstream: "https://ghcr.io", InternalAddr: "mirror-ghcr:5000", Backend: "pvc"},
}

func TestImageCacheServiceList(t *testing.T) {
	clients := map[string]*fakeImageCacheClient{
		"mirror-docker:5000": {
			catalog: []string{"library/alpine", "library/busybox"},
			tags: map[string][]string{
				"library/alpine":  {"3.20"},
				"library/busybox": {"1.36", "latest"},
			},
			manifests: map[string]fakeImageCacheManifest{
				"library/alpine@3.20":    {digest: digest('a'), size: 3000, layers: 1},
				"library/busybox@1.36":   {digest: digest('b'), size: 2000, layers: 2},
				"library/busybox@latest": {digest: digest('c'), size: -1, layers: 3},
			},
		},
		"mirror-ghcr:5000": {pingErr: errors.New("connection refused")},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, clients)

	info, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(info.Mirrors) != 2 {
		t.Fatalf("mirrors = %d, want 2", len(info.Mirrors))
	}
	if info.CollectedAt == "" {
		t.Fatal("collected_at must be set")
	}

	docker := info.Mirrors[0]
	if !docker.Reachable || docker.Error != "" {
		t.Fatalf("docker mirror = %+v, want reachable without error", docker)
	}
	if len(docker.Repositories) != 2 {
		t.Fatalf("docker repos = %d, want 2", len(docker.Repositories))
	}
	if got := docker.Repositories[1].Tags; len(got) != 2 || got[0].Digest != digest('b') || got[1].SizeBytes != -1 || got[1].LayerCount != 3 {
		t.Fatalf("busybox tags = %+v, want digests/sizes propagated", got)
	}

	ghcr := info.Mirrors[1]
	if ghcr.Reachable {
		t.Fatalf("ghcr mirror = %+v, want reachable:false", ghcr)
	}
	if ghcr.Error == "" {
		t.Fatal("ghcr mirror must carry an error")
	}
	if len(ghcr.Repositories) != 0 {
		t.Fatalf("ghcr repos = %d, want 0", len(ghcr.Repositories))
	}
}

func TestImageCacheServiceListEmptyAndPartialFailure(t *testing.T) {
	// An empty (reachable) mirror is a successful empty listing; a repo whose
	// tag listing fails surfaces as a listing note, not a hard failure.
	clients := map[string]*fakeImageCacheClient{
		"mirror-docker:5000": {
			catalog: []string{"library/alpine", "library/broken"},
			tags:    map[string][]string{"library/alpine": {"3.20"}},
			tagsErr: map[string]error{"library/broken": errors.New("tags unavailable")},
			manifests: map[string]fakeImageCacheManifest{
				"library/alpine@3.20": {digest: digest('a'), size: 10, layers: 1},
			},
		},
		"mirror-ghcr:5000": {},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, clients)

	info, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if info.Mirrors[0].Error == "" {
		t.Fatal("partial tag failure must be noted in error")
	}
	if len(info.Mirrors[0].Repositories) != 1 {
		t.Fatalf("repos = %d, want the healthy repo only", len(info.Mirrors[0].Repositories))
	}
	empty := info.Mirrors[1]
	if !empty.Reachable || empty.Error != "" || len(empty.Repositories) != 0 {
		t.Fatalf("empty mirror = %+v, want reachable with no repos and no error", empty)
	}
}

func TestImageCacheServiceListCatalogDisabled(t *testing.T) {
	clients := map[string]*fakeImageCacheClient{
		"mirror-docker:5000": {catalogErr: domain.ErrRegistryCatalogDisabled},
		"mirror-ghcr:5000":   {},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, clients)

	info, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List must not hard-fail on a catalog-disabled mirror: %v", err)
	}
	m := info.Mirrors[0]
	if !m.Reachable || m.Error == "" {
		t.Fatalf("catalog-disabled mirror = %+v, want reachable with an error string", m)
	}
}

func TestImageCacheServicePruneTagToDigest(t *testing.T) {
	client := &fakeImageCacheClient{
		manifests: map[string]fakeImageCacheManifest{
			"library/alpine@3.20": {digest: digest('a'), size: 1, layers: 1},
		},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{"mirror-docker:5000": client})

	res, err := svc.Prune(context.Background(), "docker-io", []domain.ImageCachePruneRef{{Repository: "library/alpine", Tag: "3.20"}})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Pruned != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v, want 1 pruned/0 errors", res)
	}
	if got := res.Items[0].Digest; got != digest('a') {
		t.Fatalf("item digest = %q, want resolved digest", got)
	}
	if got := client.deletedSorted(); len(got) != 1 || got[0] != "library/alpine@"+digest('a') {
		t.Fatalf("deleted = %v, want the resolved manifest", got)
	}
	if res.Message == "" {
		t.Fatal("message should describe GC reclamation timing")
	}
}

func TestImageCacheServicePruneByDigestAndAbsent(t *testing.T) {
	client := &fakeImageCacheClient{
		deleteErr: map[string]error{
			"library/gone@" + digest('c'): domain.ErrManifestNotFound,
		},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{"mirror-docker:5000": client})

	// digest supplied directly is used verbatim.
	res, err := svc.Prune(context.Background(), "docker-io", []domain.ImageCachePruneRef{{Repository: "library/alpine", Digest: digest('b')}})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Pruned != 1 || len(client.deletedSorted()) != 1 {
		t.Fatalf("digest prune = %+v deleted=%v", res, client.deletedSorted())
	}

	// a manifest already missing is "already pruned", not an error.
	res, err = svc.Prune(context.Background(), "docker-io", []domain.ImageCachePruneRef{{Repository: "library/gone", Digest: digest('c')}})
	if err != nil {
		t.Fatalf("Prune absent: %v", err)
	}
	if !res.Items[0].Pruned || res.Errors != 0 {
		t.Fatalf("absent manifest = %+v, want pruned idempotently", res.Items[0])
	}
}

func TestImageCacheServicePruneValidationAndErrors(t *testing.T) {
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{
		"mirror-docker:5000": {},
		"mirror-ghcr:5000":   {pingErr: errors.New("down")},
	})
	ctx := context.Background()

	if _, err := svc.Prune(ctx, "nope", []domain.ImageCachePruneRef{{Repository: "r", Tag: "t"}}); !errors.Is(err, domain.ErrImageCacheMirrorNotFound) {
		t.Fatalf("unknown mirror err = %v, want ErrImageCacheMirrorNotFound", err)
	}
	if _, err := svc.Prune(ctx, "docker-io", nil); !errors.Is(err, domain.ErrImageCacheInvalidRef) {
		t.Fatalf("empty refs err = %v, want ErrImageCacheInvalidRef", err)
	}
	if _, err := svc.Prune(ctx, "docker-io", []domain.ImageCachePruneRef{{Repository: "", Tag: "t"}}); !errors.Is(err, domain.ErrImageCacheInvalidRef) {
		t.Fatalf("empty repo err = %v, want ErrImageCacheInvalidRef", err)
	}
	if _, err := svc.Prune(ctx, "docker-io", []domain.ImageCachePruneRef{{Repository: "r"}}); !errors.Is(err, domain.ErrImageCacheInvalidRef) {
		t.Fatalf("no tag/digest err = %v, want ErrImageCacheInvalidRef", err)
	}
	if _, err := svc.Prune(ctx, "docker-io", []domain.ImageCachePruneRef{{Repository: "r", Tag: "t", Digest: digest('a')}}); !errors.Is(err, domain.ErrImageCacheInvalidRef) {
		t.Fatalf("both tag+digest err = %v, want ErrImageCacheInvalidRef", err)
	}
	if _, err := svc.Prune(ctx, "docker-io", []domain.ImageCachePruneRef{{Repository: "r", Digest: "not-a-digest"}}); !errors.Is(err, domain.ErrImageCacheInvalidRef) {
		t.Fatalf("bad digest err = %v, want ErrImageCacheInvalidRef", err)
	}
	if _, err := svc.Prune(ctx, "ghcr-io", []domain.ImageCachePruneRef{{Repository: "r", Tag: "t"}}); !errors.Is(err, domain.ErrImageCacheUnreachable) {
		t.Fatalf("unreachable err = %v, want ErrImageCacheUnreachable", err)
	}
}

func TestImageCacheServicePruneDeleteDisabled(t *testing.T) {
	client := &fakeImageCacheClient{
		deleteErr: map[string]error{"library/alpine@" + digest('a'): domain.ErrRegistryDeleteDisabled},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{"mirror-docker:5000": client})

	_, err := svc.Prune(context.Background(), "docker-io", []domain.ImageCachePruneRef{{Repository: "library/alpine", Digest: digest('a')}})
	if !errors.Is(err, domain.ErrRegistryDeleteDisabled) {
		t.Fatalf("err = %v, want ErrRegistryDeleteDisabled", err)
	}
}

func TestImageCacheServicePruneItemFailure(t *testing.T) {
	client := &fakeImageCacheClient{
		manifestErr: map[string]error{"library/broken@3.20": errors.New("boom")},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{"mirror-docker:5000": client})

	res, err := svc.Prune(context.Background(), "docker-io", []domain.ImageCachePruneRef{
		{Repository: "library/broken", Tag: "3.20"},
		{Repository: "library/alpine", Digest: digest('a')},
	})
	if err != nil {
		t.Fatalf("per-item failure must not fail the request: %v", err)
	}
	if res.Errors != 1 || res.Pruned != 1 {
		t.Fatalf("result = %+v, want 1 pruned/1 error", res)
	}
	if res.Items[0].Error == "" || res.Items[1].Error != "" {
		t.Fatalf("items = %+v, want first failed, second pruned", res.Items)
	}
}

func TestImageCacheServicePruneAll(t *testing.T) {
	client := &fakeImageCacheClient{
		catalog: []string{"library/alpine", "library/busybox", "library/empty"},
		tags: map[string][]string{
			"library/alpine":  {"3.20", "3.21"},
			"library/busybox": {"latest"},
			// library/empty has no tags (catalogued but pruned to nothing).
		},
		manifests: map[string]fakeImageCacheManifest{
			"library/alpine@3.20":    {digest: digest('a')},
			"library/alpine@3.21":    {digest: digest('b')},
			"library/busybox@latest": {digest: digest('c')},
		},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{"mirror-docker:5000": client})

	res, err := svc.PruneAll(context.Background(), "docker-io")
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if len(res.Mirrors) != 1 {
		t.Fatalf("mirrors = %d, want 1", len(res.Mirrors))
	}
	m := res.Mirrors[0]
	if m.ManifestsPruned != 3 || m.RepositoriesProcessed != 3 || m.Errors != 0 {
		t.Fatalf("mirror result = %+v, want 3 pruned across 3 repos", m)
	}
	want := []string{
		"library/alpine@" + digest('a'),
		"library/alpine@" + digest('b'),
		"library/busybox@" + digest('c'),
	}
	if got := client.deletedSorted(); len(got) != len(want) {
		t.Fatalf("deleted = %v, want %v", got, want)
	}
}

func TestImageCacheServicePruneAllAcrossMirrors(t *testing.T) {
	docker := &fakeImageCacheClient{
		catalog:   []string{"library/alpine"},
		tags:      map[string][]string{"library/alpine": {"3.20"}},
		manifests: map[string]fakeImageCacheManifest{"library/alpine@3.20": {digest: digest('a')}},
	}
	ghcr := &fakeImageCacheClient{
		catalog:   []string{"owner/app"},
		tags:      map[string][]string{"owner/app": {"v1"}},
		manifests: map[string]fakeImageCacheManifest{"owner/app@v1": {digest: digest('d')}},
	}
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{
		"mirror-docker:5000": docker,
		"mirror-ghcr:5000":   ghcr,
	})

	// empty mirror id means every mirror.
	res, err := svc.PruneAll(context.Background(), "")
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if len(res.Mirrors) != 2 {
		t.Fatalf("mirrors = %d, want 2", len(res.Mirrors))
	}
	if len(docker.deletedSorted()) != 1 || len(ghcr.deletedSorted()) != 1 {
		t.Fatalf("deletes docker=%v ghcr=%v, want one each", docker.deletedSorted(), ghcr.deletedSorted())
	}
	if res.Message == "" {
		t.Fatal("aggregate message expected")
	}

	if _, err := svc.PruneAll(context.Background(), "unknown"); !errors.Is(err, domain.ErrImageCacheMirrorNotFound) {
		t.Fatalf("unknown mirror err = %v, want ErrImageCacheMirrorNotFound", err)
	}
}

func TestImageCacheServicePruneAllUnreachableAndEmpty(t *testing.T) {
	svc := newFakeImageCacheService(imageCacheTestMirrors, map[string]*fakeImageCacheClient{
		"mirror-docker:5000": {pingErr: errors.New("down")},
		"mirror-ghcr:5000":   {},
	})
	res, err := svc.PruneAll(context.Background(), "")
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if res.Mirrors[0].Error == "" || res.Mirrors[0].ManifestsPruned != 0 {
		t.Fatalf("unreachable mirror = %+v, want per-mirror error", res.Mirrors[0])
	}
	if res.Mirrors[1].Error != "" || res.Mirrors[1].ManifestsPruned != 0 {
		t.Fatalf("empty mirror = %+v, want clean no-op", res.Mirrors[1])
	}
}

func TestImageCacheServiceNoMirrors(t *testing.T) {
	svc := NewImageCacheService(nil, nil, testLogger())
	info, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(info.Mirrors) != 0 {
		t.Fatalf("mirrors = %d, want 0", len(info.Mirrors))
	}
	res, err := svc.PruneAll(context.Background(), "")
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if len(res.Mirrors) != 0 {
		t.Fatalf("mirrors = %d, want 0", len(res.Mirrors))
	}
}
