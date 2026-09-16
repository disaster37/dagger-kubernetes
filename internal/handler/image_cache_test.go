package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// stubImageCacheService is a configurable domain.ImageCacheService for handler
// tests; it records the request arguments and returns canned results/errors.
type stubImageCacheService struct {
	info           *domain.ImageCacheInfo
	infoErr        error
	pruneResult    *domain.ImageCachePruneResult
	pruneErr       error
	pruneAllResult *domain.ImageCachePruneAllResult
	pruneAllErr    error

	lastMirror    string
	lastRefs      []domain.ImageCachePruneRef
	lastAllMirror string
}

func (s *stubImageCacheService) List(context.Context) (*domain.ImageCacheInfo, error) {
	return s.info, s.infoErr
}

func (s *stubImageCacheService) Prune(_ context.Context, mirrorID string, refs []domain.ImageCachePruneRef) (*domain.ImageCachePruneResult, error) {
	s.lastMirror, s.lastRefs = mirrorID, refs
	return s.pruneResult, s.pruneErr
}

func (s *stubImageCacheService) PruneAll(_ context.Context, mirrorID string) (*domain.ImageCachePruneAllResult, error) {
	s.lastAllMirror = mirrorID
	return s.pruneAllResult, s.pruneAllErr
}

var _ domain.ImageCacheService = (*stubImageCacheService)(nil)

func newImageCacheEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.GET("/api/v1/image-cache", s.adminOnly(s.handleImageCacheInfo))
	e.POST("/api/v1/image-cache/prune", s.adminOnly(s.handleImageCachePrune))
	e.POST("/api/v1/image-cache/prune-all", s.adminOnly(s.handleImageCachePruneAll))
	return e
}

// newTestServerForImageCacheEnv returns a Server (with auth) whose image cache
// collaborator is the supplied stub. Passing nil leaves it unconfigured.
func newTestServerForImageCacheEnv(t *testing.T, svc domain.ImageCacheService) (srv *Server, bearer string) {
	t.Helper()
	env := newTestEnv(t)
	env.server.imageCache = svc
	return env.server, env.loginAsAdmin(t)
}

func TestHandleImageCacheInfo(t *testing.T) {
	stub := &stubImageCacheService{info: &domain.ImageCacheInfo{
		Mirrors:     []domain.ImageCacheMirrorInfo{{ID: "docker-io", Reachable: true, Repositories: []domain.ImageCacheRepository{{Repository: "library/alpine", Tags: []domain.ImageCacheTag{{Tag: "3.20", Digest: "sha256:x"}}}}}},
		CollectedAt: "2026-01-02T03:04:05Z",
	}}
	s, bearer := newTestServerForImageCacheEnv(t, stub)

	resp := ut.PerformRequest(newImageCacheEngine(s), "GET", "/api/v1/image-cache", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}
	var info domain.ImageCacheInfo
	if err := json.Unmarshal(resp.Result().Body(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(info.Mirrors) != 1 || info.Mirrors[0].ID != "docker-io" {
		t.Fatalf("info = %+v", info)
	}
}

func TestHandleImageCacheInfoNilService(t *testing.T) {
	s, bearer := newTestServerForImageCacheEnv(t, nil)

	resp := ut.PerformRequest(newImageCacheEngine(s), "GET", "/api/v1/image-cache", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}
	var info domain.ImageCacheInfo
	if err := json.Unmarshal(resp.Result().Body(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Mirrors == nil || len(info.Mirrors) != 0 {
		t.Fatalf("mirrors = %v, want empty array", info.Mirrors)
	}
}

func TestHandleImageCachePrune(t *testing.T) {
	stub := &stubImageCacheService{pruneResult: &domain.ImageCachePruneResult{
		MirrorID: "docker-io",
		Items:    []domain.ImageCachePruneItem{{Repository: "library/alpine", Tag: "3.20", Digest: "sha256:x", Pruned: true}},
		Pruned:   1,
	}}
	s, bearer := newTestServerForImageCacheEnv(t, stub)

	body := `{"mirror_id":"docker-io","refs":[{"repository":"library/alpine","tag":"3.20"}]}`
	resp := ut.PerformRequest(newImageCacheEngine(s), "POST", "/api/v1/image-cache/prune",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	if stub.lastMirror != "docker-io" || len(stub.lastRefs) != 1 {
		t.Fatalf("service saw mirror=%q refs=%+v", stub.lastMirror, stub.lastRefs)
	}
}

func TestHandleImageCachePruneBodyErrors(t *testing.T) {
	stub := &stubImageCacheService{}
	s, bearer := newTestServerForImageCacheEnv(t, stub)
	e := newImageCacheEngine(s)

	// malformed JSON -> 400
	resp := ut.PerformRequest(e, "POST", "/api/v1/image-cache/prune",
		&ut.Body{Body: strings.NewReader("{not json"), Len: 9},
		ut.Header{Key: "Authorization", Value: bearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400", resp.Result().StatusCode())
	}
	if stub.lastMirror != "" {
		t.Fatal("service must not be called on a malformed body")
	}
}

func TestHandleImageCacheErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"invalid ref", domain.ErrImageCacheInvalidRef, http.StatusBadRequest},
		{"unknown mirror", domain.ErrImageCacheMirrorNotFound, http.StatusNotFound},
		{"delete disabled", domain.ErrRegistryDeleteDisabled, http.StatusConflict},
		{"catalog disabled", domain.ErrRegistryCatalogDisabled, http.StatusConflict},
		{"unreachable", domain.ErrImageCacheUnreachable, http.StatusBadGateway},
		{"internal", context.DeadlineExceeded, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubImageCacheService{pruneErr: tt.err}
			s, bearer := newTestServerForImageCacheEnv(t, stub)
			body := `{"mirror_id":"m","refs":[{"repository":"r","tag":"t"}]}`
			resp := ut.PerformRequest(newImageCacheEngine(s), "POST", "/api/v1/image-cache/prune",
				&ut.Body{Body: strings.NewReader(body), Len: len(body)},
				ut.Header{Key: "Authorization", Value: bearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
			if resp.Result().StatusCode() != tt.want {
				t.Fatalf("status = %d, want %d", resp.Result().StatusCode(), tt.want)
			}
		})
	}
}

func TestHandleImageCachePruneAll(t *testing.T) {
	stub := &stubImageCacheService{pruneAllResult: &domain.ImageCachePruneAllResult{
		Mirrors: []domain.ImageCachePruneAllMirror{{MirrorID: "docker-io", ManifestsPruned: 3, RepositoriesProcessed: 2}},
	}}
	s, bearer := newTestServerForImageCacheEnv(t, stub)

	// Explicit mirror.
	body := `{"mirror_id":"docker-io"}`
	resp := ut.PerformRequest(newImageCacheEngine(s), "POST", "/api/v1/image-cache/prune-all",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Result().StatusCode(), resp.Result().Body())
	}
	if stub.lastAllMirror != "docker-io" {
		t.Fatalf("mirror = %q, want docker-io", stub.lastAllMirror)
	}

	// No body tolerates an absent request (all mirrors).
	resp = ut.PerformRequest(newImageCacheEngine(s), "POST", "/api/v1/image-cache/prune-all", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("empty-body status = %d, want 200", resp.Result().StatusCode())
	}
	if stub.lastAllMirror != "" {
		t.Fatalf("mirror = %q, want empty (all)", stub.lastAllMirror)
	}
}

func TestHandleImageCacheNilServicePrune(t *testing.T) {
	s, bearer := newTestServerForImageCacheEnv(t, nil)

	body := `{"mirror_id":"docker-io","refs":[{"repository":"r","tag":"t"}]}`
	resp := ut.PerformRequest(newImageCacheEngine(s), "POST", "/api/v1/image-cache/prune",
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Authorization", Value: bearer}, ut.Header{Key: "Content-Type", Value: "application/json"})
	if resp.Result().StatusCode() != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.Result().StatusCode())
	}

	resp = ut.PerformRequest(newImageCacheEngine(s), "POST", "/api/v1/image-cache/prune-all", nil,
		ut.Header{Key: "Authorization", Value: bearer})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("prune-all status = %d, want 200", resp.Result().StatusCode())
	}
	var result domain.ImageCachePruneAllResult
	if err := json.Unmarshal(resp.Result().Body(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Mirrors == nil || len(result.Mirrors) != 0 {
		t.Fatalf("mirrors = %v, want empty array", result.Mirrors)
	}
}

func TestHandleImageCacheAdminOnly(t *testing.T) {
	stub := &stubImageCacheService{info: &domain.ImageCacheInfo{Mirrors: []domain.ImageCacheMirrorInfo{}}}
	env := newTestEnv(t)
	env.server.imageCache = stub
	e := newImageCacheEngine(env.server)
	userBearer, _ := env.createUserAndToken(t)

	for _, req := range []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/image-cache"},
		{"POST", "/api/v1/image-cache/prune"},
		{"POST", "/api/v1/image-cache/prune-all"},
	} {
		resp := ut.PerformRequest(e, req.method, req.path, nil, ut.Header{Key: "Authorization", Value: userBearer})
		if resp.Result().StatusCode() != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, want 403 for non-admin", req.method, req.path, resp.Result().StatusCode())
		}
	}
}
