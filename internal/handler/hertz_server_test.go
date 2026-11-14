package handler

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// compile-time assertion that Server.configure returns *server.Hertz.
var _ func(*Server) (*server.Hertz, error) = (*Server).configure

// hasRoute reports whether the Hertz engine registered method+path.
func hasRoute(routes route.RoutesInfo, method, path string) bool {
	for _, r := range routes {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

func TestServerConfigureUsesHertz(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, s *Server)
		wantErrSub  string
		checkRoutes bool
	}{
		{
			name: "host ports mode registers Hertz engine and routes",
			setup: func(_ *testing.T, s *Server) {
				s.cfg.ControlAddr = "127.0.0.1:0"
				s.cfg.ControlListener = nil
			},
			checkRoutes: true,
		},
		{
			name: "pre-bound ControlListener injection",
			setup: func(t *testing.T, s *Server) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("listen: %v", err)
				}
				t.Cleanup(func() { _ = ln.Close() })
				s.cfg.ControlListener = ln
			},
			checkRoutes: true,
		},
		{
			name: "valid TLS cert and key paths",
			setup: func(t *testing.T, s *Server) {
				ca, err := repository.NewMintingCA(2 * time.Hour)
				if err != nil {
					t.Fatalf("NewMintingCA: %v", err)
				}
				certPEM, keyPEM, err := ca.IssueServerCertificate(
					"localhost", "dagger-kubernetes", []string{"localhost"}, time.Hour,
				)
				if err != nil {
					t.Fatalf("IssueServerCertificate: %v", err)
				}
				dir := t.TempDir()
				certPath := filepath.Join(dir, "server.crt")
				keyPath := filepath.Join(dir, "server.key")
				if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
					t.Fatalf("write cert: %v", err)
				}
				if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
					t.Fatalf("write key: %v", err)
				}
				s.cfg.CertPath = certPath
				s.cfg.KeyPath = keyPath
			},
			checkRoutes: true,
		},
		{
			name: "invalid TLS cert path returns wrapped error",
			setup: func(t *testing.T, s *Server) {
				dir := t.TempDir()
				s.cfg.CertPath = filepath.Join(dir, "missing.crt")
				s.cfg.KeyPath = filepath.Join(dir, "missing.key")
			},
			wantErrSub: "load control plane TLS cert",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			s := env.server
			tc.setup(t, s)

			h, err := s.configure()
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("configure() unexpected error: %v", err)
			}
			if h == nil || h.Engine == nil {
				t.Fatal("configure() returned nil *server.Hertz or nil Engine")
			}

			if tc.checkRoutes {
				for _, routePath := range []string{"/healthz", "/readyz"} {
					resp := ut.PerformRequest(h.Engine, "GET", routePath, nil)
					if got := resp.Result().StatusCode(); got != http.StatusOK {
						t.Fatalf("GET %s status = %d, want 200", routePath, got)
					}
				}
				// The Prometheus adaptor needs a real connection writer, so
				// ut.PerformRequest cannot exercise /metrics. Assert the route
				// is registered here; the live server test below performs the
				// actual GET /metrics request.
				if !hasRoute(h.Routes(), "GET", "/metrics") {
					t.Fatal("GET /metrics route is not registered on the Hertz engine")
				}
			}
		})
	}
}

func TestServerStartAndShutdownSetsHertzEngine(t *testing.T) {
	env := newTestEnv(t)
	s := env.server

	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen control: %v", err)
	}
	dataLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = controlLn.Close()
		t.Fatalf("listen data: %v", err)
	}

	s.cfg.ControlAddr = controlLn.Addr().String()
	s.cfg.ControlListener = controlLn
	s.cfg.DataAddr = dataLn.Addr().String()
	s.cfg.DataListener = dataLn

	ca, err := repository.NewMintingCA(2 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueServerCertificate(
		"localhost", "dagger-kubernetes", []string{"localhost"}, time.Hour,
	)
	if err != nil {
		t.Fatalf("IssueServerCertificate: %v", err)
	}
	dataCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx, dataCert); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = s.Shutdown(shutdownCtx)
	})

	if s.hertz == nil {
		t.Fatal("Server.hertz must be non-nil after Start")
	}

	healthURL := fmt.Sprintf("http://%s/healthz", controlLn.Addr().String())
	client := &http.Client{
		Timeout:   200 * time.Millisecond,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, reqErr := client.Get(healthURL)
		if reqErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Hertz control plane did not become ready on %s: last err=%v", healthURL, reqErr)
		}
		time.Sleep(25 * time.Millisecond)
	}

	metricsURL := fmt.Sprintf("http://%s/metrics", controlLn.Addr().String())
	metricsResp, err := client.Get(metricsURL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	_ = metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", metricsResp.StatusCode)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}

	// Second Shutdown and canceled-context Shutdown must not panic.
	canceledCtx, cancelImmediate := context.WithCancel(context.Background())
	cancelImmediate()
	_ = s.Shutdown(canceledCtx)
}

func TestServerShutdownBeforeStartIsSafe(t *testing.T) {
	env := newTestEnv(t)
	if err := env.server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Start returned error: %v", err)
	}
}
