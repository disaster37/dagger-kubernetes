package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// stubCLICache is an in-memory cache for integration tests.
type stubCLICache struct {
	mu    sync.Mutex
	items map[string]*stubCacheItem
}

type stubCacheItem struct {
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

// buildCLITarball returns a gzip'd tar archive containing a single executable
// `dagger` entry, mimicking the upstream release asset.
func buildCLITarball(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "dagger", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// startCLIServer boots a full supervisor wired with a CLI service whose
// upstream points at the returned httptest server. Returns the control-plane
// URL, the admin token, and the upstream tarball digest.
func startCLIServer(t *testing.T, controlAddr, dataAddr string) (serverURL, adminToken, digest string, tarballBytes []byte) {
	t.Helper()

	tarball := buildCLITarball(t, "#!/bin/sh\necho dagger\n")
	digest = sha256Hex(tarball)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"tag_name":"v0.21.8"}]`)
		case "/download/v0.21.8/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", digest, domain.AssetFilename("v0.21.8", "linux", "amd64"))
		case "/download/v0.21.8/" + domain.AssetFilename("v0.21.8", "linux", "amd64"):
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("integration-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)

	admin, err := usersSvc.Create(context.Background(), "admin", "password123", domain.RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	adminToken, _, err = tokensSvc.Generate(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", []string{"0.21"}, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(
		service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger),
		groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	cliCache := newStubCLICache()
	cliUpstream := repository.NewGitHubCLIUpstream(repository.GitHubCLIUpstreamConfig{
		ReleasesURL:  upstream.URL + "/releases",
		DownloadBase: upstream.URL + "/download",
		Timeout:      10 * time.Second,
	})
	cliSvc := service.NewCLIService(versionResolver, cliUpstream, cliCache, "http://localhost"+controlAddr, time.Hour, logger, observ.NewMetrics(nil))

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: controlAddr,
		DataAddr:    dataAddr,
		DataHost:    "localhost",
		PipelineURL: "http://localhost" + controlAddr,
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store), CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
		CLI: cliSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	time.Sleep(500 * time.Millisecond)

	return fmt.Sprintf("http://localhost%s", controlAddr), adminToken, digest, tarball
}

func cliGet(t *testing.T, url, token string) (status int, header http.Header, body []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, body
}

func TestCLILatestEndpoint(t *testing.T) {
	serverURL, token, digest, _ := startCLIServer(t, freeAddr(t), freeAddr(t))

	status, _, body := cliGet(t, serverURL+"/api/v1/cli/versions/latest?os=linux&arch=amd64", token)
	if status != http.StatusOK {
		t.Fatalf("latest status = %d, body = %s", status, body)
	}

	var art domain.CLIArtifact
	if err := json.Unmarshal(body, &art); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if art.Version != "v0.21.8" {
		t.Fatalf("version = %q, want v0.21.8", art.Version)
	}
	if art.URL != serverURL+"/api/v1/cli/v0.21.8?os=linux&arch=amd64" {
		t.Fatalf("url = %q, want %s", art.URL, serverURL+"/api/v1/cli/v0.21.8?os=linux&arch=amd64")
	}
	if art.SHA256 != digest {
		t.Fatalf("sha = %q, want %q", art.SHA256, digest)
	}
}

func TestCLIDownloadEndpoint(t *testing.T) {
	serverURL, token, digest, tarball := startCLIServer(t, freeAddr(t), freeAddr(t))

	status, header, body := cliGet(t, serverURL+"/api/v1/cli/v0.21.8?os=linux&arch=amd64", token)
	if status != http.StatusOK {
		t.Fatalf("download status = %d, body = %s", status, body)
	}
	if ct := header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("Content-Type = %q, want application/gzip", ct)
	}
	if cd := header.Get("Content-Disposition"); cd != `attachment; filename="dagger_v0.21.8_linux_amd64.tar.gz"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if sha256Hex(body) != digest {
		t.Fatalf("body sha = %q, want %q", sha256Hex(body), digest)
	}
	if len(body) != len(tarball) {
		t.Fatalf("body len = %d, want %d", len(body), len(tarball))
	}

	// The streamed body must be a valid gzip'd tar with a `dagger` entry.
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if hdr.Name != "dagger" {
		t.Fatalf("entry name = %q, want dagger", hdr.Name)
	}
}

func TestCLIDownloadNotAllowedVersion(t *testing.T) {
	serverURL, token, _, _ := startCLIServer(t, freeAddr(t), freeAddr(t))

	status, _, _ := cliGet(t, serverURL+"/api/v1/cli/v0.22.0?os=linux&arch=amd64", token)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestCLIDownloadUnknownVersion(t *testing.T) {
	serverURL, token, _, _ := startCLIServer(t, freeAddr(t), freeAddr(t))

	// v0.21.99 is allowed (minor 0.21 in the allowlist) but does not exist
	// upstream, so the upstream 404 must surface as HTTP 404.
	status, _, _ := cliGet(t, serverURL+"/api/v1/cli/v0.21.99?os=linux&arch=amd64", token)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}
