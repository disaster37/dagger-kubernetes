package handler

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	hertzclient "github.com/cloudwego/hertz/pkg/app/client"
	"github.com/cloudwego/hertz/pkg/network/standard"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/hertz-contrib/reverseproxy"
)

// leaderForwardedHeader marks a request that has already been forwarded to the
// leader. A request carrying it is served locally to prevent a forwarding loop
// when it lands on a follower (e.g. a stale leader address mid-election).
const leaderForwardedHeader = "X-Dagger-Kubernetes-Leader-Forwarded"

// leaderInfo is the subset of the Raft store the forward middleware needs.
// *repository.RaftStore implements it.
type leaderInfo interface {
	IsLeader() bool
	LeaderAddress() string
}

// isReadOnlyMethod reports whether a method is served locally by followers
// (stale reads, ADR-016 D6). Every other method is a Raft write or touches
// leader-local state and must run on the leader.
func isReadOnlyMethod(method string) bool {
	switch method {
	case "GET", "HEAD", "OPTIONS":
		return true
	default:
		return false
	}
}

// leaderPinnedRoute reports whether (method, path) must be served by the
// leader. All mutating methods are leader-pinned; two GET routes are also
// pinned because they read leader-local state:
//
//   - /api/v1/traces/:traceID/live — SSE; the liveHub is per-pod and events
//     are produced only on the leader.
//   - /api/v1/fleet/:version/purge-cache — the purge-job status lives in the
//     in-memory EngineCachePurgeService of the pod that accepted the POST.
func leaderPinnedRoute(method, path string) bool {
	if !isReadOnlyMethod(method) {
		return true
	}
	if strings.HasPrefix(path, "/api/v1/traces/") && strings.HasSuffix(path, "/live") {
		return true
	}
	if strings.HasPrefix(path, "/api/v1/fleet/") && strings.HasSuffix(path, "/purge-cache") {
		return true
	}
	return false
}

// leaderForward is the global middleware that forwards leader-pinned requests
// to the current Raft leader. It is a no-op on the leader, when no leader info
// is injected, for follower-safe routes, and when the request already carries
// the loop-prevention header; otherwise it rewrites and proxies the request to
// the leader's control plane.
func (s *Server) leaderForward() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if s.leaderInfo == nil ||
			s.leaderInfo.IsLeader() ||
			!leaderPinnedRoute(string(c.Method()), string(c.Path())) ||
			c.Request.Header.Get(leaderForwardedHeader) != "" {
			c.Next(ctx)
			return
		}
		if _, _, ok := s.controlForwardTarget(s.leaderInfo.LeaderAddress()); !ok {
			writeError(c, consts.StatusServiceUnavailable, "no raft leader available")
			c.Abort()
			return
		}
		if s.leaderProxy == nil {
			writeError(c, consts.StatusServiceUnavailable, "no raft leader available")
			c.Abort()
			return
		}
		c.Request.Header.Set(leaderForwardedHeader, "1")
		s.leaderProxy.ServeHTTP(ctx, c)
		c.Abort()
	}
}

// controlForwardTarget derives the scheme + host:port of the leader's control
// plane from the leader's Raft address and this server's config. The leader's
// Raft address carries the pod FQDN; the control port comes from ControlAddr
// (all pods in a fleet use the same control port). The scheme is https when a
// control-plane certificate is configured, http for a bare dev deployment.
func (s *Server) controlForwardTarget(leaderAddress string) (scheme, hostPort string, ok bool) {
	host, _, err := net.SplitHostPort(leaderAddress)
	if err != nil || host == "" {
		return "", "", false
	}
	_, controlPort, err := net.SplitHostPort(s.cfg.ControlAddr)
	if err != nil || controlPort == "" {
		return "", "", false
	}
	scheme = "http"
	if s.cfg.CertPath != "" && s.cfg.KeyPath != "" {
		scheme = "https"
	}
	return scheme, net.JoinHostPort(host, controlPort), true
}

// leaderForwardTLSConfig builds the client TLS config for the internal forward
// hop. Verification is full — certificate verification is never disabled:
// RootCAs is the injected per-provider pool (nil = system pool for
// cert-manager/external), and ServerName is left empty so Go's tls.Client
// derives it from the dialed leader FQDN. Minimum version TLS 1.2.
func (s *Server) leaderForwardTLSConfig() *tls.Config {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.leaderForwardRootCAs != nil {
		tlsCfg.RootCAs = s.leaderForwardRootCAs
	}
	return tlsCfg
}

// buildLeaderForward constructs the reverse proxy used by leaderForward. One
// proxy handles both buffered JSON writes and the streaming SSE route:
// WithResponseBodyStream lets the backend body stream so Hertz flushes it
// incrementally. The standard dialer is required because netpoll does not do
// client TLS. The director rewrites the request to the current leader's
// control plane; the error handler maps a transport/verification failure to
// 502 "leader unreachable".
func (s *Server) buildLeaderForward() error {
	p, err := reverseproxy.NewSingleHostReverseProxy("https://leader.invalid",
		hertzclient.WithResponseBodyStream(true),
		hertzclient.WithDialTimeout(2*time.Second),
		hertzclient.WithDialer(standard.NewDialer()),
		hertzclient.WithTLSConfig(s.leaderForwardTLSConfig()),
	)
	if err != nil {
		return err
	}
	p.SetDirector(func(req *protocol.Request) {
		scheme, hostPort, ok := s.controlForwardTarget(s.leaderInfo.LeaderAddress())
		if !ok {
			// The leader vanished between the middleware check and the
			// director: dial an unroutable target so the error handler
			// returns 502 instead of forwarding to the wrong host.
			req.URI().SetScheme("http")
			req.URI().SetHost("leader.invalid")
			req.Header.SetHostBytes([]byte("leader.invalid"))
			return
		}
		req.URI().SetScheme(scheme)
		req.URI().SetHost(hostPort)
		req.Header.SetHostBytes([]byte(hostPort))
	})
	p.SetErrorHandler(func(c *app.RequestContext, err error) {
		s.logger.WithError(err).Error("leader forward failed")
		writeError(c, consts.StatusBadGateway, "leader unreachable")
	})
	s.leaderProxy = p
	return nil
}
