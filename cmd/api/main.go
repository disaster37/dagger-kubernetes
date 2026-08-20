package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/disaster/dagger-kubernetes/config"
	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

func main() {
	app := &cli.App{
		Name:  "supervisor",
		Usage: "dagger-kubernetes control plane",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Value: "config/config.app.yaml",
				Usage: "path to config file",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "migrate-tokens",
				Usage: "import flat-file tokens as users with API tokens",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "config",
						Value: "config/config.app.yaml",
						Usage: "path to config file",
					},
					&cli.StringFlag{
						Name:  "tokens-file",
						Value: "",
						Usage: "override path to the legacy tokens file",
					},
					&cli.BoolFlag{
						Name:  "dry-run",
						Value: false,
						Usage: "report what would be imported without writing",
					},
				},
				Action: runMigrateTokens,
			},
		},
		Action: run,
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func run(c *cli.Context) error {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observ.NewLogger(cfg.LogLevel, cfg.LogFormat)

	logger.WithFields(logrus.Fields{
		"control_addr": cfg.Server.ControlAddr,
		"data_addr":    cfg.Server.DataAddr,
		"public_url":   cfg.Server.PublicURL,
		"tls_provider": cfg.TLS.Provider,
	}).Info("dagger-kubernetes supervisor starting")

	// The pipeline-view base URL is server.public_url. config.Load already
	// validated it as an absolute http(s) URL.
	cacheHost, cacheBackends, err := validateCacheConfig(cfg)
	if err != nil {
		return fmt.Errorf("validate cache config: %w", err)
	}
	if cfg.Cache.Backend == "registry" {
		// Always resolve the public cache vhost so the emitted cache ref points
		// at the Supervisor proxy, never the raw registry.
		cfg.Cache.PublicHost = cacheHost
	}

	// The cache vhost is served on the same listener as the control plane, so
	// its rewritten upload Locations must use the same scheme as server.public_url.
	// Empty (parse failure / no scheme) means the handler falls back to https.
	cacheScheme := ""
	if u, err := url.Parse(cfg.Server.PublicURL); err == nil {
		cacheScheme = u.Scheme
	}

	// The k8s clientset is needed early: it selects the TLS provider
	// (Secret-backed minting CA) and is reused for raft TLS auto-mode and the
	// fleet provider below.
	clientset, err := newK8sClientset()
	if err != nil {
		logger.WithError(err).Warn("k8s clientset unavailable; raft TLS auto-mode, minting CA sharing, and fleet provider will fall back")
	}

	// Resolve per-backend password_secret refs into Password (best-effort;
	// mirrors loadCacheTokenFromSecret). A missing secret leaves Password empty
	// (the backend will 401, which is observable) and never fails startup.
	if err := resolveRegistryBackendSecrets(c.Context, clientset, cfg.Fleet.Namespace, cacheBackends, logger); err != nil {
		logger.WithError(err).Warn("resolve registry backend secrets failed")
	}

	tlsProvider, err := selectTLSProvider(cfg, clientset)
	if err != nil {
		return fmt.Errorf("create TLS provider: %w", err)
	}

	serverMintingCA, err := tlsProvider.MintingCA()
	if err != nil {
		return fmt.Errorf("get minting CA: %w", err)
	}

	serverTLS, err := tlsProvider.ServerTLSCert()
	if err != nil {
		return fmt.Errorf("get server TLS cert: %w", err)
	}

	// Determine control plane TLS cert/key paths based on provider type.
	controlTLSCertPath := cfg.TLS.CertPath
	controlTLSKeyPath := cfg.TLS.KeyPath
	if cfg.TLS.Provider == "embedded" {
		controlTLSCertPath = filepath.Join(cfg.TLS.CAPath, "server.crt")
		controlTLSKeyPath = filepath.Join(cfg.TLS.CAPath, "server.key")
	}

	versionResolver, err := service.NewResolver(cfg.Version.Floor, cfg.Version.Allowlist, nil)
	if err != nil {
		return fmt.Errorf("create version resolver: %w", err)
	}

	sessions := service.NewStore(cfg.LeaseTTL)

	cacheBackend := &service.Cache{
		Type:       cfg.Cache.Backend,
		Registry:   cfg.Cache.Registry,
		PublicHost: cacheHost,
		S3:         domain.S3Ref{Bucket: cfg.Cache.S3.Bucket, Region: cfg.Cache.S3.Region},
	}

	metrics := observ.NewMetrics(prometheus.DefaultRegisterer)

	// --- Database + multi-user RBAC wiring ---
	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()

	raftStore, jwtSecret, tokenEncKey, err := initRaftStore(ctx, cfg, clientset, logger)
	if err != nil {
		return err
	}
	defer func() { _ = raftStore.Close() }()
	jwtSvc := service.NewJWTService(jwtSecret, cfg.Auth.JWT.AccessTTL, cfg.Auth.JWT.RefreshTTL)

	userRepo := repository.NewUserRepo(raftStore)
	groupRepo := repository.NewGroupRepo(raftStore)
	projectRepo := repository.NewProjectRepo(raftStore)
	tokenRepo := repository.NewTokenRepo(raftStore)
	traceMetaRepo := repository.NewTraceMetaRepo(raftStore)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, tokenEncKey)

	// Legacy flat-file validator (nil when no tokens_file configured).
	var legacyValidator domain.TokenValidator
	if cfg.Auth.Internal.Enabled && cfg.Auth.Internal.TokensFile != "" {
		legacyValidator = service.NewTokenValidator(cfg.Auth.Internal.TokensFile, logger)
	}

	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, legacyValidator, logger)

	// Bootstrap admin (idempotent: only when user count is 0).
	if err := bootstrapAdmin(ctx, cfg, usersSvc, logger); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}

	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)

	liveHub := repository.NewLiveHub()
	pipelineLifecycle := service.NewPipelineLifecycle(attributionSvc, traceMetaRepo, sessions, liveHub, cfg.Pipeline, logger, metrics)

	var oauthSvc service.OAuthProvider
	var oauthProvider string
	if cfg.Auth.OAuth.Enabled {
		switch cfg.Auth.OAuth.Provider {
		case "github":
			oauthSvc = service.NewGitHubOAuthService(&cfg.Auth.OAuth, usersSvc, groupRepo, jwtSvc, logger)
			oauthProvider = "github"
		case "oidc":
			oauthSvc = service.NewOIDCOAuthService(&cfg.Auth.OAuth, usersSvc, groupRepo, jwtSvc, logger)
			oauthProvider = "oidc"
		default:
			// validateAuthConfig already rejected this, but fail closed.
			return fmt.Errorf("unsupported oauth provider: %s", cfg.Auth.OAuth.Provider)
		}
	}

	// --- Fleet + telemetry wiring ---
	provider, err := createProvider(cfg, clientset, logger)
	if err != nil {
		return fmt.Errorf("create fleet provider: %w", err)
	}
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: cfg.Fleet.MaxReplicasPerVersion,
		MaxSessionsPerReplica: cfg.Fleet.MaxSessionsPerReplica,
		ReplicaIdleTTL:        cfg.Fleet.ReplicaIdleTTL,
		VersionRetention:      cfg.Fleet.VersionRetention,
		MinReplicasPerVersion: cfg.Fleet.MinReplicasPerVersion,
	}, logger, metrics)

	traces := repository.NewSpanTreeReconstructor(cfg.Telemetry.TempoURL)
	logsClient := repository.NewLogsClient(cfg.Telemetry.LokiURL)

	// --- Cache stats / status / GC wiring ---
	metricsClient := repository.NewMetricsClient(cfg.Telemetry.VictoriaURL)

	var router *service.RegistryRouter
	var routesRepo *repository.CacheRoutesRepo
	if cfg.Cache.Backend == "registry" {
		routesRepo = repository.NewCacheRoutesRepo(raftStore)
		router = service.NewRegistryRouter(cacheBackends, routesRepo, logger)
		if err := router.RefreshCharges(ctx); err != nil {
			logger.WithError(err).Warn("refresh cache charges failed")
		}
	}

	cacheToken := cfg.Cache.AuthToken
	if cacheToken == "" {
		cacheToken = loadCacheTokenFromSecret(ctx, clientset, cfg.Fleet.Namespace, logger)
	}
	if cfg.Cache.Backend == "registry" && cacheToken == "" {
		logger.Warn("cache proxy auth disabled (no cache.auth_token and no engine-registry-auth secret token): dev mode only")
	}

	cacheStatsSvc := service.NewCacheStatsService(cacheBackend, router, metricsClient, provider, cfg.Cache.GC, logger, metrics)
	historyPurgeSvc := service.NewHistoryPurgeService(traceMetaRepo, logsClient, metricsClient, cfg.History.GC, logger, metrics)
	statusSvc := service.NewStatusService(cfg, cacheBackend, router, fleetManager, logger)
	connectSvc := service.NewConnectService(cfg, cacheBackend, versionResolver, tokensSvc, logger)

	server := handler.NewServer(&handler.ServerConfig{
		ControlAddr:  cfg.Server.ControlAddr,
		DataAddr:     cfg.Server.DataAddr,
		DataHost:     cfg.Server.DataHost,
		CacheHost:    cacheHost,
		CacheScheme:  cacheScheme,
		CacheToken:   cacheToken,
		CollectorURL: cfg.Telemetry.CollectorURL,
		VictoriaURL:  cfg.Telemetry.VictoriaURL,
		CertPath:     controlTLSCertPath,
		KeyPath:      controlTLSKeyPath,
		PipelineURL:  cfg.Server.PublicURL,
	}, &handler.Deps{
		Logger:               logger,
		Metrics:              metrics,
		MintingCA:            serverMintingCA,
		FleetManager:         fleetManager,
		Sessions:             sessions,
		CacheBackend:         cacheBackend,
		VersionResolver:      versionResolver,
		Auth:                 authSvc,
		InternalAuthEnabled:  cfg.Auth.Internal.Enabled,
		OAuthCookieSecure:    cfg.Auth.OAuth.CookieSecure,
		CookieCfg:            cfg.Auth.Cookie,
		CORSAllowedOrigins:   cfg.Auth.CORS.AllowedOrigins,
		Users:                usersSvc,
		Groups:               groupsSvc,
		Projects:             projectsSvc,
		Tokens:               tokensSvc,
		Quota:                quotaSvc,
		Attribution:          attributionSvc,
		TraceMeta:            traceMetaRepo,
		Traces:               traces,
		Logs:                 logsClient,
		OAuth:                oauthSvc,
		OAuthProvider:        oauthProvider,
		JWT:                  jwtSvc,
		CacheStatsProvider:   cacheStatsSvc,
		CachePurger:          cacheStatsSvc,
		HistoryStatsProvider: historyPurgeSvc,
		HistoryPurger:        historyPurgeSvc,
		StatusProvider:       statusSvc,
		Connect:              connectSvc,
		Router:               router,
		LiveHub:              liveHub,
		Lifecycle:            pipelineLifecycle,
	})

	if err := server.Start(ctx, serverTLS); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	stopStaleSweep := pipelineLifecycle.StartStaleSweep(ctx)
	defer stopStaleSweep()

	stopGC := cacheStatsSvc.StartGCSweeper(ctx)
	defer stopGC()

	stopHistoryGC := historyPurgeSvc.StartGCSweeper(ctx)
	defer stopHistoryGC()

	sweepTicker := time.NewTicker(30 * time.Second)
	defer sweepTicker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweepTicker.C:
				if err := fleetManager.Sweep(ctx); err != nil {
					logger.WithError(err).Error("sweep error")
				}
				expired := sessions.ReapOrphans()
				if len(expired) > 0 {
					metrics.ActiveLeases.Sub(float64(len(expired)))
				}
				if routesRepo != nil {
					if n, err := routesRepo.ReapUploadSessions(ctx, time.Hour); err != nil {
						logger.WithError(err).Error("reap upload sessions error")
					} else if n > 0 {
						logger.WithField("reaped", n).Debug("reaped stale upload sessions")
					}
				}
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.WithField("signal", sig.String()).Info("received signal, shutting down")
	cancel()

	// Cancel pending disconnect-grace timers before the Raft store closes so
	// late callbacks do not issue applies against a closed store.
	pipelineLifecycle.Stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.WithError(err).Error("shutdown error")
	}

	logger.Info("supervisor stopped")
	return nil
}

// initRaftStore validates the raft config, builds the peer resolver + TLS
// transport, opens the Raft store, waits for a leader, starts the
// leadership/membership goroutines, and resolves the JWT secret +
// token-encryption key (the leader provisions, followers wait for replication —
// ADR-016 D5/D6). The caller owns closing the returned store.
func initRaftStore(ctx context.Context, cfg *domain.Config, clientset kubernetes.Interface, logger *logrus.Logger) (*repository.RaftStore, []byte, []byte, error) {
	if err := validateRaftConfig(cfg, clientset); err != nil {
		return nil, nil, nil, fmt.Errorf("validate raft config: %w", err)
	}

	hostname, _ := os.Hostname()
	discovery := raftDiscoveryConfig(cfg)
	resolver := repository.NewPeerResolver(discovery)
	advertise, err := repository.DeriveAdvertiseAddr(discovery, hostname)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("derive raft advertise addr: %w", err)
	}

	isMultiNode := cfg.Raft.Replicas > 1 || len(cfg.Raft.Peers) > 1
	if isMultiNode && !cfg.Raft.TLS.Enabled {
		logger.Warn("raft multi-node is configured but raft.tls.enabled is false: " +
			"Raft replication traffic — including password hashes, token hashes/ciphertexts, " +
			"the JWT secret, and the token-encryption key — will flow in CLEARTEXT over the " +
			"network (CWE-319/CWE-311). Enable raft.tls.enabled for multi-node in production.")
	}
	if isMultiNode && !cfg.Raft.TLS.ClientAuth {
		logger.Warn("raft multi-node is configured with raft.tls.client_auth=false: peers will not " +
			"verify each other's certificates, so an unauthenticated peer could join the cluster " +
			"(CWE-295). Keep raft.tls.client_auth=true (mTLS) for multi-node in production.")
	}
	if isMultiNode && cfg.TLS.Provider == "embedded" && (clientset == nil || cfg.CA.MintingCASecret == "") && isMintingCAOnPerPodStorage(cfg) {
		logger.Warn("multi-node is configured with the embedded TLS provider but the minting CA " +
			"cannot be shared across pods (no K8s clientset or ca.minting_ca_secret is empty), and " +
			"tls.ca_path is stored under the per-pod database directory. Each pod will mint a " +
			"DISTINCT engine-client CA, so engine mTLS client certs issued by one pod will be " +
			"REJECTED by other pods' data-plane listeners (CWE-295). To fix: run with a K8s " +
			"clientset and ca.minting_ca_secret set (the embedded provider then shares the CA via " +
			"that Secret), mount a shared ReadWriteMany volume at tls.ca_path, or switch " +
			"tls.provider to cert-manager/external with a shared CA.")
	}

	var raftTLS *tls.Config
	if cfg.Raft.TLS.Enabled {
		dnsNames, ipAddrs := repository.PodSANs(discovery, hostname)
		raftTLS, err = buildRaftTLSConfig(cfg, isMultiNode, clientset, dnsNames, ipAddrs, raftNodeCommonName(cfg, resolver, hostname), hostname, logger)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("build raft TLS: %w", err)
		}
	}

	raftStore, err := repository.NewRaftStore(repository.RaftStoreConfig{
		Dir:               cfg.Database.Dir,
		NodeID:            cfg.Raft.NodeID,
		BindAddr:          cfg.Raft.BindAddr,
		AdvertiseAddr:     advertise,
		Resolver:          resolver,
		ApplyTimeout:      cfg.Raft.ApplyTimeout,
		SnapshotThreshold: cfg.Raft.SnapshotThreshold,
		SnapshotInterval:  cfg.Raft.SnapshotInterval,
		TrailingLogs:      cfg.Raft.TrailingLogs,
		TLS:               raftTLS,
	}, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open database: %w", err)
	}

	// Wait until A leader exists (any node, not necessarily this one).
	// Followers serve stale reads and return ErrNotLeader on writes (ADR-016 D6).
	leaderCtx, leaderCancel := context.WithTimeout(ctx, cfg.Raft.LeaderWaitTimeout)
	err = raftStore.WaitForLeader(leaderCtx)
	leaderCancel()
	if err != nil {
		_ = raftStore.Close()
		return nil, nil, nil, fmt.Errorf("wait for raft leader: %w", err)
	}

	go observeLeadership(ctx, raftStore, logger)
	go joinLoop(ctx, raftStore, resolver, logger)

	metaStore := repository.NewMetaStore(raftStore)
	jwtSecret, err := resolveJWTSecret(ctx, raftStore, metaStore, cfg.Auth.JWT.Secret, cfg.Raft.LeaderWaitTimeout, logger)
	if err != nil {
		_ = raftStore.Close()
		return nil, nil, nil, fmt.Errorf("load jwt secret: %w", err)
	}
	tokenEncKey, err := resolveTokenEncryptionKey(ctx, raftStore, metaStore, cfg.Auth.Token.EncryptionKey, cfg.Raft.LeaderWaitTimeout, logger)
	if err != nil {
		_ = raftStore.Close()
		return nil, nil, nil, fmt.Errorf("load token encryption key: %w", err)
	}
	return raftStore, jwtSecret, tokenEncKey, nil
}

// raftNodeCommonName returns the leaf-certificate common name for this node:
// the configured node_id, else the resolver's self ID (the StatefulSet pod
// name), else the hostname.
func raftNodeCommonName(cfg *domain.Config, resolver repository.PeerResolver, hostname string) string {
	if cfg.Raft.NodeID != "" {
		return cfg.Raft.NodeID
	}
	if self, err := resolver.Self(); err == nil && self.ID != "" {
		return self.ID
	}
	return hostname
}

// minJWTSecretLen is the minimum accepted HS256 signing key length. HS256
// security is bounded by the key's entropy; NIST SP 800-131A requires at
// least 112 bits for HMAC, and RFC 7518 mandates a key at least as long as
// the hash output (256 bits) for HS256 (CWE-326).
const minJWTSecretLen = 32

// loadOrCreateJWTSecret returns the configured JWT secret, or generates and
// persists a 32-byte random secret in the DB meta table on first boot.
// Configured secrets shorter than 32 bytes are rejected: a weak key makes
// HS256 tokens forgeable offline, which would bypass authentication entirely.
func loadOrCreateJWTSecret(ctx context.Context, ms *repository.MetaStore, configured string, logger *logrus.Logger) ([]byte, error) {
	if configured != "" {
		if len(configured) < minJWTSecretLen {
			return nil, fmt.Errorf("auth.jwt.secret too short (%d bytes): HS256 requires at least %d bytes", len(configured), minJWTSecretLen)
		}
		return []byte(configured), nil
	}
	return loadOrCreateMetaSecret(ctx, ms, "jwt_secret", "jwt secret", "generated and persisted JWT secret", logger, false)
}

// minTokenEncKeyLen is the minimum accepted AES-256-GCM encryption key length
// (32 bytes = 256 bits). Mirrors the JWT secret rule (minJWTSecretLen).
const minTokenEncKeyLen = 32

// loadOrCreateTokenEncryptionKey returns the configured token encryption key,
// or generates and persists a 32-byte random key in the DB meta table on first
// boot (mirroring loadOrCreateJWTSecret). A configured key shorter than 32
// bytes is rejected.
//
// AES-256-GCM requires a key of exactly 32 bytes, but operators may configure
// secrets of any length >= 32 bytes (and the auto-generated meta value is a
// 64-char hex string), so the raw secret material is always SHA-256-derived
// into a fixed 32-byte AES key rather than used directly.
func loadOrCreateTokenEncryptionKey(ctx context.Context, ms *repository.MetaStore, configured string, logger *logrus.Logger) ([]byte, error) {
	if configured != "" {
		if len(configured) < minTokenEncKeyLen {
			return nil, fmt.Errorf("auth.token.encryption_key too short (%d bytes): requires at least %d bytes", len(configured), minTokenEncKeyLen)
		}
		return deriveAESKey([]byte(configured)), nil
	}
	raw, err := loadOrCreateMetaSecret(ctx, ms, "token_encryption_key", "token encryption key",
		"generated and persisted token encryption key (configure auth.token.encryption_key in production)", logger, true)
	if err != nil {
		return nil, err
	}
	return deriveAESKey(raw), nil
}

// deriveAESKey returns a fixed 32-byte AES-256 key derived from arbitrary
// secret material via SHA-256 (HKDF-style single-step). Deterministic across
// restarts and safe for any input length.
func deriveAESKey(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// loadOrCreateMetaSecret returns the persisted value for metaKey, or generates
// and persists a fresh 32-byte hex secret in the DB meta table on first boot.
// errLabel names the secret in error messages; logMsg is logged (INFO, or WARN
// when warn=true) only when a new secret is generated.
func loadOrCreateMetaSecret(ctx context.Context, ms *repository.MetaStore, metaKey, errLabel, logMsg string, logger *logrus.Logger, warn bool) ([]byte, error) {
	if existing, err := ms.Get(ctx, metaKey); err == nil {
		return []byte(existing), nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("get %s: %w", errLabel, err)
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate %s: %w", errLabel, err)
	}
	secret := hex.EncodeToString(b)
	if err := ms.Set(ctx, metaKey, secret); err != nil {
		return nil, fmt.Errorf("persist %s: %w", errLabel, err)
	}
	if warn {
		logger.Warn(logMsg)
	} else {
		logger.Info(logMsg)
	}
	return []byte(secret), nil
}

// bootstrapAdmin creates the first admin user when the users table is empty.
// When no password is configured, a random 16-byte hex password is generated
// and logged once at WARN (the only place a credential is ever logged).
func bootstrapAdmin(ctx context.Context, cfg *domain.Config, users *service.UserService, logger *logrus.Logger) error {
	count, err := users.Count(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	username := cfg.Auth.BootstrapAdmin.Username
	if username == "" {
		username = "admin"
	}
	password := cfg.Auth.BootstrapAdmin.Password
	generated := false
	if password == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generate bootstrap password: %w", err)
		}
		password = hex.EncodeToString(b)
		generated = true
	}

	if _, err := users.Create(ctx, username, password, domain.RoleAdmin); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}

	fields := logrus.Fields{
		"username":  username,
		"generated": generated,
	}
	if generated {
		// The generated password is unrecoverable; log it exactly once so the
		// operator can log in and rotate it (the only place a credential is
		// ever logged). Configured passwords are never logged.
		fields["password"] = password
	}
	logger.WithFields(fields).Warn("bootstrap admin created")
	return nil
}

// runMigrateTokens is the `supervisor migrate-tokens` CLI subcommand.
func runMigrateTokens(c *cli.Context) error {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := observ.NewLogger(cfg.LogLevel, cfg.LogFormat)

	if err := validateMigrateTokensSingleNode(cfg); err != nil {
		return err
	}

	tokensFile := c.String("tokens-file")
	if tokensFile == "" {
		tokensFile = cfg.Auth.Internal.TokensFile
	}
	if tokensFile == "" {
		return fmt.Errorf("no tokens file configured (set --tokens-file or auth.internal.tokens_file)")
	}

	ctx := c.Context
	raftStore, err := repository.NewRaftStore(repository.RaftStoreConfig{
		Dir:               cfg.Database.Dir,
		NodeID:            cfg.Raft.NodeID,
		BindAddr:          cfg.Raft.BindAddr,
		Peers:             toRaftPeers(cfg.Raft.Peers),
		ApplyTimeout:      cfg.Raft.ApplyTimeout,
		SnapshotThreshold: cfg.Raft.SnapshotThreshold,
		SnapshotInterval:  cfg.Raft.SnapshotInterval,
		TrailingLogs:      cfg.Raft.TrailingLogs,
	}, logger)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = raftStore.Close() }()

	// migrate-tokens must write, so it requires THIS node to be the leader
	// (single-node operation, or scale the cluster to 1 first).
	leaderCtx, leaderCancel := context.WithTimeout(ctx, cfg.Raft.LeaderWaitTimeout)
	if err := raftStore.WaitForSelfLeadership(leaderCtx); err != nil {
		leaderCancel()
		return fmt.Errorf("wait for self leadership: %w", err)
	}
	leaderCancel()

	userRepo := repository.NewUserRepo(raftStore)
	groupRepo := repository.NewGroupRepo(raftStore)
	tokenRepo := repository.NewTokenRepo(raftStore)
	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	metaStore := repository.NewMetaStore(raftStore)
	tokenEncKey, err := loadOrCreateTokenEncryptionKey(ctx, metaStore, cfg.Auth.Token.EncryptionKey, logger)
	if err != nil {
		return fmt.Errorf("load token encryption key: %w", err)
	}
	tokensSvc := service.NewTokenService(tokenRepo, logger, tokenEncKey)

	res, err := service.ImportTokensFile(ctx, tokensFile, usersSvc, tokensSvc, groupsSvc, logger, c.Bool("dry-run"))
	if err != nil {
		return err
	}
	fmt.Printf("migrate-tokens: imported=%d skipped=%d\n", res.Imported, res.Skipped)
	for _, u := range res.Usernames {
		fmt.Printf("  %s\n", u)
	}
	return nil
}

// toRaftPeers converts domain.RaftPeer config entries into repository peers.
func toRaftPeers(peers []domain.RaftPeer) []repository.RaftPeer {
	out := make([]repository.RaftPeer, 0, len(peers))
	for _, p := range peers {
		out = append(out, repository.RaftPeer{ID: p.ID, Address: p.Address})
	}
	return out
}

// validateMigrateTokensSingleNode rejects migrate-tokens against a multi-node
// cluster. migrate-tokens opens its own Raft node and must write; a lone node
// cannot reach quorum against a multi-node cluster (and would risk
// split-brain). Run it against a single-node cluster or via the running
// leader's API instead.
func validateMigrateTokensSingleNode(cfg *domain.Config) error {
	if cfg.Raft.Replicas > 1 || len(cfg.Raft.Peers) > 1 {
		return fmt.Errorf("migrate-tokens must run against a single-node cluster (raft.replicas=1 and raft.peers empty); " +
			"for multi-node, run it via the running leader's API or scale the cluster to 1 first")
	}
	return nil
}

// observeLeadership logs Raft leadership changes until ctx is cancelled.
func observeLeadership(ctx context.Context, store *repository.RaftStore, logger *logrus.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case isLeader, ok := <-store.LeaderCh():
			if !ok {
				return
			}
			logger.WithField("is_leader", isLeader).Info("raft leadership changed")
		}
	}
}

// joinLoop (leader only) periodically reconciles the running raft
// configuration with the resolver's voter list: AddVoter for missing voters
// and RemoveServer for removed voters (scale-up/scale-down, ADR-016 D7).
func joinLoop(ctx context.Context, store *repository.RaftStore, resolver repository.PeerResolver, logger *logrus.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !store.IsLeader() {
				continue
			}
			desired, err := resolver.Resolve()
			if err != nil {
				logger.WithError(err).Warn("joinLoop: resolve peers failed")
				continue
			}
			added, removed, err := store.ReconcileMembership(desired, 10*time.Second)
			if err != nil {
				logger.WithError(err).Warn("joinLoop: reconcile membership failed")
				continue
			}
			if len(added) > 0 || len(removed) > 0 {
				logger.WithFields(logrus.Fields{
					"added":   added,
					"removed": removed,
				}).Info("joinLoop: raft membership reconciled")
			}
		}
	}
}

// waitForMetaSecret polls metaStore.Get(key) every 500ms until a value is
// present or the timeout expires. Reads from the local FSM (stale reads are
// fine here — the value, once replicated, is stable).
func waitForMetaSecret(ctx context.Context, ms *repository.MetaStore, key string, timeout time.Duration) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if v, err := ms.Get(waitCtx, key); err == nil {
			return []byte(v), nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("wait for meta key %s: %w", key, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// resolveJWTSecret returns the JWT secret: the leader provisions it, followers
// wait for the leader's value to replicate, and a configured secret short-
// circuits both (no write needed).
func resolveJWTSecret(ctx context.Context, store *repository.RaftStore, ms *repository.MetaStore, configured string, timeout time.Duration, logger *logrus.Logger) ([]byte, error) {
	if store.IsLeader() || configured != "" {
		return loadOrCreateJWTSecret(ctx, ms, configured, logger)
	}
	return waitForMetaSecret(ctx, ms, "jwt_secret", timeout)
}

// resolveTokenEncryptionKey mirrors resolveJWTSecret for the token-encryption
// key (SHA-256-derived to a fixed 32-byte AES-256 key).
func resolveTokenEncryptionKey(ctx context.Context, store *repository.RaftStore, ms *repository.MetaStore, configured string, timeout time.Duration, logger *logrus.Logger) ([]byte, error) {
	if store.IsLeader() || configured != "" {
		return loadOrCreateTokenEncryptionKey(ctx, ms, configured, logger)
	}
	raw, err := waitForMetaSecret(ctx, ms, "token_encryption_key", timeout)
	if err != nil {
		return nil, err
	}
	return deriveAESKey(raw), nil
}

// raftDiscoveryConfig maps the domain raft config to the repository discovery
// config. The raft port is left unset here; the repository derives it from
// bind_addr (raftDiscoveryConfig's BindAddr) via raftPort.
func raftDiscoveryConfig(cfg *domain.Config) repository.RaftDiscoveryConfig {
	namespace := cfg.Raft.Namespace
	if namespace == "" {
		namespace = cfg.Fleet.Namespace
	}
	return repository.RaftDiscoveryConfig{
		NodeID:          cfg.Raft.NodeID,
		AdvertiseAddr:   cfg.Raft.AdvertiseAddr,
		BindAddr:        cfg.Raft.BindAddr,
		Peers:           toRaftPeers(cfg.Raft.Peers),
		Replicas:        cfg.Raft.Replicas,
		StatefulSetName: cfg.Raft.StatefulSetName,
		HeadlessService: cfg.Raft.HeadlessService,
		Namespace:       namespace,
		ClusterDomain:   cfg.Raft.ClusterDomain,
	}
}

// validateRaftConfig fails fast on config that would break multi-node raft or
// TLS (ADR-016 §5).
func validateRaftConfig(cfg *domain.Config, clientset kubernetes.Interface) error {
	tlsCfg := cfg.Raft.TLS
	if tlsCfg.Enabled {
		manual := tlsCfg.CACertPath != "" || tlsCfg.CertPath != "" || tlsCfg.KeyPath != ""
		if manual && (tlsCfg.CACertPath == "" || tlsCfg.CertPath == "" || tlsCfg.KeyPath == "") {
			return fmt.Errorf("raft.tls: ca_cert/cert/key must all be set together")
		}
		if !manual && cfg.Raft.Replicas > 1 && tlsCfg.CASecret == "" && clientset == nil {
			return fmt.Errorf("raft TLS auto-mode for multi-node requires K8s or manual CA files")
		}
	}
	if cfg.Raft.Replicas > 1 && cfg.Raft.StatefulSetName == "" && len(cfg.Raft.Peers) == 0 {
		return fmt.Errorf("raft multi-node requires statefulset_name or explicit peers")
	}
	if cfg.Raft.AdvertiseAddr != "" && cfg.Raft.Replicas > 1 {
		if host, _, err := net.SplitHostPort(cfg.Raft.AdvertiseAddr); err == nil {
			if host == "127.0.0.1" || host == "0.0.0.0" || host == "::" {
				return fmt.Errorf("raft.advertise_addr must be routable for multi-node")
			}
		}
	}
	return nil
}

// buildRaftTLSConfig builds the raft transport tls.Config from config,
// issuing/reusing this node's leaf certificate (ADR-016 D1/D2/D3).
func buildRaftTLSConfig(cfg *domain.Config, isMultiNode bool, clientset kubernetes.Interface, dnsNames []string, ipAddrs []net.IP, commonName, hostname string, logger *logrus.Logger) (*tls.Config, error) {
	dir := cfg.Raft.TLS.Dir
	if dir == "" {
		dir = filepath.Join(cfg.Database.Dir, "tls")
	}
	namespace := cfg.Raft.Namespace
	if namespace == "" {
		namespace = cfg.Fleet.Namespace
	}
	// The internal CA bootstrap node is ordinal 0 of the StatefulSet. It is
	// auto-detected from the pod hostname (<sts>-0); raft.tls.ca_bootstrap
	// forces it explicitly (ADR-016 §1.2).
	caBootstrap := raftCABootstrap(cfg, hostname)
	_, tlsCfg, err := repository.LoadOrBuildRaftTLS(repository.RaftTLSConfig{
		Enabled:           true,
		Dir:               dir,
		Validity:          cfg.Raft.TLS.Validity,
		Organization:      cfg.Raft.TLS.Organization,
		CACertPath:        cfg.Raft.TLS.CACertPath,
		CertPath:          cfg.Raft.TLS.CertPath,
		KeyPath:           cfg.Raft.TLS.KeyPath,
		CASecret:          cfg.Raft.TLS.CASecret,
		CABootstrap:       caBootstrap,
		ClientAuth:        cfg.Raft.TLS.ClientAuth,
		SecretPollTimeout: cfg.Raft.LeaderWaitTimeout,
	}, isMultiNode, clientset, namespace, dnsNames, ipAddrs, commonName, logger)
	if err != nil {
		return nil, err
	}
	return tlsCfg, nil
}

// raftCABootstrap reports whether this node should generate + share the
// internal raft CA: raft.tls.ca_bootstrap forces it, else ordinal 0 of the
// StatefulSet (pod hostname "<sts>-0") is auto-detected (ADR-016 §1.2).
// A single-node deployment always bootstraps: its pod hostname may not end in
// "-0" (e.g. a plain Deployment), and there is no other node to share with.
func raftCABootstrap(cfg *domain.Config, hostname string) bool {
	if cfg.Raft.TLS.CABootstrap {
		return true
	}
	if cfg.Raft.Replicas <= 1 && len(cfg.Raft.Peers) <= 1 {
		return true
	}
	return strings.HasSuffix(hostname, "-0")
}

func selectTLSProvider(cfg *domain.Config, clientset kubernetes.Interface) (domain.CAProvider, error) {
	// The minting CA is always auto-bootstrapped and shared across pods via
	// ca.minting_ca_secret when a K8s clientset is available (multi-node),
	// regardless of the server-TLS provider below. The provider only decides
	// where the data/control-plane server certificate comes from.
	minting := mintingProvider(cfg, clientset)
	switch cfg.TLS.Provider {
	case "embedded":
		return minting, nil
	case "cert-manager":
		return repository.NewCertManagerProvider(cfg.TLS.CertPath, cfg.TLS.KeyPath, minting), nil
	case "external":
		return repository.NewExternalProvider(cfg.TLS.CertPath, cfg.TLS.KeyPath, minting), nil
	default:
		return nil, fmt.Errorf("unknown TLS provider: %s", cfg.TLS.Provider)
	}
}

// mintingProvider returns the minting-CA provider. It shares the CA across
// pods via ca.minting_ca_secret when a K8s clientset is available, otherwise
// falls back to the local-file behavior (single-node dev/test or a shared RWX
// volume). The minting CA signs short-lived engine client certs and is an
// internal CA — it never needs a public/cert-manager issuer, so it is
// auto-bootstrapped for every server-TLS provider.
func mintingProvider(cfg *domain.Config, clientset kubernetes.Interface) *repository.EmbeddedProvider {
	if clientset != nil && cfg.CA.MintingCASecret != "" {
		namespace := cfg.Raft.Namespace
		if namespace == "" {
			namespace = cfg.Fleet.Namespace
		}
		hostname, _ := os.Hostname()
		return repository.NewEmbeddedProviderWithSecret(
			cfg.TLS.CAPath,
			cfg.CA.ClientCertTTL,
			cfg.CA.MintingCASecret,
			namespace,
			clientset,
			mintingCABootstrap(cfg, hostname),
			cfg.Raft.LeaderWaitTimeout,
			cfg.Server.DataHost,
		)
	}
	return repository.NewEmbeddedProvider(cfg.TLS.CAPath, cfg.CA.ClientCertTTL, cfg.Server.DataHost)
}

// mintingCABootstrap reports whether this node should generate + share the
// minting CA: ordinal 0 of the StatefulSet (pod name "<sts>-0") auto-detected
// from the hostname, mirroring the raft CA bootstrap rule (ADR-016 D3). A
// single-node deployment always bootstraps: its pod hostname may not end in
// "-0" (e.g. a plain Deployment), and there is no other node to share with.
func mintingCABootstrap(cfg *domain.Config, hostname string) bool {
	if cfg.Raft.Replicas <= 1 && len(cfg.Raft.Peers) <= 1 {
		return true
	}
	return strings.HasSuffix(hostname, "-0")
}

// isMintingCAOnPerPodStorage reports whether the embedded minting CA path
// (tls.ca_path) is stored under the per-pod Raft data directory. With the
// StatefulSet conversion, each pod has its own PVC at database.dir, so a
// minting CA under that path is per-pod and breaks engine mTLS trust across
// pods (CWE-295).
func isMintingCAOnPerPodStorage(cfg *domain.Config) bool {
	caPath := cfg.TLS.CAPath
	dbDir := cfg.Database.Dir
	if caPath == "" || dbDir == "" {
		return false
	}
	rel, err := filepath.Rel(dbDir, caPath)
	if err != nil {
		return false
	}
	// caPath is under dbDir (e.g. /var/lib/dagger-kubernetes/ca under /var/lib/dagger-kubernetes).
	return rel != "." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func createProvider(cfg *domain.Config, clientset kubernetes.Interface, logger *logrus.Logger) (domain.FleetProvider, error) {
	if err := validateFleetEnv(&cfg.Fleet); err != nil {
		return nil, err
	}
	if clientset == nil {
		logger.Warn("k8s clientset unavailable; falling back to in-memory stub provider — " +
			"engine fleet will be empty and provisioning will not persist")
		return repository.NewStubProvider(), nil
	}

	k8sCfg := repository.K8sProviderConfig{
		Namespace:           cfg.Fleet.Namespace,
		ImageRegistry:       cfg.Fleet.EngineImageRegistry,
		StorageClass:        cfg.Fleet.EngineStorageClass,
		StorageSize:         cfg.Fleet.EngineStorageSize,
		CPURequest:          cfg.Fleet.EngineCPURequest,
		CPULimit:            cfg.Fleet.EngineCPULimit,
		MemoryRequest:       cfg.Fleet.EngineMemoryRequest,
		MemoryLimit:         cfg.Fleet.EngineMemoryLimit,
		TerminationGraceSec: int64(cfg.Fleet.EngineTerminationGrace),
		NodeSelector:        cfg.Fleet.EngineNodeSelector,
		Tolerations:         parseTolerations(cfg.Fleet.EngineTolerations),
		ExtraArgs:           cfg.Fleet.EngineExtraArgs,
		PullPolicy:          corev1.PullPolicy(cfg.Fleet.EnginePullPolicy),
		Privileged:          cfg.Fleet.EnginePrivileged,
		ExtraEnv:            cfg.Fleet.EngineExtraEnv,
		ExtraEnvFrom:        cfg.Fleet.EngineExtraEnvFrom,
		CASecret:            cfg.Fleet.EngineCASecret,
		CAKey:               cfg.Fleet.EngineCASecretKey,
		Debug:               cfg.Fleet.EngineDebug,
		LogFormat:           cfg.Fleet.EngineLogFormat,
		RegistryMirrors:     cfg.Fleet.EngineRegistryMirrors,
	}

	return repository.NewK8sProvider(clientset, k8sCfg), nil
}

// validateFleetEnv rejects engine env configuration that Kubernetes would
// refuse at StatefulSet admission (duplicate container env names) or that is
// internally inconsistent. Called once at startup (fail fast).
func validateFleetEnv(fleet *domain.FleetConfig) error {
	// DAGGER_KUBERNETES_TOKEN is always injected from a secret; SSL_CERT_FILE and
	// NODE_EXTRA_CA_CERTS are injected when CA injection is enabled.
	reserved := map[string]bool{"DAGGER_KUBERNETES_TOKEN": true}
	if fleet.EngineCASecret != "" {
		reserved["SSL_CERT_FILE"] = true
		reserved["NODE_EXTRA_CA_CERTS"] = true
	}
	for name := range fleet.EngineExtraEnv {
		if err := validateEnvName(name, "engine_extra_env", reserved); err != nil {
			return err
		}
	}
	for name, src := range fleet.EngineExtraEnvFrom {
		if err := validateEnvName(name, "engine_extra_env_from", reserved); err != nil {
			return err
		}
		if _, dup := fleet.EngineExtraEnv[name]; dup {
			return fmt.Errorf("env var %s is set in both fleet.engine_extra_env and fleet.engine_extra_env_from", name)
		}
		if src.SecretName == "" {
			return fmt.Errorf("fleet.engine_extra_env_from.%s: secret_name must not be empty", name)
		}
		if src.Key == "" {
			return fmt.Errorf("fleet.engine_extra_env_from.%s: key must not be empty", name)
		}
	}
	if fleet.EngineCASecret != "" && fleet.EngineCASecretKey == "" {
		return fmt.Errorf("fleet.engine_ca_secret_key must not be empty when fleet.engine_ca_secret is set")
	}
	return nil
}

// validateEnvName rejects empty operator-supplied env var names and names the
// supervisor injects itself. source is the fleet.* config key for errors.
func validateEnvName(name, source string, reserved map[string]bool) error {
	if name == "" {
		return fmt.Errorf("fleet.%s contains an empty env var name", source)
	}
	if reserved[name] {
		return fmt.Errorf("fleet.%s must not set %s: injected by the supervisor", source, name)
	}
	return nil
}

func newK8sClientset() (kubernetes.Interface, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restCfg, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot load in-cluster or kubeconfig: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	return clientset, nil
}

// registryHostFrom strips the repository path from an OCI registry ref
// ("cache.reg/dagger-cache" -> "cache.reg").
func registryHostFrom(registry string) string {
	host, _, ok := strings.Cut(registry, "/")
	if !ok {
		return registry
	}
	return host
}

// cacheAddrRe constrains backend internal_addr to host[:port] with no
// scheme/path (defense against SSRF via config, CWE-918).
var cacheAddrRe = regexp.MustCompile(`^[A-Za-z0-9._:-]+(:[0-9]+)?$`)

// validateCacheConfig resolves the cache vhost and effective backend list, and
// fails fast on configuration that would break the proxy (vhost collision,
// empty backends, duplicate IDs, scheme/path in internal_addr).
func validateCacheConfig(cfg *domain.Config) (string, []domain.RegistryBackend, error) {
	if cfg.Cache.Backend != "registry" {
		return "", nil, nil
	}

	controlHost := hostOf(cfg.Server.PublicURL)
	if controlHost == "" {
		return "", nil, fmt.Errorf("server.public_url must be an absolute URL (scheme://host) so the cache vhost can be derived")
	}
	cacheHost := cfg.Cache.PublicHost
	if cacheHost == "" {
		cacheHost = fmt.Sprintf("cache.%s", controlHost)
	}
	if cacheHost == controlHost {
		return "", nil, fmt.Errorf("cache.public_host (%s) must differ from the control-plane host (%s); set a dedicated cache vhost", cacheHost, controlHost)
	}

	var backends []domain.RegistryBackend
	if len(cfg.Cache.Registries) > 0 {
		seen := make(map[string]bool, len(cfg.Cache.Registries))
		for _, b := range cfg.Cache.Registries {
			if b.ID == "" {
				return "", nil, fmt.Errorf("cache.registries entry with empty id")
			}
			if seen[b.ID] {
				return "", nil, fmt.Errorf("duplicate cache backend id: %s", b.ID)
			}
			seen[b.ID] = true
			if b.InternalAddr == "" {
				return "", nil, fmt.Errorf("cache.registries entry %s: internal_addr must not be empty", b.ID)
			}
			if !cacheAddrRe.MatchString(b.InternalAddr) {
				return "", nil, fmt.Errorf("cache backend internal_addr must be host[:port] (no scheme/path): %s", b.InternalAddr)
			}
			backends = append(backends, b)
		}
	} else {
		addr := cfg.Cache.InternalAddr
		if addr == "" {
			addr = registryHostFrom(cfg.Cache.Registry)
		}
		if addr == "" {
			return "", nil, fmt.Errorf("cache: no backend registry configured")
		}
		if !cacheAddrRe.MatchString(addr) {
			return "", nil, fmt.Errorf("cache backend internal_addr must be host[:port] (no scheme/path): %s", addr)
		}
		backends = []domain.RegistryBackend{{ID: "default", InternalAddr: addr}}
	}

	if len(backends) == 0 {
		return "", nil, fmt.Errorf("cache: no backend registry configured")
	}
	return cacheHost, backends, nil
}

// hostOf strips scheme, port and path from a URL, returning its hostname.
// (An explicit port is dropped so the derived cache vhost matches the
// ingress Host header / TLS SAN, which never carry a port.)
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

// resolveRegistryBackendSecrets fills Password from each backend's
// password_secret ref (mirrors loadCacheTokenFromSecret). A missing
// clientset/secret leaves Password empty (non-K8s deployments must set
// Password directly in config). Resolution is per-backend best-effort: a
// missing/unreadable Secret for one backend logs a WARN and leaves that
// backend's Password empty (the backend will 401, which is observable) so
// the remaining backends still resolve. The returned error aggregates any
// per-backend failures so the caller can surface them; startup never fails.
// Secret values are never logged.
func resolveRegistryBackendSecrets(ctx context.Context, clientset kubernetes.Interface, namespace string, backends []domain.RegistryBackend, logger *logrus.Logger) error {
	var errs []error
	for i := range backends {
		ref := backends[i].PasswordSecret
		if ref == nil || backends[i].Password != "" {
			continue // nothing to resolve, or explicit password wins
		}
		if ref.Name == "" {
			logger.WithFields(logrus.Fields{
				"backend_id":  backends[i].ID,
				"secret_name": ref.Name,
			}).Warn("cache backend password_secret: empty name; skipping")
			continue
		}
		if clientset == nil {
			logger.WithFields(logrus.Fields{
				"backend_id":  backends[i].ID,
				"secret_name": ref.Name,
			}).Warn("cache backend password_secret: k8s clientset unavailable; set cache.registries[].password directly")
			continue
		}
		ns := namespace
		if ns == "" {
			ns = "dagger-kubernetes"
		}
		secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			logger.WithFields(logrus.Fields{
				"backend_id":  backends[i].ID,
				"secret_name": ref.Name,
			}).WithError(err).Warn("cache backend password_secret: read failed; leaving password empty")
			errs = append(errs, fmt.Errorf("read password secret %q for backend %q: %w", ref.Name, backends[i].ID, err))
			continue
		}
		key := ref.Key
		if key == "" {
			key = "password"
		}
		backends[i].Password = string(secret.Data[key])
		if backends[i].Password == "" {
			logger.WithFields(logrus.Fields{
				"backend_id":  backends[i].ID,
				"secret_name": ref.Name,
				"secret_key":  key,
			}).Warn("cache backend password_secret resolved to empty password")
		}
	}
	return errors.Join(errs...)
}

// loadCacheTokenFromSecret reads the engine→Supervisor-proxy bearer token from
// the engine-registry-auth K8s secret. Returns "" (with a WARN) when K8s is
// unavailable or the secret/key is missing.
func loadCacheTokenFromSecret(ctx context.Context, clientset kubernetes.Interface, namespace string, logger *logrus.Logger) string {
	if clientset == nil {
		logger.Warn("cache auth token: k8s clientset unavailable; cannot read engine-registry-auth secret")
		return ""
	}
	if namespace == "" {
		namespace = "dagger-kubernetes"
	}
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "engine-registry-auth", metav1.GetOptions{})
	if err != nil {
		logger.WithError(err).Warn("cache auth token: engine-registry-auth secret unavailable")
		return ""
	}
	token := string(secret.Data["token"])
	if token == "" {
		logger.Warn("cache auth token: engine-registry-auth secret has no token key")
		return ""
	}
	return token
}

// parseTolerations parses tolerations in the key[:value[:effect]] format.
func parseTolerations(raw []string) []corev1.Toleration {
	tols := make([]corev1.Toleration, 0, len(raw))
	for _, rawTol := range raw {
		// SplitN always yields at least the key.
		parts := strings.SplitN(rawTol, ":", 3)
		tol := corev1.Toleration{
			Key:      parts[0],
			Operator: corev1.TolerationOpEqual,
		}
		if len(parts) >= 2 && parts[1] != "" {
			tol.Value = parts[1]
		}
		if len(parts) >= 3 && parts[2] != "" {
			tol.Effect = corev1.TaintEffect(parts[2])
		}
		if tol.Value == "" {
			tol.Operator = corev1.TolerationOpExists
		}
		tols = append(tols, tol)
	}
	return tols
}
