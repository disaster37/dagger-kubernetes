package handler

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
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
	"github.com/cloudwego/hertz/pkg/app/client"
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
	// project CRUD, cache purge). It restores the 4 MiB protection that the
	// pre-StreamBody hertz default (MaxRequestBodySize) provided before
	// WithStreamBody(true) was enabled for the cache vhost. Without it, an
	// unauthenticated caller could send an arbitrarily large body to a public
	// endpoint (e.g. /api/v1/auth/login) and c.Body() would buffer it fully
	// into memory (CWE-400/CWE-770).
	maxControlBody = 4 << 20 // 4 MiB

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
// decoding. The cache vhost is exempt: its bodies are streamed to the backend
// by the reverse proxy and never pass through this helper.
func readBoundedBody(c *app.RequestContext, max int) ([]byte, error) {
	if c.Request.IsBodyStream() {
		// LimitReader stops after max+1 bytes; the +1 lets us detect overflow.
		r := io.LimitReader(c.Request.BodyStream(), int64(max)+1)
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if len(b) > max {
			return nil, errBodyTooLarge
		}
		return b, nil
	}
	body, err := c.Body()
	if err != nil {
		return nil, err
	}
	if len(body) > max {
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

// cacheRouter is the subset of *service.RegistryRouter used by the cache proxy
// handler. Introduced so tests can inject a stub (mirrors the
// domain.CacheBackend stub pattern). *service.RegistryRouter satisfies it.
type cacheRouter interface {
	Backends() []domain.RegistryBackend
	RouteForPull(ctx context.Context, repo, ref string) (domain.RegistryBackend, error)
	RouteForBlobPull(ctx context.Context, repo, digest string) (domain.RegistryBackend, error)
	RouteForPush(repo string) (domain.RegistryBackend, error)
	RouteForUploadStart(repo string) (domain.RegistryBackend, error)
	RouteForUploadResume(ctx context.Context, uuid string) (domain.RegistryBackend, error)
	RecordUploadSession(ctx context.Context, uuid, repo, backendID string) error
	CompleteUpload(ctx context.Context, uuid, digest, backendID string) error
	RecordManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error
	MarkDown(backendID string)
}

// routeKind classifies a proxied OCI request for post-response recording.
type routeKind int

const (
	routeOther routeKind = iota
	routeManifest
	routeUploadStart
	routeUploadComplete
)

// errBackendAuth signals a backend 401 (WWW-Authenticate suppressed) so the
// error handler can map it to a distinct 502 message.
var errBackendAuth = errors.New("cache backend auth failed")

// cacheProxyBackendIDKey is the RequestContext key holding the chosen backend
// ID (read by the proxy error handler to mark the backend down).
const cacheProxyBackendIDKey = "dagger_cache_backend_id"

// OCI request-path regexes (defense vs path traversal / SSRF — the target is
// always from config, but the path is forwarded, so validate its shape).
//
// Repo/ref/uuid captures are single path segments ([^/]+). The manifest ref
// is intentionally [^/]+ (not .+) so a client cannot smuggle "../" sequences
// that the backend would normalise into a different /v2/ endpoint. A further
// validOCIPathSegment check rejects "."/ ".." and control characters.
var (
	rePing           = regexp.MustCompile(`^/v2/?$`)
	reManifest       = regexp.MustCompile(`^/v2/([^/]+)/manifests/([^/]+)$`)
	reBlobUpload     = regexp.MustCompile(`^/v2/([^/]+)/blobs/uploads/?$`)
	reBlobUploadUUID = regexp.MustCompile(`^/v2/([^/]+)/blobs/uploads/([^/]+)$`)
	reBlob           = regexp.MustCompile(`^/v2/([^/]+)/blobs/(sha256:[a-f0-9]{64})$`)
	reTags           = regexp.MustCompile(`^/v2/([^/]+)/tags/list$`)
	reCatalog        = regexp.MustCompile(`^/v2/_catalog$`)
	reDigest         = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// validDigest reports whether d has the sha256:<hex> shape expected from a
// Docker-Content-Digest header before it is persisted in the routing table.
func validDigest(d string) bool {
	return reDigest.MatchString(d)
}

// validOCIPathSegment reports whether a captured path segment is safe to
// forward to the backend. It rejects empty, ".", ".." (path traversal) and
// control characters (CWE-22/CWE-20). Legitimate OCI repo names, tags, digests
// and upload UUIDs never contain these.
func validOCIPathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Deps bundles the collaborators injected into the Server. Replacing the old
// 11-param constructor, this is far easier to maintain and construct in tests.
type Deps struct {
	Logger              *logrus.Logger
	Metrics             *observ.Metrics
	MintingCA           domain.MintingCA
	FleetManager        *service.Manager
	Sessions            domain.SessionStore
	CacheBackend        domain.CacheBackend
	VersionResolver     domain.VersionResolver
	Auth                *service.AuthService
	InternalAuthEnabled bool // mirrors cfg.Auth.Internal.Enabled
	OAuthCookieSecure   bool // mirrors cfg.Auth.OAuth.CookieSecure
	Users               *service.UserService
	Groups              *service.GroupService
	Projects            *service.ProjectService
	Tokens              *service.TokenService
	Quota               *service.QuotaService
	Attribution         *service.AttributionService
	TraceMeta           domain.TraceMetaRepository
	Traces              domain.TraceRepository
	Logs                domain.LogRepository
	OAuth               service.OAuthProvider // nil when disabled
	OAuthProvider       string                // "github" | "oidc" | "" when disabled
	JWT                 *service.JWTService
	CacheStatsProvider  domain.CacheStatsProvider
	CachePurger         domain.CachePurger
	StatusProvider      domain.StatusProvider
	Connect             *service.ConnectService
	Router              *service.RegistryRouter
}

// ServerConfig holds the non-injected server configuration (addresses + URLs).
type ServerConfig struct {
	ControlAddr  string
	DataAddr     string
	DataHost     string
	CacheHost    string // dedicated cache vhost (Host header to match)
	CacheScheme  string // scheme for rewritten upload Locations; "" or invalid ⇒ "https"
	CacheToken   string // engine→proxy bearer; "" = proxy auth disabled
	CollectorURL string
	VictoriaURL  string
	CertPath     string
	KeyPath      string
}

// Server is the control-plane HTTP server + mTLS data-plane listener.
type Server struct {
	cfg             *ServerConfig
	logger          *logrus.Logger
	metrics         *observ.Metrics
	mintingCA       domain.MintingCA
	fleetManager    *service.Manager
	sessions        domain.SessionStore
	cacheBackend    domain.CacheBackend
	versionResolver domain.VersionResolver
	liveHub         *repository.LiveHub
	traces          domain.TraceRepository
	logs            domain.LogRepository
	hertz           *server.Hertz
	tlsListener     net.Listener
	dataConnSem     chan struct{}

	// Auth + RBAC collaborators.
	auth                *service.AuthService
	internalAuthEnabled bool
	oauthCookieSecure   bool
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
	cacheProxy    *reverseproxy.ReverseProxy // single instance; target chosen per-request in the director
	router        cacheRouter
	cacheToken    string

	cacheStats  domain.CacheStatsProvider
	cachePurger domain.CachePurger
	status      domain.StatusProvider
	connect     *service.ConnectService
}

// NewServer constructs a Server from a config and a Deps bundle.
func NewServer(cfg *ServerConfig, deps *Deps) *Server {
	s := &Server{
		cfg:             cfg,
		logger:          deps.Logger,
		metrics:         deps.Metrics,
		mintingCA:       deps.MintingCA,
		fleetManager:    deps.FleetManager,
		sessions:        deps.Sessions,
		cacheBackend:    deps.CacheBackend,
		versionResolver: deps.VersionResolver,
		liveHub:         repository.NewLiveHub(),
		traces:          deps.Traces,
		logs:            deps.Logs,
		dataConnSem:     make(chan struct{}, maxDataConnections),

		auth:                deps.Auth,
		internalAuthEnabled: deps.InternalAuthEnabled,
		oauthCookieSecure:   deps.OAuthCookieSecure,
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

		cacheStats:  deps.CacheStatsProvider,
		cachePurger: deps.CachePurger,
		status:      deps.StatusProvider,
		connect:     deps.Connect,
		cacheToken:  cfg.CacheToken,
	}

	// Only store a non-nil router: assigning a nil *service.RegistryRouter to
	// the cacheRouter interface would produce a typed-nil whose methods panic.
	if deps.Router != nil {
		s.router = deps.Router
	}
	return s
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

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    s.mintingCA.CertPool(),
		MinVersion:   tls.VersionTLS12,
	}

	tlsLn, err := net.Listen("tcp", s.cfg.DataAddr)
	if err != nil {
		return fmt.Errorf("tcp listen: %w", err)
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
			conn := tls.Server(raw, tlsConfig)
			if err := conn.Handshake(); err != nil {
				s.logger.WithError(err).Debug("tls handshake failed")
				_ = raw.Close()
				continue
			}
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
	}()

	return nil
}

// configure builds the Hertz engine with all routes and middleware registered
// but does not run it. Split out so tests can drive routes via ut.PerformRequest
// without binding a port.
func (s *Server) configure() (*server.Hertz, error) {
	s.buildProxies()

	opts := []config.Option{
		server.WithHostPorts(s.cfg.ControlAddr),
		// Read timeout disabled: cache-proxy blob uploads are unbounded (multi-GB).
		// Control-API request bodies are capped per-handler (handleEngines 1 MiB),
		// so disabling the global read timeout only relaxes the cache vhost.
		server.WithReadTimeout(0),
		// Stream request bodies so multi-GB blob uploads are not buffered (or
		// rejected by the 4 MiB MaxRequestBodySize default) before reaching the
		// cache proxy. Small control-API bodies are still read eagerly by their
		// handlers and capped per-handler.
		server.WithStreamBody(true),
	}
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

	if s.cacheProxy != nil {
		h.Use(s.cacheHostMiddleware())
	}

	// Data-plane + telemetry endpoints.
	h.POST("/v1/engines", s.handleEngines)
	h.POST("/v1/traces", s.handleOTel("traces"))
	h.POST("/v1/logs", s.handleOTel("logs"))
	h.POST("/v1/metrics", s.handleOTel("metrics"))

	h.GET("/v1/versions", s.handleAdminVersions)
	h.GET("/api/v1/fleet", s.handleFleetInfo)
	h.GET("/api/v1/cache", s.handleCacheInfo)
	h.POST("/api/v1/cache/purge", s.adminOnly(s.handleCachePurge))
	h.POST("/api/v1/cache/purge-all", s.adminOnly(s.handleCachePurgeAll))
	h.GET("/api/v1/status", s.handlePlatformStatus)
	h.GET("/api/v1/connect/env", s.handleConnectEnv)

	// Auth (public + self).
	h.POST("/api/v1/auth/login", s.handleLogin)
	h.POST("/api/v1/auth/refresh", s.handleRefresh)
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
	h.GET("/api/v1/traces/:traceID/logs", s.handleTracesLogs)
	h.GET("/api/v1/traces/:traceID/live", s.handleTracesLive)

	h.GET("/api/v1/logs/:traceID", s.handleLogsRoutes)

	h.GET("/api/v1/metrics", s.handleMetricsProxy)
	h.Any("/api/v1/metrics/*s", s.handleMetricsProxy)

	h.GET("/healthz", s.handleHealthz)
	h.GET("/readyz", s.handleReadyz)
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

	if s.cfg.CacheHost != "" && s.router != nil && len(s.router.Backends()) > 0 {
		// ResponseBodyStream makes the proxy stream the backend's response body
		// (blob pulls can be multi-GB); without it the hertz client buffers the
		// entire response in memory.
		p, err := reverseproxy.NewSingleHostReverseProxy(
			fmt.Sprintf("http://%s", s.router.Backends()[0].InternalAddr),
			client.WithResponseBodyStream(true),
		)
		if err != nil {
			s.logger.WithError(err).Error("invalid cache proxy URL")
		} else {
			p.SetDirector(s.cacheProxyDirector())
			p.SetModifyResponse(s.cacheProxyModifyResponse())
			p.SetErrorHandler(func(c *app.RequestContext, err error) {
				s.logger.WithError(err).Error("cache proxy error")
				if errors.Is(err, errBackendAuth) {
					writeError(c, consts.StatusBadGateway, "cache backend auth failed")
					return
				}
				if id, ok := c.Get(cacheProxyBackendIDKey); ok {
					if backendID, ok := id.(string); ok && backendID != "" {
						s.router.MarkDown(backendID)
					}
				}
				writeError(c, consts.StatusBadGateway, "cache backend unreachable")
			})
			s.cacheProxy = p
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
	s.sessions.Register(certFP, verStr, result.PodName, instanceID, req.TraceID, id.UserID)
	// Display aid (D11): record the group only when the user has exactly one.
	// SetGroupID is used because the lease is shared with the store and read
	// concurrently (e.g. by List during quota checks).
	if len(id.GroupIDs) == 1 {
		s.sessions.SetGroupID(certFP, id.GroupIDs[0])
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
		// Content-Length lets us fail fast before buffering (CWE-400).
		if cl := c.Request.Header.ContentLength(); cl > maxControlBody {
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
			for _, sum := range service.ExtractTraceSummaries(body) {
				s.attribution.Ingest(ctx, sum.TraceID, attributionUserID(id), sum.CIRepo, sum.GitRemote, sum.CIProvider, sum.Version, sum.Status, sum.DurationMS, sum.StartedAt)
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
			if len(traceID) > 0 {
				s.liveHub.Broadcast(traceID, map[string]string{"type": "trace_update"})
			}
		}
	case "logs":
		for _, traceID := range service.ExtractLogTraceIDs(body) {
			if len(traceID) > 0 {
				s.liveHub.Broadcast(traceID, map[string]string{"type": "logs_update"})
			}
		}
	}
}

func (s *Server) handleAdminVersions(_ context.Context, c *app.RequestContext) {
	if !s.requireAuth(c) {
		return
	}

	versions := s.versionResolver.AllReleases()
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.String()
	}
	writeJSON(c, out)
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
		s.logger.Error("not a TLS connection")
		return
	}

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		s.logger.Error("no client certificate")
		return
	}

	clientCert := state.PeerCertificates[0]
	fp := fmt.Sprintf("%x", clientCert.SerialNumber)

	lease, err := s.sessions.Get(fp)
	if err != nil {
		s.logger.WithField("fp", fp).WithError(err).Error("lease not found")
		return
	}

	_ = s.sessions.IncInFlight(fp)
	defer func() { _ = s.sessions.DecInFlight(fp) }()

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

	_ = s.sessions.Touch(fp)

	// Set initial deadlines; the io.Copy goroutines will refresh them.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	_ = backend.SetDeadline(time.Now().Add(10 * time.Minute))

	errc := make(chan error, 2)
	go func() {
		_, e := io.Copy(backend, conn)
		errc <- e
	}()
	go func() {
		_, e := io.Copy(conn, backend)
		errc <- e
	}()

	<-errc
	_ = conn.Close()
	_ = backend.Close()
}

// handleNoRoute serves the embedded SPA for unmatched routes. Cache-host
// requests never reach this handler: when the cache proxy is configured,
// cacheHostMiddleware intercepts them before routing.
func (s *Server) handleNoRoute(ctx context.Context, c *app.RequestContext) {
	s.serveUI(ctx, c)
}

func (s *Server) cacheHostMiddleware() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if !strings.EqualFold(string(c.Host()), s.cfg.CacheHost) {
			c.Next(ctx)
			return
		}
		s.serveCacheHost(ctx, c)
		c.Abort()
	}
}

func (s *Server) serveCacheHost(ctx context.Context, c *app.RequestContext) {
	if s.cacheProxy == nil {
		writeError(c, consts.StatusBadGateway, "cache backend unreachable")
		return
	}
	if !s.requireCacheAuth(c) {
		return
	}
	backend, kind, err := s.routeCacheRequest(ctx, c)
	if err != nil {
		s.writeCacheRouteError(c, err)
		return
	}
	// Stash backend target + creds on the inbound request headers; the
	// director reads and deletes them. Credentials never logged.
	c.Request.Header.Set("X-Dagger-Cache-Target", backend.InternalAddr)
	c.Request.Header.Set("X-Dagger-Cache-User", backend.Username)
	c.Request.Header.Set("X-Dagger-Cache-Pass", backend.Password)
	c.Set(cacheProxyBackendIDKey, backend.ID)

	s.cacheProxy.ServeHTTP(ctx, c)

	// Post-process: record routes for successful pushes (non-racy; next pull
	// is a separate run). Upload-session recording also happens here (Hertz
	// flushes the response only after the handler returns, so the engine's
	// next PATCH cannot arrive before the session row is committed).
	s.recordCacheRoute(ctx, c, backend, kind)
}

// requireCacheAuth validates the engine's cache token (bearer or basic). With
// no configured token it allows all traffic (dev mode).
func (s *Server) requireCacheAuth(c *app.RequestContext) bool {
	if s.cacheToken == "" {
		return true // dev mode; proxy auth disabled
	}
	tok := extractCacheToken(c)
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cacheToken)) != 1 {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// extractCacheToken parses Authorization: `Bearer <t>` → t; `Basic <b>` →
// base64-decode, split on the first ':', take the password (index 1); empty
// otherwise.
func extractCacheToken(c *app.RequestContext) string {
	auth := string(c.Request.Header.Peek("Authorization"))
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		return strings.TrimPrefix(auth, "Bearer ")
	case strings.HasPrefix(auth, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err != nil {
			return ""
		}
		_, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return ""
		}
		return pass
	default:
		return ""
	}
}

// routeCacheRequest parses the OCI path and dispatches to the router.
func (s *Server) routeCacheRequest(ctx context.Context, c *app.RequestContext) (domain.RegistryBackend, routeKind, error) {
	path := string(c.Path())
	method := string(c.Method())
	invalid := func() (domain.RegistryBackend, routeKind, error) {
		return domain.RegistryBackend{}, routeOther, service.ErrInvalidOCIPath
	}

	// Non-routed endpoints (ping, tags list, catalog): any healthy backend.
	if method == "GET" && (rePing.MatchString(path) || reTags.MatchString(path) || reCatalog.MatchString(path)) {
		if reTags.MatchString(path) {
			if m := reTags.FindStringSubmatch(path); m != nil && !validOCIPathSegment(m[1]) {
				return invalid()
			}
		}
		b, err := s.routeLeastCharged()
		return b, routeOther, err
	}

	if m := reManifest.FindStringSubmatch(path); m != nil {
		repo, ref := m[1], m[2]
		if !validOCIPathSegment(repo) || !validOCIPathSegment(ref) {
			return invalid()
		}
		switch method {
		case "GET", "HEAD":
			b, err := s.router.RouteForPull(ctx, repo, ref)
			return b, routeOther, err
		case "PUT":
			b, err := s.router.RouteForPush(repo)
			return b, routeManifest, err
		}
		return invalid()
	}

	if method == "POST" {
		if m := reBlobUpload.FindStringSubmatch(path); m != nil {
			if !validOCIPathSegment(m[1]) {
				return invalid()
			}
			b, err := s.router.RouteForUploadStart(m[1])
			return b, routeUploadStart, err
		}
	}

	if m := reBlobUploadUUID.FindStringSubmatch(path); m != nil {
		repo, uuid := m[1], m[2]
		if !validOCIPathSegment(repo) || !validOCIPathSegment(uuid) {
			return invalid()
		}
		if method != "PATCH" && method != "PUT" {
			return invalid()
		}
		kind := routeOther
		if method == "PUT" && string(c.QueryArgs().Peek("digest")) != "" {
			kind = routeUploadComplete
		}
		b, err := s.router.RouteForUploadResume(ctx, uuid)
		return b, kind, err
	}

	if m := reBlob.FindStringSubmatch(path); m != nil {
		if !validOCIPathSegment(m[1]) {
			return invalid()
		}
		if method != "GET" && method != "HEAD" {
			return invalid()
		}
		b, err := s.router.RouteForBlobPull(ctx, m[1], m[2])
		return b, routeOther, err
	}

	return invalid()
}

// routeLeastCharged picks any healthy backend (least-charged) for non-routed
// OCI endpoints (ping, tags list, catalog). RouteForPush implements the
// least-charged strategy on the router.
func (s *Server) routeLeastCharged() (domain.RegistryBackend, error) {
	return s.router.RouteForPush("")
}

// writeCacheRouteError maps routing errors to HTTP responses.
func (s *Server) writeCacheRouteError(c *app.RequestContext, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidOCIPath):
		writeError(c, consts.StatusBadRequest, "invalid OCI request path")
	case errors.Is(err, service.ErrNoBackend):
		writeError(c, consts.StatusServiceUnavailable, "no cache backend available")
	case errors.Is(err, service.ErrRouteNotFound):
		writeError(c, consts.StatusNotFound, "cache route not found")
	default:
		s.logger.WithError(err).Error("cache route error")
		writeError(c, consts.StatusBadGateway, "cache backend unreachable")
	}
}

// recordCacheRoute runs after ServeHTTP and best-effort records routing-table
// rows for successful pushes. Recording failures never panic the handler.
func (s *Server) recordCacheRoute(ctx context.Context, c *app.RequestContext, backend domain.RegistryBackend, kind routeKind) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.WithField("panic", r).Warn("cache route recording panicked")
		}
	}()

	status := c.Response.StatusCode()
	path := string(c.Path())
	method := string(c.Method())

	switch kind {
	case routeManifest:
		if method != "PUT" || (status != consts.StatusCreated && status != consts.StatusAccepted) {
			return
		}
		m := reManifest.FindStringSubmatch(path)
		if m == nil {
			return
		}
		digest := string(c.Response.Header.Peek("Docker-Content-Digest"))
		if !validDigest(digest) {
			digest = ""
		}
		if err := s.router.RecordManifest(ctx, m[1], m[2], digest, backend.ID, 0); err != nil {
			s.logger.WithError(err).Warn("record manifest route failed")
		}
	case routeUploadStart:
		if method != "POST" || status != consts.StatusAccepted {
			return
		}
		m := reBlobUpload.FindStringSubmatch(path)
		if m == nil {
			return
		}
		uuid := uploadUUIDFromResponse(c)
		if uuid == "" {
			return
		}
		if err := s.router.RecordUploadSession(ctx, uuid, m[1], backend.ID); err != nil {
			s.logger.WithError(err).Warn("record upload session failed")
		}
	case routeUploadComplete:
		if method != "PUT" || status != consts.StatusCreated {
			return
		}
		m := reBlobUploadUUID.FindStringSubmatch(path)
		if m == nil {
			return
		}
		digest := string(c.QueryArgs().Peek("digest"))
		if digest == "" {
			return
		}
		// Validate the digest shape before persisting it in the routing
		// table (the manifest path already does this). A compliant registry
		// only accepts sha256:<hex>, but defense-in-depth keeps a malicious
		// or buggy backend from polluting the table with an arbitrary
		// string that could later be matched by a different code path
		// (CWE-20/CWE-918).
		if !validDigest(digest) {
			s.logger.WithField("digest", digest).Warn("upload complete: ignoring malformed digest")
			return
		}
		if err := s.router.CompleteUpload(ctx, m[2], digest, backend.ID); err != nil {
			s.logger.WithError(err).Warn("complete upload failed")
		}
	}
}

// uploadUUIDFromResponse extracts the blob-upload UUID from the (already
// rewritten) Location header, or the Docker-Upload-UUID response header.
func uploadUUIDFromResponse(c *app.RequestContext) string {
	loc := c.Response.Header.Get("Location")
	if loc != "" {
		if u, err := url.Parse(loc); err == nil {
			if m := reBlobUploadUUID.FindStringSubmatch(u.Path); m != nil {
				return m[2]
			}
		}
	}
	return c.Response.Header.Get("Docker-Upload-UUID")
}

// cacheProxyDirector rewrites the outbound request: strips the client's
// supervisor token, injects per-backend Basic auth, and retargets the host to
// the chosen backend (read from internal headers stashed by serveCacheHost).
func (s *Server) cacheProxyDirector() func(*protocol.Request) {
	return func(req *protocol.Request) {
		// Never forward the engine's supervisor token to the backend.
		req.Header.Del("Authorization")

		target := string(req.Header.Peek("X-Dagger-Cache-Target"))
		user := string(req.Header.Peek("X-Dagger-Cache-User"))
		pass := string(req.Header.Peek("X-Dagger-Cache-Pass"))
		req.Header.Del("X-Dagger-Cache-Target")
		req.Header.Del("X-Dagger-Cache-User")
		req.Header.Del("X-Dagger-Cache-Pass")

		if target == "" {
			return // leave default target; errorHandler will surface 502
		}
		// Defense-in-depth (CWE-918): the target is stashed by serveCacheHost
		// from validated config, but never trust a header on its own. Only
		// dial a host that is one of the configured backends; otherwise leave
		// the default target so the error handler surfaces a 502 instead of
		// reaching an attacker-influenced address.
		if !s.isConfiguredCacheTarget(target) {
			return
		}
		if u, err := url.Parse(fmt.Sprintf("http://%s", target)); err == nil {
			req.URI().SetScheme(u.Scheme)
			req.URI().SetHost(u.Host)
			req.Header.SetHostBytes([]byte(u.Host))
		}
		if user != "" || pass != "" {
			req.Header.Set("Authorization", basicAuthHeader(user, pass))
		}
	}
}

// isConfiguredCacheTarget reports whether addr matches the InternalAddr of one
// of the configured cache backends. Used by the proxy director to refuse to
// dial anything not present in validated config (CWE-918 defense-in-depth).
func (s *Server) isConfiguredCacheTarget(addr string) bool {
	if s.router == nil {
		return false
	}
	for _, b := range s.router.Backends() {
		if b.InternalAddr == addr {
			return true
		}
	}
	return false
}

// basicAuthHeader renders an RFC 7617 Basic Authorization value. Never log it.
func basicAuthHeader(user, pass string) string {
	return fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", user, pass))))
}

// cacheProxyModifyResponse rewrites upload-start Location headers back to the
// Supervisor cache vhost and suppresses backend WWW-Authenticate challenges.
func (s *Server) cacheProxyModifyResponse() func(*protocol.Response) error {
	return func(resp *protocol.Response) error {
		if loc := resp.Header.Get("Location"); loc != "" && isOCIUploadLocation(loc) {
			resp.Header.Set("Location", s.rewriteUploadLocation(loc))
		}
		if resp.StatusCode() == consts.StatusUnauthorized {
			resp.Header.Del("WWW-Authenticate")
			return errBackendAuth
		}
		return nil
	}
}

// isOCIUploadLocation reports whether loc is a blob-upload Location
// (path contains /blobs/uploads/).
func isOCIUploadLocation(loc string) bool {
	return strings.Contains(loc, "/blobs/uploads/")
}

// cacheScheme returns the scheme used to rewrite backend upload Locations back
// to the Supervisor cache vhost. Only http/https are accepted; anything else
// (including empty) falls back to https.
func (s *Server) cacheScheme() string {
	switch s.cfg.CacheScheme {
	case "http", "https":
		return s.cfg.CacheScheme
	default:
		return "https"
	}
}

// rewriteUploadLocation rebuilds a backend upload Location as
// <scheme>://<cacheHost><path>?<query>, preserving the query string. The
// scheme comes from config (cacheScheme, default https). On parse failure or
// when no cache vhost is configured it returns the original (pass-through).
func (s *Server) rewriteUploadLocation(loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if s.cfg.CacheHost == "" {
		return loc
	}
	path := u.Path
	if u.RawQuery != "" {
		path = fmt.Sprintf("%s?%s", path, u.RawQuery)
	}
	return fmt.Sprintf("%s://%s%s", s.cacheScheme(), s.cfg.CacheHost, path)
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
	body, err := readBoundedBody(c, maxControlBody)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(c, consts.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(c, consts.StatusBadRequest, "invalid body")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(c, consts.StatusBadRequest, "invalid request")
		return false
	}
	return true
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
