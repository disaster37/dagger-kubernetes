package handler

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/disaster37/go-raft/httpext"
	rafttransport "github.com/disaster37/go-raft/transport"

	"github.com/disaster/dagger-kubernetes/internal/repository"
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

// serverLeaderInfo adapts the server's leaderInfo to httpext.LeaderInfo,
// reading the current leader dynamically on every call.
type serverLeaderInfo struct {
	s *Server
}

func (l *serverLeaderInfo) IsLeader() bool {
	return l.s.leaderInfo != nil && l.s.leaderInfo.IsLeader()
}

func (l *serverLeaderInfo) LeaderAddress() string {
	if l.s.leaderInfo == nil {
		return ""
	}
	return l.s.leaderInfo.LeaderAddress()
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
// leader. All mutating methods are leader-pinned; three GET route families are
// also pinned because they read leader-local state or perform a Raft write
// despite being read-only by method:
//
//   - /api/v1/traces/:traceID/live and /api/v1/traces/live — SSE; the liveHub
//     is per-pod and events are produced only on the leader. The list-level
//     stream matches the same prefix/suffix rule automatically.
//   - /api/v1/fleet/:version/purge-cache — the purge-job status lives in the
//     in-memory EngineCachePurgeService of the pod that accepted the POST.
//   - /api/v1/auth/oauth/{github,oidc}/callback — a GET that runs the OAuth
//     exchange and then completeOAuthLogin → EnsureOAuthUser / group
//     reconciliation, all of which are Raft writes. A follower would return
//     503, so ~2/3 of OAuth logins would fail on a 3-pod cluster without this.
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
	if strings.HasPrefix(path, "/api/v1/auth/oauth/") && strings.HasSuffix(path, "/callback") {
		return true
	}
	return false
}

// effectiveForwardTLSProvider returns the dynamic mTLS provider for the
// follower->leader forward hop. When none was injected it builds a static
// provider trusting the minting CA pool (the internal listener always serves a
// minting-CA leaf in every provider mode).
func (s *Server) effectiveForwardTLSProvider() *rafttransport.DynamicTLSProvider {
	if s.cfg.ForwardTLSProvider != nil {
		return s.cfg.ForwardTLSProvider
	}
	if s.forwardTLSProvider != nil {
		return s.forwardTLSProvider
	}
	pool := s.mintingCA.CertPool()
	return rafttransport.NewDynamicTLSProvider(tls.Certificate{}, pool, pool, false)
}

// getOrBuildForwarder returns the cached httpext.Forwarder for the current
// internal control port, rebuilding it when the port changes. It returns
// (nil, false) when no leader info is injected or the internal port is unknown.
func (s *Server) getOrBuildForwarder() (*httpext.Forwarder, bool) {
	if s.leaderInfo == nil || s.internalControlPort <= 0 {
		return nil, false
	}
	s.forwarderMu.Lock()
	defer s.forwarderMu.Unlock()
	if s.forwarder != nil && s.forwarderPort == s.internalControlPort {
		return s.forwarder, true
	}
	s.forwarder = httpext.NewForwarder(httpext.LeaderForwardConfig{
		LeaderInfo:            &serverLeaderInfo{s: s},
		TargetPort:            s.internalControlPort,
		Scheme:                "https",
		HeaderName:            leaderForwardedHeader,
		TLSProvider:           s.effectiveForwardTLSProvider(),
		PinnedRouteFn:         leaderPinnedRoute,
		DialTimeout:           2 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	})
	// A reverse proxy must pass redirects through unchanged: following them
	// would drop the Set-Cookie headers on the OAuth callback's 302 and hand
	// the browser the redirect target's body instead of the redirect.
	repository.DisableForwarderRedirects(s.forwarder)
	s.forwarderPort = s.internalControlPort
	return s.forwarder, true
}

// leaderForward is the global middleware that forwards leader-pinned requests
// to the current Raft leader via go-raft's httpext.Forwarder. It is a no-op on
// the leader, when no leader info is injected, for follower-safe routes, and
// when the request already carries the loop-prevention header; otherwise it
// streams the request to the leader's internal control plane and streams the
// response back (preserving incremental SSE and 64 MiB OTLP bodies).
func (s *Server) leaderForward() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		marker := string(c.Request.Header.Peek(leaderForwardedHeader))
		if s.leaderInfo == nil ||
			s.leaderInfo.IsLeader() ||
			!leaderPinnedRoute(string(c.Method()), string(c.Path())) ||
			marker != "" {
			c.Request.Header.Del(leaderForwardedHeader)
			c.Next(ctx)
			return
		}

		fwd, ok := s.getOrBuildForwarder()
		if !ok {
			s.leaderForwardFailure(c, consts.StatusServiceUnavailable, "no raft leader available")
			c.Abort()
			return
		}

		hdr := make(map[string][]string)
		c.Request.Header.VisitAll(func(k, v []byte) {
			name := textproto.CanonicalMIMEHeaderKey(string(k))
			if httpext.IsHopByHopHeader(name) {
				return
			}
			hdr[name] = append(hdr[name], string(v))
		})
		hdr[forwardedClientIPHeader] = []string{s.clientIP(c)}

		var bodyReader io.Reader
		if c.Request.IsBodyStream() {
			bodyReader = c.Request.BodyStream()
		} else if raw := c.Request.Body(); len(raw) > 0 {
			bodyReader = bytes.NewReader(raw)
		}

		resp, err := fwd.Forward(ctx, &httpext.ForwardRequest{
			Method:     string(c.Method()),
			RequestURI: string(c.Request.URI().RequestURI()),
			Header:     hdr,
			Body:       bodyReader,
		})
		if err != nil {
			if errors.Is(err, httpext.ErrNoLeader) {
				s.leaderForwardFailure(c, consts.StatusServiceUnavailable, "no raft leader available")
			} else {
				s.logger.WithError(err).Error("leader forward failed")
				s.leaderForwardFailure(c, consts.StatusBadGateway, "leader unreachable")
			}
			c.Abort()
			return
		}

		for k, values := range resp.Header {
			if httpext.IsHopByHopHeader(k) {
				continue
			}
			for _, v := range values {
				c.Response.Header.Add(k, v)
			}
		}
		c.SetStatusCode(resp.StatusCode)
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			c.Response.ImmediateHeaderFlush = true
		}
		// Hand the upstream body to Hertz. Do NOT defer resp.Body.Close():
		// Hertz's writeBodyStream flushes every chunk incrementally and closes
		// resp.Body when streaming finishes.
		c.SetBodyStream(resp.Body, -1)
		c.Abort()
	}
}

// leaderForwardFailure returns the fixed OAuth error screen for browser OAuth
// GETs so an infrastructure failure never renders a raw body or bounces the
// user back into the authorize endpoint. Every other route keeps the existing
// writeError behavior.
func (s *Server) leaderForwardFailure(c *app.RequestContext, status int, message string) {
	if isOAuthBrowserRoute(string(c.Method()), string(c.Path())) {
		c.Response.Header.Set("Cache-Control", "no-store")
		c.Redirect(consts.StatusFound, []byte(fmt.Sprintf("%s?error=oauth", oauthErrorRedirectBase)))
		return
	}
	writeError(c, status, message)
}

// isOAuthBrowserRoute reports whether (method, path) is a browser OAuth
// login/callback GET.
func isOAuthBrowserRoute(method, path string) bool {
	return method == "GET" && strings.HasPrefix(path, "/api/v1/auth/oauth/")
}
