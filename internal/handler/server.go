package handler

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/hertz-contrib/reverseproxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const (
	// maxRequestBodyBytes caps /v1/engines request bodies (B3).
	maxRequestBodyBytes = 1 << 20 // 1 MiB

	// maxControlBody caps control-plane JSON request bodies (login, user/group/
	// project CRUD). It restores the 4 MiB protection that the pre-StreamBody
	// hertz default (MaxRequestBodySize) provided before WithStreamBody(true) was
	// enabled. Without it, an unauthenticated caller could send an arbitrarily
	// large body to a public endpoint (e.g. /api/v1/auth/login) and c.Body()
	// would buffer it fully into memory (CWE-400/CWE-770).
	maxControlBody = 4 << 20 // 4 MiB

	// defaultOTLPMaxBodyBytes caps OTLP ingest bodies when ServerConfig does
	// not supply a limit. OTLP log/trace batches are legitimately large, so
	// this is deliberately separate from (and much larger than) the 4 MiB
	// control-API cap.
	defaultOTLPMaxBodyBytes = int64(64 << 20) // 64 MiB

	maxDataConnections = 512 // concurrent data-plane connections (M4).

	otelSignalKey = "otel_signal" // per-request OTel signal label (B1).
	otelErrorKey  = "otel_error"  // set when the OTel proxy hit a transport error (B1).
)

// errBodyTooLarge is returned by readBoundedBody when the request body exceeds
// the supplied cap. It is never sent to the client verbatim (handlers map it to
// 413).
var errBodyTooLarge = errors.New("request body too large")

// readBoundedBody reads at most max+1 bytes of the request body. With
// server.WithStreamBody(true), c.Body() buffers the entire streamed body into
// memory with no upper bound — a DoS vector on every control-plane endpoint
// (CWE-400/CWE-770). This helper bounds the read whether the body is already
// buffered or still a stream, so handlers can enforce a per-endpoint cap before
// decoding.
func readBoundedBody(c *app.RequestContext, maxBytes int) ([]byte, error) {
	if c.Request.IsBodyStream() {
		// LimitReader stops after maxBytes+1 bytes; the +1 lets us detect overflow.
		r := io.LimitReader(c.Request.BodyStream(), int64(maxBytes)+1)
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if len(b) > maxBytes {
			return nil, errBodyTooLarge
		}
		return b, nil
	}
	body, err := c.Body()
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// EngineRequest is the body of POST /v1/engines. Must match the Dagger
// CLI's cloud.EngineRequest shape exactly (see dagger/dagger internal/cloud/client.go).
type EngineRequest struct {
	Image                string   `json:"image"`
	Module               string   `json:"module"`
	Function             string   `json:"function"`
	ExecCmd              []string `json:"exec_cmd"`
	ClientID             string   `json:"client_id"`
	MinimumEngineVersion string   `json:"minimum_engine_version"`
	TraceID              string   `json:"trace_id"`
}

// EngineSpecResponse is the body of a successful POST /v1/engines response.
type EngineSpecResponse struct {
	Image      string                          `json:"image"`
	URL        string                          `json:"url"`
	Cert       *domain.SerializableCertificate `json:"cert"`
	InstanceID string                          `json:"instance_id"`
	Location   string                          `json:"location"`
	OrgID      string                          `json:"org_id,omitempty"`
	UserID     string                          `json:"user_id,omitempty"`
}

// ErrorResponse is the standard error body shape.
type ErrorResponse struct {
	Message string `json:"message"`
}

// traceURLResponse is the body of GET /api/v1/traces/:traceID/url.
type traceURLResponse struct {
	TraceID string `json:"trace_id"`
	URL     string `json:"url"`
}

// Deps bundles the collaborators injected into the Server. Replacing the old
// 11-param constructor, this is far easier to maintain and construct in tests.
type Deps struct {
	Logger               *logrus.Logger
	Metrics              *observ.Metrics
	MintingCA            domain.MintingCA
	FleetManager         *service.Manager
	Sessions             domain.SessionStore
	SessionRegistry      domain.SessionRegistry
	VersionResolver      domain.VersionResolver
	Auth                 *service.AuthService
	InternalAuthEnabled  bool // mirrors cfg.Auth.Internal.Enabled
	OAuthCookieSecure    bool // mirrors cfg.Auth.OAuth.CookieSecure
	CookieCfg            domain.CookieConfig
	CORSAllowedOrigins   []string
	Users                *service.UserService
	Groups               *service.GroupService
	Projects             *service.ProjectService
	Tokens               *service.TokenService
	Quota                *service.QuotaService
	Attribution          *service.AttributionService
	TraceMeta            domain.TraceMetaRepository
	Traces               domain.TraceRepository
	Logs                 domain.LogRepository
	OAuth                service.OAuthProvider // nil when disabled
	OAuthProvider        string                // "github" | "oidc" | "" when disabled
	JWT                  *service.JWTService
	EngineCachePurger    domain.EngineCachePurger
	HistoryStatsProvider domain.HistoryStatsProvider
	HistoryPurger        domain.HistoryPurger
	StatusProvider       domain.StatusProvider
	Connect              *service.ConnectService
	LiveHub              service.TraceEventBroadcaster // concrete *repository.LiveHub satisfies it; nil = create one
	Lifecycle            *service.PipelineLifecycle
	CLI                  *service.CLIService
	CIWrapperPath        string // path to pre-built dagger-kubernetes-ci binary
	StartupProvider      domain.StartupProvider
	ImageCache           domain.ImageCacheService
	EngineMetrics        *service.EngineMetricsService // nil = trace metrics endpoint disabled
}

// ServerConfig holds the non-injected server configuration (addresses + URLs).
type ServerConfig struct {
	ControlAddr string
	DataAddr    string
	// ControlListener and DataListener optionally supply pre-bound listeners.
	// When set they take precedence over ControlAddr/DataAddr, which removes
	// the probe-then-bind race that made integration tests flaky (a port freed
	// by a freeAddr-style helper can be claimed by another listener before the
	// server binds it; Hertz panics on a lost control-plane race). Production
	// leaves both nil and binds the addresses above as before.
	ControlListener net.Listener
	DataListener    net.Listener
	DataHost        string
	CollectorURL    string
	VictoriaURL     string
	CertPath        string
	KeyPath         string
	PipelineURL     string // base for pipeline-view links (= server.public_url, absolute http(s))
	// OTelMaxBodyBytes caps OTLP ingest request bodies (bytes). 0 = default
	// (64 MiB). The control-API maxControlBody cap is unaffected.
	OTelMaxBodyBytes int64
}

// Server is the control-plane HTTP server + mTLS data-plane listener.
type Server struct {
	cfg             *ServerConfig
	logger          *logrus.Logger
	metrics         *observ.Metrics
	mintingCA       domain.MintingCA
	fleetManager    *service.Manager
	sessions        domain.SessionStore
	sessionRegistry domain.SessionRegistry
	versionResolver domain.VersionResolver
	liveHub         *repository.LiveHub
	lifecycle       *service.PipelineLifecycle
	traces          domain.TraceRepository
	logs            domain.LogRepository
	hertz           *server.Hertz
	tlsListener     net.Listener
	dataConnSem     chan struct{}

	// Auth + RBAC collaborators.
	auth                *service.AuthService
	internalAuthEnabled bool
	oauthCookieSecure   bool
	cookieCfg           domain.CookieConfig
	corsAllowedOrigins  []string
	users               *service.UserService
	groups              *service.GroupService
	projects            *service.ProjectService
	tokens              *service.TokenService
	quota               *service.QuotaService
	attribution         *service.AttributionService
	traceMeta           domain.TraceMetaRepository
	jwt                 *service.JWTService
	oauth               service.OAuthProvider
	oauthProvider       string
	limiter             *attemptLimiter

	otelProxy     *reverseproxy.ReverseProxy
	victoriaProxy *reverseproxy.ReverseProxy

	engineCachePurger domain.EngineCachePurger
	historyStats      domain.HistoryStatsProvider
	historyPurger     domain.HistoryPurger
	status            domain.StatusProvider
	startupProvider   domain.StartupProvider
	connect           *service.ConnectService
	cli               *service.CLIService
	ciWrapperPath     string // path to pre-built dagger-kubernetes-ci binary
	imageCache        domain.ImageCacheService
	engineMetrics     *service.EngineMetricsService
}

// NewServer constructs a Server from a config and a Deps bundle.
func NewServer(cfg *ServerConfig, deps *Deps) *Server {
	return &Server{
		cfg:             cfg,
		logger:          deps.Logger,
		metrics:         deps.Metrics,
		mintingCA:       deps.MintingCA,
		fleetManager:    deps.FleetManager,
		sessions:        deps.Sessions,
		sessionRegistry: deps.SessionRegistry,
		versionResolver: deps.VersionResolver,
		liveHub:         resolveLiveHub(deps.LiveHub),
		lifecycle:       deps.Lifecycle,
		traces:          deps.Traces,
		logs:            deps.Logs,
		dataConnSem:     make(chan struct{}, maxDataConnections),

		auth:                deps.Auth,
		internalAuthEnabled: deps.InternalAuthEnabled,
		oauthCookieSecure:   deps.OAuthCookieSecure,
		cookieCfg:           deps.CookieCfg,
		corsAllowedOrigins:  deps.CORSAllowedOrigins,
		users:               deps.Users,
		groups:              deps.Groups,
		projects:            deps.Projects,
		tokens:              deps.Tokens,
		quota:               deps.Quota,
		attribution:         deps.Attribution,
		traceMeta:           deps.TraceMeta,
		jwt:                 deps.JWT,
		oauth:               deps.OAuth,
		oauthProvider:       deps.OAuthProvider,
		limiter:             newAttemptLimiter(),

		engineCachePurger: deps.EngineCachePurger,
		historyStats:      deps.HistoryStatsProvider,
		historyPurger:     deps.HistoryPurger,
		status:            deps.StatusProvider,
		startupProvider:   deps.StartupProvider,
		connect:           deps.Connect,
		cli:               deps.CLI,
		ciWrapperPath:     deps.CIWrapperPath,
		imageCache:        deps.ImageCache,
		engineMetrics:     deps.EngineMetrics,
	}
}

// resolveLiveHub returns the concrete *repository.LiveHub to use for SSE
// subscribe/unsubscribe, preferring the broadcaster injected via Deps.LiveHub
// (a *repository.LiveHub in practice) and falling back to a fresh hub.
func resolveLiveHub(b service.TraceEventBroadcaster) *repository.LiveHub {
	if h, ok := b.(*repository.LiveHub); ok && h != nil {
		return h
	}
	return repository.NewLiveHub()
}

// Start boots the control-plane HTTP server and the mTLS data-plane listener.
//
//nolint:gocritic // tlsCert is passed by value to keep the Start signature stable.
func (s *Server) Start(ctx context.Context, tlsCert tls.Certificate) error {
	h, err := s.configure()
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	s.hertz = h

	go func() {
		s.logger.WithField("addr", s.cfg.ControlAddr).Info("control plane listening")
		if err := s.hertz.Run(); err != nil {
			s.logger.WithError(err).Error("control plane error")
		}
	}()

	// Log exactly which certificate the data plane is about to serve. When
	// clients fail to trust it (or hang in "connecting to engine"), this is
	// the first clue: subject/issuer/SANs/expiry tell the operator whether
	// the intended (e.g. Let's Encrypt) certificate is actually in use.
	s.logDataPlaneServingCert(&tlsCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    s.mintingCA.CertPool(),
		MinVersion:   tls.VersionTLS12,
	}

	tlsLn := s.cfg.DataListener
	if tlsLn == nil {
		tlsLn, err = net.Listen("tcp", s.cfg.DataAddr)
		if err != nil {
			return fmt.Errorf("tcp listen: %w", err)
		}
	}

	s.tlsListener = tlsLn

	go func() {
		s.logger.WithField("addr", s.cfg.DataAddr).Info("data plane listening")
		for {
			raw, err := s.tlsListener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				s.logger.WithError(err).Error("tcp accept error")
				continue
			}
			s.serveTLSConn(raw, tlsConfig)
		}
	}()

	return nil
}

// tlsHandshakeInfo collects per-connection context while a data-plane TLS
// handshake runs, so both success and failure logs can carry the remote
// address and the SNI the client requested.
type tlsHandshakeInfo struct {
	remoteAddr string
	serverName string
}

// serveTLSConn performs the mTLS handshake for one accepted data-plane
// connection and, on success, dispatches it to the L4 engine tunnel. Every
// outcome is logged: a client that does not trust the server certificate
// (e.g. a Dagger CLI whose system trust pool rejects the embedded self-signed
// cert) aborts the handshake with a TLS alert, and without this log the
// failure is completely silent while the client retries forever.
func (s *Server) serveTLSConn(raw net.Conn, base *tls.Config) {
	info := &tlsHandshakeInfo{remoteAddr: raw.RemoteAddr().String()}

	cfg := base.Clone()
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		info.serverName = hello.ServerName
		return nil, nil
	}

	conn := tls.Server(raw, cfg)
	if err := conn.Handshake(); err != nil {
		reason := classifyTLSHandshakeError(err)
		s.metrics.DataHandshakeFailuresTotal.WithLabelValues(reason).Inc()
		s.logger.WithFields(logrus.Fields{
			"remote_addr": info.remoteAddr,
			"sni":         info.serverName,
			"reason":      reason,
		}).WithError(err).Warn("data plane TLS handshake failed")
		_ = raw.Close()
		return
	}

	state := conn.ConnectionState()
	s.logger.WithFields(logrus.Fields{
		"remote_addr":    info.remoteAddr,
		"sni":            info.serverName,
		"client_cn":      clientCN(&state),
		"client_cert_fp": clientFingerprint(&state),
		"tls_version":    tlsVersionName(state.Version),
		"cipher_suite":   tls.CipherSuiteName(state.CipherSuite),
	}).Debug("data plane TLS handshake succeeded")

	select {
	case s.dataConnSem <- struct{}{}:
		go func() {
			defer func() { <-s.dataConnSem }()
			s.handleDataConn(conn)
		}()
	default:
		s.logger.Warn("data plane connection limit reached, dropping connection")
		_ = conn.Close()
	}
}

// logDataPlaneServingCert logs the certificate the data-plane listener serves
// once at startup: subject, issuer, DNS SANs, validity window and SHA-256
// fingerprint. Operators comparing this against the certificate they intended
// to provide (e.g. via dataIngress.tls.secretName) can spot a wrong keypair
// immediately instead of debugging client hangs.
func (s *Server) logDataPlaneServingCert(tlsCert *tls.Certificate) {
	if tlsCert == nil || len(tlsCert.Certificate) == 0 {
		s.logger.Warn("data plane TLS certificate is empty; clients will fail the handshake")
		return
	}
	cert := tlsCert.Leaf
	if cert == nil {
		parsed, err := x509.ParseCertificate(tlsCert.Certificate[0])
		if err != nil {
			s.logger.WithError(err).Warn("data plane TLS certificate could not be parsed; clients may fail the handshake")
			return
		}
		cert = parsed
	}
	s.logger.WithFields(logrus.Fields{
		"subject":        cert.Subject.String(),
		"issuer":         cert.Issuer.String(),
		"dns_names":      cert.DNSNames,
		"not_before":     cert.NotBefore.UTC(),
		"not_after":      cert.NotAfter.UTC(),
		"cert_sha256":    hex.EncodeToString(certSHA256(cert.Raw)),
		"days_to_expiry": int(time.Until(cert.NotAfter).Hours() / 24),
	}).Info("data plane TLS server certificate")
}

// certSHA256 returns the SHA-256 fingerprint of a DER-encoded certificate.
func certSHA256(der []byte) []byte {
	sum := sha256.Sum256(der)
	return sum[:]
}

// classifyTLSHandshakeError maps a server-side handshake error to a stable,
// grep-friendly reason string. The distinction matters for diagnosing why a
// Dagger client hangs in "connecting to engine":
//   - "remote error: tls: bad certificate" / "unknown certificate authority"
//     means the CLIENT rejected the server certificate (it is not trusted, or
//     its SANs do not cover the dialed hostname);
//   - x509 verify errors on the server side mean the supervisor rejected the
//     client certificate (mTLS), e.g. a lease cert from another pod's CA.
func classifyTLSHandshakeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "remote error: tls: bad certificate"):
		return "client_rejected_server_certificate"
	case strings.Contains(msg, "remote error: tls: unknown certificate authority"):
		return "client_rejected_server_certificate_unknown_ca"
	case strings.Contains(msg, "remote error: tls: certificate required"):
		return "client_certificate_required"
	case strings.Contains(msg, "remote error: tls: handshake failure"):
		return "remote_handshake_failure"
	case strings.Contains(msg, "remote error: tls: no application protocol"):
		return "no_alpn_agreement"
	case strings.Contains(msg, "remote error: tls:"):
		return "remote_tls_alert"
	case strings.Contains(msg, "client didn't provide a certificate"):
		return "no_client_certificate"
	case strings.Contains(msg, "certificate signed by unknown authority"):
		return "client_certificate_unknown_authority"
	case strings.Contains(msg, "certificate has expired") || strings.Contains(msg, "certificate is not yet valid"):
		return "client_certificate_not_valid"
	case strings.Contains(msg, "client certificate authentication failed"):
		return "client_certificate_authentication_failed"
	default:
		return "handshake_error"
	}
}

// clientCN returns the subject common name of the peer certificate that
// authenticated a data-plane connection, or "" when none is present.
func clientCN(state *tls.ConnectionState) string {
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return state.PeerCertificates[0].Subject.CommonName
}

// tlsVersionName renders a TLS protocol version as a human-readable string.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

// configure builds the Hertz engine with all routes and middleware registered
// but does not run it. Split out so tests can drive routes via ut.PerformRequest
// without binding a port.
func (s *Server) configure() (*server.Hertz, error) {
	s.buildProxies()

	var opts []config.Option
	if s.cfg.ControlListener != nil {
		opts = append(opts, server.WithListener(s.cfg.ControlListener))
	} else {
		opts = append(opts, server.WithHostPorts(s.cfg.ControlAddr))
	}
	opts = append(opts,
		// Read timeout disabled: long-lived OTLP/telemetry streams can be slow
		// to produce a complete body. Control-API request bodies are still
		// capped per-handler via readBoundedBody/maxControlBody.
		server.WithReadTimeout(0),
		// Stream request bodies so large OTLP payloads are not buffered (or
		// rejected by the 4 MiB MaxRequestBodySize default) before the handler
		// reads them. Small control-API bodies are still read eagerly and
		// capped per-handler.
		server.WithStreamBody(true),
	)
	if s.cfg.CertPath != "" && s.cfg.KeyPath != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.CertPath, s.cfg.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("load control plane TLS cert: %w", err)
		}
		opts = append(opts, server.WithTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}))
	}
	h := server.Default(opts...)

	h.Use(s.requestLog())
	h.Use(s.securityHeaders())
	h.Use(s.corsMiddleware())

	// Data-plane + telemetry endpoints.
	h.POST("/v1/engines", s.handleEngines)
	h.POST("/v1/traces", s.handleOTel("traces"))
	h.POST("/v1/logs", s.handleOTel("logs"))
	h.POST("/v1/metrics", s.handleOTel("metrics"))

	h.GET("/api/v1/fleet", s.handleFleetInfo)
	h.POST("/api/v1/fleet/:version/purge-cache", s.adminOnly(s.handleFleetPurgeCache))
	h.GET("/api/v1/fleet/:version/purge-cache", s.adminOnly(s.handleFleetPurgeStatus))
	h.GET("/api/v1/history", s.handleHistoryInfo)
	h.POST("/api/v1/history/purge", s.adminOnly(s.handleHistoryPurge))
	h.POST("/api/v1/history/purge-all", s.adminOnly(s.handleHistoryPurgeAll))
	h.GET("/api/v1/status", s.handlePlatformStatus)
	h.GET("/api/v1/image-cache", s.adminOnly(s.handleImageCacheInfo))
	h.POST("/api/v1/image-cache/prune", s.adminOnly(s.handleImageCachePrune))
	h.POST("/api/v1/image-cache/prune-all", s.adminOnly(s.handleImageCachePruneAll))
	h.GET("/api/v1/connect/env", s.handleConnectEnv)
	h.GET("/api/v1/cli/versions/latest", s.handleCLILatest)
	h.GET("/api/v1/cli/:version", s.handleCLIDownload)
	h.GET("/api/v1/cli/ci-wrapper/latest", s.handleCIWrapperDownload)

	// Auth (public + self).
	h.POST("/api/v1/auth/login", s.handleLogin)
	h.POST("/api/v1/auth/refresh", s.handleRefresh)
	h.POST("/api/v1/auth/logout", s.handleLogout)
	h.GET("/api/v1/auth/providers", s.handleProviders)
	h.GET("/api/v1/auth/oauth/github/login", s.handleOAuthLogin)
	h.GET("/api/v1/auth/oauth/github/callback", s.handleOAuthCallback)
	h.GET("/api/v1/auth/oauth/oidc/login", s.handleOAuthOIDCLogin)
	h.GET("/api/v1/auth/oauth/oidc/callback", s.handleOAuthOIDCCallback)
	h.GET("/api/v1/auth/me", s.handleMe)
	h.PUT("/api/v1/auth/password", s.handleChangePassword)

	// Self-service API tokens.
	h.GET("/api/v1/tokens/me", s.handleMyTokenMeta)
	h.POST("/api/v1/tokens/me", s.handleMyTokenCreate)
	h.PUT("/api/v1/tokens/me/regenerate", s.handleMyTokenRegenerate)
	h.DELETE("/api/v1/tokens/me", s.handleMyTokenRevoke)

	// Admin user CRUD.
	h.GET("/api/v1/users", s.adminOnly(s.handleUsersList))
	h.POST("/api/v1/users", s.adminOnly(s.handleUserCreate))
	h.GET("/api/v1/users/:id", s.adminOnly(s.handleUserGet))
	h.PUT("/api/v1/users/:id", s.adminOnly(s.handleUserUpdate))
	h.DELETE("/api/v1/users/:id", s.adminOnly(s.handleUserDelete))
	h.PUT("/api/v1/users/:id/password", s.adminOnly(s.handleUserResetPassword))
	h.PUT("/api/v1/users/:id/groups", s.adminOnly(s.handleUserGroups))
	h.GET("/api/v1/users/:id/token", s.adminOnly(s.handleUserTokenMeta))
	h.DELETE("/api/v1/users/:id/token", s.adminOnly(s.handleUserTokenRevoke))

	// Admin group CRUD + members.
	h.GET("/api/v1/groups", s.adminOnly(s.handleGroupsList))
	h.POST("/api/v1/groups", s.adminOnly(s.handleGroupCreate))
	h.GET("/api/v1/groups/:id", s.adminOnly(s.handleGroupGet))
	h.PUT("/api/v1/groups/:id", s.adminOnly(s.handleGroupUpdate))
	h.DELETE("/api/v1/groups/:id", s.adminOnly(s.handleGroupDelete))
	h.GET("/api/v1/groups/:id/members", s.adminOnly(s.handleGroupMembers))
	h.PUT("/api/v1/groups/:id/members", s.adminOnly(s.handleGroupSetMembers))

	// Admin project CRUD.
	h.GET("/api/v1/projects", s.adminOnly(s.handleProjectsList))
	h.POST("/api/v1/projects", s.adminOnly(s.handleProjectCreate))
	h.PUT("/api/v1/projects/:id", s.adminOnly(s.handleProjectUpdate))
	h.DELETE("/api/v1/projects/:id", s.adminOnly(s.handleProjectDelete))

	// Traces (scoped + authorized).
	h.GET("/api/v1/traces", s.handleTracesList)
	h.GET("/api/v1/traces/:traceID", s.handleTracesDetail)
	h.GET("/api/v1/traces/:traceID/url", s.handleTracesURL)
	h.GET("/api/v1/traces/:traceID/logs", s.handleTracesLogs)
	h.GET("/api/v1/traces/:traceID/search", s.handleTracesSearch)
	h.GET("/api/v1/traces/:traceID/live", s.handleTracesLive)
	h.GET("/api/v1/traces/:traceID/metrics", s.handleTraceMetrics)

	h.GET("/api/v1/metrics", s.handleMetricsProxy)
	h.Any("/api/v1/metrics/*s", s.handleMetricsProxy)

	h.GET("/healthz", s.handleHealthz)
	h.GET("/readyz", s.handleReadyz)
	h.GET("/startup", s.handleStartup)
	h.GET("/metrics", adaptor.HertzHandler(promhttp.Handler()))

	h.NoRoute(s.handleNoRoute)

	return h, nil
}

// buildProxies constructs the reverse proxies once at startup (B6) instead of
// per request.
func (s *Server) buildProxies() {
	if s.cfg.CollectorURL != "" {
		target, err := url.Parse(s.cfg.CollectorURL)
		if err != nil {
			s.logger.WithError(err).Error("invalid collector url")
		} else {
			p := s.newHertzProxy(s.cfg.CollectorURL, func(req *protocol.Request) {
				req.Header.Del("Authorization")
				req.URI().SetScheme(target.Scheme)
				req.URI().SetHost(target.Host)
				req.Header.SetHostBytes([]byte(target.Host))
			}, "collector")
			if p != nil {
				p.SetErrorHandler(func(c *app.RequestContext, err error) {
					sig, _ := c.Get(otelSignalKey)
					signal, _ := sig.(string)
					s.metrics.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
					s.logger.WithError(err).Error("otel proxy error")
					c.Set(otelErrorKey, true)
					writeError(c, consts.StatusBadGateway, "collector unreachable")
				})
				s.otelProxy = p
			}
		}
	}

	if s.cfg.VictoriaURL != "" {
		target, err := url.Parse(s.cfg.VictoriaURL)
		if err != nil {
			s.logger.WithError(err).Error("invalid victoria url")
		} else {
			p := s.newHertzProxy(s.cfg.VictoriaURL, func(req *protocol.Request) {
				req.Header.Del("Authorization")
				path := strings.TrimPrefix(string(req.URI().Path()), "/api/v1/metrics")
				if path == "" || path == "/" {
					path = "/api/v1/query"
				}
				req.URI().SetScheme(target.Scheme)
				req.URI().SetHost(target.Host)
				req.URI().SetPath(path)
				req.Header.SetHostBytes([]byte(target.Host))
			}, "victoria")
			if p != nil {
				p.SetErrorHandler(func(c *app.RequestContext, err error) {
					s.logger.WithError(err).Error("victoria proxy error")
					writeError(c, consts.StatusBadGateway, "metrics query failed")
				})
				s.victoriaProxy = p
			}
		}
	}
}

// newHertzProxy constructs a reverse proxy for the given target URL. The
// optional director customises request rewriting; pass nil for pass-through.
func (s *Server) newHertzProxy(targetURL string, director func(*protocol.Request), name string) *reverseproxy.ReverseProxy {
	p, err := reverseproxy.NewSingleHostReverseProxy(targetURL)
	if err != nil {
		s.logger.WithError(err).WithField("url", targetURL).Error(fmt.Sprintf("invalid %s proxy URL", name))
		return nil
	}
	if director != nil {
		p.SetDirector(director)
	}
	return p
}

// Shutdown stops both listeners.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.tlsListener != nil {
		_ = s.tlsListener.Close()
	}
	if s.hertz != nil {
		return s.hertz.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleEngines(ctx context.Context, c *app.RequestContext) {
	id, ok := s.resolveIdentity(c)
	if !ok {
		return
	}

	body, err := readBoundedBody(c, maxRequestBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(c, consts.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(c, consts.StatusBadRequest, "invalid body")
		return
	}

	var req EngineRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c, consts.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	// Bound and sanitize the client-supplied trace ID before it is persisted
	// (trace_meta primary key) and reflected into URLs/logs (CWE-770/CWE-20).
	if !validTraceID(req.TraceID) {
		writeError(c, consts.StatusBadRequest, "invalid trace_id")
		return
	}

	s.logger.WithFields(logrus.Fields{
		"image":    req.Image,
		"module":   req.Module,
		"trace_id": req.TraceID,
		"user_id":  id.UserID,
	}).Info("engine provision request")

	engineVersion, err := s.extractVersion(req.Image)
	if err != nil {
		writeError(c, consts.StatusBadRequest, "invalid image")
		return
	}

	verStr := engineVersion.String()
	s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "request").Inc()

	if !s.versionResolver.IsAllowed(engineVersion) {
		s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "rejected").Inc()
		writeError(c, consts.StatusBadRequest, fmt.Sprintf("version %s not allowed (floor %s)", verStr, s.versionResolver.Floor()))
		return
	}

	// Quota gate (admins bypass).
	if err := s.quota.CheckEngineAccess(ctx, id); err != nil {
		s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "rejected").Inc()
		s.writeServiceError(c, err)
		return
	}

	start := time.Now()
	result, err := s.fleetManager.Acquire(ctx, verStr)
	if err != nil {
		s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "error").Inc()
		writeError(c, consts.StatusTooManyRequests, "no engine capacity")
		return
	}
	s.metrics.EngineAcquireDuration.WithLabelValues(verStr).Observe(time.Since(start).Seconds())

	clientCert, err := s.mintingCA.MintClientCert(result.PodName)
	if err != nil {
		s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "error").Inc()
		writeError(c, consts.StatusInternalServerError, "certificate minting failed")
		return
	}

	instanceID := fmt.Sprintf("%s-%d", result.PodName, time.Now().Unix())
	certFP := clientCert.Fingerprint()

	// Register the lease through the replicated store so EVERY pod can resolve
	// this certificate to the lease: the data-plane tunnel may land on any pod
	// (the -data Service load-balances), not just the one that served this
	// provision request.
	// Display aid (D11): record the group only when the user has exactly one.
	groupID := ""
	if len(id.GroupIDs) == 1 {
		groupID = id.GroupIDs[0]
	}
	if err := s.sessionRegistry.Register(ctx, certFP, verStr, result.PodName, instanceID, req.TraceID, id.UserID, groupID); err != nil {
		// Leader failover window (or a follower that still received this
		// request before leader-routed Services converge): keep the lease in
		// the local store so the tunnel is not lost outright.
		s.logger.WithError(err).Warn("session registration via raft failed; registering locally")
		s.sessions.Register(certFP, verStr, result.PodName, instanceID, req.TraceID, id.UserID)
		if groupID != "" {
			s.sessions.SetGroupID(certFP, groupID)
		}
	}
	s.metrics.ActiveLeases.Inc()
	s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "success").Inc()

	// Attribution: record trace_id -> user_id (best-effort; "" for synthetic
	// identities that have no users-table row). The engine version is persisted
	// here too because the Dagger CLI never emits dagger.io/engine.version.
	s.attribution.Provision(ctx, req.TraceID, attributionUserID(id), verStr)

	engineURL := s.cfg.DataHost
	if _, _, err := net.SplitHostPort(engineURL); err != nil {
		engineURL = fmt.Sprintf("%s:443", engineURL)
	}

	resp := EngineSpecResponse{
		Image:      result.Image,
		URL:        engineURL,
		Cert:       clientCert,
		InstanceID: instanceID,
		Location:   "k8s",
		OrgID:      "",
		UserID:     id.Username,
	}

	c.JSON(consts.StatusCreated, resp)
}

func (s *Server) extractVersion(image string) (*domain.Version, error) {
	parts := strings.Split(image, ":")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid image format: %s", image)
	}

	v, err := s.versionResolver.ResolveMinimal(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid version: %w", err)
	}
	return v, nil
}

// handleOTel reverse-proxies OTLP signals to the collector. For traces it also
// best-effort extracts root-span metadata and runs attribution before proxying.
func (s *Server) handleOTel(signal string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		id, ok := s.resolveIdentity(c)
		if !ok {
			return
		}

		if s.otelProxy == nil {
			s.metrics.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
			writeError(c, consts.StatusInternalServerError, "collector misconfigured")
			return
		}

		// Defense-in-depth: reject oversized OTLP bodies up front by
		// Content-Length. With StreamBody on, a chunked body has no
		// Content-Length and is bounded only by the read below; a declared
		// Content-Length lets us fail fast before buffering (CWE-400). OTLP
		// batches use a dedicated, larger cap than the control-API bodies.
		limit := s.cfg.OTelMaxBodyBytes
		if limit <= 0 {
			limit = defaultOTLPMaxBodyBytes
		}
		if cl := c.Request.Header.ContentLength(); int64(cl) > limit {
			s.metrics.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
			writeError(c, consts.StatusRequestEntityTooLarge, "otel body too large")
			return
		}

		// Best-effort attribution for traces (Hertz buffers the body, so
		// reading it does not consume it for the reverse proxy). The same
		// bytes are reused below to fan out live SSE re-fetch events.
		var body []byte
		if signal == "traces" || signal == "logs" {
			body, _ = c.Body()
		}
		if signal == "traces" && len(body) > 0 {
			sums := service.ExtractTraceSummaries(body)
			for i := range sums {
				s.attribution.Ingest(ctx, sums[i].TraceID, attributionUserID(id), sums[i].CIRepo, sums[i].GitRemote, sums[i].CIProvider, sums[i].Version, sums[i].Status, sums[i].DurationMS, sums[i].StartedAt)
			}
		}

		s.metrics.OTelIngestTotal.WithLabelValues(signal, "request").Inc()
		c.Set(otelSignalKey, signal)

		s.otelProxy.ServeHTTP(ctx, c)

		// Only count success when the error handler did not fire (B1).
		if _, errored := c.Get(otelErrorKey); !errored {
			s.metrics.OTelIngestTotal.WithLabelValues(signal, "success").Inc()
		}

		// Broadcast a lightweight re-fetch signal to live SSE subscribers,
		// independent of proxy success.
		s.broadcastOTelUpdate(signal, body)
	}
}

// broadcastOTelUpdate fans out a lightweight re-fetch event to live SSE
// subscribers for every trace ID present in the ingested OTLP body so clients
// re-fetch steps/logs without waiting for the next poll.
func (s *Server) broadcastOTelUpdate(signal string, body []byte) {
	if s.liveHub == nil || len(body) == 0 {
		return
	}
	switch signal {
	case "traces":
		for _, traceID := range service.ExtractTraceIDs(body) {
			if traceID != "" {
				s.liveHub.Broadcast(traceID, map[string]string{"type": "trace_update"})
			}
		}
	case "logs":
		for _, traceID := range service.ExtractLogTraceIDs(body) {
			if traceID != "" {
				s.liveHub.Broadcast(traceID, map[string]string{"type": "logs_update"})
			}
		}
	}
}

// handleTracesURL returns the self-hosted pipeline-view URL for a trace.
// Gated by authorizeTraceRequest (owner/member/admin; unknown meta ->
// admin-only). URL derivation does not require the trace to exist in Tempo.
func (s *Server) handleTracesURL(_ context.Context, c *app.RequestContext) {
	traceID, ok := s.authorizeTraceRequest(c)
	if !ok {
		return
	}
	// Validate the trace ID charset before URL derivation so a malformed
	// id yields 400 (bad request) rather than 500 (misconfigured base).
	// authorizeTraceRequest only checks non-empty (CWE-20).
	if !validTraceID(traceID) {
		writeError(c, consts.StatusBadRequest, "invalid trace ID")
		return
	}
	u, ok := s.pipelineViewURL(traceID)
	if !ok {
		writeError(c, consts.StatusInternalServerError, "pipeline url misconfigured")
		return
	}
	writeJSON(c, traceURLResponse{TraceID: traceID, URL: u})
}

// pipelineViewURL builds the self-hosted pipeline-view URL for traceID, logging
// a warning when the configured base is invalid. Callers decide whether an
// invalid base is fatal (endpoint) or best-effort (trace detail enrichment).
func (s *Server) pipelineViewURL(traceID string) (string, bool) {
	u, err := domain.PipelineViewURL(s.cfg.PipelineURL, traceID)
	if err != nil {
		s.logger.WithError(err).WithField("trace_id", traceID).Warn("pipeline url misconfigured")
		return "", false
	}
	return u, true
}

func (s *Server) handleFleetInfo(_ context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}

	infos, err := s.fleetManager.AllFleetInfo()
	if err != nil {
		s.logger.WithError(err).Error("fleet info unavailable")
		writeError(c, consts.StatusInternalServerError, "fleet unavailable")
		return
	}
	writeJSON(c, infos)
}

func (s *Server) handleDataConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		s.logger.WithField("remote_addr", conn.RemoteAddr().String()).Error("not a TLS connection")
		return
	}

	state := tlsConn.ConnectionState()
	fp := clientFingerprint(&state)
	if fp == "" {
		s.logger.WithFields(logrus.Fields{
			"remote_addr": conn.RemoteAddr().String(),
			"sni":         state.ServerName,
		}).Error("no client certificate")
		return
	}

	s.serveDataTunnel(fp, conn)
}

// clientFingerprint extracts the client-cert fingerprint (serial number hex)
// from a TLS connection state. Returns "" when no client certificate is
// present. Extracted so tests can drive serveDataTunnel without a real TLS
// handshake.
func clientFingerprint(state *tls.ConnectionState) string {
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", state.PeerCertificates[0].SerialNumber)
}

// serveDataTunnel is the body of the L4 data-plane tunnel for the client-cert
// fingerprint fp. On every exit path after IncInFlight it decrements the
// in-flight count and notifies the pipeline lifecycle, which marks the
// associated trace failed when this was the last tunnel for the lease.
func (s *Server) serveDataTunnel(fp string, conn net.Conn) {
	lease, err := s.sessions.Get(fp)
	if err != nil {
		s.logger.WithField("fp", fp).WithError(err).Error("lease not found")
		return
	}

	_ = s.sessions.IncInFlight(fp)
	if s.lifecycle != nil {
		s.lifecycle.OnTunnelOpen(lease.TraceID)
	}
	defer func() {
		remaining, _ := s.sessions.DecInFlightAndGet(fp)
		if s.lifecycle != nil {
			s.lifecycle.OnTunnelClosed(lease.TraceID, remaining)
		}
	}()

	fleet, err := s.fleetManager.GetVersionFleet(lease.Version)
	if err != nil {
		s.logger.WithError(err).Error("get version fleet failed")
		return
	}

	var targetIP string
	for _, r := range fleet.Ordinals {
		if r.Name == lease.ReplicaPod {
			targetIP = r.PodIP
			break
		}
	}

	if targetIP == "" {
		s.logger.WithField("pod", lease.ReplicaPod).Error("target pod not found")
		return
	}

	backend, err := net.DialTimeout("tcp", net.JoinHostPort(targetIP, "9999"), 5*time.Second)
	if err != nil {
		s.logger.WithField("ip", targetIP).WithError(err).Error("backend dial failed")
		return
	}
	defer func() { _ = backend.Close() }()

	s.touchSession(fp)

	// A long-running pipeline holds a single tunnel open for its whole
	// lifetime. If the lease's LastActivity were only set here, the reaper
	// would expire it mid-run (default lease_ttl is 2m) and the fleet sweeper
	// would scale the engine down underneath the client. Refresh the lease and
	// the connection deadlines on a heartbeat so the tunnel (and its engine)
	// survive long-running sessions.
	const (
		tunnelIdleTimeout = 10 * time.Minute
		heartbeatInterval = 30 * time.Second
	)
	_ = conn.SetDeadline(time.Now().Add(tunnelIdleTimeout))
	_ = backend.SetDeadline(time.Now().Add(tunnelIdleTimeout))

	errc := make(chan error, 2)
	go func() {
		_, e := io.Copy(backend, conn)
		errc <- e
	}()
	go func() {
		_, e := io.Copy(conn, backend)
		errc <- e
	}()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-heartbeat.C:
			s.touchSession(fp)
			_ = conn.SetDeadline(time.Now().Add(tunnelIdleTimeout))
			_ = backend.SetDeadline(time.Now().Add(tunnelIdleTimeout))
		case <-errc:
			_ = conn.Close()
			_ = backend.Close()
			return
		}
	}
}

// touchSession refreshes the lease liveness through the replicated store so
// every pod's reaper and the fleet sweeper keep seeing the session as live.
// Falls back to the local store when the raft apply is unavailable (leader
// failover window) so the local reaper never kills a live tunnel.
func (s *Server) touchSession(fp string) {
	if s.sessionRegistry != nil {
		if err := s.sessionRegistry.Touch(context.Background(), fp); err == nil {
			return
		}
	}
	_ = s.sessions.Touch(fp)
}

// handleNoRoute serves the embedded SPA for unmatched routes.
func (s *Server) handleNoRoute(ctx context.Context, c *app.RequestContext) {
	s.serveUI(ctx, c)
}

func (s *Server) requestLog() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		c.Next(ctx)
		s.logger.WithFields(logrus.Fields{
			"method":      string(c.Method()),
			"path":        string(c.Path()),
			"status":      c.Response.StatusCode(),
			"duration_ms": time.Since(start).Milliseconds(),
		}).Info("request completed")
	}
}

// securityHeaders sets baseline hardening headers on every response:
// clickjacking protection for the admin UI (CWE-1021), MIME-sniffing
// protection, and referrer suppression so the SSE ?token= query param (D14)
// cannot leak via a Referer header (CWE-200).
func (s *Server) securityHeaders() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		c.Response.Header.Set("X-Content-Type-Options", "nosniff")
		c.Response.Header.Set("X-Frame-Options", "DENY")
		c.Response.Header.Set("Content-Security-Policy", "frame-ancestors 'none'")
		c.Response.Header.Set("Referrer-Policy", "no-referrer")
		c.Next(ctx)
	}
}

// corsMiddleware adds permissive CORS headers only when the request Origin
// exactly matches an entry in the configured allowlist (empty = same-origin
// only, no Access-Control-Allow-Origin). Never emits "*" with credentials.
// OPTIONS preflight requests for allowed origins are answered directly with
// 204 + the allowed methods/headers. Vary: Origin is emitted whenever an
// Origin header is present (allowed or not) so a shared/intermediate cache
// cannot serve a response computed for one origin to a different origin
// (cross-origin response confusion, CWE-349 / OWASP A05).
func (s *Server) corsMiddleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		origin := string(c.Request.Header.Peek("Origin"))
		if origin == "" {
			c.Next(ctx)
			return
		}
		// An Origin header is present: the response varies by origin regardless
		// of whether this origin is allowed, so caches must key on it.
		c.Response.Header.Add("Vary", "Origin")
		if s.originAllowed(origin) {
			c.Response.Header.Set("Access-Control-Allow-Origin", origin)
			c.Response.Header.Set("Access-Control-Allow-Credentials", "true")
			if string(c.Method()) == "OPTIONS" {
				c.Response.Header.Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
				c.Response.Header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				c.SetStatusCode(consts.StatusNoContent)
				c.Abort()
				return
			}
		}
		c.Next(ctx)
	}
}

// originAllowed reports whether origin exactly matches an entry in the CORS
// allowlist.
func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.corsAllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

func writeError(c *app.RequestContext, status int, message string) {
	c.JSON(status, ErrorResponse{Message: message})
}

func writeJSON(c *app.RequestContext, v any) {
	c.JSON(consts.StatusOK, v)
}

// formatTime renders timestamps in the API's canonical RFC3339/UTC form.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// adminOnly gates a handler behind requireAdmin; the wrapped handler can
// retrieve the resolved identity with identityOf(c).
func (s *Server) adminOnly(h app.HandlerFunc) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if _, ok := s.requireAdmin(c); !ok {
			return
		}
		h(ctx, c)
	}
}

// decodeBody unmarshals the request body into v. On read/decode failure it
// writes a 400 (or 413 when the body exceeds maxControlBody) and returns false.
// The bound is enforced via readBoundedBody so a streamed body (StreamBody on)
// cannot exhaust memory (CWE-400/CWE-770).
func decodeBody(c *app.RequestContext, v any) bool {
	body, ok := readControlBody(c)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(c, consts.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

// readControlBody reads and bounds the request body. On failure it writes the
// appropriate 400/413 response and returns ok=false.
func readControlBody(c *app.RequestContext) (body []byte, ok bool) {
	body, err := readBoundedBody(c, maxControlBody)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(c, consts.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writeError(c, consts.StatusBadRequest, "invalid body")
		return nil, false
	}
	return body, true
}

// traceIDRe bounds client-supplied trace IDs: they are persisted as the
// trace_meta primary key and reflected in URLs, so their length and charset
// are constrained (CWE-770/CWE-20). Real Dagger trace IDs are hex; the
// slightly wider charset keeps the API contract tolerant.
var traceIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validTraceID reports whether a client-supplied trace ID is acceptable.
// Empty is allowed (attribution is best-effort and skips it).
func validTraceID(id string) bool {
	return id == "" || traceIDRe.MatchString(id)
}

// clampLimit parses a limit query param, defaulting to DefaultTraceLimit and
// capping at MaxTraceLimit.
func clampLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return domain.DefaultTraceLimit
	}
	if n > domain.MaxTraceLimit {
		return domain.MaxTraceLimit
	}
	return n
}
