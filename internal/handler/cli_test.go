package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// stubCLICache is an in-memory cache for handler tests. It stores tarballs
// keyed by version|os|arch and supports Has, Get, Put, and Dir.
type stubCLICache struct {
	mu    sync.Mutex
	items map[string]*stubCacheItem
}

type stubCacheItem struct {
	path string
	data []byte
}

func newStubCLICache() *stubCLICache {
	return &stubCLICache{items: make(map[string]*stubCacheItem)}
}

func (c *stubCLICache) key(version, osName, arch string) string {
	return version + "|" + osName + "|" + arch
}

func (c *stubCLICache) Has(ctx context.Context, version, osName, arch string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.items[c.key(version, osName, arch)]
	return ok, nil
}

func (c *stubCLICache) Get(ctx context.Context, version, osName, arch string) (string, bool) {
	c.mu.Lock()
	item, ok := c.items[c.key(version, osName, arch)]
	c.mu.Unlock()
	if !ok {
		return "", false
	}
	// Write data to a temp file so the caller can open it.
	f, err := os.CreateTemp("", "cli-cache-*")
	if err != nil {
		return "", false
	}
	if _, err := f.Write(item.data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", false
	}
	_ = f.Close()
	return f.Name(), true
}

func (c *stubCLICache) Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	computed := hex.EncodeToString(sum[:])
	if computed != sha256Hex {
		return "", domain.ErrCLIChecksumMismatch
	}
	c.mu.Lock()
	c.items[c.key(version, osName, arch)] = &stubCacheItem{data: data}
	c.mu.Unlock()
	return "", nil
}

func (c *stubCLICache) Dir() string { return "" }

// cliTestUpstream is a minimal CLIUpstream stub for handler tests.
type cliTestUpstream struct {
	releases    []string
	checksums   map[string]string
	tarball     []byte
	checksumErr error
	tarballErr  error
}

func (u *cliTestUpstream) List(context.Context) ([]string, error) { return u.releases, nil }
func (u *cliTestUpstream) FetchChecksums(_ context.Context, _ string) (map[string]string, error) {
	if u.checksumErr != nil {
		return nil, u.checksumErr
	}
	return u.checksums, nil
}
func (u *cliTestUpstream) FetchTarball(_ context.Context, _, _, _ string) (io.ReadCloser, int64, error) {
	if u.tarballErr != nil {
		return nil, 0, u.tarballErr
	}
	return io.NopCloser(bytes.NewReader(u.tarball)), int64(len(u.tarball)), nil
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newCLIHandlerEnv builds a Server whose CLI service + version resolver are
// backed by the supplied upstream stub (mirrors the production wiring where the
// same resolver gates both the handler and the service).
func newCLIHandlerEnv(t *testing.T, up domain.CLIUpstream, allowlist []string) *testEnv {
	t.Helper()
	env := newTestEnv(t)

	resolver, err := service.NewResolver("v0.19.0", allowlist, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cache := newStubCLICache()
	env.server.versionResolver = resolver
	env.server.cli = service.NewCLIService(resolver, up, cache, "https://supv.example.com", time.Hour, observ.NewTestLogger(), observ.NewMetrics(nil))
	return env
}

// cliUpstreamWithTarball builds a stub serving v0.21.8 for linux/amd64.
func cliUpstreamWithTarball(version string) (upstream *cliTestUpstream, tarball []byte) {
	data := []byte("tarball-bytes-" + version)
	filename := domain.AssetFilename(version, "linux", "amd64")
	return &cliTestUpstream{
		releases:  []string{version},
		checksums: map[string]string{filename: shaHex(data)},
		tarball:   data,
	}, data
}

func TestHandleCLILatestShape(t *testing.T) {
	up, data := cliUpstreamWithTarball("v0.21.8")
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/versions/latest?os=linux&arch=amd64", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Result().StatusCode(), resp.Result().Body())
	}

	var art domain.CLIArtifact
	if err := json.Unmarshal(resp.Result().Body(), &art); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if art.Version != "v0.21.8" || art.OS != "linux" || art.Arch != "amd64" {
		t.Fatalf("artifact = %+v", art)
	}
	if art.Filename != "dagger_v0.21.8_linux_amd64.tar.gz" {
		t.Fatalf("filename = %q", art.Filename)
	}
	if art.URL != "https://supv.example.com/api/v1/cli/v0.21.8?os=linux&arch=amd64" {
		t.Fatalf("url = %q", art.URL)
	}
	if art.SHA256 != shaHex(data) {
		t.Fatalf("sha = %q", art.SHA256)
	}
	if art.Size != int64(len(data)) {
		t.Fatalf("size = %d", art.Size)
	}
}

func TestHandleCLILatestDefaultsOSArch(t *testing.T) {
	up, _ := cliUpstreamWithTarball("v0.21.8")
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/versions/latest", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCLILatestNoAllowedVersion(t *testing.T) {
	up := &cliTestUpstream{releases: []string{"v0.18.0"}}
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/versions/latest", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCLIDownload(t *testing.T) {
	up, data := cliUpstreamWithTarball("v0.21.8")
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/v0.21.8?os=linux&arch=amd64", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	if ct := resp.Result().Header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("Content-Type = %q, want application/gzip", ct)
	}
	if cd := resp.Result().Header.Get("Content-Disposition"); cd != `attachment; filename="dagger_v0.21.8_linux_amd64.tar.gz"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if got := string(resp.Result().Body()); got != string(data) {
		t.Fatalf("body = %q, want %q", got, data)
	}
}

func TestHandleCLIAuthGating(t *testing.T) {
	up, _ := cliUpstreamWithTarball("v0.21.8")
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	for _, path := range []string{"/api/v1/cli/versions/latest", "/api/v1/cli/v0.21.8"} {
		resp := ut.PerformRequest(e, "GET", path, nil)
		if resp.Result().StatusCode() != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", path, resp.Result().StatusCode())
		}
	}
}

func TestHandleCLIDisabled(t *testing.T) {
	env := newTestEnv(t) // cli left nil
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	for _, path := range []string{"/api/v1/cli/versions/latest", "/api/v1/cli/v0.21.8"} {
		resp := ut.PerformRequest(e, "GET", path, nil, ut.Header{Key: "Authorization", Value: auth})
		if resp.Result().StatusCode() != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", path, resp.Result().StatusCode())
		}
	}
}

func TestHandleCLIInvalidOSArch(t *testing.T) {
	up, _ := cliUpstreamWithTarball("v0.21.8")
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/versions/latest?os=windows&arch=amd64", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}

	resp = ut.PerformRequest(e, "GET", "/api/v1/cli/v0.21.8?os=linux&arch=386", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("download: expected 400, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCLIDownloadInvalidVersion(t *testing.T) {
	up, _ := cliUpstreamWithTarball("v0.21.8")
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	for _, version := range []string{"0.21", "notaversion"} {
		resp := ut.PerformRequest(e, "GET", "/api/v1/cli/"+version, nil, ut.Header{Key: "Authorization", Value: auth})
		if resp.Result().StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", version, resp.Result().StatusCode())
		}
	}
}

func TestHandleCLIDownloadNotAllowed(t *testing.T) {
	up, _ := cliUpstreamWithTarball("v0.20.5")
	env := newCLIHandlerEnv(t, up, []string{"0.21"})
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/v0.20.5", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCLIDownloadUnknownVersion(t *testing.T) {
	up := &cliTestUpstream{
		releases:   []string{"v0.21.8"},
		checksums:  map[string]string{domain.AssetFilename("v9.9.9", "linux", "amd64"): "deadbeef"},
		tarballErr: domain.ErrCLINotFound,
	}
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/v9.9.9", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCLIDownloadChecksumMismatch(t *testing.T) {
	up := &cliTestUpstream{
		releases:  []string{"v0.21.8"},
		checksums: map[string]string{domain.AssetFilename("v0.21.8", "linux", "amd64"): "deadbeef"},
		tarball:   []byte("data"),
	}
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/v0.21.8", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.Result().StatusCode())
	}
}

func TestHandleCLIDownloadUpstreamUnavailable(t *testing.T) {
	up := &cliTestUpstream{releases: []string{"v0.21.8"}, checksumErr: domain.ErrCLIUpstreamUnavailable}
	env := newCLIHandlerEnv(t, up, nil)
	e := newTestEngine(env.server)

	auth := env.loginAsAdmin(t)
	resp := ut.PerformRequest(e, "GET", "/api/v1/cli/v0.21.8", nil, ut.Header{Key: "Authorization", Value: auth})
	if resp.Result().StatusCode() != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.Result().StatusCode())
	}
}

func TestWriteCLIErrorDefault(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	e := newTestEngine(s)

	e.GET("/test-cli-error", func(_ context.Context, c *app.RequestContext) {
		s.writeCLIError(c, errors.New("boom"))
	})

	resp := ut.PerformRequest(e, "GET", "/test-cli-error", nil)
	if resp.Result().StatusCode() != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.Result().StatusCode())
	}
	if got := string(resp.Result().Body()); !bytes.Contains([]byte(got), []byte("cli provisioning failed")) {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteCLIErrorVersionNotAllowed(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	e := newTestEngine(s)

	e.GET("/test-cli-not-allowed", func(_ context.Context, c *app.RequestContext) {
		s.writeCLIError(c, domain.ErrCLIVersionNotAllowed)
	})

	resp := ut.PerformRequest(e, "GET", "/test-cli-not-allowed", nil)
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Result().StatusCode())
	}
}
