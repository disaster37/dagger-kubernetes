package handler

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/disaster37/go-secguard/bantracker"
	"github.com/disaster37/go-secguard/headers"
	"github.com/disaster37/go-secguard/lockout"
)

// forwardedClientIPHeader carries the original client IP across the
// follower->leader forward hop. It is honored only on the internal control
// listener (control_addr port + 2) so a public client cannot spoof it.
const forwardedClientIPHeader = "X-Dagger-Kubernetes-Client-IP"

// securityExemptPaths are never subject to the security guard (liveness probes
// and the Prometheus scrape endpoint).
var securityExemptPaths = map[string]struct{}{
	"/healthz": {},
	"/readyz":  {},
	"/startup": {},
	"/metrics": {},
}

// isSecurityExemptPath reports whether path bypasses the security guard. Only
// API routes (/api/*, /v1/*) are guarded; SPA assets and probes are exempt.
func isSecurityExemptPath(path string) bool {
	if _, ok := securityExemptPaths[path]; ok {
		return true
	}
	return !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/v1/")
}

// clientIP resolves the spoof-proof client IP for a request. On the internal
// control listener the forwarded client IP header is trusted; everywhere else
// the go-secguard realip walk is used (X-Forwarded-For honored only from
// trusted proxies).
func (s *Server) clientIP(c *app.RequestContext) string {
	if s.guard == nil {
		return c.ClientIP()
	}
	if s.onInternalListener(c) {
		if fwd := strings.TrimSpace(string(c.Request.Header.Peek(forwardedClientIPHeader))); fwd != "" {
			if ip := net.ParseIP(fwd); ip != nil {
				return ip.String()
			}
		}
	}
	ip, err := s.guard.ClientIP(
		c.RemoteAddr().String(),
		string(c.Request.Header.Peek("X-Forwarded-For")),
		string(c.Request.Header.Peek("X-Real-IP")),
	)
	if err != nil || ip == nil {
		return c.ClientIP()
	}
	return ip.String()
}

// onInternalListener reports whether the request arrived on the internal-only
// control listener (control_addr port + 2).
func (s *Server) onInternalListener(c *app.RequestContext) bool {
	if s.internalControlPort <= 0 {
		return false
	}
	conn := c.GetConn()
	if conn == nil || conn.LocalAddr() == nil {
		return false
	}
	_, port, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return false
	}
	return port == strconv.Itoa(s.internalControlPort)
}

// publicURLOrigin returns the scheme://host origin of server.public_url, or ""
// when it is unset/unparseable.
func (s *Server) publicURLOrigin() string {
	u, err := url.Parse(s.cfg.PipelineURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// secguardMiddleware enforces the go-secguard guardrails before leader
// forwarding: CSRF mutation-origin validation, global IP ban checks, and the
// per-IP auth DoS rate limit. It is a no-op when no guard is injected.
func (s *Server) secguardMiddleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if s.guard == nil {
			c.Next(ctx)
			return
		}
		path := string(c.Path())
		if isSecurityExemptPath(path) {
			c.Next(ctx)
			return
		}

		if len(s.corsAllowedOrigins) > 0 {
			allowed := append([]string{s.publicURLOrigin()}, s.corsAllowedOrigins...)
			if err := headers.CheckMutationOrigin(
				string(c.Method()),
				string(c.Request.Header.Peek("Origin")),
				string(c.Request.Header.Peek("Referer")),
				allowed,
			); err != nil {
				s.metrics.SecurityRejectionsTotal.WithLabelValues("origin_not_allowed").Inc()
				writeError(c, consts.StatusForbidden, "origin not allowed")
				c.Abort()
				return
			}
		}

		ip := s.clientIP(c)
		// The admin ban-management endpoints stay reachable from a banned IP
		// so an operator can always inspect and lift bans (operational
		// recovery); every other API route is subject to the ban check.
		if !strings.HasPrefix(path, "/api/v1/security/bans") && s.guard.BanTracker.IsBanned(ip) {
			s.metrics.SecurityRejectionsTotal.WithLabelValues("ip_banned").Inc()
			writeError(c, consts.StatusForbidden, "IP address is temporarily banned due to suspicious activity")
			c.Abort()
			return
		}

		if strings.HasPrefix(path, "/api/v1/auth/") && s.guard.Config.RateLimit.Enabled {
			if !s.guard.RateLimiter.Allow(ip, s.guard.Config.RateLimit.MaxRequests, s.guard.Config.RateLimit.Window) {
				s.metrics.SecurityRejectionsTotal.WithLabelValues("rate_limited").Inc()
				writeError(c, consts.StatusTooManyRequests, "rate limit exceeded, try again later")
				c.Abort()
				return
			}
		}

		c.Next(ctx)
	}
}

// recordLoginFailure records a failed password login against the lockout
// limiter and the credential-stuffing ban tracker.
func (s *Server) recordLoginFailure(username, ip string) {
	if s.guard == nil {
		return
	}
	key := lockout.LoginLimitKey(username, ip)
	s.guard.Lockout.RecordFailure(key)
	banned := s.guard.BanTracker.RecordAndBan(
		ip,
		bantracker.FingerprintLogin(username),
		s.guard.Config.Ban.MaxUniqueFailures,
		s.guard.Config.Ban.BanDuration,
		s.guard.Config.Ban.Window,
	)
	if banned {
		s.metrics.SecurityIPBansTotal.WithLabelValues("login").Inc()
	}
}

// recordTokenFailure records an invalid bearer/API token against the ban
// tracker (bearer-token brute force).
func (s *Server) recordTokenFailure(ip, bearer string) {
	if s.guard == nil || bearer == "" {
		return
	}
	banned := s.guard.BanTracker.RecordAndBan(
		ip,
		bantracker.FingerprintToken(bearer),
		s.guard.Config.Ban.MaxUniqueFailures,
		s.guard.Config.Ban.BanDuration,
		s.guard.Config.Ban.Window,
	)
	if banned {
		s.metrics.SecurityIPBansTotal.WithLabelValues("token").Inc()
	}
}

// handleSecurityBansList returns the active IP bans (admin only).
func (s *Server) handleSecurityBansList(_ context.Context, c *app.RequestContext) {
	if s.guard == nil {
		writeJSON(c, []bantracker.BanInfo{})
		return
	}
	writeJSON(c, s.guard.BanTracker.ListBans())
}

// handleSecurityBanDelete lifts the ban for :ip (admin only).
func (s *Server) handleSecurityBanDelete(_ context.Context, c *app.RequestContext) {
	ip := c.Param("ip")
	if net.ParseIP(ip) == nil {
		writeError(c, consts.StatusBadRequest, "invalid IP address")
		return
	}
	if s.guard != nil {
		s.guard.BanTracker.Unban(ip)
	}
	c.SetStatusCode(consts.StatusNoContent)
}

// securityHeadersFor returns the browser hardening headers for a path/TLS
// combination via go-secguard.
func securityHeadersFor(path string, isTLS bool) map[string]string {
	return headers.DefaultSecurityHeaders(path, isTLS)
}
