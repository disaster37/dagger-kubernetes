package handler

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fixedIPProvider returns a single ready replica pinned to a fixed IP so
// serveDataTunnel dials a local test listener instead of a stub 10.0.0.x IP.
type fixedIPProvider struct {
	*repository.StubProvider
	pod string
	ip  string
}

func (p *fixedIPProvider) GetReplicas(version string) ([]domain.Replica, error) {
	return []domain.Replica{{
		Name:      p.pod,
		Ordinal:   0,
		Version:   version,
		PodIP:     p.ip,
		Ready:     true,
		StartedAt: time.Now(),
	}}, nil
}

// startDataTunnelBackend binds a listener on 127.0.0.1:9999 (the engine port
// serveDataTunnel dials) and accepts connections until the listener closes.
// Returns the listener (or nil if the port is unavailable, in which case the
// test is skipped).
func startDataTunnelBackend(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Skipf("engine port 9999 unavailable: %v", err)
		return
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			backend, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
			}(backend)
		}
	}()
}

// setupDataTunnel wires the server fleet to the fixed-IP provider, seeds a
// running trace, and registers a lease for certFP pointing at the trace.
func setupDataTunnel(t *testing.T, env *testEnv, certFP, traceID string) {
	t.Helper()
	version := "v0.21.4"
	pod := domain.PodName(version, 0)
	env.server.fleetManager = service.NewManager(&fixedIPProvider{StubProvider: repository.NewStubProvider(), pod: pod, ip: "127.0.0.1"},
		env.sessions, service.ManagerConfig{}, env.server.logger, nil)

	if err := env.server.traceMeta.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID: traceID, Status: "running", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	env.sessions.Register(certFP, version, pod, "inst-1", traceID, "u1")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServeDataTunnelMarksTraceFailedOnClose(t *testing.T) {
	startDataTunnelBackend(t)

	env := newTestEnv(t)
	s := env.server
	const certFP = "fp-close"
	setupDataTunnel(t, env, certFP, testTraceID)

	liveConn := subscribeCaptureClient(t, s.liveHub)

	client, server := net.Pipe()
	defer client.Close()
	go s.serveDataTunnel(certFP, server)

	waitFor(t, "tunnel in-flight", func() bool {
		for _, l := range env.sessions.List() {
			if l.CertFP == certFP && l.InFlight == 1 {
				return true
			}
		}
		return false
	})

	// Close the client side: the tunnel's io.Copy returns and the defer marks
	// the trace failed.
	_ = client.Close()

	waitFor(t, "trace marked failed", func() bool {
		m, err := s.traceMeta.Get(context.Background(), testTraceID)
		return err == nil && m.Status == "failed"
	})

	m, _ := s.traceMeta.Get(context.Background(), testTraceID)
	if m.FailureReason != "client connection lost" {
		t.Fatalf("failure_reason = %q, want %q", m.FailureReason, "client connection lost")
	}

	// The live hub must broadcast a re-fetch event to connected viewers.
	waitForString(t, liveConn, `"type":"trace_update"`)
}

func TestServeDataTunnelNoFailWhenInFlight(t *testing.T) {
	startDataTunnelBackend(t)

	env := newTestEnv(t)
	s := env.server
	const certFP = "fp-multi"
	setupDataTunnel(t, env, certFP, testTraceID)

	// Simulate a second open tunnel on the same lease (the CLI can open more
	// than one connection per cert fingerprint).
	if err := env.sessions.IncInFlight(certFP); err != nil {
		t.Fatalf("IncInFlight: %v", err)
	}

	client, server := net.Pipe()
	defer client.Close()
	go s.serveDataTunnel(certFP, server)

	waitFor(t, "two tunnels in-flight", func() bool {
		for _, l := range env.sessions.List() {
			if l.CertFP == certFP && l.InFlight == 2 {
				return true
			}
		}
		return false
	})

	_ = client.Close()

	// The closed tunnel decremented to 1; the trace must still be running.
	time.Sleep(200 * time.Millisecond)
	m, err := s.traceMeta.Get(context.Background(), testTraceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Status == "failed" {
		t.Fatalf("trace must not fail while another tunnel is in flight (status=%q)", m.Status)
	}
}

func TestServeDataTunnelLeaseNotFound(t *testing.T) {
	env := newTestEnv(t)
	s := env.server

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// No lease for this fingerprint: serveDataTunnel must return without
	// panicking and without marking any trace failed.
	s.serveDataTunnel("no-such-fp", server)

	if _, err := s.traceMeta.Get(context.Background(), "no-such-fp"); err == nil {
		t.Fatal("no trace should have been created for a missing lease")
	}
}

func TestClientFingerprint(t *testing.T) {
	if got := clientFingerprint(&tls.ConnectionState{}); got != "" {
		t.Fatalf("clientFingerprint(no peer) = %q, want empty", got)
	}
}
