package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// stubCLIUpstream is a call-counting CLIUpstream for CLIService tests.
type stubCLIUpstream struct {
	mu            sync.Mutex
	listCalls     int
	checksumCalls int
	tarballCalls  int

	releases    []string
	checksums   map[string]string
	tarball     []byte
	listErr     error
	checksumErr error
	tarballErr  error
}

func (u *stubCLIUpstream) List(context.Context) ([]string, error) {
	u.mu.Lock()
	u.listCalls++
	u.mu.Unlock()
	if u.listErr != nil {
		return nil, u.listErr
	}
	return u.releases, nil
}

func (u *stubCLIUpstream) FetchChecksums(_ context.Context, _ string) (map[string]string, error) {
	u.mu.Lock()
	u.checksumCalls++
	u.mu.Unlock()
	if u.checksumErr != nil {
		return nil, u.checksumErr
	}
	return u.checksums, nil
}

func (u *stubCLIUpstream) FetchTarball(_ context.Context, _, _, _ string) (io.ReadCloser, int64, error) {
	u.mu.Lock()
	u.tarballCalls++
	u.mu.Unlock()
	if u.tarballErr != nil {
		return nil, 0, u.tarballErr
	}
	return io.NopCloser(bytes.NewReader(u.tarball)), int64(len(u.tarball)), nil
}

func (u *stubCLIUpstream) counts() (listCalls, checksumCalls, tarballCalls int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.listCalls, u.checksumCalls, u.tarballCalls
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newCLITestService wires a CLIService backed by a stub upstream + a real
// FileCLICache in a temp dir.
func newCLITestService(t *testing.T, allowlist []string, up *stubCLIUpstream) *CLIService {
	t.Helper()
	resolver, err := NewResolver("v0.19.0", allowlist, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cache, err := repository.NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	return NewCLIService(resolver, up, cache, "https://supv.example.com", time.Hour, observ.NewTestLogger(), observ.NewMetrics(nil))
}

// stubTarball returns tarball bytes plus a checksums map keyed by its asset
// filename.
func stubTarball(version string) (tarball []byte, checksums map[string]string) {
	data := []byte("fake-tarball-" + version)
	return data, map[string]string{domain.AssetFilename(version, "linux", "amd64"): shaHex(data)}
}

func TestCLIServiceResolveLatestNoAllowlist(t *testing.T) {
	up := &stubCLIUpstream{releases: []string{"v0.21.7", "v0.21.8", "v0.18.0", "not-a-version"}}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)
	art, err := svc.ResolveLatest(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if art.Version != "v0.21.8" {
		t.Fatalf("version = %q, want v0.21.8", art.Version)
	}
	if art.SHA256 != shaHex(data) {
		t.Fatalf("sha = %q", art.SHA256)
	}
	if art.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", art.Size, len(data))
	}
	if art.URL != "https://supv.example.com/api/v1/cli/v0.21.8?os=linux&arch=amd64" {
		t.Fatalf("url = %q", art.URL)
	}
}

func TestCLIServiceResolveLatestWithAllowlist(t *testing.T) {
	up := &stubCLIUpstream{releases: []string{"v0.20.9", "v0.21.8", "v0.19.1"}}
	data, sums := stubTarball("v0.20.9")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, []string{"0.20"}, up)
	art, err := svc.ResolveLatest(context.Background(), "linux", "amd64")
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if art.Version != "v0.20.9" {
		t.Fatalf("version = %q, want v0.20.9 (allowlist 0.20 only)", art.Version)
	}
}

func TestCLIServiceResolveLatestAllowlistExcludesAll(t *testing.T) {
	up := &stubCLIUpstream{releases: []string{"v0.21.8", "v0.20.0"}}
	svc := newCLITestService(t, []string{"0.22"}, up)

	_, err := svc.ResolveLatest(context.Background(), "linux", "amd64")
	if !errors.Is(err, domain.ErrCLINotFound) {
		t.Fatalf("err = %v, want ErrCLINotFound", err)
	}
}

func TestCLIServiceResolveLatestListError(t *testing.T) {
	up := &stubCLIUpstream{listErr: domain.ErrCLIUpstreamUnavailable}
	svc := newCLITestService(t, nil, up)

	_, err := svc.ResolveLatest(context.Background(), "linux", "amd64")
	if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
	}
}

func TestCLIServiceReleaseListTTLCaching(t *testing.T) {
	up := &stubCLIUpstream{releases: []string{"v0.21.8"}}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)
	if _, err := svc.ResolveLatest(context.Background(), "linux", "amd64"); err != nil {
		t.Fatalf("ResolveLatest #1: %v", err)
	}
	if _, err := svc.ResolveLatest(context.Background(), "linux", "amd64"); err != nil {
		t.Fatalf("ResolveLatest #2: %v", err)
	}

	listCalls, _, _ := up.counts()
	if listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 (TTL cached)", listCalls)
	}
}

func TestCLIServiceReleaseListSingleFlight(t *testing.T) {
	up := &stubCLIUpstream{releases: []string{"v0.21.8"}}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.ResolveLatest(context.Background(), "linux", "amd64")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	listCalls, _, _ := up.counts()
	if listCalls != 1 {
		t.Fatalf("listCalls = %d, want 1 (single-flight on cold release-list cache)", listCalls)
	}
}

func TestCLIServiceReleaseListTTLExpiry(t *testing.T) {
	up := &stubCLIUpstream{releases: []string{"v0.21.8"}}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	resolver, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cache, err := repository.NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	svc := NewCLIService(resolver, up, cache, "https://supv.example.com", 0, observ.NewTestLogger(), observ.NewMetrics(nil))

	for i := 0; i < 2; i++ {
		if _, err := svc.ResolveLatest(context.Background(), "linux", "amd64"); err != nil {
			t.Fatalf("ResolveLatest #%d: %v", i+1, err)
		}
	}
	listCalls, _, _ := up.counts()
	if listCalls != 2 {
		t.Fatalf("listCalls = %d, want 2 (TTL expired)", listCalls)
	}
}

func TestCLIServiceEnsureCachedCacheHitAvoidsFetch(t *testing.T) {
	up := &stubCLIUpstream{}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)
	if _, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64"); err != nil {
		t.Fatalf("EnsureCached #1: %v", err)
	}
	if _, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64"); err != nil {
		t.Fatalf("EnsureCached #2: %v", err)
	}

	_, checksumCalls, tarballCalls := up.counts()
	if checksumCalls != 1 || tarballCalls != 1 {
		t.Fatalf("checksumCalls=%d tarballCalls=%d, want 1/1", checksumCalls, tarballCalls)
	}
}

func TestCLIServiceEnsureCachedChecksumMismatch(t *testing.T) {
	up := &stubCLIUpstream{}
	data, _ := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = map[string]string{domain.AssetFilename("v0.21.8", "linux", "amd64"): "deadbeef"}

	svc := newCLITestService(t, nil, up)
	_, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64")
	if !errors.Is(err, domain.ErrCLIChecksumMismatch) {
		t.Fatalf("err = %v, want ErrCLIChecksumMismatch", err)
	}
}

func TestCLIServiceEnsureCachedVersionValidation(t *testing.T) {
	up := &stubCLIUpstream{}
	svc := newCLITestService(t, []string{"0.21"}, up)

	tests := []struct {
		name    string
		version string
	}{
		{name: "partial version", version: "0.21"},
		{name: "invalid version", version: "notaversion"},
		{name: "not allowed", version: "v0.20.5"},
		{name: "below floor", version: "v0.18.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.EnsureCached(context.Background(), tt.version, "linux", "amd64")
			if !errors.Is(err, domain.ErrCLIVersionNotAllowed) {
				t.Fatalf("err = %v, want ErrCLIVersionNotAllowed", err)
			}
		})
	}
}

func TestCLIServiceEnsureCachedFullPatchZero(t *testing.T) {
	up := &stubCLIUpstream{}
	data, sums := stubTarball("v0.21.0")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)
	// v0.21.0 is a full release (patch explicitly zero) and must be accepted,
	// not confused with the partial "0.21" form.
	art, err := svc.EnsureCached(context.Background(), "v0.21.0", "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureCached(v0.21.0): %v", err)
	}
	if art.Version != "v0.21.0" || art.Filename != "dagger_v0.21.0_linux_amd64.tar.gz" {
		t.Fatalf("artifact = %+v", art)
	}

	// A bare "0.21" (no patch) is still rejected as a partial.
	if _, err := svc.EnsureCached(context.Background(), "0.21", "linux", "amd64"); !errors.Is(err, domain.ErrCLIVersionNotAllowed) {
		t.Fatalf("EnsureCached(0.21) err = %v, want ErrCLIVersionNotAllowed", err)
	}
}

func TestCLIServiceEnsureCachedUpstreamErrors(t *testing.T) {
	t.Run("checksum fetch error", func(t *testing.T) {
		up := &stubCLIUpstream{checksumErr: domain.ErrCLIUpstreamUnavailable}
		svc := newCLITestService(t, nil, up)
		_, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})

	t.Run("missing checksum", func(t *testing.T) {
		up := &stubCLIUpstream{checksums: map[string]string{}}
		svc := newCLITestService(t, nil, up)
		_, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
			t.Fatalf("err = %v, want ErrCLIUpstreamUnavailable", err)
		}
	})

	t.Run("tarball fetch error", func(t *testing.T) {
		up := &stubCLIUpstream{tarballErr: domain.ErrCLINotFound}
		up.checksums = map[string]string{domain.AssetFilename("v0.21.8", "linux", "amd64"): "deadbeef"}
		svc := newCLITestService(t, nil, up)
		_, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64")
		if !errors.Is(err, domain.ErrCLINotFound) {
			t.Fatalf("err = %v, want ErrCLINotFound", err)
		}
	})
}

func TestCLIServiceEnsureCachedConcurrentSingleFetch(t *testing.T) {
	up := &stubCLIUpstream{}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	_, checksumCalls, tarballCalls := up.counts()
	if checksumCalls != 1 || tarballCalls != 1 {
		t.Fatalf("checksumCalls=%d tarballCalls=%d, want 1/1 (deduped)", checksumCalls, tarballCalls)
	}
}

func TestCLIServiceEnsureCachedConcurrentErrorPropagation(t *testing.T) {
	up := &stubCLIUpstream{}
	data, _ := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = map[string]string{domain.AssetFilename("v0.21.8", "linux", "amd64"): "deadbeef"}

	svc := newCLITestService(t, nil, up)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, domain.ErrCLIChecksumMismatch) {
			t.Fatalf("goroutine %d: err = %v, want ErrCLIChecksumMismatch", i, err)
		}
	}
}

// disappearingCache is a CLICache wrapper whose Get succeeds once then reports
// a miss, exercising Open's "cached tarball disappeared" path.
type disappearingCache struct {
	real domain.CLICache
	gets int
}

func (d *disappearingCache) Get(version, osName, arch string) (string, bool) {
	d.gets++
	if d.gets > 1 {
		return "", false
	}
	return d.real.Get(version, osName, arch)
}

func (d *disappearingCache) Put(version, osName, arch string, r io.Reader, sum string) (string, error) {
	return d.real.Put(version, osName, arch, r, sum)
}

func (d *disappearingCache) Dir() string { return d.real.Dir() }

func TestCLIServiceOpenCachedDisappeared(t *testing.T) {
	up := &stubCLIUpstream{}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	resolver, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	realCache, err := repository.NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	cache := &disappearingCache{real: realCache}
	svc := NewCLIService(resolver, up, cache, "https://supv.example.com", time.Hour, observ.NewTestLogger(), observ.NewMetrics(nil))

	_, _, err = svc.Open(context.Background(), "v0.21.8", "linux", "amd64")
	if !errors.Is(err, domain.ErrCLINotFound) {
		t.Fatalf("err = %v, want ErrCLINotFound", err)
	}
}

// bogusPathCache reports a cache hit at a path that does not exist, exercising
// Open's file-open error path.
type bogusPathCache struct{}

func (bogusPathCache) Get(_, _, _ string) (string, bool) { return "/nonexistent/tar.gz", true }
func (bogusPathCache) Put(_, _, _ string, _ io.Reader, _ string) (string, error) {
	return "/nonexistent/tar.gz", nil
}
func (bogusPathCache) Dir() string { return "/nonexistent" }

func TestCLIServiceOpenFileError(t *testing.T) {
	resolver, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	svc := NewCLIService(resolver, &stubCLIUpstream{}, bogusPathCache{}, "https://supv.example.com", time.Hour, observ.NewTestLogger(), observ.NewMetrics(nil))

	_, _, err = svc.Open(context.Background(), "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("Open = nil, want file-open error")
	}
}

func TestCLIServiceOpen(t *testing.T) {
	up := &stubCLIUpstream{}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	svc := newCLITestService(t, nil, up)
	rc, size, err := svc.Open(context.Background(), "v0.21.8", "linux", "amd64")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("body = %q, want %q", got, data)
	}
}

func TestCLIServiceOpenInvalidVersion(t *testing.T) {
	svc := newCLITestService(t, nil, &stubCLIUpstream{})
	_, _, err := svc.Open(context.Background(), "badversion", "linux", "amd64")
	if !errors.Is(err, domain.ErrCLIVersionNotAllowed) {
		t.Fatalf("err = %v, want ErrCLIVersionNotAllowed", err)
	}
}

func TestCLIServiceArtifactDegradesOnMissingFiles(t *testing.T) {
	svc := newCLITestService(t, nil, &stubCLIUpstream{})
	art := svc.artifact("v0.21.8", "linux", "amd64", "/nonexistent/artifact.tar.gz")
	if art.SHA256 != "" {
		t.Fatalf("sha = %q, want empty", art.SHA256)
	}
	if art.Size != -1 {
		t.Fatalf("size = %d, want -1", art.Size)
	}
}

func TestCLIServiceNilMetrics(t *testing.T) {
	up := &stubCLIUpstream{}
	data, sums := stubTarball("v0.21.8")
	up.tarball = data
	up.checksums = sums

	resolver, err := NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cache, err := repository.NewFileCLICache(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCLICache: %v", err)
	}
	svc := NewCLIService(resolver, up, cache, "https://supv.example.com", time.Hour, observ.NewTestLogger(), nil)

	// Cache miss + fetch path (nil metrics must not panic).
	if _, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64"); err != nil {
		t.Fatalf("EnsureCached: %v", err)
	}
	// Cache hit path.
	if _, err := svc.EnsureCached(context.Background(), "v0.21.8", "linux", "amd64"); err != nil {
		t.Fatalf("EnsureCached hit: %v", err)
	}
	// Error path (fetch failure → incCache("error")).
	if _, err := svc.EnsureCached(context.Background(), "v0.21.9", "linux", "amd64"); !errors.Is(err, domain.ErrCLIUpstreamUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
