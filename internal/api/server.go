package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/adaptor"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/hertz-contrib/reverseproxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/auth"
	"github.com/disaster/dagger-kubernetes/internal/ca"
	"github.com/disaster/dagger-kubernetes/internal/cache"
	"github.com/disaster/dagger-kubernetes/internal/fleet"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/session"
	"github.com/disaster/dagger-kubernetes/internal/telemetry"
	"github.com/disaster/dagger-kubernetes/internal/version"
)

const (
	maxRequestBodyBytes = 1 << 20 // 1 MiB cap on /v1/engines request bodies (B3).

	otelSignalKey = "otel_signal" // per-request OTel signal label (B1).
	otelErrorKey  = "otel_error"  // set when the OTel proxy hit a transport error (B1).
)

type EngineRequest struct {
	Image                string `json:"image"`
	Module               string `json:"module"`
	Function             string `json:"function"`
	ExecCmd              string `json:"exec_cmd"`
	ClientID             string `json:"client_id"`
	MinimumEngineVersion string `json:"minimum_engine_version"`
	TraceID              string `json:"trace_id"`
}

type EngineSpecResponse struct {
	Image      string                      `json:"image"`
	URL        string                      `json:"url"`
	Cert       *ca.SerializableCertificate `json:"cert"`
	InstanceID string                      `json:"instance_id"`
	Location   string                      `json:"location"`
	OrgID      string                      `json:"org_id,omitempty"`
	UserID     string                      `json:"user_id,omitempty"`
}

type ErrorResponse struct {
	Message string `json:"message"`
}

type Server struct {
	cfg             *ServerConfig
	logger          *logrus.Logger
	metrics         *observ.Metrics
	mintingCA       *ca.MintingCA
	fleetManager    *fleet.Manager
	sessions        *session.Store
	cacheBackend    *cache.Backend
	versionResolver *version.Resolver
	liveHub         *telemetry.LiveHub
	tokenValidator  *auth.TokenValidator
	hertz           *server.Hertz
	tlsListener     net.Listener

	otelProxy     *reverseproxy.ReverseProxy
	victoriaProxy *reverseproxy.ReverseProxy
	cacheProxy    *reverseproxy.ReverseProxy
}

type ServerConfig struct {
	ControlAddr  string
	DataAddr     string
	DataHost     string
	PublicURL    string
	CacheHost    string
	InternalReg  string
	CollectorURL string
	TempoURL     string
	LokiURL      string
	VictoriaURL  string
	TokensFile   string
}

func NewServer(
	cfg *ServerConfig,
	logger *logrus.Logger,
	metrics *observ.Metrics,
	mintingCA *ca.MintingCA,
	fleetManager *fleet.Manager,
	sessions *session.Store,
	cacheBackend *cache.Backend,
	versionResolver *version.Resolver,
	tokenValidator *auth.TokenValidator,
) *Server {
	return &Server{
		cfg:             cfg,
		logger:          logger,
		metrics:         metrics,
		mintingCA:       mintingCA,
		fleetManager:    fleetManager,
		sessions:        sessions,
		cacheBackend:    cacheBackend,
		versionResolver: versionResolver,
		liveHub:         telemetry.NewLiveHub(),
		tokenValidator:  tokenValidator,
	}
}

//nolint:gocritic
func (s *Server) Start(ctx context.Context, tlsCert tls.Certificate) error {
	s.hertz = s.configure()

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

	tlsLn, err := tls.Listen("tcp", s.cfg.DataAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("tls listen: %w", err)
	}

	s.tlsListener = tlsLn

	go func() {
		s.logger.WithField("addr", s.cfg.DataAddr).Info("data plane listening")
		for {
			conn, err := s.tlsListener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				s.logger.WithError(err).Error("tls accept error")
				continue
			}
			go s.handleDataConn(conn)
		}
	}()

	return nil
}

// configure builds the Hertz engine with all routes and middleware registered
// but does not run it. Split out so tests can drive routes via ut.PerformRequest
// without binding a port.
func (s *Server) configure() *server.Hertz {
	s.buildProxies()

	h := server.Default(
		server.WithHostPorts(s.cfg.ControlAddr),
		server.WithReadTimeout(10*time.Second),
	)

	h.Use(s.requestLog())

	if s.cacheProxy != nil {
		h.Use(s.cacheHostMiddleware())
	}

	h.POST("/v1/engines", s.handleEngines)
	h.POST("/v1/traces", s.handleOTel("traces"))
	h.POST("/v1/logs", s.handleOTel("logs"))
	h.POST("/v1/metrics", s.handleOTel("metrics"))

	h.GET("/v1/versions", s.handleAdminVersions)
	h.GET("/api/v1/fleet", s.handleFleetInfo)
	h.GET("/api/v1/cache", s.handleCacheInfo)

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

	return h
}

// buildProxies constructs the reverse proxies once at startup (B6) instead of
// per request.
func (s *Server) buildProxies() {
	if s.cfg.CollectorURL != "" {
		p := s.newHertzProxy(s.cfg.CollectorURL, nil, "collector")
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

	if s.cfg.VictoriaURL != "" {
		target, err := url.Parse(s.cfg.VictoriaURL)
		if err != nil {
			s.logger.WithError(err).Error("invalid victoria url")
		} else {
			p := s.newHertzProxy(s.cfg.VictoriaURL, func(req *protocol.Request) {
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

	if s.cfg.CacheHost != "" && s.cfg.InternalReg != "" {
		target := fmt.Sprintf("http://%s", s.cfg.InternalReg)
		p := s.newHertzProxy(target, nil, "cache")
		if p != nil {
			p.SetErrorHandler(func(c *app.RequestContext, err error) {
				s.logger.WithError(err).Error("cache proxy error")
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

func (s *Server) Shutdown(ctx context.Context) error {
	if s.tlsListener != nil {
		_ = s.tlsListener.Close()
	}
	if s.hertz != nil {
		return s.hertz.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleHealthz(_ context.Context, c *app.RequestContext) {
	c.SetStatusCode(consts.StatusOK)
	_, _ = c.WriteString("ok")
}

func (s *Server) handleReadyz(_ context.Context, c *app.RequestContext) {
	c.SetStatusCode(consts.StatusOK)
	_, _ = c.WriteString("ready")
}

func (s *Server) handleEngines(ctx context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := c.Body()
	if err != nil {
		writeError(c, consts.StatusBadRequest, "invalid body")
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeError(c, consts.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req EngineRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c, consts.StatusBadRequest, "invalid request")
		return
	}

	s.logger.WithFields(logrus.Fields{
		"image":    req.Image,
		"trace_id": req.TraceID,
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
	s.sessions.Register(clientCert.Fingerprint(), verStr, result.PodName, instanceID, req.TraceID)
	s.metrics.ActiveLeases.Inc()
	s.metrics.EngineAcquireTotal.WithLabelValues(verStr, "success").Inc()

	resp := EngineSpecResponse{
		Image:      result.Image,
		URL:        fmt.Sprintf("%s:443", s.cfg.DataHost),
		Cert:       clientCert,
		InstanceID: instanceID,
		Location:   "k8s",
		OrgID:      "",
		UserID:     "",
	}

	c.JSON(consts.StatusCreated, resp)
}

func (s *Server) extractVersion(image string) (*version.Version, error) {
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

func (s *Server) handleOTel(signal string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
			writeError(c, consts.StatusUnauthorized, "unauthorized")
			return
		}

		if s.otelProxy == nil {
			s.metrics.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
			writeError(c, consts.StatusInternalServerError, "collector misconfigured")
			return
		}

		s.metrics.OTelIngestTotal.WithLabelValues(signal, "request").Inc()
		c.Set(otelSignalKey, signal)

		s.otelProxy.ServeHTTP(ctx, c)

		// Only count success when the error handler did not fire (B1).
		if _, errored := c.Get(otelErrorKey); !errored {
			s.metrics.OTelIngestTotal.WithLabelValues(signal, "success").Inc()
		}
	}
}

func (s *Server) handleAdminVersions(_ context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
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
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	infos, err := s.fleetManager.AllFleetInfo()
	if err != nil {
		writeError(c, consts.StatusInternalServerError, "fleet unavailable")
		return
	}
	writeJSON(c, infos)
}

func (s *Server) handleCacheInfo(_ context.Context, c *app.RequestContext) {
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}

	writeJSON(c, map[string]string{
		"backend":  s.cacheBackend.Type,
		"registry": s.cacheBackend.Registry,
	})
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

	podIP, err := s.fleetManager.GetVersionFleet(lease.Version)
	if err != nil {
		s.logger.WithError(err).Error("get version fleet failed")
		return
	}

	var targetIP string
	for _, r := range podIP.Ordinals {
		if r.Name == lease.ReplicaPod {
			targetIP = r.PodIP
			break
		}
	}

	if targetIP == "" {
		s.logger.WithField("pod", lease.ReplicaPod).Error("target pod not found")
		return
	}

	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:9999", targetIP), 5*time.Second)
	if err != nil {
		s.logger.WithField("ip", targetIP).WithError(err).Error("backend dial failed")
		return
	}
	defer func() { _ = backend.Close() }()

	_ = s.sessions.Touch(fp)

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

func (s *Server) handleNoRoute(ctx context.Context, c *app.RequestContext) {
	if s.cacheProxy != nil && strings.EqualFold(string(c.Host()), s.cfg.CacheHost) {
		s.serveCacheHost(ctx, c)
		return
	}
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
	if _, err := s.tokenValidator.ValidateRequest(c); err != nil {
		writeError(c, consts.StatusUnauthorized, "unauthorized")
		return
	}
	s.cacheProxy.ServeHTTP(ctx, c)
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

func writeError(c *app.RequestContext, status int, message string) {
	c.JSON(status, ErrorResponse{Message: message})
}

func writeJSON(c *app.RequestContext, v interface{}) {
	c.JSON(consts.StatusOK, v)
}
