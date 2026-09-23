package handler

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app/server"
)

// internalControlPortFromControlAddr derives the internal-only control
// listener port from server.control_addr (port + 2; :8080 -> 8082). The +2
// slot is free: +1 is the Raft transport. It returns an error when the
// address has no parsable port or the derived port is out of range so the
// caller can refuse to start the listener (fail-closed).
func internalControlPortFromControlAddr(controlAddr string) (int, error) {
	_, portStr, err := net.SplitHostPort(controlAddr)
	if err != nil {
		return 0, fmt.Errorf("split control_addr %q: %w", controlAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("parse control_addr port %q: %w", portStr, err)
	}
	if port <= 0 || port >= 65533 {
		return 0, fmt.Errorf("control_addr port %d out of range for internal listener derivation", port)
	}
	return port + 2, nil
}

// startInternalControlListener boots the internal-only control listener
// (ADR-041): the same route table as the public control plane, served over a
// minting-CA-signed leaf on control+2 (8082), pod-to-pod only. It is the
// target of the follower->leader forward hop. Every failure mode is
// log-only (mirroring startRelayListener): the public control plane is never
// brought down by a problem with the internal listener.
func (s *Server) startInternalControlListener() {
	if s.cfg.InternalTLSCert == nil {
		s.logger.Error("internal control listener disabled: no internal TLS certificate")
		return
	}
	port, err := internalControlPortFromControlAddr(s.cfg.ControlAddr)
	if err != nil {
		s.logger.WithField("control_addr", s.cfg.ControlAddr).
			WithError(err).Error("internal control listener disabled: invalid server.control_addr")
		return
	}
	s.internalControlPort = port
	host, _, _ := net.SplitHostPort(s.cfg.ControlAddr)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*s.cfg.InternalTLSCert},
		MinVersion:   tls.VersionTLS12,
	}

	var ln net.Listener
	if s.cfg.InternalListener != nil {
		ln = s.cfg.InternalListener
	} else {
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			s.logger.WithField("addr", addr).
				WithError(err).Error("internal control listener disabled: bind failed")
			return
		}
	}
	s.internalListener = ln

	internal := server.Default(
		server.WithListener(ln),
		server.WithReadTimeout(0),
		server.WithStreamBody(true),
		server.WithTLS(tlsCfg),
	)
	s.registerRoutes(internal)
	s.internalHertz = internal

	go func() {
		s.logger.WithField("addr", ln.Addr().String()).Info("internal control plane listening")
		if err := internal.Run(); err != nil {
			s.logger.WithError(err).Error("internal control plane error")
		}
	}()
}
