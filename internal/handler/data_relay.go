package handler

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// serveDataRelay is the follower half of the data-plane L4 relay: it dials the
// leader's dedicated relay-in port and pumps raw bytes bidirectionally. TLS
// terminates end-to-end on the leader, so the relay never inspects or
// terminates the tunnel; it only moves ciphertext. The connection counts
// against the same dataConnSem cap as a locally-terminated tunnel.
func (s *Server) serveDataRelay(client net.Conn) {
	select {
	case s.dataConnSem <- struct{}{}:
	default:
		_ = client.Close()
		s.logger.Warn("data relay connection limit reached, dropping connection")
		return
	}
	defer func() { <-s.dataConnSem; _ = client.Close() }()

	if s.leaderInfo == nil {
		return
	}
	addr := s.leaderInfo.LeaderAddress()
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		s.logger.WithError(err).WithField("leader", addr).Warn("data relay leader address invalid")
		return
	}
	backend, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(s.relayInPort)), 5*time.Second)
	if err != nil {
		s.logger.WithError(err).WithField("leader", host).Warn("data relay dial leader failed")
		return
	}
	defer func() { _ = backend.Close() }()

	// Pump both directions. Each direction half-closes its write side when its
	// read EOFs, so a client close_notify/half-close is not truncated while the
	// opposite direction (e.g. the leader's response stream) is still live.
	// Wait for both directions before the deferred full closes.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(backend, client)
		halfCloseWrite(backend)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, backend)
		halfCloseWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// halfCloseWrite closes only the write side of c when the underlying
// connection supports it (e.g. *net.TCPConn). A no-op for connections without
// CloseWrite (e.g. net.Pipe in tests).
func halfCloseWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// relayPortFromDataAddr derives the relay-in port from server.data_addr
// (port + 1). It returns an error when the address has no parsable port so the
// caller can refuse to start the relay listener (fail-closed).
func relayPortFromDataAddr(dataAddr string) (int, error) {
	_, portStr, err := net.SplitHostPort(dataAddr)
	if err != nil {
		return 0, fmt.Errorf("split data_addr %q: %w", dataAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("parse data_addr port %q: %w", portStr, err)
	}
	if port <= 0 || port >= 65535 {
		return 0, fmt.Errorf("data_addr port %d out of range for relay derivation", port)
	}
	return port + 1, nil
}

// startRelayListener binds the leader-only relay-in listener on
// server.data_addr port + 1 and serves it until the listener is closed. A
// connection received here is terminal: the leader performs the mTLS handshake
// and dispatches the tunnel. A pod that has stepped down rejects relay-in
// connections at accept, so a stale follower window only yields a dial failure
// that the Dagger CLI's gRPC reconnect already tolerates (ADR-026).
func (s *Server) startRelayListener(tlsConfig *tls.Config) {
	port, err := relayPortFromDataAddr(s.cfg.DataAddr)
	if err != nil {
		s.logger.WithError(err).WithField("data_addr", s.cfg.DataAddr).Error("data relay listener disabled: invalid server.data_addr")
		return
	}
	s.relayInPort = port
	host, _, _ := net.SplitHostPort(s.cfg.DataAddr)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.logger.WithError(err).WithField("addr", addr).Error("data relay listener disabled: bind failed")
		return
	}
	s.relayListener = ln

	go func() {
		s.logger.WithField("addr", ln.Addr().String()).Info("data relay listener listening")
		for {
			raw, err := ln.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				s.logger.WithError(err).Error("data relay accept error")
				continue
			}
			if s.leaderInfo != nil && !s.leaderInfo.IsLeader() {
				// Stepped down: relay-in is leader-only. Closing forces the
				// follower to re-discover the new leader instead of relaying
				// forever.
				_ = raw.Close()
				continue
			}
			s.serveTLSConn(raw, tlsConfig)
		}
	}()
}
