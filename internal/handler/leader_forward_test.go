package handler

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// fakeLeaderInfo implements leaderInfo for tests.
type fakeLeaderInfo struct {
	leader bool
	addr   string
}

func (f *fakeLeaderInfo) IsLeader() bool        { return f.leader }
func (f *fakeLeaderInfo) LeaderAddress() string { return f.addr }

// leaderBackend is an httptest leader that records the last forwarded request.
type leaderBackend struct {
	srv *httptest.Server

	mu        sync.Mutex
	hits      int
	method    string
	path      string
	body      string
	forwarded string
}

func newLeaderBackend(t *testing.T) *leaderBackend {
	t.Helper()
	lb := &leaderBackend{}
	lb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lb.mu.Lock()
		lb.hits++
		lb.method = r.Method
		lb.path = r.URL.RequestURI()
		lb.body = string(b)
		lb.forwarded = r.Header.Get(leaderForwardedHeader)
		lb.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("from-leader"))
	}))
	t.Cleanup(lb.srv.Close)
	return lb
}

// hostPort returns the backend's host:port.
func (lb *leaderBackend) hostPort() string {
	return strings.TrimPrefix(lb.srv.URL, "http://")
}

func (lb *leaderBackend) snapshot() (hits int, method, path, body, forwarded string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.hits, lb.method, lb.path, lb.body, lb.forwarded
}

func (lb *leaderBackend) reset() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.hits, lb.method, lb.path, lb.body, lb.forwarded = 0, "", "", "", ""
}

// newLeaderForwardEngine builds a route engine with the leader-forward
// middleware and a catch-all local handler that echoes the request.
func newLeaderForwardEngine(s *Server) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.Use(s.leaderForward())
	e.Any("/*path", func(_ context.Context, c *app.RequestContext) {
		c.String(consts.StatusOK, "local:%s:%s", string(c.Method()), string(c.Path()))
	})
	return e
}

func TestLeaderPinnedRoute(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{"POST", "/api/v1/users", true},
		{"PUT", "/api/v1/users/1", true},
		{"DELETE", "/api/v1/users/1", true},
		{"PATCH", "/api/v1/users/1", true},
		{"POST", "/v1/engines", true},
		{"POST", "/v1/logs", true},
		{"POST", "/v1/traces", true},
		{"GET", "/api/v1/traces/abc/live", true},
		{"GET", "/api/v1/fleet/v0.19.0/purge-cache", true},
		// OAuth callbacks are GETs that perform Raft writes (EnsureOAuthUser
		// + group reconciliation), so they must be leader-pinned. The
		// provider-specific login GETs are read-only and follower-safe.
		{"GET", "/api/v1/auth/oauth/github/callback", true},
		{"GET", "/api/v1/auth/oauth/oidc/callback", true},
		{"GET", "/api/v1/auth/oauth/github/login", false},
		{"GET", "/api/v1/auth/oauth/oidc/login", false},
		{"GET", "/api/v1/auth/providers", false},
		{"GET", "/api/v1/traces", false},
		{"GET", "/api/v1/traces/abc", false},
		{"GET", "/api/v1/traces/abc/logs", false},
		{"GET", "/api/v1/traces/abc/metrics", false},
		{"GET", "/api/v1/fleet", false},
		{"GET", "/api/v1/fleet/v0.19.0", false},
		{"GET", "/api/v1/status", false},
		{"GET", "/api/v1/users", false},
		{"HEAD", "/api/v1/status", false},
		{"OPTIONS", "/api/v1/status", false},
	}
	for _, tc := range tests {
		if got := leaderPinnedRoute(tc.method, tc.path); got != tc.want {
			t.Errorf("leaderPinnedRoute(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestLeaderForwardRouting(t *testing.T) {
	backend := newLeaderBackend(t)
	backendHostPort := backend.hostPort()
	_, backendPort, err := net.SplitHostPort(backendHostPort)
	if err != nil {
		t.Fatalf("split backend host:port: %v", err)
	}
	controlAddr := ":" + backendPort

	// A closed port for the transport-error case.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dead: %v", err)
	}
	deadAddr := deadLn.Addr().String()
	_ = deadLn.Close()

	env := newTestEnv(t)
	s := env.server
	s.cfg.ControlAddr = controlAddr
	if err := s.buildLeaderForward(); err != nil {
		t.Fatalf("buildLeaderForward: %v", err)
	}
	e := newLeaderForwardEngine(s)

	tests := []struct {
		name          string
		leader        bool
		addr          string
		controlAddr   string
		method        string
		path          string
		body          string
		forwardHeader bool
		wantStatus    int
		wantBody      string
		wantForwarded bool
	}{
		{
			name:       "leader serves locally",
			leader:     true,
			addr:       backendHostPort,
			method:     "POST",
			path:       "/api/v1/users",
			body:       "payload",
			wantStatus: http.StatusOK,
			wantBody:   "local:POST:/api/v1/users",
		},
		{
			name:       "follower serves a read locally",
			leader:     false,
			addr:       backendHostPort,
			method:     "GET",
			path:       "/api/v1/traces/xyz",
			wantStatus: http.StatusOK,
			wantBody:   "local:GET:/api/v1/traces/xyz",
		},
		{
			name:          "follower forwards a write",
			leader:        false,
			addr:          backendHostPort,
			method:        "POST",
			path:          "/api/v1/users",
			body:          "payload",
			wantStatus:    http.StatusOK,
			wantBody:      "from-leader",
			wantForwarded: true,
		},
		{
			name:          "follower forwards SSE live (GET, leader-pinned)",
			leader:        false,
			addr:          backendHostPort,
			method:        "GET",
			path:          "/api/v1/traces/xyz/live",
			wantStatus:    http.StatusOK,
			wantBody:      "from-leader",
			wantForwarded: true,
		},
		{
			name:          "follower forwards purge-cache status (GET, leader-pinned)",
			leader:        false,
			addr:          backendHostPort,
			method:        "GET",
			path:          "/api/v1/fleet/v0.19.0/purge-cache",
			wantStatus:    http.StatusOK,
			wantBody:      "from-leader",
			wantForwarded: true,
		},
		{
			name:          "follower forwards github oauth callback (GET, Raft write)",
			leader:        false,
			addr:          backendHostPort,
			method:        "GET",
			path:          "/api/v1/auth/oauth/github/callback?code=abc&state=xyz",
			wantStatus:    http.StatusOK,
			wantBody:      "from-leader",
			wantForwarded: true,
		},
		{
			name:          "follower forwards oidc oauth callback (GET, Raft write)",
			leader:        false,
			addr:          backendHostPort,
			method:        "GET",
			path:          "/api/v1/auth/oauth/oidc/callback?code=abc&state=xyz",
			wantStatus:    http.StatusOK,
			wantBody:      "from-leader",
			wantForwarded: true,
		},
		{
			name:          "already-forwarded served locally",
			leader:        false,
			addr:          backendHostPort,
			method:        "POST",
			path:          "/api/v1/users",
			body:          "payload",
			forwardHeader: true,
			wantStatus:    http.StatusOK,
			wantBody:      "local:POST:/api/v1/users",
		},
		{
			name:       "no leader known",
			leader:     false,
			addr:       "",
			method:     "POST",
			path:       "/api/v1/users",
			body:       "payload",
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   `{"message":"no raft leader available"}`,
		},
		{
			name:        "leader unreachable",
			leader:      false,
			addr:        deadAddr,
			controlAddr: deadAddr,
			method:      "POST",
			path:        "/api/v1/users",
			body:        "payload",
			wantStatus:  http.StatusBadGateway,
			wantBody:    `{"message":"leader unreachable"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend.reset()
			s.leaderInfo = &fakeLeaderInfo{leader: tc.leader, addr: tc.addr}
			if tc.controlAddr != "" {
				s.cfg.ControlAddr = tc.controlAddr
				t.Cleanup(func() { s.cfg.ControlAddr = controlAddr })
			} else {
				s.cfg.ControlAddr = controlAddr
			}

			var body *ut.Body
			if tc.body != "" {
				body = &ut.Body{Body: strings.NewReader(tc.body), Len: len(tc.body)}
			}
			var headers []ut.Header
			if tc.forwardHeader {
				headers = append(headers, ut.Header{Key: leaderForwardedHeader, Value: "1"})
			}
			resp := ut.PerformRequest(e, tc.method, tc.path, body, headers...)
			if got := resp.Result().StatusCode(); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", got, tc.wantStatus, resp.Result().Body())
			}
			if got := strings.TrimSpace(string(resp.Result().Body())); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}

			hits, method, path, gotBody, forwarded := backend.snapshot()
			if tc.wantForwarded {
				if hits != 1 {
					t.Fatalf("backend hits = %d, want 1", hits)
				}
				if method != tc.method || path != tc.path {
					t.Fatalf("backend got %s %s, want %s %s", method, path, tc.method, tc.path)
				}
				if gotBody != tc.body {
					t.Fatalf("backend body = %q, want %q", gotBody, tc.body)
				}
				if forwarded != "1" {
					t.Fatalf("backend forwarded header = %q, want 1", forwarded)
				}
			} else if hits != 0 {
				t.Fatalf("backend must not be hit for a locally-served request, hits = %d", hits)
			}
		})
	}
}

func TestControlForwardTarget(t *testing.T) {
	env := newTestEnv(t)
	s := env.server

	tests := []struct {
		name        string
		controlAddr string
		certPath    string
		keyPath     string
		leaderAddr  string
		wantScheme  string
		wantHost    string
		wantOK      bool
	}{
		{
			name:        "bare dev http",
			controlAddr: ":8080",
			leaderAddr:  "pod-0.headless.ns.svc.cluster.local:8081",
			wantScheme:  "http",
			wantHost:    "pod-0.headless.ns.svc.cluster.local:8080",
			wantOK:      true,
		},
		{
			name:        "tls configured https",
			controlAddr: "0.0.0.0:8443",
			certPath:    "/tls/tls.crt",
			keyPath:     "/tls/tls.key",
			leaderAddr:  "pod-1.headless.ns.svc:8081",
			wantScheme:  "https",
			wantHost:    "pod-1.headless.ns.svc:8443",
			wantOK:      true,
		},
		{
			name:        "no leader address",
			controlAddr: ":8080",
			leaderAddr:  "",
			wantOK:      false,
		},
		{
			name:        "malformed leader address",
			controlAddr: ":8080",
			leaderAddr:  "not-a-hostport",
			wantOK:      false,
		},
		{
			name:        "malformed control address",
			controlAddr: "not-a-hostport",
			leaderAddr:  "pod-0.headless.ns.svc:8081",
			wantOK:      false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.cfg.ControlAddr = tc.controlAddr
			s.cfg.CertPath = tc.certPath
			s.cfg.KeyPath = tc.keyPath
			scheme, host, ok := s.controlForwardTarget(tc.leaderAddr)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if scheme != tc.wantScheme || host != tc.wantHost {
				t.Fatalf("target = %s://%s, want %s://%s", scheme, host, tc.wantScheme, tc.wantHost)
			}
		})
	}
}

func TestLeaderForwardTLSConfig(t *testing.T) {
	env := newTestEnv(t)
	s := env.server

	pool := x509.NewCertPool()
	s.leaderForwardRootCAs = pool
	cfg := s.leaderForwardTLSConfig()
	if cfg.RootCAs != pool {
		t.Fatal("embedded mode must use the injected minting CA pool as RootCAs")
	}
	if cfg.ServerName != "" {
		t.Fatalf("ServerName = %q, want empty so Go derives it from the dialed leader FQDN", cfg.ServerName)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify must never be enabled for the forward hop")
	}

	// cert-manager/external: nil pool = system pool.
	s.leaderForwardRootCAs = nil
	cfg = s.leaderForwardTLSConfig()
	if cfg.RootCAs != nil {
		t.Fatal("cert-manager/external mode must leave RootCAs nil (system pool)")
	}
}

// TestLeaderForwardTLSVerification proves exact-FQDN verification works with
// the minting CA pool produced by the embedded provider: a leaf whose SAN is
// the leader's exact FQDN verifies, a non-matching name fails.
func TestLeaderForwardTLSVerification(t *testing.T) {
	ca, err := repository.NewMintingCA(time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssuePeerCertificate(
		"supervisor-server", "dagger-kubernetes",
		[]string{"leader.headless.ns.svc.cluster.local"},
		[]net.IP{net.ParseIP("127.0.0.1")},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("IssuePeerCertificate: %v", err)
	}
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{leaf},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if tc, ok := conn.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
			}(conn)
		}
	}()

	env := newTestEnv(t)
	s := env.server
	s.leaderForwardRootCAs = ca.CertPool()

	good := s.leaderForwardTLSConfig()
	good.ServerName = "leader.headless.ns.svc.cluster.local"
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(), good)
	if err != nil {
		t.Fatalf("dial with exact FQDN SAN: %v", err)
	}
	_ = conn.Close()

	bad := s.leaderForwardTLSConfig()
	bad.ServerName = "wrong.headless.ns.svc.cluster.local"
	if _, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ln.Addr().String(), bad); err == nil {
		t.Fatal("dial with a non-matching name must fail verification")
	}
}

// TestLeaderForwardStreamsSSE proves the /live route is forwarded with
// incremental flush (not buffered until EOF): the backend holds the second
// event until the test has read the first.
func TestLeaderForwardStreamsSSE(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: one\ndata: {\"n\":1}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-release
		_, _ = w.Write([]byte("event: two\ndata: {\"n\":2}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		releaseNow()
		backend.Close()
	})
	backendHostPort := strings.TrimPrefix(backend.URL, "http://")
	_, backendPort, err := net.SplitHostPort(backendHostPort)
	if err != nil {
		t.Fatalf("split backend: %v", err)
	}

	env := newTestEnv(t)
	s := env.server
	s.leaderInfo = &fakeLeaderInfo{leader: false, addr: backendHostPort}
	s.cfg.ControlAddr = ":" + backendPort
	if err := s.buildLeaderForward(); err != nil {
		t.Fatalf("buildLeaderForward: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := server.Default(server.WithListener(ln))
	h.Use(s.leaderForward())
	h.GET("/api/v1/traces/:traceID/live", func(_ context.Context, c *app.RequestContext) {
		c.String(consts.StatusOK, "local-live")
	})
	go h.Spin()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Shutdown(shutdownCtx)
	})
	time.Sleep(200 * time.Millisecond)

	url := "http://" + ln.Addr().String() + "/api/v1/traces/abc/live"
	firstLine := make(chan string, 1)
	restCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		firstLine <- line
		// Wait until the test has observed the first event before the backend
		// is allowed to produce the second.
		<-release
		rest, err := io.ReadAll(reader)
		if err != nil {
			errCh <- err
			return
		}
		restCh <- string(rest)
	}()

	select {
	case line := <-firstLine:
		if !strings.Contains(line, "event: one") {
			t.Fatalf("first line = %q, want the first SSE event", line)
		}
	case err := <-errCh:
		t.Fatalf("read first SSE event: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE event was buffered: the forward hop did not stream incrementally")
	}

	// Only now let the backend send the second event.
	releaseNow()
	select {
	case rest := <-restCh:
		if !strings.Contains(rest, "event: two") {
			t.Fatalf("rest of stream = %q, want the second SSE event", rest)
		}
	case err := <-errCh:
		t.Fatalf("read rest of stream: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading the second SSE event")
	}
}
