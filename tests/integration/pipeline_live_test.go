package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const pipelinesLiveTraceID = "0123456789abcdef0123456789abcdef"

// otlpRootSpanBody builds an OTLP ExportTraceServiceRequest with a single
// parentless root span (field 1 = ResourceSpans), the same shape the handler
// unit tests use for ingest.
func otlpRootSpanBody(t *testing.T, traceIDHex string) []byte {
	t.Helper()
	traceID, err := hex.DecodeString(traceIDHex)
	if err != nil {
		t.Fatalf("decode trace id: %v", err)
	}
	spanID, err := hex.DecodeString("1111111111111111")
	if err != nil {
		t.Fatalf("decode span id: %v", err)
	}
	now := time.Now().UnixNano()
	rs := &tracepb.ResourceSpans{
		ScopeSpans: []*tracepb.ScopeSpans{
			{Spans: []*tracepb.Span{{
				TraceId:           traceID,
				SpanId:            spanID,
				StartTimeUnixNano: uint64(now),
				EndTimeUnixNano:   uint64(now + int64(time.Second)),
				Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
			}}},
		},
	}
	rsBytes, err := proto.Marshal(rs)
	if err != nil {
		t.Fatalf("marshal resource spans: %v", err)
	}
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(body, rsBytes)
}

// postOTLPTrace ingests one OTLP trace batch through the real /v1/traces proxy.
func postOTLPTrace(t *testing.T, controlURL, token string, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/v1/traces", controlURL), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build OTLP request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/traces: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/traces status = %d, want 200", resp.StatusCode)
	}
}

// TestPipelinesOverviewSSE proves the pipelines-overview live stream
// end-to-end: an authenticated GET /api/v1/traces/live stays open, an OTLP
// trace ingest pushes {"type":"pipelines_update"} over SSE, and the re-fetch
// target (GET /api/v1/traces) reflects the new pipeline. Uses
// freeListener-bound control/data listeners and a timed Shutdown (AGENTS.md
// integration mandates).
func TestPipelinesOverviewSSE(t *testing.T) {
	logger := observ.NewTestLogger()

	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	controlLn, dataLn := freeListener(t), freeListener(t)
	controlAddr, dataAddr := listenerAddr(controlLn), listenerAddr(dataLn)
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
	mintingCA, err := repository.NewMintingCA(2 * time.Hour)
	if err != nil {
		t.Fatalf("NewMintingCA: %v", err)
	}
	versionResolver, err := service.NewResolver("v0.19.0", nil, nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
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
	liveHub := repository.NewLiveHub()

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr:     controlAddr,
		DataAddr:        dataAddr,
		ControlListener: controlLn,
		DataListener:    dataLn,
		DataHost:        "localhost",
		CollectorURL:    collector.URL,
		PipelineURL:     "https://supv.example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo,
		Traces:  repository.NewSpanTreeReconstructor(""),
		Logs:    repository.NewLogsClient(""),
		JWT:     jwtSvc,
		LiveHub: liveHub,
	})

	serverTLS, err := mintingCA.TLSCertificate()
	if err != nil {
		t.Fatalf("TLSCertificate: %v", err)
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStart()
	if err := srv.Start(startCtx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(500 * time.Millisecond)

	controlURL := fmt.Sprintf("http://%s", controlLn.Addr().String())

	// Open the SSE stream with bearer auth (the EventSource contract uses the
	// cookie/`?token=` fallback; the header is equivalent for this client).
	// Response headers may be withheld until the first event flushes, so the
	// request runs in a goroutine.
	sseCtx, cancelSSE := context.WithCancel(context.Background())
	t.Cleanup(cancelSSE)
	sseReq, err := http.NewRequestWithContext(sseCtx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/traces/live", controlURL), http.NoBody)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	sseReq.Header.Set("Authorization", "Bearer "+adminToken)

	eventCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(sseReq)
		if err != nil {
			errCh <- fmt.Errorf("SSE request: %w", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errCh <- fmt.Errorf("SSE status = %d, want 200", resp.StatusCode)
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), `"type":"pipelines_update"`) {
				eventCh <- struct{}{}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("SSE stream ended before pipelines_update: %w", err)
			return
		}
		errCh <- errors.New("SSE stream ended before pipelines_update")
	}()

	// Re-ingest the batch until the subscriber is registered: a broadcast to a
	// topic with no subscriber is dropped by design, and the subscription
	// completes asynchronously after the request is accepted.
	body := otlpRootSpanBody(t, pipelinesLiveTraceID)
	received := false
	deadline := time.Now().Add(10 * time.Second)
	for !received && time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("%v", err)
		default:
		}
		postOTLPTrace(t, controlURL, adminToken, body)
		select {
		case <-eventCh:
			received = true
		case err := <-errCh:
			t.Fatalf("%v", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !received {
		t.Fatal("timed out waiting for the pipelines_update SSE event")
	}

	// The re-fetch target must now show the ingested pipeline.
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/traces", controlURL), http.NoBody)
	if err != nil {
		t.Fatalf("build list request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/traces: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/traces status = %d, want 200", resp.StatusCode)
	}
	var rows []domain.TraceListResult
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode trace list: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.TraceID == pipelinesLiveTraceID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("trace %s missing from GET /api/v1/traces: %+v", pipelinesLiveTraceID, rows)
	}
}
