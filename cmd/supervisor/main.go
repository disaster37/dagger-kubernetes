package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

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

	provider := fleet.NewStubProvider()
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
