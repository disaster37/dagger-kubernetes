package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

var testDigest = "sha256:" + strings.Repeat("a", 64)

// stubCacheRouter is an in-memory cacheRouter for handler tests.
type stubCacheRouter struct {
	backend     domain.RegistryBackend
	pullErr     error
	pushErr     error
	blobErr     error
	resumeErr   error
	uploadErr   error
	recordErr   error
	manifestErr error

	pullCalls   int
	blobCalls   int
	pushCalls   int
	startCalls  int
	resumeCalls int

	recordedSessions []string
	completedUploads []string
	recordedManifest []string
	markedDown       []string
}

func (s *stubCacheRouter) Backends() []domain.RegistryBackend {
	return []domain.RegistryBackend{s.backend}
}
func (s *stubCacheRouter) RouteForPull(context.Context, string, string) (domain.RegistryBackend, error) {
	s.pullCalls++
	return s.backend, s.pullErr
}
func (s *stubCacheRouter) RouteForBlobPull(context.Context, string, string) (domain.RegistryBackend, error) {
	s.blobCalls++
	return s.backend, s.blobErr
}
func (s *stubCacheRouter) RouteForPush(string) (domain.RegistryBackend, error) {
	s.pushCalls++
	return s.backend, s.pushErr
}
func (s *stubCacheRouter) RouteForUploadStart(string) (domain.RegistryBackend, error) {
	s.startCalls++
	return s.backend, s.uploadErr
}
func (s *stubCacheRouter) RouteForUploadResume(context.Context, string) (domain.RegistryBackend, error) {
	s.resumeCalls++
	return s.backend, s.resumeErr
}
func (s *stubCacheRouter) RecordUploadSession(_ context.Context, uuid, _, _ string) error {
	s.recordedSessions = append(s.recordedSessions, uuid)
	return s.recordErr
}
func (s *stubCacheRouter) CompleteUpload(_ context.Context, uuid, digest, _ string) error {
	s.completedUploads = append(s.completedUploads, uuid+":"+digest)
	return s.recordErr
}
func (s *stubCacheRouter) RecordManifest(_ context.Context, repo, tag, digest, _ string, _ int64) error {
	s.recordedManifest = append(s.recordedManifest, repo+":"+tag+":"+digest)
	return s.manifestErr
}
func (s *stubCacheRouter) MarkDown(id string) { s.markedDown = append(s.markedDown, id) }

func newReqCtx(method, path, query string) *app.RequestContext {
	c := app.NewContext(0)
	c.Request.Header.SetMethod(method)
	c.Request.URI().SetPath(path)
	if query != "" {
		c.Request.URI().SetQueryString(query)
	}
	return c
}

func newCacheServer(router cacheRouter, token, host string) *Server {
	return &Server{
		cfg:        &ServerConfig{CacheHost: host, CacheToken: token},
		logger:     observ.NewTestLogger(),
		router:     router,
		cacheToken: token,
	}
}

func TestRequireCacheAuth(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		auth    string
		wantOK  bool
		want401 bool
	}{
		{"bearer-correct", "secret", "Bearer secret", true, false},
		{"bearer-wrong", "secret", "Bearer wrong", false, true},
		{"bearer-empty", "secret", "", false, true},
		{"basic-password-correct", "secret", basicAuthHeader("any", "secret"), true, false},
		{"basic-password-wrong", "secret", basicAuthHeader("any", "nope"), false, true},
		{"dev-mode-allows", "", "", true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheServer(&stubCacheRouter{}, tc.token, "cache.example.com")
			c := app.NewContext(0)
			if tc.auth != "" {
				c.Request.Header.Set("Authorization", tc.auth)
			}
			got := s.requireCacheAuth(c)
			if got != tc.wantOK {
				t.Fatalf("requireCacheAuth = %v, want %v", got, tc.wantOK)
			}
			if tc.want401 && c.Response.StatusCode() != consts.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", c.Response.StatusCode())
			}
		})
	}
}

func TestExtractCacheToken(t *testing.T) {
	tests := []struct {
		name string
		auth string
		want string
	}{
		{"bearer", "Bearer tok", "tok"},
		{"basic", basicAuthHeader("user", "pass"), "pass"},
		{"basic-empty-password", basicAuthHeader("onlyuser", ""), ""},
		{"empty", "", ""},
		{"unknown-scheme", "Token abc", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := app.NewContext(0)
			if tc.auth != "" {
				c.Request.Header.Set("Authorization", tc.auth)
			}
			if got := extractCacheToken(c); got != tc.want {
				t.Fatalf("extractCacheToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRouteCacheRequest(t *testing.T) {
	backend := domain.RegistryBackend{ID: "b1", InternalAddr: "127.0.0.1:5000"}
	noBackend := func() *stubCacheRouter {
		return &stubCacheRouter{backend: backend}
	}

	tests := []struct {
		name       string
		stub       *stubCacheRouter
		method     string
		path       string
		query      string
		wantID     string
		wantKind   routeKind
		wantErr    error
		wantMethod string // which stub method should be called
	}{
		{"ping", noBackend(), "GET", "/v2/", "", "b1", routeOther, nil, "push"},
		{"manifest-get", noBackend(), "GET", "/v2/dagger-cache/manifests/v0-21-4", "", "b1", routeOther, nil, "pull"},
		{"manifest-head", noBackend(), "HEAD", "/v2/dagger-cache/manifests/v0-21-4", "", "b1", routeOther, nil, "pull"},
		{"manifest-put", noBackend(), "PUT", "/v2/dagger-cache/manifests/v0-21-4", "", "b1", routeManifest, nil, "push"},
		{"upload-start", noBackend(), "POST", "/v2/dagger-cache/blobs/uploads/", "", "b1", routeUploadStart, nil, "start"},
		{"upload-patch", noBackend(), "PATCH", "/v2/dagger-cache/blobs/uploads/uuid1", "", "b1", routeOther, nil, "resume"},
		{"upload-complete", noBackend(), "PUT", "/v2/dagger-cache/blobs/uploads/uuid1", "digest=" + testDigest, "b1", routeUploadComplete, nil, "resume"},
		{"blob-get", noBackend(), "GET", "/v2/dagger-cache/blobs/" + testDigest, "", "b1", routeOther, nil, "blob"},
		{"tags", noBackend(), "GET", "/v2/dagger-cache/tags/list", "", "b1", routeOther, nil, "push"},
		{"catalog", noBackend(), "GET", "/v2/_catalog", "", "b1", routeOther, nil, "push"},
		{"invalid-path", noBackend(), "GET", "/v2/dagger-cache/unknown", "", "", routeOther, service.ErrInvalidOCIPath, ""},
		{"bad-digest", noBackend(), "GET", "/v2/dagger-cache/blobs/not-a-digest", "", "", routeOther, service.ErrInvalidOCIPath, ""},
		{"manifest-ref-traversal", noBackend(), "GET", "/v2/dagger-cache/manifests/../../v2/_catalog", "", "", routeOther, service.ErrInvalidOCIPath, ""},
		{"manifest-repo-traversal", noBackend(), "GET", "/v2/../manifests/v0-21-4", "", "", routeOther, service.ErrInvalidOCIPath, ""},
		{"upload-uuid-traversal", noBackend(), "PATCH", "/v2/dagger-cache/blobs/uploads/..", "", "", routeOther, service.ErrInvalidOCIPath, ""},
		{"blob-repo-traversal", noBackend(), "GET", "/v2/../blobs/" + testDigest, "", "", routeOther, service.ErrInvalidOCIPath, ""},
		{"tags-repo-traversal", noBackend(), "GET", "/v2/../tags/list", "", "", routeOther, service.ErrInvalidOCIPath, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheServer(tc.stub, "", "cache.example.com")
			c := newReqCtx(tc.method, tc.path, tc.query)
			got, kind, err := s.routeCacheRequest(context.Background(), c)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if got.ID != tc.wantID {
				t.Fatalf("backend = %q, want %q", got.ID, tc.wantID)
			}
			if kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", kind, tc.wantKind)
			}
			switch tc.wantMethod {
			case "pull":
				if tc.stub.pullCalls != 1 {
					t.Fatalf("pullCalls = %d", tc.stub.pullCalls)
				}
			case "push":
				if tc.stub.pushCalls != 1 {
					t.Fatalf("pushCalls = %d", tc.stub.pushCalls)
				}
			case "start":
				if tc.stub.startCalls != 1 {
					t.Fatalf("startCalls = %d", tc.stub.startCalls)
				}
			case "resume":
				if tc.stub.resumeCalls != 1 {
					t.Fatalf("resumeCalls = %d", tc.stub.resumeCalls)
				}
			case "blob":
				if tc.stub.blobCalls != 1 {
					t.Fatalf("blobCalls = %d", tc.stub.blobCalls)
				}
			}
		})
	}
}

func TestWriteCacheRouteError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"invalid-oci", service.ErrInvalidOCIPath, consts.StatusBadRequest},
		{"no-backend", service.ErrNoBackend, consts.StatusServiceUnavailable},
		{"route-not-found", service.ErrRouteNotFound, consts.StatusNotFound},
		{"unknown", errors.New("boom"), consts.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheServer(&stubCacheRouter{}, "", "cache.example.com")
			c := app.NewContext(0)
			s.writeCacheRouteError(c, tc.err)
			if c.Response.StatusCode() != tc.status {
				t.Fatalf("status = %d, want %d", c.Response.StatusCode(), tc.status)
			}
		})
	}
}

func TestCacheProxyDirector(t *testing.T) {
	s := newCacheServer(&stubCacheRouter{
		backend: domain.RegistryBackend{ID: "b1", InternalAddr: "backend:5000"},
	}, "", "cache.example.com")
	director := s.cacheProxyDirector()

	req := protocol.NewRequest("GET", "http://example.com/v2/dagger-cache/manifests/v0-21-4", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Dagger-Cache-Target", "backend:5000")
	req.Header.Set("X-Dagger-Cache-User", "user")
	req.Header.Set("X-Dagger-Cache-Pass", "pass")

	director(req)

	if got := string(req.Header.Peek("Authorization")); got != basicAuthHeader("user", "pass") {
		t.Fatalf("Authorization = %q, want backend basic auth", got)
	}
	for _, h := range []string{"X-Dagger-Cache-Target", "X-Dagger-Cache-User", "X-Dagger-Cache-Pass"} {
		if req.Header.Peek(h) != nil {
			t.Fatalf("internal header %s not stripped", h)
		}
	}
	if string(req.Host()) != "backend:5000" {
		t.Fatalf("host = %q, want backend:5000", req.Host())
	}
}

func TestCacheProxyDirectorRejectsUnknownTarget(t *testing.T) {
	// Defense-in-depth (CWE-918): a target not present in validated config
	// must not be dialled, even if the internal header is somehow set.
	s := newCacheServer(&stubCacheRouter{
		backend: domain.RegistryBackend{ID: "b1", InternalAddr: "backend:5000"},
	}, "", "cache.example.com")
	director := s.cacheProxyDirector()

	req := protocol.NewRequest("GET", "http://example.com/v2/", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Dagger-Cache-Target", "evil.example:5000")
	req.Header.Set("X-Dagger-Cache-User", "user")
	req.Header.Set("X-Dagger-Cache-Pass", "pass")
	director(req)

	if got := string(req.Header.Peek("Authorization")); got != "" {
		t.Fatalf("Authorization = %q, want empty (target rejected)", got)
	}
	if string(req.Host()) == "evil.example:5000" {
		t.Fatalf("host = evil target, should not be retargeted")
	}
}

func TestCacheProxyDirectorNoCredsStripsAuth(t *testing.T) {
	s := newCacheServer(&stubCacheRouter{
		backend: domain.RegistryBackend{ID: "b1", InternalAddr: "backend:5000"},
	}, "", "cache.example.com")
	director := s.cacheProxyDirector()

	req := protocol.NewRequest("GET", "http://example.com/v2/", nil)
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("X-Dagger-Cache-Target", "backend:5000")
	director(req)

	if got := string(req.Header.Peek("Authorization")); got != "" {
		t.Fatalf("Authorization = %q, want empty (no backend creds)", got)
	}
}

func TestCacheProxyModifyResponse(t *testing.T) {
	s := newCacheServer(&stubCacheRouter{}, "", "cache.example.com")
	mr := s.cacheProxyModifyResponse()

	// Upload Location rewrite.
	resp := &protocol.Response{}
	resp.SetStatusCode(consts.StatusAccepted)
	resp.Header.Set("Location", "http://backend:5000/v2/dagger-cache/blobs/uploads/uuid1?digest="+testDigest)
	if err := mr(resp); err != nil {
		t.Fatalf("mr: %v", err)
	}
	got := resp.Header.Get("Location")
	want := "https://cache.example.com/v2/dagger-cache/blobs/uploads/uuid1?digest=" + testDigest
	if got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}

	// Non-upload Location untouched.
	resp2 := &protocol.Response{}
	resp2.Header.Set("Location", "http://backend:5000/v2/dagger-cache/manifests/v0-21-4")
	if err := mr(resp2); err != nil {
		t.Fatalf("mr2: %v", err)
	}
	if resp2.Header.Get("Location") != "http://backend:5000/v2/dagger-cache/manifests/v0-21-4" {
		t.Fatalf("Location = %q", resp2.Header.Get("Location"))
	}

	// 401 → strip WWW-Authenticate, return sentinel.
	resp3 := &protocol.Response{}
	resp3.SetStatusCode(consts.StatusUnauthorized)
	resp3.Header.Set("WWW-Authenticate", "Bearer realm=registry")
	err := mr(resp3)
	if !errors.Is(err, errBackendAuth) {
		t.Fatalf("err = %v, want errBackendAuth", err)
	}
	if resp3.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("WWW-Authenticate should be stripped on 401")
	}
}

func TestRewriteUploadLocationScheme(t *testing.T) {
	tests := []struct {
		name   string
		scheme string
		loc    string
		want   string
	}{
		{
			name:   "default-https",
			scheme: "",
			loc:    "http://backend:5000/v2/dagger-cache/blobs/uploads/uuid1?digest=" + testDigest,
			want:   "https://cache.example.com/v2/dagger-cache/blobs/uploads/uuid1?digest=" + testDigest,
		},
		{
			name:   "explicit-http",
			scheme: "http",
			loc:    "http://backend:5000/v2/dagger-cache/blobs/uploads/uuid1",
			want:   "http://cache.example.com/v2/dagger-cache/blobs/uploads/uuid1",
		},
		{
			name:   "explicit-https",
			scheme: "https",
			loc:    "http://backend:5000/v2/dagger-cache/blobs/uploads/uuid1",
			want:   "https://cache.example.com/v2/dagger-cache/blobs/uploads/uuid1",
		},
		{
			name:   "invalid-scheme-falls-back-to-https",
			scheme: "ftp",
			loc:    "http://backend:5000/v2/dagger-cache/blobs/uploads/uuid1",
			want:   "https://cache.example.com/v2/dagger-cache/blobs/uploads/uuid1",
		},
		{
			name:   "parse-failure-pass-through",
			scheme: "https",
			loc:    "://not-a-url",
			want:   "://not-a-url",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newCacheServer(&stubCacheRouter{}, "", "cache.example.com")
			s.cfg.CacheScheme = tc.scheme
			if got := s.rewriteUploadLocation(tc.loc); got != tc.want {
				t.Fatalf("rewriteUploadLocation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecordCacheRoute(t *testing.T) {
	backend := domain.RegistryBackend{ID: "b1", InternalAddr: "127.0.0.1:5000"}

	t.Run("manifest-put-201", func(t *testing.T) {
		stub := &stubCacheRouter{backend: backend}
		s := newCacheServer(stub, "", "cache.example.com")
		c := newReqCtx("PUT", "/v2/dagger-cache/manifests/v0-21-4", "")
		c.Response.SetStatusCode(consts.StatusCreated)
		c.Response.Header.Set("Docker-Content-Digest", testDigest)

		s.recordCacheRoute(context.Background(), c, backend, routeManifest)
		if len(stub.recordedManifest) != 1 || stub.recordedManifest[0] != "dagger-cache:v0-21-4:"+testDigest {
			t.Fatalf("recordedManifest = %v", stub.recordedManifest)
		}
	})

	t.Run("manifest-put-malformed-digest", func(t *testing.T) {
		stub := &stubCacheRouter{backend: backend}
		s := newCacheServer(stub, "", "cache.example.com")
		c := newReqCtx("PUT", "/v2/dagger-cache/manifests/v0-21-4", "")
		c.Response.SetStatusCode(consts.StatusCreated)
		c.Response.Header.Set("Docker-Content-Digest", "../../v2/_catalog")

		s.recordCacheRoute(context.Background(), c, backend, routeManifest)
		if len(stub.recordedManifest) != 1 || stub.recordedManifest[0] != "dagger-cache:v0-21-4:" {
			t.Fatalf("recordedManifest = %v", stub.recordedManifest)
		}
	})

	t.Run("manifest-put-non-2xx-noop", func(t *testing.T) {
		stub := &stubCacheRouter{backend: backend}
		s := newCacheServer(stub, "", "cache.example.com")
		c := newReqCtx("PUT", "/v2/dagger-cache/manifests/v0-21-4", "")
		c.Response.SetStatusCode(consts.StatusInternalServerError)

		s.recordCacheRoute(context.Background(), c, backend, routeManifest)
		if len(stub.recordedManifest) != 0 {
			t.Fatalf("recordedManifest = %v, want none", stub.recordedManifest)
		}
	})

	t.Run("upload-start-202", func(t *testing.T) {
		stub := &stubCacheRouter{backend: backend}
		s := newCacheServer(stub, "", "cache.example.com")
		c := newReqCtx("POST", "/v2/dagger-cache/blobs/uploads/", "")
		c.Response.SetStatusCode(consts.StatusAccepted)
		c.Response.Header.Set("Location", "https://cache.example.com/v2/dagger-cache/blobs/uploads/uuid1")

		s.recordCacheRoute(context.Background(), c, backend, routeUploadStart)
		if len(stub.recordedSessions) != 1 || stub.recordedSessions[0] != "uuid1" {
			t.Fatalf("recordedSessions = %v", stub.recordedSessions)
		}
	})

	t.Run("upload-complete-201", func(t *testing.T) {
		stub := &stubCacheRouter{backend: backend}
		s := newCacheServer(stub, "", "cache.example.com")
		c := newReqCtx("PUT", "/v2/dagger-cache/blobs/uploads/uuid1", "digest="+testDigest)
		c.Response.SetStatusCode(consts.StatusCreated)

		s.recordCacheRoute(context.Background(), c, backend, routeUploadComplete)
		if len(stub.completedUploads) != 1 || stub.completedUploads[0] != "uuid1:"+testDigest {
			t.Fatalf("completedUploads = %v", stub.completedUploads)
		}
	})

	t.Run("upload-complete-malformed-digest-ignored", func(t *testing.T) {
		stub := &stubCacheRouter{backend: backend}
		s := newCacheServer(stub, "", "cache.example.com")
		c := newReqCtx("PUT", "/v2/dagger-cache/blobs/uploads/uuid1", "digest=../../v2/_catalog")
		c.Response.SetStatusCode(consts.StatusCreated)

		s.recordCacheRoute(context.Background(), c, backend, routeUploadComplete)
		if len(stub.completedUploads) != 0 {
			t.Fatalf("completedUploads = %v, want none (malformed digest rejected)", stub.completedUploads)
		}
	})
}

func TestReadBoundedBodyRejectsOversized(t *testing.T) {
	// Buffered (non-stream) path: a body larger than max is rejected without
	// being returned to the handler (CWE-400/CWE-770).
	c := app.NewContext(0)
	c.Request.SetBody([]byte(strings.Repeat("x", maxControlBody+1)))

	_, err := readBoundedBody(c, maxControlBody)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v, want errBodyTooLarge", err)
	}

	// At-limit body is accepted.
	c2 := app.NewContext(0)
	c2.Request.SetBody([]byte(strings.Repeat("x", maxControlBody)))
	b, err := readBoundedBody(c2, maxControlBody)
	if err != nil || len(b) != maxControlBody {
		t.Fatalf("err=%v len=%d, want len=%d", err, len(b), maxControlBody)
	}
}

func TestCacheProxyEndToEnd(t *testing.T) {
	var sawAuthMu sync.Mutex
	var sawAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/dagger-cache/manifests/v0-21-4":
			sawAuthMu.Lock()
			sawAuth = r.Header.Get("Authorization")
			sawAuthMu.Unlock()
			w.Header().Set("Docker-Content-Digest", testDigest)
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg","size":0},"layers":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()

	stub := &stubCacheRouter{backend: domain.RegistryBackend{
		ID: "b1", InternalAddr: strings.TrimPrefix(backend.URL, "http://"), Username: "backend", Password: "secret",
	}}
	s := newCacheServer(stub, "client-token", "cache.example.com")
	s.buildProxies()
	if s.cacheProxy == nil {
		t.Fatal("cache proxy not built")
	}

	e := route.NewEngine(config.NewOptions(nil))
	e.Use(s.cacheHostMiddleware())

	// Correct token → proxied; backend receives its own Basic creds, not the
	// client's token.
	resp := ut.PerformRequest(e, "GET", "http://cache.example.com/v2/dagger-cache/manifests/v0-21-4", nil,
		ut.Header{Key: "Authorization", Value: "Bearer client-token"})
	if resp.Result().StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
	}
	sawAuthMu.Lock()
	gotAuth := sawAuth
	sawAuthMu.Unlock()
	if gotAuth != basicAuthHeader("backend", "secret") {
		t.Fatalf("backend saw Authorization = %q, want backend creds", gotAuth)
	}

	// Wrong token → 401 (never reaches backend).
	sawAuthMu.Lock()
	sawAuth = ""
	sawAuthMu.Unlock()
	resp = ut.PerformRequest(e, "GET", "http://cache.example.com/v2/dagger-cache/manifests/v0-21-4", nil,
		ut.Header{Key: "Authorization", Value: "Bearer wrong"})
	if resp.Result().StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Result().StatusCode())
	}
	sawAuthMu.Lock()
	gotAuth = sawAuth
	sawAuthMu.Unlock()
	if gotAuth != "" {
		t.Fatal("backend should not be reached on auth failure")
	}
}
