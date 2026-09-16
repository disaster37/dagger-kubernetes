package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fakeOCIRegistry is a minimal in-memory OCI Distribution v2 registry (the Zot
// API contract): ping, catalog, tags, manifest GET (with Docker-Content-Digest)
// and manifest DELETE by digest.
type fakeOCIRegistry struct {
	mu    sync.Mutex
	repos map[string]map[string]fakeOCIImage // repo -> tag -> image
	srv   *httptest.Server
}

type fakeOCIImage struct {
	digest string
	size   int64
	layers int64
}

func newFakeOCIRegistry(t *testing.T, repos map[string]map[string]fakeOCIImage) *fakeOCIRegistry {
	t.Helper()
	f := &fakeOCIRegistry{repos: repos}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOCIRegistry) host() string {
	return strings.TrimPrefix(f.srv.URL, "http://")
}

func fakeDigest(repo, tag string) string {
	sum := sha256.Sum256([]byte(repo + ":" + tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *fakeOCIRegistry) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch path {
	case "/v2/", "/v2":
		w.WriteHeader(http.StatusOK)
		return
	case "/v2/_catalog":
		f.mu.Lock()
		names := make([]string, 0, len(f.repos))
		for name := range f.repos {
			names = append(names, name)
		}
		f.mu.Unlock()
		sort.Strings(names)
		writeJSONRaw(w, map[string]any{"repositories": names})
		return
	}

	rest := strings.TrimPrefix(path, "/v2/")
	if repo, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		f.mu.Lock()
		tags := make([]string, 0)
		for tag := range f.repos[repo] {
			tags = append(tags, tag)
		}
		f.mu.Unlock()
		sort.Strings(tags)
		writeJSONRaw(w, map[string]any{"name": repo, "tags": tags})
		return
	}

	idx := strings.LastIndex(rest, "/manifests/")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	repo, ref := rest[:idx], rest[idx+len("/manifests/"):]

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		f.mu.Lock()
		img, ok := f.repos[repo][ref]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", img.digest)
		layers := make([]map[string]any, 0, img.layers)
		for i := int64(0); i < img.layers; i++ {
			layers = append(layers, map[string]any{"digest": fmt.Sprintf("sha256:%064x", i), "size": img.size / (img.layers + 1)})
		}
		body, _ := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"config":        map[string]any{"digest": fakeDigest(repo, "config"), "size": img.size / (img.layers + 1)},
			"layers":        layers,
		})
		_, _ = w.Write(body)
	case http.MethodDelete:
		f.mu.Lock()
		removed := false
		for tag, img := range f.repos[repo] {
			if img.digest == ref {
				delete(f.repos[repo], tag)
				removed = true
			}
		}
		f.mu.Unlock()
		if !removed {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeJSONRaw(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// imageCacheTestEnv is a running supervisor with the image-cache service wired
// to a fake OCI registry, plus an admin API token.
type imageCacheTestEnv struct {
	base       string
	adminToken string
}

func newImageCacheTestEnv(t *testing.T, fake *fakeOCIRegistry) *imageCacheTestEnv {
	t.Helper()
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
	adminToken, _, err := tokensSvc.Generate(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))

	mirrors := []domain.ImageCacheMirror{
		{ID: "docker-io", Host: "docker.io", Upstream: "https://registry-1.docker.io", InternalAddr: fake.host(), Backend: "s3"},
		{ID: "down", Host: "down.example", Upstream: "https://down.example", InternalAddr: "127.0.0.1:1", Backend: "pvc"},
	}
	imageCacheSvc := service.NewImageCacheService(mirrors, func(addr string) domain.DistributionClient {
		return repository.NewDistributionClient(addr)
	}, logger)

	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(
		service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger),
		groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr := listenerAddr(controlLn)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        listenerAddr(dataLn),
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces,
		Logs: logsClient, JWT: jwtSvc, ImageCache: imageCacheSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(500 * time.Millisecond)

	return &imageCacheTestEnv{base: fmt.Sprintf("http://localhost%s", controlAddr), adminToken: adminToken}
}

func (e *imageCacheTestEnv) do(t *testing.T, method, path, payload string) (status int, body []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.base+path, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

func TestImageCacheListPrunePruneAllIntegration(t *testing.T) {
	fake := newFakeOCIRegistry(t, map[string]map[string]fakeOCIImage{
		"library/alpine": {
			"3.20": {digest: fakeDigest("library/alpine", "3.20"), size: 300, layers: 2},
			"3.21": {digest: fakeDigest("library/alpine", "3.21"), size: 310, layers: 2},
		},
		"library/busybox": {
			"latest": {digest: fakeDigest("library/busybox", "latest"), size: 200, layers: 1},
		},
	})
	env := newImageCacheTestEnv(t, fake)

	// --- List ---
	status, body := env.do(t, "GET", "/api/v1/image-cache", "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", status, body)
	}
	var info domain.ImageCacheInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(info.Mirrors) != 2 {
		t.Fatalf("mirrors = %d, want 2", len(info.Mirrors))
	}
	if !info.Mirrors[0].Reachable || info.Mirrors[0].Error != "" {
		t.Fatalf("reachable mirror = %+v", info.Mirrors[0])
	}
	if len(info.Mirrors[0].Repositories) != 2 {
		t.Fatalf("repos = %d, want 2", len(info.Mirrors[0].Repositories))
	}
	if info.Mirrors[1].Reachable || info.Mirrors[1].Error == "" {
		t.Fatalf("down mirror = %+v, want reachable:false + error in the 200 body", info.Mirrors[1])
	}

	// --- Prune selected (tag -> digest) ---
	status, body = env.do(t, "POST", "/api/v1/image-cache/prune",
		`{"mirror_id":"docker-io","refs":[{"repository":"library/alpine","tag":"3.20"}]}`)
	if status != http.StatusOK {
		t.Fatalf("prune status = %d, body=%s", status, body)
	}
	var prune domain.ImageCachePruneResult
	if err := json.Unmarshal(body, &prune); err != nil {
		t.Fatalf("decode prune: %v", err)
	}
	if prune.Pruned != 1 || prune.Errors != 0 {
		t.Fatalf("prune = %+v, want 1 pruned", prune)
	}

	// The tag is gone from a re-list.
	status, body = env.do(t, "GET", "/api/v1/image-cache", "")
	if status != http.StatusOK {
		t.Fatalf("re-list status = %d", status)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode re-list: %v", err)
	}
	alpine := findRepo(info.Mirrors[0].Repositories, "library/alpine")
	if alpine == nil || len(alpine.Tags) != 1 || alpine.Tags[0].Tag != "3.21" {
		t.Fatalf("alpine after prune = %+v, want only 3.21", alpine)
	}

	// --- Prune invalid digest -> 400 ---
	status, _ = env.do(t, "POST", "/api/v1/image-cache/prune",
		`{"mirror_id":"docker-io","refs":[{"repository":"library/alpine","digest":"nope"}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid digest status = %d, want 400", status)
	}

	// --- Unknown mirror -> 404 ---
	status, _ = env.do(t, "POST", "/api/v1/image-cache/prune",
		`{"mirror_id":"missing","refs":[{"repository":"library/alpine","tag":"3.21"}]}`)
	if status != http.StatusNotFound {
		t.Fatalf("unknown mirror status = %d, want 404", status)
	}

	// --- Prune all ---
	status, body = env.do(t, "POST", "/api/v1/image-cache/prune-all", `{"mirror_id":"docker-io"}`)
	if status != http.StatusOK {
		t.Fatalf("prune-all status = %d, body=%s", status, body)
	}
	var all domain.ImageCachePruneAllResult
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("decode prune-all: %v", err)
	}
	if len(all.Mirrors) != 1 || all.Mirrors[0].ManifestsPruned != 2 || all.Mirrors[0].RepositoriesProcessed != 2 {
		t.Fatalf("prune-all = %+v, want 2 manifests across 2 repos", all.Mirrors)
	}

	// Everything is gone.
	status, body = env.do(t, "GET", "/api/v1/image-cache", "")
	if status != http.StatusOK {
		t.Fatalf("final list status = %d", status)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("decode final list: %v", err)
	}
	if len(info.Mirrors[0].Repositories) != 0 {
		t.Fatalf("final repos = %+v, want empty", info.Mirrors[0].Repositories)
	}
}

func TestImageCachePruneAbsentIsIdempotentIntegration(t *testing.T) {
	fake := newFakeOCIRegistry(t, map[string]map[string]fakeOCIImage{
		"library/alpine": {"3.20": {digest: fakeDigest("library/alpine", "3.20"), size: 300, layers: 1}},
	})
	env := newImageCacheTestEnv(t, fake)

	body := `{"mirror_id":"docker-io","refs":[{"repository":"library/alpine","tag":"does-not-exist"}]}`
	status, raw := env.do(t, "POST", "/api/v1/image-cache/prune", body)
	if status != http.StatusOK {
		t.Fatalf("prune status = %d, body=%s", status, raw)
	}
	var prune domain.ImageCachePruneResult
	if err := json.Unmarshal(raw, &prune); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if prune.Pruned != 1 || prune.Errors != 0 {
		t.Fatalf("absent manifest prune = %+v, want idempotent success", prune)
	}
}

func findRepo(repos []domain.ImageCacheRepository, name string) *domain.ImageCacheRepository {
	for i := range repos {
		if repos[i].Repository == name {
			return &repos[i]
		}
	}
	return nil
}
