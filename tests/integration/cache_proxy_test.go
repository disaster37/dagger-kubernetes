package integration

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

const cacheProxyDigest = "sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

// fakeRegistryBackend emulates one OCI Distribution v2 registry.
type fakeRegistryBackend struct {
	id        string
	mu        sync.Mutex
	auth      string // last Authorization header received
	uploads   map[string]string
	blobs     map[string]bool
	manifests map[string]string
	hits      int
}

func newFakeRegistryBackend(t *testing.T, id string) (*fakeRegistryBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeRegistryBackend{
		id:        id,
		uploads:   map[string]string{},
		blobs:     map[string]bool{},
		manifests: map[string]string{},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		fb.auth = r.Header.Get("Authorization")
		fb.hits++

		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/blobs/uploads/"):
			repo := strings.TrimSuffix(strings.TrimPrefix(path, "/v2/"), "/blobs/uploads/")
			uuid := fmt.Sprintf("upload-%s-%d", fb.id, fb.hits)
			fb.uploads[uuid] = repo
			w.Header().Set("Location", fmt.Sprintf("http://%s/v2/%s/blobs/uploads/%s", r.Host, repo, uuid))
			w.WriteHeader(http.StatusAccepted)
		case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && strings.Contains(path, "/blobs/uploads/"):
			parts := strings.Split(strings.Trim(path, "/"), "/")
			uuid := parts[len(parts)-1]
			if _, ok := fb.uploads[uuid]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodPut {
				if dgst := r.URL.Query().Get("digest"); dgst != "" {
					fb.blobs[dgst] = true
					delete(fb.uploads, uuid)
				}
				w.WriteHeader(http.StatusCreated)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(path, "/manifests/"):
			key := manifestKeyFromPath(path)
			fb.manifests[key] = cacheProxyDigest
			w.Header().Set("Docker-Content-Digest", cacheProxyDigest)
			w.WriteHeader(http.StatusCreated)
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(path, "/manifests/"):
			key := manifestKeyFromPath(path)
			dgst, ok := fb.manifests[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", dgst)
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg","size":0},"layers":[]}`))
		case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.Contains(path, "/blobs/"):
			parts := strings.Split(strings.Trim(path, "/"), "/")
			if fb.blobs[parts[len(parts)-1]] {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case path == "/v2/" || path == "/v2":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return fb, ts
}

func manifestKeyFromPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/v2/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return ""
	}
	return fmt.Sprintf("%s:%s", strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-1])
}

func TestCacheProxyMultiBackendIntegration(t *testing.T) {
	logger := observ.NewTestLogger()

	fb1, ts1 := newFakeRegistryBackend(t, "reg-1")
	fb2, ts2 := newFakeRegistryBackend(t, "reg-2")

	store := newIntegrationStore(t)

	backends := []domain.RegistryBackend{
		{ID: "reg-1", InternalAddr: strings.TrimPrefix(ts1.URL, "http://"), Username: "u1", Password: "p1"},
		{ID: "reg-2", InternalAddr: strings.TrimPrefix(ts2.URL, "http://"), Username: "u2", Password: "p2"},
	}
	routes := repository.NewCacheRoutesRepo(store)
	router := service.NewRegistryRouter(backends, routes, func(b domain.RegistryBackend) domain.RegistryClient {
		return repository.NewRegistryStatsClientWithAuth(b.InternalAddr, b.Username, b.Password)
	}, logger)

	// Seed charge so reg-1 is heavier than reg-2; pushes must land on reg-2.
	if err := router.RecordManifest(context.Background(), "dagger-cache", "seed", cacheProxyDigest, "reg-1", 1000); err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	if err := router.RefreshCharges(context.Background()); err != nil {
		t.Fatalf("RefreshCharges: %v", err)
	}

	mintingCA, err := repository.NewMintingCA(2 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: ":18094",
		DataAddr:    ":18457",
		DataHost:    "localhost",
		CacheHost:   "cache.example.com",
		CacheToken:  "client-token",
	}, &handler.Deps{
		Logger:    logger,
		Metrics:   observ.NewMetrics(nil),
		MintingCA: mintingCA,
		Router:    router,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Shutdown(context.Background())
	time.Sleep(500 * time.Millisecond)

	base := "http://localhost:18094"
	client := &http.Client{Timeout: 5 * time.Second}

	doReq := func(method, url string, body io.Reader, headers map[string]string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Host = "cache.example.com"
		req.Header.Set("Authorization", "Bearer client-token")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// 1. Blob upload start → least-charged backend (reg-2).
	resp, _ := doReq(http.MethodPost, fmt.Sprintf("%s/v2/dagger-cache/blobs/uploads/", base), nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload start status = %d, want 202", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://cache.example.com/v2/dagger-cache/blobs/uploads/") {
		t.Fatalf("Location = %q, want rewritten to cache vhost", loc)
	}
	// Map the cache vhost back to the local test listener while keeping the
	// Host header (the engine would resolve cache.example.com via DNS/ingress).
	uploadURL := fmt.Sprintf("%s%s", base, strings.TrimPrefix(loc, "https://cache.example.com"))

	// 2. Upload PATCH + PUT to the rewritten Location → same backend affinity.
	resp, _ = doReq(http.MethodPatch, uploadURL, strings.NewReader("data"), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload patch status = %d, want 202", resp.StatusCode)
	}
	resp, _ = doReq(http.MethodPut, fmt.Sprintf("%s?digest=%s", uploadURL, cacheProxyDigest), strings.NewReader(""), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload complete status = %d, want 201", resp.StatusCode)
	}

	// 3. Manifest push → least-charged backend (reg-2).
	resp, _ = doReq(http.MethodPut, fmt.Sprintf("%s/v2/dagger-cache/manifests/v0-21-4", base), strings.NewReader(`{"config":{"digest":"sha256:cfg","size":0},"layers":[]}`), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("manifest push status = %d, want 201", resp.StatusCode)
	}

	// 4. Manifest pull → routes back to reg-2 (routing-table hit).
	resp, _ = doReq(http.MethodGet, fmt.Sprintf("%s/v2/dagger-cache/manifests/v0-21-4", base), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest pull status = %d, want 200", resp.StatusCode)
	}

	fb2.mu.Lock()
	reg2Hits := fb2.hits
	reg2Auth := fb2.auth
	fb2.mu.Unlock()
	fb1.mu.Lock()
	reg1Hits := fb1.hits
	fb1.mu.Unlock()

	if reg2Hits == 0 {
		t.Fatal("least-charged backend (reg-2) received no requests")
	}
	if reg1Hits != 0 {
		t.Fatalf("reg-1 received %d requests, want 0 (should be least-charged → reg-2)", reg1Hits)
	}
	// The engine's supervisor token must never reach the backend; the backend
	// sees its own Basic credentials instead.
	if reg2Auth != fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte("u2:p2"))) {
		t.Fatalf("backend Authorization = %q, want backend Basic creds", reg2Auth)
	}
	if strings.Contains(reg2Auth, "client-token") {
		t.Fatal("client token leaked to the backend")
	}
}
