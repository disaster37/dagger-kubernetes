package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/hertz-contrib/websocket"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	remoteCacheMaxMessageBytes = domain.RCMaxMessageBytes
	remoteCachePingInterval    = domain.RCPingInterval
	remoteCacheCloseRevoked    = domain.RCCloseTokenRevoked
)

// handleRemoteCacheWS serves GET /v1/engine on the leader. The engine dials it
// with an org engine token as HTTP Basic; the org is resolved from the token
// identity itself (never from user membership or X-Dagger-Org).
func (s *Server) handleRemoteCacheWS(ctx context.Context, c *app.RequestContext) {
	if s.remoteCache == nil {
		writeError(c, consts.StatusNotFound, "remote cache disabled")
		return
	}
	id, ok := s.resolveIdentity(ctx, c)
	if !ok {
		return
	}
	if id.OrgID == "" {
		writeError(c, consts.StatusForbidden, "organization token required")
		return
	}
	namespace := s.remoteCacheNamespaceFor(id.OrgID)
	bearer := s.bearerFromRequest(c)
	readOnly := string(c.Request.Header.Peek("X-Dagger-Cache-Mode")) == "read-only"
	upgrader := websocket.HertzUpgrader{CheckOrigin: func(*app.RequestContext) bool { return true }}
	if err := upgrader.Upgrade(c, func(conn *websocket.Conn) {
		s.serveRemoteCacheConn(ctx, conn, namespace, bearer, readOnly)
	}); err != nil {
		s.logger.WithError(err).Debug("remote cache websocket upgrade failed")
	}
}

func (s *Server) serveRemoteCacheConn(ctx context.Context, conn *websocket.Conn, namespace, bearer string, readOnly bool) {
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(remoteCacheMaxMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(3 * remoteCachePingInterval))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(3 * remoteCachePingInterval))
	})

	// gorilla/hertz allow exactly one concurrent writer, so every frame —
	// replies, service requests and pings — goes through this mutex.
	var writeMu sync.Mutex
	send := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, data)
	}
	rc := s.remoteCache.NewConn(namespace, readOnly, send)
	defer rc.Close()

	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(remoteCachePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				if bearer != "" {
					id, err := s.auth.Resolve(ctx, bearer)
					if err != nil {
						if errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrSessionRevoked) {
							_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(remoteCacheCloseRevoked, "token revoked"), time.Now().Add(5*time.Second))
							_ = conn.Close()
							return
						}
					} else if !s.orgAccessStillValid(ctx, id, namespace) {
						_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(remoteCacheCloseRevoked, "org access revoked"), time.Now().Add(5*time.Second))
						_ = conn.Close()
						return
					}
				}
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		rc.HandleFrame(ctx, msg)
	}
}

// orgAccessStillValid re-checks the caller's organization membership on every
// WebSocket heartbeat so a removed member's open engine connection closes.
func (s *Server) orgAccessStillValid(ctx context.Context, id *domain.Identity, orgID string) bool {
	if id == nil {
		return false
	}
	if id.IsAdmin() {
		return true
	}
	if id.OrgID != "" {
		return id.OrgID == orgID
	}
	if s.orgs == nil || id.UserID == "" {
		return false
	}
	member, err := s.orgs.IsOrgMember(ctx, orgID, id.UserID)
	return err == nil && member
}

// remoteCacheNamespaceFor maps an org ID to its cache namespace. Deprecated
// single-org shared isolation collapses every org to "_shared".
func (s *Server) remoteCacheNamespaceFor(orgID string) string {
	if s.cfg.FleetIsolation == "shared" {
		return "_shared"
	}
	return orgID
}

// relayRemoteCacheWS upgrades a follower-side /v1/engine request and relays
// frames to the leader's internal control listener (ADR-055). The leader is
// dialed by its internal service hostname (not the advertise IP) so the
// minting-CA leaf's SAN matches; the caller's Authorization is forwarded and
// the leader resolves the org from that token.
func (s *Server) relayRemoteCacheWS(ctx context.Context, c *app.RequestContext) {
	if s.wsDialer == nil || s.leaderInfo == nil || s.internalControlPort <= 0 {
		writeError(c, consts.StatusServiceUnavailable, "no raft leader available")
		return
	}
	host := hostOnly(s.cfg.InternalControlURL)
	if host == "" {
		leader := s.leaderInfo.LeaderAddress()
		host, _, _ = net.SplitHostPort(leader)
		if host == "" {
			host = leader
		}
	}
	if host == "" {
		writeError(c, consts.StatusServiceUnavailable, "internal control host unknown")
		return
	}
	target := fmt.Sprintf("wss://%s/v1/engine", net.JoinHostPort(host, fmt.Sprintf("%d", s.internalControlPort)))
	headers := map[string][]string{}
	if auth := string(c.Request.Header.Peek("Authorization")); auth != "" {
		headers["Authorization"] = []string{auth}
	}
	upstream, err := s.wsDialer.Dial(ctx, target, host, headers)
	if err != nil {
		s.logger.WithError(err).Warn("remote cache leader relay dial failed")
		writeError(c, consts.StatusBadGateway, "leader unreachable")
		return
	}
	upgrader := websocket.HertzUpgrader{CheckOrigin: func(*app.RequestContext) bool { return true }}
	if err := upgrader.Upgrade(c, func(client *websocket.Conn) {
		defer func() { _ = client.Close() }()
		defer func() { _ = upstream.Close() }()
		errc := make(chan error, 2)
		go func() {
			for {
				mt, msg, err := client.ReadMessage()
				if err != nil {
					errc <- err
					return
				}
				if err := upstream.WriteMessage(mt, msg); err != nil {
					errc <- err
					return
				}
			}
		}()
		go func() {
			for {
				mt, msg, err := upstream.ReadMessage()
				if err != nil {
					errc <- err
					return
				}
				if err := client.WriteMessage(mt, msg); err != nil {
					errc <- err
					return
				}
			}
		}()
		if err := <-errc; err != nil {
			s.logger.WithError(err).Debug("remote cache relay ended")
		}
	}); err != nil {
		_ = upstream.Close()
		s.logger.WithError(err).Debug("remote cache relay upgrade failed")
	}
}
