package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// TestCloudParityRoutes proves the new Cloud-compatible routes are mounted and
// that unauthenticated access is rejected. All listeners are pre-bound with
// freeListener (no hardcoded/probe-then-close ports).
func TestCloudParityRoutes(t *testing.T) {
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)
	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("cloud-parity-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	deviceSvc := service.NewDeviceAuthService(service.NewMemoryDeviceAuthStore(), usersSvc, groupRepo, jwtSvc, 10*time.Minute, 5*time.Second, logger)
	graphql := repository.NewCloudGraphQL(userRepo, repository.NewOrgRepo(store), groupRepo, traceMetaRepo, repository.NewCheckRunRepo(store), domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10})

	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	fleetManager := service.NewManager(repository.NewStubProvider(), sessions, service.ManagerConfig{}, logger, observ.NewMetrics(nil))

	control := freeListener(t)
	data := freeListener(t)
	internal := freeListener(t)
	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:      listenerAddr(control),
		DataAddr:         listenerAddr(data),
		ControlListener:  control,
		DataListener:     data,
		InternalListener: internal,
		DataHost:         "localhost",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Tokens: tokensSvc, TraceMeta: traceMetaRepo, JWT: jwtSvc,
		DeviceAuth: deviceSvc, CloudGraphQL: graphql,
	})
	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(300 * time.Millisecond)

	base := fmt.Sprintf("http://%s", control.Addr().String())

	// Device code is public.
	resp, err := http.Post(base+"/oauth/device/code", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("device code: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device code status = %d", resp.StatusCode)
	}
	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&code); err != nil {
		t.Fatal(err)
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		t.Fatalf("device code response = %+v", code)
	}

	// GraphQL requires auth.
	resp2, err := http.Post(base+"/query", "application/json", bytes.NewReader([]byte(`{"query":"{ user { id } }"}`)))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query status = %d, want 401", resp2.StatusCode)
	}

	// OTLP read-back requires auth.
	resp3, err := http.Get(fmt.Sprintf("%s/v1/traces/0123456789abcdef0123456789abcdef", base))
	if err != nil {
		t.Fatalf("otlp stream: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("otlp stream status = %d, want 401", resp3.StatusCode)
	}
}
