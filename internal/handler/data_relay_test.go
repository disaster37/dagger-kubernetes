package handler

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// relayEchoBackend starts a TCP listener that echoes every byte it receives
// until the connection closes. Returns the listener (callers read its port).
func relayEchoBackend(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen relay backend: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

func listenerPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is not TCP: %T", ln.Addr())
	}
	return addr.Port
}

// freeRelayPortPair finds adjacent free TCP ports (data, relay) for
// server.data_addr and its derived relay-in port, retrying to avoid the
// inherent bind race.
func freeRelayPortPair(t *testing.T) (dataPort, relayPort int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		port := listenerPort(t, ln)
		_ = ln.Close()
		if port < 1 || port+1 > 65534 {
			continue
		}
		next, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+1))
		if err != nil {
			continue
		}
		_ = next.Close()
		return port, port + 1
	}
	t.Fatal("could not find an adjacent free port pair")
	return 0, 0
}

func TestRelayPortFromDataAddr(t *testing.T) {
	tests := []struct {
		name     string
		dataAddr string
		want     int
		wantErr  bool
	}{
		{"empty host", ":8443", 8444, false},
		{"explicit host", "0.0.0.0:9000", 9001, false},
		{"highest valid data port", "127.0.0.1:65534", 65535, false},
		{"port zero", ":0", 0, true},
		{"relay port out of range", "127.0.0.1:65535", 0, true},
		{"non-numeric port", "127.0.0.1:abc", 0, true},
		{"missing port", "127.0.0.1", 0, true},
		{"empty address", "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := relayPortFromDataAddr(tc.dataAddr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("relayPortFromDataAddr(%q) err = %v, wantErr %v", tc.dataAddr, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("relayPortFromDataAddr(%q) = %d, want %d", tc.dataAddr, got, tc.want)
			}
		})
	}
}

func TestStartRelayListenerBindsAndFollowerRejects(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	dataPort, relayPort := freeRelayPortPair(t)
	s.cfg.DataAddr = fmt.Sprintf("127.0.0.1:%d", dataPort)
	s.leaderInfo = &fakeLeaderInfo{leader: false, addr: ""}

	s.startRelayListener(nil)
	if s.relayListener == nil {
		t.Fatal("relay listener must be bound for a valid data_addr")
	}
	t.Cleanup(func() {
		if s.relayListener != nil {
			_ = s.relayListener.Close()
		}
	})
	if s.relayInPort != relayPort {
		t.Fatalf("relayInPort = %d, want %d (data_addr port + 1)", s.relayInPort, relayPort)
	}

	// A pod that has stepped down must reject relay-in connections at accept
	// so a stale follower can never re-relay.
	conn, err := net.DialTimeout("tcp", s.relayListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial relay-in: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("follower must close relay-in connections")
	}
}

func TestStartRelayListenerInvalidDataAddr(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	s.cfg.DataAddr = "not-a-hostport"
	s.startRelayListener(nil)
	if s.relayListener != nil {
		t.Fatal("relay listener must not be bound for an invalid data_addr")
	}
	if s.relayInPort != 0 {
		t.Fatalf("relayInPort = %d, want 0 for an invalid data_addr", s.relayInPort)
	}
}

func TestServeDataRelayPumpsBothWays(t *testing.T) {
	backend := relayEchoBackend(t)

	env := newTestEnv(t)
	s := env.server
	s.relayInPort = listenerPort(t, backend)
	s.leaderInfo = &fakeLeaderInfo{leader: false, addr: "127.0.0.1:1"}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	done := make(chan struct{})
	go func() {
		s.serveDataRelay(server)
		close(done)
	}()

	go func() {
		_, _ = client.Write([]byte("ping"))
	}()
	buf := make([]byte, 4)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read relayed bytes: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("relayed bytes = %q, want ping", buf)
	}

	_ = client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveDataRelay did not return after the client closed")
	}
}

func TestServeDataRelayNoLeaderClosesClient(t *testing.T) {
	env := newTestEnv(t)
	s := env.server
	// A listener records whether the relay dialed at all.
	backend := relayEchoBackend(t)
	s.relayInPort = listenerPort(t, backend)
	s.leaderInfo = &fakeLeaderInfo{leader: false, addr: ""}

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		s.serveDataRelay(server)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveDataRelay blocked with no leader address")
	}

	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection must be closed when no leader is known")
	}
}

func TestServeDataRelayDialFailureClosesClient(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listenerPort(t, dead)
	_ = dead.Close()

	env := newTestEnv(t)
	s := env.server
	s.relayInPort = port
	s.leaderInfo = &fakeLeaderInfo{leader: false, addr: "127.0.0.1:1"}

	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		s.serveDataRelay(server)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveDataRelay did not return after a dial failure")
	}

	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection must be closed after a dial failure")
	}
}

// TestServeDataRelayHalfClose proves a client half-close (CloseWrite) does not
// kill the opposite direction: the backend answers only after seeing EOF from
// the client, and the relay must still deliver its response.
func TestServeDataRelayHalfClose(t *testing.T) {
	// Backend: read until EOF, then write a response and close.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn) // blocks until the relay half-closes
		_, _ = conn.Write([]byte("pong"))
	}()

	env := newTestEnv(t)
	s := env.server
	s.relayInPort = listenerPort(t, backend)
	s.leaderInfo = &fakeLeaderInfo{leader: false, addr: "127.0.0.1:1"}

	// Real TCP pair so CloseWrite is meaningful (net.Pipe has no half-close).
	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen client: %v", err)
	}
	t.Cleanup(func() { _ = clientLn.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := clientLn.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	clientConn, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	serverConn := <-accepted
	t.Cleanup(func() { _ = serverConn.Close() })

	go s.serveDataRelay(serverConn)

	if _, err := clientConn.Write([]byte("ping")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if err := clientConn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close client: %v", err)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(clientConn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("response = %q, want pong (half-close truncated the opposite direction)", got)
	}
}
