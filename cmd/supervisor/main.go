package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/disaster/dagger-cache/internal/api"
	"github.com/disaster/dagger-cache/internal/ca"
	"github.com/disaster/dagger-cache/internal/cache"
	"github.com/disaster/dagger-cache/internal/config"
	"github.com/disaster/dagger-cache/internal/fleet"
	"github.com/disaster/dagger-cache/internal/observ"
	"github.com/disaster/dagger-cache/internal/session"
	"github.com/disaster/dagger-cache/internal/version"
)

func main() {
	configFile := flag.String("config", "config.app.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger, err := observ.NewLogger(cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("dagger-cache supervisor starting",
		zap.String("control_addr", cfg.Server.ControlAddr),
		zap.String("data_addr", cfg.Server.DataAddr),
		zap.String("public_url", cfg.Server.PublicURL),
	)

	mintingCA, err := ca.NewMintingCA(cfg.CA.ClientCertTTL)
	if err != nil {
		logger.Fatal("failed to create minting CA", zap.Error(err))
	}

	versionResolver, err := version.NewResolver(cfg.Version.Floor, cfg.Version.Allowlist, nil)
	if err != nil {
		logger.Fatal("failed to create version resolver", zap.Error(err))
	}

	sessions := session.NewStore(cfg.LeaseTTL)

	cacheBackend := &cache.Backend{
		Type:     cfg.Cache.Backend,
		Registry: cfg.Cache.Registry,
		S3:       cache.S3Ref{Bucket: cfg.Cache.S3.Bucket, Region: cfg.Cache.S3.Region},
	}

	provider := fleet.NewStubProvider()
	fleetManager := fleet.NewManager(provider, sessions, fleet.ManagerConfig{
		MaxReplicasPerVersion: cfg.Fleet.MaxReplicasPerVersion,
		MaxSessionsPerReplica: cfg.Fleet.MaxSessionsPerReplica,
		ReplicaIdleTTL:        cfg.Fleet.ReplicaIdleTTL,
		VersionRetention:      cfg.Fleet.VersionRetention,
		MinReplicasPerVersion: cfg.Fleet.MinReplicasPerVersion,
	}, logger)

	serverTLS, err := mintingCA.TLSCertificate()
	if err != nil {
		logger.Fatal("failed to create server TLS cert", zap.Error(err))
	}

	server := api.NewServer(&api.ServerConfig{
		ControlAddr:  cfg.Server.ControlAddr,
		DataAddr:     cfg.Server.DataAddr,
		DataHost:     cfg.Server.DataHost,
		PublicURL:    cfg.Server.PublicURL,
		UIURL:        cfg.Server.UIURL,
		CollectorURL: cfg.Telemetry.CollectorURL,
		TempoURL:     cfg.Telemetry.TempoURL,
		TokensFile:   cfg.Auth.Internal.TokensFile,
	}, logger, mintingCA, fleetManager, sessions, cacheBackend, versionResolver)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx, serverTLS); err != nil {
		logger.Fatal("failed to start server", zap.Error(err))
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
					logger.Error("sweep error", zap.Error(err))
				}
			}
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", zap.Error(err))
	}

	logger.Info("supervisor stopped")
}
