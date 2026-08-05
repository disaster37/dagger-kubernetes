package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
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

	"github.com/disaster/dagger-kubernetes/internal/api"
	"github.com/disaster/dagger-kubernetes/internal/auth"
	"github.com/disaster/dagger-kubernetes/internal/ca"
	"github.com/disaster/dagger-kubernetes/internal/cache"
	"github.com/disaster/dagger-kubernetes/internal/config"
	"github.com/disaster/dagger-kubernetes/internal/fleet"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/session"
	"github.com/disaster/dagger-kubernetes/internal/version"
)

func main() {
	app := &cli.App{
		Name:  "supervisor",
		Usage: "dagger-cache control plane",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Value: "config.app.yaml",
				Usage: "path to config file",
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

	logger := observ.NewLogger(cfg.LogLevel)

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

	versionResolver, err := version.NewResolver(cfg.Version.Floor, cfg.Version.Allowlist, nil)
	if err != nil {
		return fmt.Errorf("create version resolver: %w", err)
	}

	sessions := session.NewStore(cfg.LeaseTTL)

	cacheBackend := &cache.Backend{
		Type:       cfg.Cache.Backend,
		Registry:   cfg.Cache.Registry,
		PublicHost: cfg.Cache.PublicHost,
		S3:         cache.S3Ref{Bucket: cfg.Cache.S3.Bucket, Region: cfg.Cache.S3.Region},
	}

	metrics := observ.NewMetrics(prometheus.DefaultRegisterer)

	tokenValidator := auth.NewTokenValidator(cfg.Auth.Internal.TokensFile, cfg.Auth.Internal.Enabled, logger)

	provider := createProvider(cfg, logger)
	fleetManager := fleet.NewManager(provider, sessions, fleet.ManagerConfig{
		MaxReplicasPerVersion: cfg.Fleet.MaxReplicasPerVersion,
		MaxSessionsPerReplica: cfg.Fleet.MaxSessionsPerReplica,
		ReplicaIdleTTL:        cfg.Fleet.ReplicaIdleTTL,
		VersionRetention:      cfg.Fleet.VersionRetention,
		MinReplicasPerVersion: cfg.Fleet.MinReplicasPerVersion,
	}, logger, metrics)

	server := api.NewServer(&api.ServerConfig{
		ControlAddr:  cfg.Server.ControlAddr,
		DataAddr:     cfg.Server.DataAddr,
		DataHost:     cfg.Server.DataHost,
		PublicURL:    cfg.Server.PublicURL,
		CacheHost:    cfg.Cache.PublicHost,
		InternalReg:  cfg.Cache.InternalAddr,
		CollectorURL: cfg.Telemetry.CollectorURL,
		TempoURL:     cfg.Telemetry.TempoURL,
		LokiURL:      cfg.Telemetry.LokiURL,
		VictoriaURL:  cfg.Telemetry.VictoriaURL,
		TokensFile:   cfg.Auth.Internal.TokensFile,
	}, logger, metrics, serverMintingCA, fleetManager, sessions, cacheBackend, versionResolver, tokenValidator)

	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()

	if err := server.Start(ctx, serverTLS); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

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

func selectTLSProvider(cfg *config.Config) (ca.Provider, error) {
	switch cfg.TLS.Provider {
	case "embedded":
		return ca.NewEmbeddedProvider(cfg.TLS.CAPath, cfg.CA.ClientCertTTL), nil
	case "cert-manager":
		return ca.NewCertManagerProvider(cfg.TLS.CertPath, cfg.TLS.KeyPath), nil
	case "external":
		return ca.NewExternalProvider(cfg.TLS.CertPath, cfg.TLS.KeyPath), nil
	default:
		return nil, fmt.Errorf("unknown TLS provider: %s", cfg.TLS.Provider)
	}
}

func createProvider(cfg *config.Config, logger *logrus.Logger) fleet.Provider {
	clientset, err := newK8sClientset()
	if err != nil {
		logger.WithError(err).Warn("failed to create k8s clientset, using stub provider")
		return fleet.NewStubProvider()
	}

	tolerations, err := parseTolerations(cfg.Fleet.EngineTolerations)
	if err != nil {
		logger.WithError(err).Warn("failed to parse engine tolerations, using stub provider")
		return fleet.NewStubProvider()
	}

	k8sCfg := fleet.K8sProviderConfig{
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
		Tolerations:         tolerations,
		ExtraArgs:           cfg.Fleet.EngineExtraArgs,
		PullPolicy:          corev1.PullPolicy(cfg.Fleet.EnginePullPolicy),
		Privileged:          cfg.Fleet.EnginePrivileged,
	}

	return fleet.NewK8sProvider(clientset, k8sCfg)
}

func newK8sClientset() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		config, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot load in-cluster or kubeconfig: %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	return clientset, nil
}

func parseTolerations(raw []string) ([]corev1.Toleration, error) {
	var tols []corev1.Toleration
	for _, rawTol := range raw {
		parts := strings.SplitN(rawTol, ":", 3)
		if len(parts) < 1 {
			return nil, fmt.Errorf("invalid toleration: %s", rawTol)
		}
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
	return tols, nil
}
