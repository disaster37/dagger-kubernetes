package handler

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func TestInternalControlPortFromControlAddr(t *testing.T) {
	tests := []struct {
		name        string
		controlAddr string
		wantPort    int
		wantErr     bool
	}{
		{name: "bare port", controlAddr: ":8080", wantPort: 8082},
		{name: "wildcard host", controlAddr: "0.0.0.0:8080", wantPort: 8082},
		{name: "custom port", controlAddr: ":8443", wantPort: 8445},
		{name: "host and port", controlAddr: "127.0.0.1:8443", wantPort: 8445},
		{name: "unparsable address", controlAddr: "not-a-hostport", wantErr: true},
		{name: "port out of range", controlAddr: ":65533", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port, err := internalControlPortFromControlAddr(tc.controlAddr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for control_addr %q, got port %d", tc.controlAddr, port)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if port != tc.wantPort {
				t.Fatalf("port = %d, want %d", port, tc.wantPort)
			}
		})
	}
}

// TestInternalControlListenerServesMintingCALeaf proves the internal listener
// presents the minted leaf: a client trusting only the minting-CA pool
// completes a handshake when the ServerName matches a leaf SAN and fails when
// it does not.
func TestInternalControlListenerServesMintingCALeaf(t *testing.T) {
	ca, err := repository.NewMintingCA(time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueServerCertificate(
		"supervisor-internal", "dagger-kubernetes",
		[]string{"internal-control.test"},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("IssueServerCertificate: %v", err)
	}
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	env := newTestEnv(t)
	s := env.server
	s.cfg.ControlAddr = ":8080"
	s.cfg.InternalTLSCert = &leaf
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.cfg.InternalListener = ln

	s.startInternalControlListener()
	if s.internalListener == nil {
		t.Fatal("internal listener not bound")
	}
	if s.internalControlPort != 8082 {
		t.Fatalf("internalControlPort = %d, want 8082", s.internalControlPort)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	})

	dial := func(serverName string) error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp",
			s.internalListener.Addr().String(),
			&tls.Config{
				RootCAs:    ca.CertPool(),
				ServerName: serverName,
				MinVersion: tls.VersionTLS12,
			})
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}

	if err := dial("internal-control.test"); err != nil {
		t.Fatalf("handshake with a matching ServerName: %v", err)
	}
	if err := dial("wrong.internal-control.test"); err == nil {
		t.Fatal("handshake with a non-matching ServerName must fail verification")
	}
}

// TestInternalControlListenerDisabledWithoutCert proves a nil InternalTLSCert
// disables the listener (log-only, no bind, no crash).
func TestInternalControlListenerDisabledWithoutCert(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	s.cfg.ControlAddr = ":8080"
	s.cfg.InternalTLSCert = nil

	s.startInternalControlListener()

	if s.internalListener != nil {
		t.Fatal("internal listener must not be bound without a TLS certificate")
	}
	if s.internalHertz != nil {
		t.Fatal("internal engine must not be created without a TLS certificate")
	}
	if s.internalControlPort != 0 {
		t.Fatalf("internalControlPort = %d, want 0 when disabled", s.internalControlPort)
	}
}

// TestInternalControlListenerShutdown proves Shutdown closes the internal
// listener so new dials fail.
func TestInternalControlListenerShutdown(t *testing.T) {
	ca, err := repository.NewMintingCA(time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	certPEM, keyPEM, err := ca.IssueServerCertificate(
		"supervisor-internal", "dagger-kubernetes",
		[]string{"internal-control.test"},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("IssueServerCertificate: %v", err)
	}
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	env := newTestEnv(t)
	s := env.server
	s.cfg.ControlAddr = ":8080"
	s.cfg.InternalTLSCert = &leaf
	s.cfg.InternalListener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s.startInternalControlListener()
	addr := s.internalListener.Addr().String()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("internal listener still accepting connections after Shutdown")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
