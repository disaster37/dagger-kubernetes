package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// TestTraceMetricsEndpoint proves the real handler wiring end-to-end: a fake
// VictoriaMetrics backend answers query_range, and
// GET /api/v1/traces/:id/metrics returns the curated engine series for a trace
// whose meta carries a version + start/duration.
func TestTraceMetricsEndpoint(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[100,"1.5"],[110,"2.5"]]}]}}`))
	}))
	defer vm.Close()

	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr, dataAddr := listenerAddr(controlLn), listenerAddr(dataLn)
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, nil)
	jwtSvc := service.NewJWTService([]byte("integration-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)

	admin, err := usersSvc.Create(context.Background(), "admin", "password123", domain.RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	adminToken, _, err := tokensSvc.Generate(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	mintingCA, _ := repository.NewMintingCA(2 * time.Hour)
	versionResolver, _ := service.NewResolver("v0.19.0", nil, nil)
	sessions := service.NewStore(2 * time.Minute)
	store.SetSessionSink(sessions)
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(
		service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger),
		groupRepo, traceMetaRepo, logger)
	traces := repository.NewSpanTreeReconstructor("")
	logsClient := repository.NewLogsClient("")

	metricsClient := repository.NewMetricsClient(vm.URL)
	engineMetrics := service.NewEngineMetricsService(metricsClient, "dagger-kubernetes", 15*time.Second, logger)

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        dataAddr,
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
		PipelineURL:     "https://supv.example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logsClient, JWT: jwtSvc,
		EngineMetrics: engineMetrics,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(500 * time.Millisecond)

	const traceID = "abcdef0123456789abcdef0123456789"
	if err := traceMetaRepo.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID:    traceID,
		Version:    "v0.21.4",
		StartedAt:  time.Now().Add(-time.Minute),
		DurationMS: 30000,
	}); err != nil {
		t.Fatalf("seed trace meta: %v", err)
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("http://localhost%s/api/v1/traces/%s/metrics", controlAddr, traceID), http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body domain.TraceMetrics
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.TraceID != traceID {
		t.Fatalf("trace_id = %q, want %q", body.TraceID, traceID)
	}
	if len(body.Series) != 6 {
		t.Fatalf("series = %d, want 6", len(body.Series))
	}
	if body.Series[0].Name != "cpu" || body.Series[0].Unit != "cores" {
		t.Fatalf("series[0] = %+v, want cpu/cores", body.Series[0])
	}
	if len(body.Series[0].Points) != 2 || body.Series[0].Points[0].V != 1.5 {
		t.Fatalf("series[0].points = %+v, want two points starting at 1.5", body.Series[0].Points)
	}
}
