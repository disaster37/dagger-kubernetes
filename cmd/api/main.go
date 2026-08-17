package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	corev1 "k8s.io/api/core/v1"
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
		Usage: "dagger-cache control plane",
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
	}).Info("dagger-cache supervisor starting")

	tlsProvider, err := selectTLSProvider(cfg)
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
		PublicHost: cfg.Cache.PublicHost,
		S3:         domain.S3Ref{Bucket: cfg.Cache.S3.Bucket, Region: cfg.Cache.S3.Region},
	}

	metrics := observ.NewMetrics(prometheus.DefaultRegisterer)

	// --- Database + multi-user RBAC wiring ---
	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()

	db, err := repository.OpenSQLite(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := repository.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	metaStore := repository.NewMetaStore(db)
	jwtSecret, err := loadOrCreateJWTSecret(ctx, metaStore, cfg.Auth.JWT.Secret, logger)
	if err != nil {
		return fmt.Errorf("load jwt secret: %w", err)
	}
	jwtSvc := service.NewJWTService(jwtSecret, cfg.Auth.JWT.AccessTTL, cfg.Auth.JWT.RefreshTTL)

	userRepo := repository.NewUserRepo(db)
	groupRepo := repository.NewGroupRepo(db)
	projectRepo := repository.NewProjectRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	traceMetaRepo := repository.NewTraceMetaRepo(db)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger)

	// Legacy flat-file validator (nil when no tokens_file configured).
	var legacyValidator domain.TokenValidator
	if cfg.Auth.Internal.TokensFile != "" {
		legacyValidator = service.NewTokenValidator(cfg.Auth.Internal.TokensFile, cfg.Auth.Internal.Enabled, logger)
	}

	authSvc := service.NewAuthService(service.AuthServiceConfig{
		Disabled: !cfg.Auth.Internal.Enabled,
	}, usersSvc, groupRepo, tokensSvc, jwtSvc, legacyValidator, logger)

	// Bootstrap admin (idempotent: only when user count is 0).
	if err := bootstrapAdmin(ctx, cfg, usersSvc, logger); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}

	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)

	var oauthSvc *service.GitHubOAuthService
	if cfg.Auth.OAuth.Enabled {
		oauthSvc = service.NewGitHubOAuthService(&cfg.Auth.OAuth, usersSvc, groupRepo, jwtSvc, logger)
	}

	// --- Fleet + telemetry wiring ---
	provider, err := createProvider(cfg, logger)
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

	registryHost := cfg.Cache.InternalAddr
	if registryHost == "" {
		registryHost = registryHostFrom(cfg.Cache.Registry)
	}
	var registryClient *repository.RegistryStatsClient
	if cfg.Cache.Backend == "registry" && registryHost != "" {
		registryClient = repository.NewRegistryStatsClient(registryHost)
	}

	cacheStatsSvc := service.NewCacheStatsService(cacheBackend, registryClient, metricsClient, provider, cfg.Cache.GC, logger, metrics)
	statusSvc := service.NewStatusService(cfg, cacheBackend, registryClient, fleetManager, logger)

	server := handler.NewServer(&handler.ServerConfig{
		ControlAddr:  cfg.Server.ControlAddr,
		DataAddr:     cfg.Server.DataAddr,
		DataHost:     cfg.Server.DataHost,
		CacheHost:    cfg.Cache.PublicHost,
		InternalReg:  cfg.Cache.InternalAddr,
		CollectorURL: cfg.Telemetry.CollectorURL,
		VictoriaURL:  cfg.Telemetry.VictoriaURL,
		CertPath:     controlTLSCertPath,
		KeyPath:      controlTLSKeyPath,
	}, &handler.Deps{
		Logger:             logger,
		Metrics:            metrics,
		MintingCA:          serverMintingCA,
		FleetManager:       fleetManager,
		Sessions:           sessions,
		CacheBackend:       cacheBackend,
		VersionResolver:    versionResolver,
		Auth:               authSvc,
		AuthDisabled:       !cfg.Auth.Internal.Enabled,
		Users:              usersSvc,
		Groups:             groupsSvc,
		Projects:           projectsSvc,
		Tokens:             tokensSvc,
		Quota:              quotaSvc,
		Attribution:        attributionSvc,
		TraceMeta:          traceMetaRepo,
		Traces:             traces,
		Logs:               logsClient,
		OAuth:              oauthSvc,
		JWT:                jwtSvc,
		CacheStatsProvider: cacheStatsSvc,
		CachePurger:        cacheStatsSvc,
		StatusProvider:     statusSvc,
	})

	if err := server.Start(ctx, serverTLS); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	stopGC := cacheStatsSvc.StartGCSweeper(ctx)
	defer stopGC()

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
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.WithField("signal", sig.String()).Info("received signal, shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.WithError(err).Error("shutdown error")
	}

	logger.Info("supervisor stopped")
	return nil
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
	const key = "jwt_secret"
	if existing, err := ms.Get(ctx, key); err == nil {
		return []byte(existing), nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("get jwt secret: %w", err)
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate jwt secret: %w", err)
	}
	secret := hex.EncodeToString(b)
	if err := ms.Set(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("persist jwt secret: %w", err)
	}
	logger.Info("generated and persisted JWT secret")
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

	tokensFile := c.String("tokens-file")
	if tokensFile == "" {
		tokensFile = cfg.Auth.Internal.TokensFile
	}
	if tokensFile == "" {
		return fmt.Errorf("no tokens file configured (set --tokens-file or auth.internal.tokens_file)")
	}

	ctx := c.Context
	db, err := repository.OpenSQLite(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := repository.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	userRepo := repository.NewUserRepo(db)
	groupRepo := repository.NewGroupRepo(db)
	tokenRepo := repository.NewTokenRepo(db)
	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)

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

func selectTLSProvider(cfg *domain.Config) (domain.CAProvider, error) {
	switch cfg.TLS.Provider {
	case "embedded":
		return repository.NewEmbeddedProvider(cfg.TLS.CAPath, cfg.CA.ClientCertTTL, cfg.Server.DataHost), nil
	case "cert-manager":
		return repository.NewCertManagerProvider(cfg.TLS.CertPath, cfg.TLS.KeyPath, cfg.TLS.CAPath), nil
	case "external":
		return repository.NewExternalProvider(cfg.TLS.CertPath, cfg.TLS.KeyPath, cfg.TLS.CAPath), nil
	default:
		return nil, fmt.Errorf("unknown TLS provider: %s", cfg.TLS.Provider)
	}
}

func createProvider(cfg *domain.Config, logger *logrus.Logger) (domain.FleetProvider, error) {
	if err := validateFleetEnv(&cfg.Fleet); err != nil {
		return nil, err
	}
	clientset, err := newK8sClientset()
	if err != nil {
		logger.WithError(err).WithField("fleet_provider", "stub").Error(
			"k8s clientset unavailable; falling back to in-memory stub provider — " +
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
	// DAGGER_CACHE_TOKEN is always injected from a secret; SSL_CERT_FILE and
	// NODE_EXTRA_CA_CERTS are injected when CA injection is enabled.
	reserved := map[string]bool{"DAGGER_CACHE_TOKEN": true}
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
