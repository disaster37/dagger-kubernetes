package api

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/disaster/dagger-kubernetes/internal/ca"
	"github.com/disaster/dagger-kubernetes/internal/cache"
	"github.com/disaster/dagger-kubernetes/internal/fleet"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/session"
	"github.com/disaster/dagger-kubernetes/internal/telemetry"
	"github.com/disaster/dagger-kubernetes/internal/version"
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
	logger          *zap.Logger
	mintingCA       *ca.MintingCA
	fleetManager    *fleet.Manager
	sessions        *session.Store
	cacheBackend    *cache.Backend
	versionResolver *version.Resolver
	liveHub         *telemetry.LiveHub
	httpServer      *http.Server
	tlsListener     net.Listener
}

type ServerConfig struct {
	ControlAddr  string
	DataAddr     string
	DataHost     string
	PublicURL    string
	UIURL        string
	CollectorURL string
	TempoURL     string
	TokensFile   string
}

func NewServer(
	cfg *ServerConfig,
	logger *zap.Logger,
	mintingCA *ca.MintingCA,
	fleetManager *fleet.Manager,
	sessions *session.Store,
	cacheBackend *cache.Backend,
	versionResolver *version.Resolver,
) *Server {
	return &Server{
		cfg:             cfg,
		logger:          logger,
		mintingCA:       mintingCA,
		fleetManager:    fleetManager,
		sessions:        sessions,
		cacheBackend:    cacheBackend,
		versionResolver: versionResolver,
		liveHub:         telemetry.NewLiveHub(),
	}
}

//nolint:gocritic
func (s *Server) Start(ctx context.Context, tlsCert tls.Certificate) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/engines", s.handleEngines)
	mux.HandleFunc("/v1/traces", s.handleOTelProxy("traces"))
	mux.HandleFunc("/v1/logs", s.handleOTelProxy("logs"))
	mux.HandleFunc("/v1/metrics", s.handleOTelProxy("metrics"))

	mux.HandleFunc("/v1/versions", s.handleAdminVersions)
	mux.HandleFunc("/api/v1/fleet", s.handleFleetInfo)
	mux.HandleFunc("/api/v1/cache", s.handleCacheInfo)
	mux.HandleFunc("/api/v1/traces/", s.handleTracesRoutes)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.Handle("/metrics", promhttp.Handler())

	handler := withMiddleware(mux, s.logger)

	s.httpServer = &http.Server{
		Addr:              s.cfg.ControlAddr,
		ReadHeaderTimeout: 10 * time.Second,
		Handler:           handler,
	}

	go func() {
		s.logger.Info("control plane listening", zap.String("addr", s.cfg.ControlAddr))
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			s.logger.Error("control plane error", zap.Error(err))
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
		s.logger.Info("data plane listening", zap.String("addr", s.cfg.DataAddr))
		for {
			conn, err := s.tlsListener.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					return
				}
				s.logger.Error("tls accept error", zap.Error(err))
				continue
			}
			go s.handleDataConn(conn)
		}
	}()

	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.tlsListener != nil {
		_ = s.tlsListener.Close()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) handleEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token, err := extractToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req EngineRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	s.logger.Info("engine provision request",
		zap.String("token", token),
		zap.String("image", req.Image),
		zap.String("trace_id", req.TraceID),
	)

	engineVersion, err := s.extractVersion(req.Image)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	verStr := engineVersion.String()
	observ.EngineAcquireTotal.WithLabelValues(verStr, "request").Inc()

	if !s.versionResolver.IsAllowed(engineVersion) {
		observ.EngineAcquireTotal.WithLabelValues(verStr, "rejected").Inc()
		writeError(w, http.StatusBadRequest, fmt.Sprintf("version %s not allowed (floor %s)", verStr, s.versionResolver.Floor()))
		return
	}

	start := time.Now()
	result, err := s.fleetManager.Acquire(r.Context(), verStr)
	if err != nil {
		observ.EngineAcquireTotal.WithLabelValues(verStr, "error").Inc()
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	observ.EngineAcquireDuration.WithLabelValues(verStr).Observe(time.Since(start).Seconds())

	clientCert, err := s.mintingCA.MintClientCert(result.PodName)
	if err != nil {
		observ.EngineAcquireTotal.WithLabelValues(verStr, "error").Inc()
		writeError(w, http.StatusInternalServerError, "certificate minting failed")
		return
	}

	instanceID := fmt.Sprintf("%s-%d", result.PodName, time.Now().Unix())
	s.sessions.Register(clientCert.Fingerprint(), verStr, result.PodName, instanceID, req.TraceID)
	observ.ActiveLeases.Inc()
	observ.EngineAcquireTotal.WithLabelValues(verStr, "success").Inc()

	resp := EngineSpecResponse{
		Image:      result.Image,
		URL:        fmt.Sprintf("%s:443", s.cfg.DataHost),
		Cert:       clientCert,
		InstanceID: instanceID,
		Location:   "k8s",
		OrgID:      token,
		UserID:     token,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
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

func (s *Server) handleOTelProxy(signal string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		token, err := extractToken(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		_ = token

		observ.OTelIngestTotal.WithLabelValues(signal, "request").Inc()

		target, err := url.Parse(s.cfg.CollectorURL)
		if err != nil {
			observ.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
			s.logger.Error("invalid collector url", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "collector misconfigured")
			return
		}

		r.URL.Path = fmt.Sprintf("/v1/%s", signal)
		r.Host = target.Host

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			observ.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
			s.logger.Error("otel proxy error", zap.Error(err))
		}
		proxy.ServeHTTP(w, r)

		observ.OTelIngestTotal.WithLabelValues(signal, "success").Inc()
	}
}

func (s *Server) handleAdminVersions(w http.ResponseWriter, r *http.Request) {
	versions := s.versionResolver.AllReleases()
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.String()
	}
	writeJSON(w, out)
}

func (s *Server) handleFleetInfo(w http.ResponseWriter, r *http.Request) {
	infos, err := s.fleetManager.AllFleetInfo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, infos)
}

func (s *Server) handleCacheInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
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
		s.logger.Error("lease not found", zap.String("fp", fp), zap.Error(err))
		return
	}

	_ = s.sessions.IncInFlight(fp)
	defer func() { _ = s.sessions.DecInFlight(fp) }()

	podIP, err := s.fleetManager.GetVersionFleet(lease.Version)
	if err != nil {
		s.logger.Error("get version fleet failed", zap.Error(err))
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
		s.logger.Error("target pod not found", zap.String("pod", lease.ReplicaPod))
		return
	}

	backend, err := net.DialTimeout("tcp", fmt.Sprintf("%s:9999", targetIP), 5*time.Second)
	if err != nil {
		s.logger.Error("backend dial failed", zap.String("ip", targetIP), zap.Error(err))
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

func extractToken(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", fmt.Errorf("missing authorization")
	}

	if strings.HasPrefix(auth, "Basic ") {
		payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err != nil {
			return "", err
		}
		parts := strings.SplitN(string(payload), ":", 2)
		return parts[0], nil
	}

	return "", fmt.Errorf("unsupported auth scheme")
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Message: message})
}
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func withMiddleware(next http.Handler, logger *zap.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		logger.Info("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
		)

		rr := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rr, r)

		logger.Info("response",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Int("status", rr.status),
			zap.Duration("duration", time.Since(start)),
		)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (rr *responseRecorder) WriteHeader(status int) {
	rr.status = status
	rr.ResponseWriter.WriteHeader(status)
}
