package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	goauthjwt "github.com/disaster37/go-auth/jwt"
	"github.com/disaster37/go-raft/discovery"
	raftk8s "github.com/disaster37/go-raft/k8s"
	rafttransport "github.com/disaster37/go-raft/transport"
	"github.com/disaster37/go-secguard"
	"github.com/minio/minio-go/v7"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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
		"control_addr":           cfg.Server.ControlAddr,
		"data_addr":              cfg.Server.DataAddr,
		"public_url":             cfg.Server.PublicURL,
		"dataplane_tls_provider": cfg.Supervisor.Dataplane.TLS.Provider,
	}).Info("dagger-kubernetes supervisor starting")

	// The pipeline-view base URL is server.public_url. config.Load already
	// validated it as an absolute http(s) URL.

	// The k8s clientset is needed early: it selects the TLS provider
	// (Secret-backed minting CA) and is reused for raft TLS auto-mode and the
	// fleet provider below.
	clientset, err := raftk8s.NewClientset()
	if err != nil {
		logger.WithError(err).Warn("k8s clientset unavailable; raft TLS auto-mode, minting CA sharing, and fleet provider will fall back")
	}

	// --- Minting CA + dynamic server-cert provider ---
	minting := mintingProvider(cfg, clientset)
	sealer, err := repository.DeriveSealer(cfg.Auth.Token.EncryptionKey)
	if err != nil {
		return fmt.Errorf("derive at-rest sealer: %w", err)
	}
	dynamicCA, err := repository.NewDynamicMintingCA(repository.DynamicMintingCAConfig{
		Provider: minting,
		Sealer:   sealer,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("create dynamic minting CA: %w", err)
	}

	tlsProvider, err := selectTLSProvider(cfg, dynamicCA)
	if err != nil {
		return fmt.Errorf("create TLS provider: %w", err)
	}

	serverTLS, err := tlsProvider.ServerTLSCert()
	if err != nil {
		return fmt.Errorf("get server TLS cert: %w", err)
	}

	internalTLS, err := tlsProvider.InternalServerTLSCert()
	if err != nil {
		return fmt.Errorf("get internal server TLS cert: %w", err)
	}

	// Determine control plane TLS cert/key paths based on provider type.
	controlTLSCertPath := cfg.Supervisor.Dataplane.TLS.CertPath
	controlTLSKeyPath := cfg.Supervisor.Dataplane.TLS.KeyPath
	if cfg.Supervisor.Dataplane.TLS.Provider == "embedded" {
		controlTLSCertPath = filepath.Join(cfg.Supervisor.Dataplane.TLS.CAPath, "server.crt")
		controlTLSKeyPath = filepath.Join(cfg.Supervisor.Dataplane.TLS.CAPath, "server.key")
	}

	versionResolver, err := service.NewResolver(cfg.Version.Floor, cfg.Version.Allowlist, nil)
	if err != nil {
		return fmt.Errorf("create version resolver: %w", err)
	}

	sessions := service.NewStore(cfg.LeaseTTL)

	metrics := observ.NewMetrics(prometheus.DefaultRegisterer)

	// --- Database + multi-user RBAC wiring ---
	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()

	const raftInitMaxRetries = 3
	const raftInitRetryDelay = 5 * time.Second

	// The FSM meta hook installs the replicated Minting CA keyring on every
	// pod as it is applied/restored. A successful install also nudges the CA
	// sync loop so the public trust-bundle ConfigMap is refreshed promptly
	// through the phased-rotation terms instead of waiting for the 30s ticker.
	caSyncNotify := make(chan struct{}, 1)
	metaHook := func(key, value string) {
		if key != repository.MintingKeyringMetaKey {
			return
		}
		if err := dynamicCA.SetKeyringJSON(value); err != nil {
			logger.WithError(err).Warn("install minting CA keyring failed")
			return
		}
		select {
		case caSyncNotify <- struct{}{}:
		default:
		}
	}

	var raftStore *repository.RaftStore
	var jwtSecret, tokenEncKey []byte
	for attempt := 0; attempt < raftInitMaxRetries; attempt++ {
		raftStore, jwtSecret, tokenEncKey, err = initRaftStore(ctx, cfg, clientset, sessions, metaHook, logger)
		if err == nil {
			break
		}
		if attempt < raftInitMaxRetries-1 {
			logger.WithError(err).WithField("attempt", attempt+1).Warn("raft initialization failed, retrying")
			select {
			case <-ctx.Done():
				return fmt.Errorf("raft initialization cancelled: %w", ctx.Err())
			case <-time.After(raftInitRetryDelay):
			}
		} else {
			return fmt.Errorf("raft initialization failed after %d attempts: %w", raftInitMaxRetries, err)
		}
	}
	defer func() { _ = raftStore.Close() }()

	// --- Minting CA phased rotation (leader-only) ---
	if cfg.CA.KeyringRotationEnabled {
		rotatorHostname, _ := os.Hostname()
		rotator := repository.NewMintingRotator(&repository.MintingRotatorConfig{
			Store:          raftStore,
			CA:             dynamicCA,
			Bootstrap:      minting.BootstrapProvider(),
			Sealer:         sealer,
			ClientCertTTL:  cfg.CA.ClientCertTTL,
			RotationPeriod: cfg.CA.RotationPeriod,
			CheckInterval:  cfg.CA.RotationCheckInterval,
			CommonName:     "dagger-kubernetes-minting-ca",
			Organization:   "dagger-kubernetes",
			CAValidity:     cfg.CA.CAValidity,
			DNSNames:       mintingExtraSANs(cfg, rotatorHostname),
			Logger:         logger,
		})
		go rotator.Start(ctx)
	}

	// --- Sync settled CAs back to the Kubernetes Secrets/ConfigMap (leader-only) ---
	go syncCASecretLoop(ctx, cfg, clientset, raftStore, dynamicCA, caSyncNotify, logger)

	jwtSvc := service.NewJWTService(jwtSecret, cfg.Auth.JWT.AccessTTL, cfg.Auth.JWT.RefreshTTL)

	userRepo := repository.NewUserRepo(raftStore)
	groupRepo := repository.NewGroupRepo(raftStore)
	projectRepo := repository.NewProjectRepo(raftStore)
	orgRepo := repository.NewOrgRepo(raftStore)
	tokenRepo := repository.NewTokenRepo(raftStore)
	traceMetaRepo := repository.NewTraceMetaRepo(raftStore)
	checkRunRepo := repository.NewCheckRunRepo(raftStore)
	cloudGraphQL := repository.NewCloudGraphQL(userRepo, orgRepo, groupRepo, traceMetaRepo, checkRunRepo, cfg.GraphQL)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, tokenEncKey)
	orgMetaStore := repository.NewMetaStore(raftStore)
	orgsSvc := service.NewOrgService(orgRepo, groupRepo, tokensSvc, logger).
		WithMetaStore(orgMetaStore).
		WithFleetIsolation(cfg.Fleet.Isolation).
		WithDefaultQuotas(cfg.Org.DefaultQuotas)
	if _, err := orgsSvc.EnsureBootstrap(ctx, cfg.Org.Default.DisplayName); err != nil && !errors.Is(err, domain.ErrNotLeader) {
		return fmt.Errorf("bootstrap organization: %w", err)
	}
	cloudGraphQL.WithOrgCreator(func(ctx context.Context, name, display string) (*domain.Org, error) {
		return orgsSvc.Create(ctx, service.OrgInput{Name: name, DisplayName: display})
	})
	ciOIDCSvc := service.NewCIOIDCService(map[string]string{
		"github": cfg.Checks.GitHubOIDCIssuer,
		"gitlab": cfg.Checks.GitLabOIDCIssuer,
	}, cfg.Checks.OIDCAudience, orgsSvc, projectsSvc, logger)

	secretRepo := repository.NewSecretRepo(raftStore)
	forgeSourceRepo := repository.NewForgeSourceRepo(raftStore)
	githubConnRepo := repository.NewGitHubConnectionRepo(raftStore)
	githubAccountRepo := repository.NewGitHubAccountSourceRepo(raftStore)
	repoSettingRepo := repository.NewRepoSettingRepo(raftStore)
	githubOAuthStateRepo := repository.NewGitHubOAuthStateRepo(raftStore)
	secretSvc := service.NewSecretService(secretRepo, tokenEncKey, logger)
	githubForge := repository.NewGitHubForge(cfg.Forge.GitHubAPIURL)
	forgeProviders := []domain.ForgeProvider{
		githubForge,
		repository.NewGitLabForge(cfg.Forge.GitLabAPIURL),
		repository.NewGiteaForge(cfg.Forge.GiteaAPIURL),
	}
	forgeSvc := service.NewForgeService(forgeSourceRepo, secretSvc, forgeProviders, cfg.Server.PublicURL, logger).
		WithGitHubSync(githubAccountRepo, repoSettingRepo)
	githubClient, err := repository.NewGitHubClient(repository.GitHubClientConfig{
		ClientID:       cfg.Forge.GitHub.OAuth.ClientID,
		ClientSecret:   cfg.Forge.GitHub.OAuth.ClientSecret,
		OAuthBaseURL:   cfg.Forge.GitHub.OAuth.OAuthBaseURL,
		DefaultAPIURL:  cfg.Forge.GitHubAPIURL,
		CACertPath:     cfg.Forge.GitHub.CACertPath,
		HTTPTimeout:    cfg.Forge.GitHub.HTTPTimeout,
		AllowedAPIURLs: cfg.Forge.GitHub.AllowedAPIURLs,
	})
	if err != nil {
		return fmt.Errorf("init github client: %w", err)
	}
	githubIntegrationSvc := service.NewGitHubIntegrationService(service.GitHubIntegrationConfig{
		PublicURL:           cfg.Server.PublicURL,
		DefaultAPIURL:       cfg.Forge.GitHubAPIURL,
		AllowedAPIURLs:      cfg.Forge.GitHub.AllowedAPIURLs,
		CORSAllowedOrigins:  cfg.Auth.CORS.AllowedOrigins,
		OAuthEnabled:        cfg.Forge.GitHub.OAuth.Enabled,
		ClientID:            cfg.Forge.GitHub.OAuth.ClientID,
		ClientSecret:        cfg.Forge.GitHub.OAuth.ClientSecret,
		Scopes:              cfg.Forge.GitHub.OAuth.Scopes,
		StateTTL:            cfg.Forge.GitHub.OAuth.StateTTL,
		DeviceFlowEnabled:   cfg.Forge.GitHub.OAuth.DeviceFlowEnabled,
		AllowedRedirectURIs: cfg.Forge.GitHub.OAuth.AllowedRedirectURIs,
	}, service.GitHubIntegrationDeps{
		Connections: githubConnRepo, AccountSources: githubAccountRepo, RepoSettings: repoSettingRepo,
		ForgeSources: forgeSourceRepo, OAuthStates: githubOAuthStateRepo,
		DeviceSessions: service.NewMemoryGitHubDeviceSessionStore(), Users: userRepo,
		Orgs: orgsSvc, Projects: projectsSvc, Sealer: secretSvc, GitHub: githubClient,
		ForgeProvider: githubForge, Logger: logger,
	})
	var checkRunner domain.CheckJobRunner
	if cfg.Checks.Enabled && clientset != nil {
		checkRunner = repository.NewK8sJobRunner(clientset, effectiveNamespace(cfg.Checks.Namespace, cfg.Fleet.Namespace), cfg.Checks.JobImage, cfg.Checks.Timeout, cfg.Server.PublicURL, cfg.Server.InternalControlURL, cfg.CA.MintingCASecret).
			WithImagePullSecrets(cfg.Checks.JobImagePullSecrets).
			WithSilent(cfg.Checks.Silent)
	}
	// Legacy flat-file validator (nil when no tokens_file configured).
	var legacyValidator domain.TokenValidator
	if cfg.Auth.Internal.Enabled && cfg.Auth.Internal.TokensFile != "" {
		legacyValidator = service.NewTokenValidator(cfg.Auth.Internal.TokensFile, logger)
	}

	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, legacyValidator, logger)
	authSvc.SetOrgRepository(orgRepo)
	usersSvc.SetOrgRepository(orgRepo)
	deviceAuthSvc := service.NewDeviceAuthService(service.NewMemoryDeviceAuthStore(), usersSvc, groupRepo, jwtSvc, cfg.Auth.Device.CodeTTL, cfg.Auth.Device.PollInterval, logger)

	// Bootstrap admin (idempotent: only when user count is 0).
	if err := bootstrapAdmin(ctx, cfg, usersSvc, logger); err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}

	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger).
		WithOrgRepository(orgRepo, projectRepo).
		WithDefaults(cfg.Org.DefaultQuotas, cfg.History.GC.MaxAge).
		WithMetrics(metrics)
	groupsSvc.WithQuotaService(quotaSvc)
	projectsSvc.WithQuotaService(quotaSvc)
	attributionSvc := service.NewAttributionService(projectsSvc, groupRepo, traceMetaRepo, logger)
	attributionSvc.SetOrgRepository(orgRepo)

	projectMapper, err := service.NewProjectMapper(cfg.Attribution.ProjectMappings)
	if err != nil {
		return fmt.Errorf("compile project mappings: %w", err)
	}
	attributionSvc.SetProjectMapper(projectMapper)

	for i, rule := range cfg.Attribution.ProjectMappings {
		if !service.ValidateGroupName(rule.Group) {
			logger.WithFields(logrus.Fields{
				"rule_index": i,
				"group":      rule.Group,
			}).Warn("attribution: project_mappings group is not a valid supervisor group name; it can never match an existing group")
		}
	}

	liveHub := repository.NewLiveHub()
	pipelineLifecycle := service.NewPipelineLifecycle(attributionSvc, traceMetaRepo, sessions, liveHub, cfg.Pipeline, logger, metrics)

	var oauthSvc service.OAuthProvider
	var oauthProvider string
	var mapper *service.GroupMapper
	if cfg.Auth.OAuth.Enabled {
		var err error
		mapper, err = service.NewGroupMapper(cfg.Auth.OAuth.GroupMappings)
		if err != nil {
			return fmt.Errorf("compile group mappings: %w", err)
		}
		oauthHTTPClient, err := service.NewOAuthHTTPClient(cfg.Auth.OAuth.CACertPath)
		if err != nil {
			return fmt.Errorf("create OAuth HTTP client: %w", err)
		}
		switch cfg.Auth.OAuth.Provider {
		case "github":
			gh := service.NewGitHubOAuthService(&cfg.Auth.OAuth, mapper, usersSvc, groupRepo, jwtSvc, logger, tokenEncKey)
			gh.SetOrgRepository(orgRepo)
			gh.SetQuotaService(quotaSvc)
			oauthSvc = gh
		case "oidc":
			oidc := service.NewOIDCOAuthService(&cfg.Auth.OAuth, mapper, usersSvc, groupRepo, jwtSvc, logger, oauthHTTPClient, tokenEncKey, raftStore.IsLeader)
			oidc.SetOrgRepository(orgRepo)
			oidc.SetQuotaService(quotaSvc)
			oauthSvc = oidc
		default:
			// validateAuthConfig already rejected this, but fail closed.
			return fmt.Errorf("unsupported oauth provider: %s", cfg.Auth.OAuth.Provider)
		}
		oauthProvider = cfg.Auth.OAuth.Provider

		// Warn about group-mapping replacements that can never match a
		// supervisor group.
		for i, rule := range cfg.Auth.OAuth.GroupMappings {
			if !strings.Contains(rule.Replacement, "$") && !service.ValidateGroupName(rule.Replacement) {
				logger.WithFields(logrus.Fields{
					"rule_index":  i,
					"replacement": rule.Replacement,
				}).Warn("oauth: group_mappings replacement is not a valid supervisor group name and has no capture reference; it can never match an existing group")
			}
		}

		// Wire OAuth revalidation when OAuth is enabled.
		revalidator := service.NewOAuthRevalidator(oauthSvc, mapper, cfg.Auth.OAuth.AdminGroups, cfg.Auth.OAuth.MappedGroupMaxRunnerSessions, usersSvc, groupRepo, logger, service.OAuthRevalidatorConfig{
			Interval:      cfg.Auth.OAuth.RevalidateInterval,
			Grace:         cfg.Auth.OAuth.RevalidateGrace,
			FailOpen:      cfg.Auth.OAuth.RevalidateFailOpen,
			SessionMaxAge: cfg.Auth.OAuth.SessionMaxAge,
		})
		revalidator.SetOrgRepository(orgRepo)
		revalidator.SetQuotaService(quotaSvc)
		authSvc.SetOAuthRevalidator(revalidator)
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
		Isolation:             cfg.Fleet.Isolation,
	}, logger, metrics)

	fleetManager.WithEngineQuota(func(ctx context.Context, orgID string) (int64, error) {
		eff, err := quotaSvc.EffectiveQuotas(ctx, orgID)
		if err != nil {
			return 0, err
		}
		return eff.MaxEngines.Hard, nil
	})
	quotaSvc.WithEngineUsage(func(ctx context.Context, orgID string) (int64, error) {
		infos, err := fleetManager.AllFleetInfoForOrg(orgID)
		if err != nil {
			return 0, err
		}
		var n int64
		for _, info := range infos {
			n += int64(info.Replicas)
		}
		return n, nil
	})

	if cfg.Fleet.Isolation == "shared" {
		logger.Warn("fleet.isolation=shared is deprecated and restricts the cluster to a single organization")
		if all, listErr := orgsSvc.List(ctx); listErr == nil && len(all) > 1 {
			return fmt.Errorf("fleet.isolation=shared requires exactly one organization, found %d: %w", len(all), domain.ErrSharedFleetMultiOrg)
		}
	}

	// Per-version local-cache purge: prune every running engine pod in-process
	// (engine dagql API over plaintext TCP). Requires only a clientset + fleet
	// provider; no CLI subsystem or S3 config.
	var engineCachePurger domain.EngineCachePurger
	if clientset != nil && provider != nil {
		pruner := repository.NewSessionEnginePruner()
		engineCachePurger = service.NewEngineCachePurgeService(provider, pruner, logger)
	}

	traces := repository.NewSpanTreeReconstructor(cfg.Telemetry.TempoURL)
	logsClient := repository.NewLogsClient(cfg.Telemetry.LokiURL)

	// --- History / status wiring ---
	metricsClient := repository.NewMetricsClient(cfg.Telemetry.VictoriaURL)

	rateWindow := cfg.Pipeline.Metrics.RateWindow

	// Trace-scoped engine resource metrics (cAdvisor container_* series).
	var engineMetricsSvc *service.EngineMetricsService
	if cfg.Pipeline.Metrics.Enabled {
		engineMetricsSvc = service.NewEngineMetricsService(metricsClient, cfg.Fleet.Namespace, cfg.Pipeline.Metrics.Step, rateWindow, logger).WithFleetIsolation(cfg.Fleet.Isolation)
	}

	// Runners-page fleet metrics: same cAdvisor source + feature flag, plus the
	// configured per-pod PVC size as the storage-capacity fallback when
	// cAdvisor reports no limit.
	var engineStorageBytes int64
	if cfg.Fleet.EngineStorageSize != "" {
		if q, err := resource.ParseQuantity(cfg.Fleet.EngineStorageSize); err == nil {
			engineStorageBytes = q.Value()
		} else {
			logger.WithError(err).Warn("invalid fleet.engine_storage_size; storage-capacity fallback disabled")
		}
	}
	var fleetMetricsSvc *service.FleetMetricsService
	if cfg.Pipeline.Metrics.Enabled {
		fleetMetricsSvc = service.NewFleetMetricsService(metricsClient, cfg.Fleet.Namespace, cfg.Pipeline.Metrics.Step, rateWindow, engineStorageBytes, logger)
	}

	// Leader-only recorder that persists engine samples tagged with trace_id so
	// the pipeline view can show a run's metrics after the engine scales down.
	var metricsRecorder *service.TraceMetricsRecorder
	if cfg.Pipeline.Metrics.Enabled {
		metricsRecorder = service.NewTraceMetricsRecorder(metricsClient, traceMetaRepo, cfg.Fleet.Namespace,
			cfg.Pipeline.Metrics.RecordInterval, rateWindow, cfg.Pipeline.Metrics.MaxRecordWindow, raftStore.IsLeader, logger).WithFleetIsolation(cfg.Fleet.Isolation)
	}

	// Shared S3 client for the S3-backed CLI cache.
	var s3Client *minio.Client
	if cfg.Cache.S3.Endpoint == "" {
		logger.Warn("cache.s3.endpoint is empty; s3-backed CLI cache disabled")
	} else {
		client, err := repository.NewS3Client(cfg.Cache.S3.Endpoint, cfg.Cache.S3.Region, cfg.Cache.S3.AccessKey, cfg.Cache.S3.SecretKey, cfg.Cache.S3.UseSSL)
		if err != nil {
			return fmt.Errorf("create s3 client: %w", err)
		}
		s3Client = client
	}

	var otlpArchive domain.OTLPArchive = repository.NewMemoryOTLPArchive()
	if s3Client != nil && cfg.Cache.S3.Bucket != "" {
		otlpArchive = repository.NewS3OTLPArchive(s3Client, cfg.Cache.S3.Bucket, "otlp-archive", logger)
	}
	otlpStreamSvc := service.NewOTLPStreamService(otlpArchive, traceMetaRepo, logger)
	var remoteCacheSvc *service.RemoteCacheService
	var wsDialer repository.WSLeaderDialer
	if cfg.Cache.Remote.Enabled {
		index, err := repository.OpenBboltCacheIndex(cfg.Cache.Remote.IndexPath)
		if err != nil {
			return fmt.Errorf("open remote cache index: %w", err)
		}
		defer func() { _ = index.Close() }()
		bucket := cfg.Cache.Remote.Bucket
		if bucket == "" {
			bucket = cfg.Cache.S3.Bucket
		}
		store, err := repository.NewS3RemoteCacheStore(
			cfg.Cache.S3.Endpoint, cfg.Cache.S3.Region, cfg.Cache.S3.AccessKey, cfg.Cache.S3.SecretKey, cfg.Cache.S3.UseSSL,
			cfg.Cache.Remote.S3EngineEndpoint, bucket, cfg.Cache.Remote.Prefix, cfg.Cache.Remote.TTL,
		)
		if err != nil {
			return fmt.Errorf("create remote cache store: %w", err)
		}
		remoteCacheSvc = service.NewRemoteCacheService(index, store, cfg.Cache.Remote.TTL, cfg.Cache.Remote.MaxBytes, cfg.Cache.Remote.Workers, cfg.Cache.Remote.PackMaxMemberBytes, cfg.Cache.Remote.GCInterval, metrics, logger)
		remoteCacheSvc.WithCacheQuota(quotaSvc)
		quotaSvc.WithCacheUsage(func(orgID string) (int64, error) {
			_, size, err := remoteCacheSvc.Stats(orgID)
			return size, err
		})
		wsDialer = repository.NewGorillaWSLeaderDialer(dynamicCA.ForwardTLSProvider())
		if kp, ok := provider.(*repository.K8sProvider); ok {
			kp.SetRemoteCache(fmt.Sprintf("%s/v1/engine", strings.TrimSuffix(cfg.Server.InternalControlURL, "/")), "")
			kp.SetOrgTokenProvider(func(ctx context.Context, orgID string) (string, error) {
				return orgsSvc.EnsureToken(ctx, orgID)
			})
			orgsSvc.SetOnTokenChange(func(ctx context.Context, orgID, token string) {
				if token == "" {
					_ = kp.DeleteOrgTokenSecret(ctx, orgID)
					return
				}
				if err := kp.EnsureOrgTokenSecret(ctx, orgID, token); err != nil {
					logger.WithError(err).WithField("org", orgID).Warn("sync engine token secret failed")
				}
			})
		}
	}
	var artifactStore domain.ArtifactStore
	if s3Client != nil && cfg.Cache.S3.Bucket != "" {
		artifactStore = repository.NewS3ArtifactStore(s3Client, cfg.Cache.S3.Bucket, "")
	}
	var workflowRunner domain.WorkflowJobRunner
	if cfg.Workflows.Enabled && clientset != nil {
		workflowRunner = repository.NewK8sWorkflowRunner(clientset, effectiveNamespace(cfg.Workflows.Namespace, cfg.Fleet.Namespace), cfg.Workflows.JobImage, cfg.Workflows.DefaultTimeout, cfg.Server.PublicURL, cfg.Server.InternalControlURL, cfg.CA.MintingCASecret).
			WithImagePullSecrets(cfg.Workflows.JobImagePullSecrets).
			WithSilent(cfg.Workflows.Silent)
	}
	envRepo := repository.NewEnvironmentRepo(raftStore)
	workflowRepo := repository.NewWorkflowRepo(raftStore)
	workflowSvc := service.NewWorkflowService(
		workflowRepo, envRepo, orgsSvc, projectsSvc, forgeSourceRepo,
		repository.NewWorkflowFileParser(), repository.NewWorkflowExprEvaluator(),
		artifactStore, workflowRunner, secretSvc,
		service.WorkflowServiceConfig{DefaultTimeout: cfg.Workflows.DefaultTimeout, MaxParallel: cfg.Workflows.MaxParallel},
		logger,
	).WithEnabled(cfg.Workflows.Enabled)
	checksSvc := service.NewChecksService(forgeSourceRepo, secretSvc, projectsSvc, orgsSvc, checkRunner, checkRunRepo, workflowSvc, forgeProviders, cfg.Server.PublicURL, cfg.Forge.WebhookSecret, logger).
		WithEnabled(cfg.Checks.Enabled).
		WithSilent(cfg.Checks.Silent).
		WithRepoSettings(repoSettingRepo)
	cloudGraphQL.WithForgeSources(forgeSourceRepo).WithProjects(projectRepo).WithPublicURL(cfg.Server.PublicURL).
		WithGitHubConnections(githubConnRepo).
		WithGitHubAccountSources(githubAccountRepo).
		WithRepoSettings(repoSettingRepo).
		WithGitHubIntegrator(githubIntegrationSvc)
	if cfg.Checks.Enabled {
		cloudGraphQL.WithCheckRerunner(checksSvc.Rerun)
	}
	historyPurgeSvc := service.NewHistoryPurgeService(traceMetaRepo, logsClient, metricsClient, cfg.History.GC, logger, metrics)
	historyPurgeSvc.SetOTLPArchive(otlpArchive)
	statusSvc := service.NewStatusService(cfg, fleetManager, logger, raftStore)
	connectSvc := service.NewConnectService(cfg, versionResolver, tokensSvc, logger).WithOrgService(orgsSvc)

	// Local image cache: admin list/prune of the configured Zot mirrors over
	// their OCI Distribution v2 API (chart-rendered image_cache.mirrors). Only
	// built when at least one mirror is configured; no mirrors means the
	// endpoints report an empty set. TLS mirrors are dialed over https:// and
	// verified against image_cache.tls_ca_path (loaded once, fail fast).
	var imageCacheSvc domain.ImageCacheService
	if len(cfg.ImageCache.Mirrors) > 0 {
		roots, err := repository.LoadCertPool(cfg.ImageCache.TLSCAPath)
		if err != nil {
			return fmt.Errorf("load image cache TLS CA: %w", err)
		}
		imageCacheSvc = service.NewImageCacheService(cfg.ImageCache.Mirrors, func(m domain.ImageCacheMirror) domain.DistributionClient {
			return repository.NewDistributionClientForMirror(m, roots)
		}, logger)
	}

	// --- On-the-fly Dagger CLI provisioning wiring ---
	var cliSvc *service.CLIService
	if cfg.CLI.Enabled {
		// S3 backend: store tarballs in the shared S3 bucket.
		if s3Client == nil {
			return fmt.Errorf("cli.enabled requires cache.s3.endpoint")
		}
		bucket := cfg.CLI.S3Bucket
		if bucket == "" {
			bucket = cfg.Cache.S3.Bucket
		}
		cliCache := repository.NewS3CLICache(s3Client, bucket, cfg.CLI.S3Prefix, logger)
		cliUpstream := repository.NewGitHubCLIUpstream(repository.GitHubCLIUpstreamConfig{
			ReleasesURL:  cfg.CLI.Upstream.ReleasesURL,
			DownloadBase: cfg.CLI.Upstream.DownloadBase,
			GitHubToken:  cfg.CLI.Upstream.GitHubToken,
			Timeout:      cfg.CLI.DownloadTimeout,
		})
		cliSvc = service.NewCLIService(versionResolver, cliUpstream, cliCache, cfg.Server.PublicURL, cfg.CLI.ReleaseListTTL, logger, metrics)
	}

	// --- Security guardrails (go-secguard) ---
	securityGuard, err := secguard.New(config.ToSecguardConfig(&cfg.Auth.Security))
	if err != nil {
		return fmt.Errorf("init security guard: %w", err)
	}

	// --- Dynamic per-handshake TLS providers (ADR-050) ---
	var dataTLSProvider, controlTLSProvider *rafttransport.DynamicTLSProvider
	if cfg.Supervisor.Dataplane.TLS.Provider == "embedded" {
		dataTLSProvider = dynamicCA.EmbeddedServerTLSProvider()
		controlTLSProvider = dynamicCA.EmbeddedServerTLSProvider()
	} else {
		caFile := filepath.Join(cfg.Supervisor.Dataplane.TLS.CAPath, "ca.crt")
		watcher, werr := repository.NewDataplaneFileWatcher(
			cfg.Supervisor.Dataplane.TLS.CertPath,
			cfg.Supervisor.Dataplane.TLS.KeyPath,
			caFile,
			cfg.Supervisor.Dataplane.TLS.ReloadPollInterval,
			logger,
		)
		if werr != nil {
			return fmt.Errorf("start dataplane file watcher: %w", werr)
		}
		go watcher.Start(ctx)
		dataTLSProvider = watcher.Provider()
		controlTLSProvider = watcher.Provider()
	}

	server := handler.NewServer(&handler.ServerConfig{
		ControlAddr:           cfg.Server.ControlAddr,
		DataAddr:              cfg.Server.DataAddr,
		DataHost:              cfg.Server.DataHost,
		CollectorURL:          cfg.Telemetry.CollectorURL,
		VictoriaURL:           cfg.Telemetry.VictoriaURL,
		CertPath:              controlTLSCertPath,
		KeyPath:               controlTLSKeyPath,
		PipelineURL:           cfg.Server.PublicURL,
		OTelMaxBodyBytes:      cfg.OTel.IngestMaxBodySize,
		OTelMaxInflightBytes:  cfg.OTel.MaxInflightBytes,
		MaxDataConns:          cfg.Server.MaxDataConns,
		CLIIncludePrereleases: cfg.CLI.IncludePrereleases,
		InternalControlURL:    cfg.Server.InternalControlURL,
		InternalDataHost:      cfg.Server.InternalDataHost,
		FleetIsolation:        cfg.Fleet.Isolation,
		ClusterDomain:         cfg.Raft.ClusterDomain,
		InternalTLSCert:       &internalTLS,
		ControlTLSProvider:    controlTLSProvider,
		DataTLSProvider:       dataTLSProvider,
		InternalTLSProvider:   dynamicCA.InternalTLSProvider(),
		ForwardTLSProvider:    dynamicCA.ForwardTLSProvider(),
	}, &handler.Deps{
		Logger:               logger,
		Metrics:              metrics,
		MintingCA:            dynamicCA,
		FleetManager:         fleetManager,
		Sessions:             sessions,
		SessionRegistry:      repository.NewSessionRepo(raftStore),
		VersionResolver:      versionResolver,
		Auth:                 authSvc,
		InternalAuthEnabled:  cfg.Auth.Internal.Enabled,
		OAuthCookieSecure:    cfg.Auth.OAuth.CookieSecure,
		CookieCfg:            cfg.Auth.Cookie,
		CORSAllowedOrigins:   cfg.Auth.CORS.AllowedOrigins,
		Users:                usersSvc,
		Groups:               groupsSvc,
		Projects:             projectsSvc,
		Orgs:                 orgsSvc,
		DeviceAuth:           deviceAuthSvc,
		CIOIDC:               ciOIDCSvc,
		CloudGraphQL:         cloudGraphQL,
		OTLPStream:           otlpStreamSvc,
		RemoteCache:          remoteCacheSvc,
		Checks:               checksSvc,
		Forge:                forgeSvc,
		GitHubIntegration:    githubIntegrationSvc,
		Secrets:              secretSvc,
		Environments:         service.NewEnvironmentService(envRepo, logger),
		Workflows:            workflowSvc,
		Artifacts:            artifactStore,
		WSLeaderDialer:       wsDialer,
		Tokens:               tokensSvc,
		Quota:                quotaSvc,
		Attribution:          attributionSvc,
		TraceMeta:            traceMetaRepo,
		Traces:               traces,
		Logs:                 logsClient,
		OAuth:                oauthSvc,
		OAuthProvider:        oauthProvider,
		JWT:                  jwtSvc,
		EngineCachePurger:    engineCachePurger,
		HistoryStatsProvider: historyPurgeSvc,
		HistoryPurger:        historyPurgeSvc,
		StatusProvider:       statusSvc,
		StartupProvider:      raftStore,
		Connect:              connectSvc,
		LiveHub:              liveHub,
		Lifecycle:            pipelineLifecycle,
		CLI:                  cliSvc,
		CIWrapperPath:        cfg.CLI.CIWrapperPath,
		ImageCache:           imageCacheSvc,
		EngineMetrics:        engineMetricsSvc,
		FleetMetrics:         fleetMetricsSvc,
		LeaderInfo:           raftStore,
		Security:             securityGuard,
	})

	if err := server.Start(ctx, serverTLS); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	stopStaleSweep := pipelineLifecycle.StartStaleSweep(ctx)
	defer stopStaleSweep()

	stopHistoryGC := historyPurgeSvc.StartGCSweeper(ctx)
	defer stopHistoryGC()
	go otlpStreamSvc.Start(ctx)
	if cfg.Checks.Enabled {
		go checksSvc.StartReconciler(ctx, raftStore.IsLeader)
	}
	if remoteCacheSvc != nil {
		go remoteCacheSvc.Start(ctx, raftStore.IsLeader)
	}
	if cfg.Workflows.Enabled {
		go workflowSvc.StartScheduler(ctx, raftStore.IsLeader)
	}

	stopMetricsRecorder := func() {}
	if metricsRecorder != nil {
		stopMetricsRecorder = metricsRecorder.Start(ctx)
	}
	defer stopMetricsRecorder()

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
				securityGuard.SweepAll()
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.WithField("signal", sig.String()).Info("received signal, shutting down")

	// Graceful Raft leave: transfer leadership, remove self, wait commit.
	shutdownTimeout := cfg.Raft.TerminationGracePeriod
	if shutdownTimeout == 0 {
		shutdownTimeout = 60 * time.Second
	}
	leaveCtx, leaveCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := raftStore.LeaveCluster(leaveCtx, shutdownTimeout); err != nil {
		logger.WithError(err).Warn("failed to leave Raft cluster gracefully")
	}
	leaveCancel()

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
func initRaftStore(ctx context.Context, cfg *domain.Config, clientset kubernetes.Interface, sessions domain.SessionStateSink, metaHook func(key, value string), logger *logrus.Logger) (store *repository.RaftStore, jwtSecret, tokenEncKey []byte, err error) {
	if err := validateRaftConfig(cfg, clientset); err != nil {
		return nil, nil, nil, fmt.Errorf("validate raft config: %w", err)
	}

	hostname, _ := os.Hostname()
	discoveryCfg := raftDiscoveryConfig(cfg)

	resolver := discovery.NewPeerResolver(&discoveryCfg)

	// Derive the advertise address (pod FQDN for StatefulSet pods).
	advertise, err := discovery.DeriveAdvertiseAddr(&discoveryCfg, hostname)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("derive raft advertise addr: %w", err)
	}

	isMultiNode := cfg.Raft.Replicas > 1 || len(cfg.Raft.Peers) > 1
	if isMultiNode && !cfg.Raft.TLS.Enabled {
		logger.Warn(`raft multi-node is configured but raft.tls.enabled is false: Raft replication traffic — including password hashes, token hashes/ciphertexts, the JWT secret, and the token-encryption key — will flow in CLEARTEXT over the network (CWE-319/CWE-311). Enable raft.tls.enabled for multi-node in production.`)
	}
	if isMultiNode && !cfg.Raft.TLS.ClientAuth {
		logger.Warn(`raft multi-node is configured with raft.tls.client_auth=false: peers will not verify each other's certificates, so an unauthenticated peer could join the cluster (CWE-295). Keep raft.tls.client_auth=true (mTLS) for multi-node in production.`)
	}
	if isMultiNode && cfg.Supervisor.Dataplane.TLS.Provider == "embedded" && (clientset == nil || cfg.CA.MintingCASecret == "") && isMintingCAOnPerPodStorage(cfg) {
		logger.Warn(`multi-node is configured with the embedded TLS provider but the minting CA cannot be shared across pods (no K8s clientset or ca.minting_ca_secret is empty), and supervisor.dataplane.tls.ca_path is stored under the per-pod database directory. Each pod will mint a DISTINCT engine-client CA, so engine mTLS client certs issued by one pod will be REJECTED by other pods' data-plane listeners (CWE-295). To fix: run with a K8s clientset and ca.minting_ca_secret set (the embedded provider then shares the CA via that Secret), mount a shared ReadWriteMany volume at supervisor.dataplane.tls.ca_path, or switch supervisor.dataplane.tls.provider to cert-manager/external with a shared CA.`)
	}

	namespace := cfg.Raft.Namespace
	if namespace == "" {
		namespace = cfg.Fleet.Namespace
	}
	tlsDir := cfg.Raft.TLS.Dir
	if tlsDir == "" {
		tlsDir = filepath.Join(cfg.Database.Dir, "tls")
	}

	raftStore, err := repository.NewRaftStore(&repository.RaftStoreConfig{
		Dir:                      cfg.Database.Dir,
		NodeID:                   cfg.Raft.NodeID,
		BindAddr:                 cfg.Raft.BindAddr,
		AdvertiseAddr:            advertise,
		Peers:                    toRaftPeers(cfg.Raft.Peers),
		Resolver:                 resolver,
		Replicas:                 cfg.Raft.Replicas,
		StatefulSetName:          cfg.Raft.StatefulSetName,
		HeadlessService:          cfg.Raft.HeadlessService,
		Namespace:                namespace,
		ClusterDomain:            cfg.Raft.ClusterDomain,
		RaftPort:                 raftPortFromBindAddr(cfg.Raft.BindAddr),
		ApplyTimeout:             cfg.Raft.ApplyTimeout,
		LeaderWaitTimeout:        cfg.Raft.LeaderWaitTimeout,
		SnapshotThreshold:        cfg.Raft.SnapshotThreshold,
		SnapshotInterval:         cfg.Raft.SnapshotInterval,
		TrailingLogs:             cfg.Raft.TrailingLogs,
		PerformanceMultiplier:    cfg.Raft.PerformanceMultiplier,
		RaftLogCacheSize:         cfg.Raft.RaftLogCacheSize,
		TransportMaxPool:         cfg.Raft.TransportMaxPool,
		NoSnapshotRestoreOnStart: cfg.Raft.NoSnapshotRestoreOnStart,
		RecoveryMode:             cfg.Raft.RecoveryMode,
		SessionSink:              sessions,
		TLSEnabled:               cfg.Raft.TLS.Enabled,
		TLSDir:                   tlsDir,
		TLSValidity:              cfg.Raft.TLS.Validity,
		TLSOrganization:          cfg.Raft.TLS.Organization,
		CACertPath:               cfg.Raft.TLS.CACertPath,
		CertPath:                 cfg.Raft.TLS.CertPath,
		KeyPath:                  cfg.Raft.TLS.KeyPath,
		CASecret:                 cfg.Raft.TLS.CASecret,
		CABootstrap:              raftCABootstrap(cfg, hostname),
		ClientAuth:               cfg.Raft.TLS.ClientAuth,
		SecretPollTimeout:        cfg.Raft.LeaderWaitTimeout,
		KeyringRotationEnabled:   cfg.Raft.TLS.KeyringRotationEnabled,
		RotationPeriod:           cfg.Raft.TLS.RotationPeriod,
		RotationCheckInterval:    cfg.Raft.TLS.RotationCheckInterval,
		SafetyMargin:             cfg.Raft.TLS.SafetyMargin,
		EncryptionKey:            cfg.Auth.Token.EncryptionKey,
		AutopilotEnabled:         cfg.Raft.Autopilot.Enabled,
		CleanupDeadServers:       cfg.Raft.Autopilot.CleanupDeadServers,
		MinQuorum:                cfg.Raft.Autopilot.MinQuorum,
		Clientset:                clientset,
		MetaHook:                 metaHook,
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

	// Startup barrier: the FSM must be caught up (Leader/Follower,
	// commit_index == applied_index, fsm_pending == 0) before reading secrets
	// from the local FSM. A follower that joined the cluster but hasn't
	// installed the leader's snapshot yet would see an empty meta store and
	// timeout on jwt_secret/token_encryption_key resolution.
	cleanCtx, cleanCancel := context.WithTimeout(ctx, cfg.Raft.LeaderWaitTimeout)
	err = raftStore.WaitForCleanState(cleanCtx)
	cleanCancel()
	if err != nil {
		_ = raftStore.Close()
		return nil, nil, nil, fmt.Errorf("wait for raft clean state: %w", err)
	}

	metaStore := repository.NewMetaStore(raftStore)
	jwtSecret, err = resolveJWTSecret(ctx, raftStore, metaStore, cfg.Auth.JWT.Secret, cfg.Raft.LeaderWaitTimeout, logger)
	if err != nil {
		_ = raftStore.Close()
		return nil, nil, nil, fmt.Errorf("load jwt secret: %w", err)
	}
	tokenEncKey, err = resolveTokenEncryptionKey(ctx, raftStore, metaStore, cfg.Auth.Token.EncryptionKey, cfg.Raft.LeaderWaitTimeout, logger)
	if err != nil {
		_ = raftStore.Close()
		return nil, nil, nil, fmt.Errorf("load token encryption key: %w", err)
	}

	// The readyz probe keeps gating pod readiness afterwards; this final
	// check is removed since WaitForCleanState now runs before secret
	// resolution.

	return raftStore, jwtSecret, tokenEncKey, nil
}

// minJWTSecretLen is the minimum accepted HS256 signing key length, sourced
// from the shared go-auth library. HS256 security is bounded by the key's
// entropy; RFC 7518 mandates a key at least as long as the hash output (256
// bits) for HS256 (CWE-326).
const minJWTSecretLen = goauthjwt.MinSecretLength

// loadOrCreateJWTSecret returns the configured JWT secret, or generates and
// persists a 32-byte random secret in the DB meta table on first boot.
// Configured secrets shorter than the go-auth minimum are rejected: a weak key
// makes HS256 tokens forgeable offline, which would bypass authentication
// entirely.
func loadOrCreateJWTSecret(ctx context.Context, ms *repository.MetaStore, configured string, logger *logrus.Logger) ([]byte, error) {
	if configured != "" {
		if err := goauthjwt.ValidateSecret(configured); err != nil {
			return nil, fmt.Errorf("auth.jwt.secret: %w", err)
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
// and written (0600) to auth.bootstrap_admin.password_file BEFORE the user is
// created, so a usable recovery copy exists before the credential does. The
// WARN log records only the file path — never the password (CWE-532). An
// unwritable file aborts startup instead of creating an unrecoverable account.
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
	passwordFile := ""
	if password == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generate bootstrap password: %w", err)
		}
		password = hex.EncodeToString(b)
		generated = true
		passwordFile = bootstrapPasswordPath(cfg)
		if err := writeBootstrapPasswordFile(passwordFile, password); err != nil {
			return err
		}
	}

	if _, err := users.Create(ctx, username, password, domain.RoleAdmin); err != nil {
		// This node never created the user: its generated password (and the
		// file just written) is not the one in effect — drop the stale copy
		// so only the creator pod holds a recovery file.
		if generated {
			if rmErr := os.Remove(passwordFile); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				logger.WithError(rmErr).WithField("path", passwordFile).Debug("stale bootstrap password file removal failed")
			}
		}
		if errors.Is(err, domain.ErrNotLeader) {
			logger.Warn("bootstrap admin: not raft leader, skipping")
			return nil
		}
		return fmt.Errorf("create bootstrap admin: %w", err)
	}

	fields := logrus.Fields{
		"username":  username,
		"generated": generated,
	}
	if generated {
		// The password itself is never logged; point the operator at the
		// 0600 recovery file instead (CWE-532). Configured passwords are
		// neither generated nor written anywhere.
		fields["password_file"] = passwordFile
	}
	logger.WithFields(fields).Warn("bootstrap admin created")
	return nil
}

// bootstrapPasswordPath returns the recovery file for a generated bootstrap
// password: auth.bootstrap_admin.password_file, defaulting to
// <database.dir>/bootstrap-admin-password.
func bootstrapPasswordPath(cfg *domain.Config) string {
	if cfg.Auth.BootstrapAdmin.PasswordFile != "" {
		return cfg.Auth.BootstrapAdmin.PasswordFile
	}
	return filepath.Join(cfg.Database.Dir, "bootstrap-admin-password")
}

// writeBootstrapPasswordFile persists the generated password with 0600
// permissions (created parents get 0750). The explicit chmod also tightens a
// pre-existing file at the same path.
func writeBootstrapPasswordFile(path, password string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create bootstrap password dir: %w", err)
		}
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%s\n", password)), 0o600); err != nil {
		return fmt.Errorf("write bootstrap password file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod bootstrap password file: %w", err)
	}
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
	raftStore, err := repository.NewRaftStore(&repository.RaftStoreConfig{
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
	orgRepo := repository.NewOrgRepo(raftStore)
	tokenRepo := repository.NewTokenRepo(raftStore)
	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	metaStore := repository.NewMetaStore(raftStore)
	tokenEncKey, err := loadOrCreateTokenEncryptionKey(ctx, metaStore, cfg.Auth.Token.EncryptionKey, logger)
	if err != nil {
		return fmt.Errorf("load token encryption key: %w", err)
	}
	tokensSvc := service.NewTokenService(tokenRepo, logger, tokenEncKey)
	orgMetaStore := repository.NewMetaStore(raftStore)
	orgsSvc := service.NewOrgService(orgRepo, groupRepo, tokensSvc, logger).
		WithMetaStore(orgMetaStore).
		WithFleetIsolation(cfg.Fleet.Isolation).
		WithDefaultQuotas(cfg.Org.DefaultQuotas)
	bootOrg, err := orgsSvc.EnsureBootstrap(ctx, cfg.Org.Default.DisplayName)
	if err != nil && !errors.Is(err, domain.ErrNotLeader) {
		return fmt.Errorf("bootstrap organization: %w", err)
	}
	orgID := ""
	if bootOrg != nil {
		orgID = bootOrg.ID
	}
	res, err := service.ImportTokensFile(ctx, tokensFile, orgID, usersSvc, tokensSvc, groupsSvc, logger, c.Bool("dry-run"))
	if err != nil {
		return err
	}
	fmt.Printf("migrate-tokens: imported=%d skipped=%d\n", res.Imported, res.Skipped)
	for _, u := range res.Usernames {
		fmt.Printf("  %s\n", u)
	}
	return nil
}

// toRaftPeers converts domain.RaftPeer config entries into discovery peers.
func toRaftPeers(peers []domain.RaftPeer) []discovery.Peer {
	out := make([]discovery.Peer, 0, len(peers))
	for _, p := range peers {
		out = append(out, discovery.Peer{ID: p.ID, Address: p.Address})
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
		return fmt.Errorf("migrate-tokens must run against a single-node cluster (raft.replicas=1 and raft.peers empty); for multi-node, run it via the running leader's API or scale the cluster to 1 first")
	}
	return nil
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

// raftDiscoveryConfig maps the domain raft config to the go-raft discovery
// config. The raft port is derived from bind_addr (default 8081) so callers
// can reference it directly.
func raftDiscoveryConfig(cfg *domain.Config) discovery.Config {
	namespace := cfg.Raft.Namespace
	if namespace == "" {
		namespace = cfg.Fleet.Namespace
	}
	return discovery.Config{
		NodeID:          cfg.Raft.NodeID,
		AdvertiseAddr:   cfg.Raft.AdvertiseAddr,
		BindAddr:        cfg.Raft.BindAddr,
		Peers:           toRaftPeers(cfg.Raft.Peers),
		Replicas:        cfg.Raft.Replicas,
		StatefulSetName: cfg.Raft.StatefulSetName,
		HeadlessService: cfg.Raft.HeadlessService,
		Namespace:       namespace,
		ClusterDomain:   cfg.Raft.ClusterDomain,
		RaftPort:        raftPortFromBindAddr(cfg.Raft.BindAddr),
	}
}

// effectiveNamespace returns the specific namespace when set, else the fleet
// namespace (matching initRaftStore's fallback).
func effectiveNamespace(specific, fleet string) string {
	if specific != "" {
		return specific
	}
	return fleet
}

// raftPortFromBindAddr returns the port from bind_addr, defaulting to 8081.
func raftPortFromBindAddr(bindAddr string) int {
	if _, portStr, err := net.SplitHostPort(bindAddr); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			return p
		}
	}
	return 8081
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

// syncCASecretLoop (leader-only) writes every settled active CA back to its
// Kubernetes Secret so future scale-up or empty-PVC pods bootstrap with the
// current CA rather than a retired one. It syncs both the Raft CA
// (<release>-raft-ca) and the Minting CA (<release>-minting-ca), and keeps the
// public Minting CA trust-bundle ConfigMap (<fullname>-minting-ca-bundle) in
// sync for Gateway API BackendTLSPolicy. It runs one pass immediately (so the
// bundle is populated as soon as initRaftStore completes on the leader), wakes
// on every keyring install via caSyncNotify (so phased-rotation terms update
// within milliseconds), and keeps the 30s ticker for leadership handoff and
// drift self-healing.
func syncCASecretLoop(ctx context.Context, cfg *domain.Config, clientset kubernetes.Interface, raftStore *repository.RaftStore, dynamicCA *repository.DynamicMintingCA, notify <-chan struct{}, logger *logrus.Logger) {
	if clientset == nil {
		return
	}
	namespace := cfg.Raft.Namespace
	if namespace == "" {
		namespace = cfg.Fleet.Namespace
	}
	runLeaderCASync := func() {
		if !raftStore.IsLeader() {
			return
		}
		// Bound each pass so an unresponsive API server cannot stall the loop.
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		syncRaftCASecret(syncCtx, clientset, namespace, cfg.Raft.TLS.CASecret, raftStore, logger)
		syncMintingCASecret(syncCtx, clientset, namespace, cfg.CA.MintingCASecret, dynamicCA, logger)
		syncMintingCAConfigMap(syncCtx, clientset, namespace, cfg.CA.MintingCAConfigMap, dynamicCA, logger)
	}

	runLeaderCASync()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-notify:
			runLeaderCASync()
		case <-ticker.C:
			runLeaderCASync()
		}
	}
}

// syncRaftCASecret writes the settled Raft CA (Term >= 3, single key) to the
// Raft CA Secret.
func syncRaftCASecret(ctx context.Context, clientset kubernetes.Interface, namespace, secretName string, raftStore *repository.RaftStore, logger *logrus.Logger) {
	if secretName == "" {
		return
	}
	km := raftStore.KeyringManager()
	if km == nil {
		return
	}
	kr := km.CurrentKeyring()
	if kr == nil || kr.Term < 3 || len(kr.Keys) != 1 {
		return
	}
	keyID, _, certPEM, keyPEM, err := raftStore.ActiveCAPEM()
	if err != nil {
		logger.WithError(err).Warn("sync raft CA secret: read active CA failed")
		return
	}
	updateCASecret(ctx, clientset, namespace, secretName, certPEM, keyPEM, keyID, logger)
}

// syncMintingCASecret writes the settled Minting CA (Term >= 3) to the Minting
// CA Secret.
func syncMintingCASecret(ctx context.Context, clientset kubernetes.Interface, namespace, secretName string, dynamicCA *repository.DynamicMintingCA, logger *logrus.Logger) {
	if secretName == "" || dynamicCA == nil {
		return
	}
	keyID, term, certPEM, keyPEM, ok := dynamicCA.ActiveCAPEM()
	if !ok || term < 3 {
		return
	}
	updateCASecret(ctx, clientset, namespace, secretName, certPEM, keyPEM, keyID, logger)
}

// updateCASecret creates or updates a CA Secret with the given cert/key when
// they differ from the stored values.
func updateCASecret(ctx context.Context, clientset kubernetes.Interface, namespace, secretName string, certPEM, keyPEM []byte, keyID string, logger *logrus.Logger) {
	secrets := clientset.CoreV1().Secrets(namespace)
	secret, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			logger.WithError(err).WithField("secret", secretName).Warn("sync CA secret: get failed")
			return
		}
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace}}
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if bytes.Equal(secret.Data["ca.crt"], certPEM) && bytes.Equal(secret.Data["ca.key"], keyPEM) {
		return
	}
	secret.Data["ca.crt"] = certPEM
	secret.Data["ca.key"] = keyPEM
	if secret.ResourceVersion == "" {
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			logger.WithError(err).WithField("secret", secretName).Warn("sync CA secret: create failed")
			return
		}
	} else if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		logger.WithError(err).WithField("secret", secretName).Warn("sync CA secret: update failed")
		return
	}
	logger.WithFields(logrus.Fields{"secret": secretName, "key_id": keyID}).Info("synced settled CA to Kubernetes Secret")
}

// syncMintingCAConfigMap publishes the public Minting CA trust bundle (key
// ca.crt, certificates only) into the ConfigMap named by
// ca.minting_ca_configmap so a Gateway API BackendTLSPolicy can reference it as
// a Core caCertificateRef. It is idempotent: a no-op when the stored bundle is
// already current, and it preserves the Helm-managed labels/annotations.
func syncMintingCAConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace, configMapName string, dynamicCA *repository.DynamicMintingCA, logger *logrus.Logger) {
	if configMapName == "" || dynamicCA == nil {
		return
	}
	bundlePEM, ok := dynamicCA.TrustBundlePEM()
	if !ok || len(bundlePEM) == 0 {
		return
	}
	updateCAConfigMap(ctx, clientset, namespace, configMapName, bundlePEM, logger)
}

// updateCAConfigMap creates or updates the CA bundle ConfigMap key ca.crt when
// it differs from the published trust bundle.
func updateCAConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace, configMapName string, bundlePEM []byte, logger *logrus.Logger) {
	configMaps := clientset.CoreV1().ConfigMaps(namespace)
	cm, err := configMaps.Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			logger.WithError(err).WithField("configmap", configMapName).Warn("sync CA configmap: get failed")
			return
		}
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace}}
	}
	if cm.Data["ca.crt"] == string(bundlePEM) {
		return
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["ca.crt"] = string(bundlePEM)
	if cm.ResourceVersion == "" {
		if _, err := configMaps.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				logger.WithError(err).WithField("configmap", configMapName).Warn("sync CA configmap: create failed")
				return
			}
			existing, getErr := configMaps.Get(ctx, configMapName, metav1.GetOptions{})
			if getErr != nil {
				logger.WithError(getErr).WithField("configmap", configMapName).Warn("sync CA configmap: re-get after create race failed")
				return
			}
			if existing.Data["ca.crt"] == string(bundlePEM) {
				return
			}
			if existing.Data == nil {
				existing.Data = map[string]string{}
			}
			existing.Data["ca.crt"] = string(bundlePEM)
			if _, err := configMaps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
				logger.WithError(err).WithField("configmap", configMapName).Warn("sync CA configmap: update after create race failed")
				return
			}
		}
	} else if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		logger.WithError(err).WithField("configmap", configMapName).Warn("sync CA configmap: update failed")
		return
	}
	logger.WithField("configmap", configMapName).Info("synced Minting CA trust bundle to Kubernetes ConfigMap")
}

func selectTLSProvider(cfg *domain.Config, dynamicCA *repository.DynamicMintingCA) (domain.CAProvider, error) {
	// The minting CA is always auto-bootstrapped and shared across pods via
	// ca.minting_ca_secret when a K8s clientset is available (multi-node),
	// regardless of the server-TLS provider below. The provider only decides
	// where the data/control-plane server certificate comes from.
	switch cfg.Supervisor.Dataplane.TLS.Provider {
	case "embedded":
		return dynamicCA, nil
	case "cert-manager":
		return repository.NewCertManagerProvider(cfg.Supervisor.Dataplane.TLS.CertPath, cfg.Supervisor.Dataplane.TLS.KeyPath, dynamicCA), nil
	case "external":
		return repository.NewExternalProvider(cfg.Supervisor.Dataplane.TLS.CertPath, cfg.Supervisor.Dataplane.TLS.KeyPath, dynamicCA), nil
	default:
		return nil, fmt.Errorf("unknown TLS provider: %s", cfg.Supervisor.Dataplane.TLS.Provider)
	}
}

// mintingProvider returns the minting-CA provider. It shares the CA across
// pods via ca.minting_ca_secret when a K8s clientset is available, otherwise
// falls back to the local-file behavior (single-node dev/test or a shared RWX
// volume). The minting CA signs short-lived engine client certs and is an
// internal CA — it never needs a public/cert-manager issuer, so it is
// auto-bootstrapped for every server-TLS provider.
func mintingProvider(cfg *domain.Config, clientset kubernetes.Interface) *repository.EmbeddedProvider {
	hostname, _ := os.Hostname()
	extraSANs := mintingExtraSANs(cfg, hostname)
	if clientset != nil && cfg.CA.MintingCASecret != "" {
		namespace := cfg.Raft.Namespace
		if namespace == "" {
			namespace = cfg.Fleet.Namespace
		}
		return repository.NewEmbeddedProviderWithSecret(
			cfg.Supervisor.Dataplane.TLS.CAPath,
			cfg.CA.ClientCertTTL,
			cfg.CA.MintingCASecret,
			namespace,
			clientset,
			mintingCABootstrap(cfg, hostname),
			cfg.Raft.LeaderWaitTimeout,
			extraSANs...,
		)
	}
	return repository.NewEmbeddedProvider(cfg.Supervisor.Dataplane.TLS.CAPath, cfg.CA.ClientCertTTL, extraSANs...)
}

// mintingExtraSANs returns the extra DNS names the Minting-CA server leaf must
// cover: this pod's own discovery FQDN SANs (so any follower can verify the
// leader's control cert on the internal forward hop, ADR-041), the engine-facing
// data host, and the in-cluster internal service SANs.
func mintingExtraSANs(cfg *domain.Config, hostname string) []string {
	discoveryCfg := raftDiscoveryConfig(cfg)
	extraSANs := discovery.ServerCertFQDNSANs(&discoveryCfg, hostname)
	extraSANs = append(extraSANs, cfg.Server.DataHost)
	extraSANs = append(extraSANs, internalServiceSANs(cfg.Server.InternalControlURL, cfg.Server.InternalDataHost, cfg.Raft.ClusterDomain)...)
	return extraSANs
}

// internalServiceSANs parses hostnames from the internal control URL and data
// host and emits, for each, the bare host plus the "<host>.<clusterDomain>"
// form when the host ends in ".svc" and a cluster domain is configured. These
// are added to the Minting-CA leaf so pods dialing the internal services (in
// either the short or fully-qualified form) verify successfully.
func internalServiceSANs(internalControlURL, internalDataHost, clusterDomain string) []string {
	var sans []string
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		sans = append(sans, s)
	}
	for _, raw := range []string{hostOf(internalControlURL), hostOf(internalDataHost)} {
		if raw == "" {
			continue
		}
		add(raw)
		if strings.Contains(raw, ".svc") {
			// Bare single-label service name, accepted by internalHostForms /
			// matchesInternalSNI for same-namespace clients.
			add(strings.SplitN(raw, ".", 2)[0])
		}
		if clusterDomain != "" && strings.HasSuffix(raw, ".svc") {
			add(fmt.Sprintf("%s.%s", raw, strings.Trim(clusterDomain, ".")))
		}
	}
	return sans
}

// hostOf extracts the hostname from a URL or host[:port] string.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		raw = u.Host
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	return strings.Trim(raw, "[]")
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
// (supervisor.dataplane.tls.ca_path) is stored under the per-pod Raft data
// directory. With the StatefulSet conversion, each pod has its own PVC at
// database.dir, so a minting CA under that path is per-pod and breaks engine
// mTLS trust across pods (CWE-295).
func isMintingCAOnPerPodStorage(cfg *domain.Config) bool {
	caPath := cfg.Supervisor.Dataplane.TLS.CAPath
	dbDir := cfg.Database.Dir
	if caPath == "" || dbDir == "" {
		return false
	}
	rel, err := filepath.Rel(dbDir, caPath)
	if err != nil {
		return false
	}
	// caPath is under dbDir (e.g. /var/lib/dagger-kubernetes/ca under /var/lib/dagger-kubernetes).
	return rel != "." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, fmt.Sprintf("..%c", filepath.Separator))
}

func createProvider(cfg *domain.Config, clientset kubernetes.Interface, logger *logrus.Logger) (domain.FleetProvider, error) {
	if err := validateFleetEnv(&cfg.Fleet); err != nil {
		return nil, err
	}
	if clientset == nil {
		logger.Warn("k8s clientset unavailable; falling back to in-memory stub provider — engine fleet will be empty and provisioning will not persist")
		return repository.NewStubProvider(), nil
	}

	k8sCfg := repository.K8sProviderConfig{
		Namespace:           cfg.Fleet.Namespace,
		ImageRegistry:       cfg.Fleet.EngineImageRegistry,
		StorageClass:        cfg.Fleet.EngineStorageClass,
		StorageSize:         cfg.Fleet.EngineStorageSize,
		PVCLabels:           cfg.Fleet.EnginePVCLabels,
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
		MirrorHTTP:          cfg.Fleet.EngineRegistryMirrorsHTTP,
		ImageCacheMirrors:   cfg.ImageCache.Mirrors,
	}

	return repository.NewK8sProvider(clientset, k8sCfg), nil
}

// validateFleetEnv rejects engine env configuration that Kubernetes would
// refuse at StatefulSet admission (duplicate container env names) or that is
// internally inconsistent. Called once at startup (fail fast).
func validateFleetEnv(fleet *domain.FleetConfig) error {
	for name := range fleet.EngineExtraEnv {
		if err := validateEnvName(name, "engine_extra_env"); err != nil {
			return err
		}
	}
	for name, src := range fleet.EngineExtraEnvFrom {
		if err := validateEnvName(name, "engine_extra_env_from"); err != nil {
			return err
		}
		if _, dup := fleet.EngineExtraEnv[name]; dup {
			return fmt.Errorf("env var %s is set in both fleet.engine_extra_env and fleet.engine_extra_env_from", name)
		}
		if src.SecretName == "" {
			return fmt.Errorf("fleet.engine_extra_env_from.%s: secretName must not be empty", name)
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

// validateEnvName rejects empty operator-supplied env var names. source is the
// fleet.* config key for errors. The supervisor no longer injects any engine
// env vars, so there are no reserved names to reject.
func validateEnvName(name, source string) error {
	if name == "" {
		return fmt.Errorf("fleet.%s contains an empty env var name", source)
	}
	return nil
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
