package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const ciStepsTraceID = "abcdef0123456789abcdef0123456789"

// stepsTraceRepo / stepsLogRepo are stubs that serve a fixed nested span tree
// and span-correlated logs, standing in for the supervisor's Tempo/Loki clients
// so the integration test needs no external services.
type stepsTraceRepo struct {
	trace *domain.TraceInfo
}

func (s *stepsTraceRepo) GetTrace(string) (*domain.TraceInfo, error) { return s.trace, nil }

type stepsLogRepo struct {
	entries []domain.LogEntry
}

func (s *stepsLogRepo) QueryTraceLogs(string, time.Time, time.Time, int) ([]domain.LogEntry, error) {
	return s.entries, nil
}

func (s *stepsLogRepo) DeleteTraceLogs(context.Context, string) error { return nil }

// startCIStepsServer boots a supervisor wired like startPipelineURLServer, but
// with stub trace/log repositories returning a fixed nested tree (root -> two
// children, one failed) + span-correlated logs.
func startCIStepsServer(t *testing.T, controlAddr, dataAddr string) (string, string) {
	t.Helper()
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
	provider := repository.NewStubProvider()
	fleetManager := service.NewManager(provider, sessions, service.ManagerConfig{
		MaxReplicasPerVersion: 3, MaxSessionsPerReplica: 8, ReplicaIdleTTL: 5 * time.Minute,
	}, logger, observ.NewMetrics(nil))
	cacheBackend := &service.Cache{Type: "registry", Registry: "cache.reg/dagger-cache"}
	quotaSvc := service.NewQuotaService(sessions, groupRepo, logger)
	attributionSvc := service.NewAttributionService(
		service.NewProjectService(repository.NewProjectRepo(store), groupRepo, logger),
		groupRepo, traceMetaRepo, logger)

	// Fixed nested tree: root (success) -> lint (success), test (failed).
	testSpan := &domain.SpanNode{
		SpanID:       "3333333333333333",
		ParentSpanID: "1111111111111111",
		Name:         "test",
		Status:       "failed",
		Attributes:   map[string]string{"error": "unit test failed"},
		Children:     []*domain.SpanNode{},
	}
	lintSpan := &domain.SpanNode{
		SpanID:       "2222222222222222",
		ParentSpanID: "1111111111111111",
		Name:         "lint",
		Status:       "success",
		Children:     []*domain.SpanNode{},
	}
	rootSpan := &domain.SpanNode{
		SpanID:     "1111111111111111",
		Name:       "build",
		Status:     "success",
		Children:   []*domain.SpanNode{lintSpan, testSpan},
		Attributes: map[string]string{},
	}
	traces := &stepsTraceRepo{trace: &domain.TraceInfo{TraceID: ciStepsTraceID, RootSpan: rootSpan, Status: "success"}}
	logs := &stepsLogRepo{entries: []domain.LogEntry{
		{Timestamp: time.Now().Add(-time.Second), SpanID: "3333333333333333", Line: "1 test failed"},
		{Timestamp: time.Now().Add(-500 * time.Millisecond), SpanID: "3333333333333333", Line: "expected 2, got 1"},
	}}

	srv := handler.NewServer(&handler.ServerConfig{
		ControlAddr: controlAddr,
		DataAddr:    dataAddr,
		DataHost:    "localhost",
		PipelineURL: "https://supv.example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, CacheBackend: cacheBackend,
		VersionResolver: versionResolver, Auth: authSvc, InternalAuthEnabled: true,
		Users: usersSvc, Groups: groupsSvc, Tokens: tokensSvc, Quota: quotaSvc,
		Attribution: attributionSvc, TraceMeta: traceMetaRepo, Traces: traces, Logs: logs, JWT: jwtSvc,
	})

	serverTLS, _ := mintingCA.TLSCertificate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	time.Sleep(500 * time.Millisecond)

	return fmt.Sprintf("http://localhost%s", controlAddr), adminToken
}

// buildCIWrapper compiles the dagger-kubernetes-ci wrapper into a temp dir and
// returns the binary path.
func buildCIWrapper(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "dagger-kubernetes-ci")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/ci")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build dagger-kubernetes-ci: %v\n%s", err, out)
	}
	return bin
}

// installFakeDagger puts a fake `dagger` on PATH that prints the trace id to
// stderr and exits 0.
func installFakeDagger(t *testing.T) {
	t.Helper()
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "dagger")
	script := fmt.Sprintf("#!/bin/sh\necho '%s' >&2\nexit 0\n", ciStepsTraceID)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dagger: %v", err)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", fakeDir, os.PathListSeparator, os.Getenv("PATH")))
}

func parseNDJSON(t *testing.T, out string) []domain.CIEvent {
	t.Helper()
	var events []domain.CIEvent
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e domain.CIEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal ndjson line %q: %v", line, err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan ndjson: %v", err)
	}
	return events
}

// TestCIWrapperStreamsNestedSteps proves the end-to-end contract: with
// --steps, the wrapper emits an ordered NDJSON event stream on stdout
// reconstructing the supervisor's nested span tree (states + logs), and with
// --steps disabled stdout stays empty (backwards compatible).
func TestCIWrapperStreamsNestedSteps(t *testing.T) {
	serverURL, adminToken := startCIStepsServer(t, ":18099", ":18460")
	bin := buildCIWrapper(t)
	installFakeDagger(t)

	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")

	// --- steps enabled ---
	cmd := exec.Command(bin,
		"--server", serverURL,
		"--token", adminToken,
		"--ui-url", "https://supv.example.com",
		"--steps",
		"--steps-poll-interval", "50ms",
		"--config", missingConfig,
		"call", "foo",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dagger-kubernetes-ci (steps): %v\n%s", err, stderr.String())
	}

	events := parseNDJSON(t, stdout.String())
	if len(events) == 0 {
		t.Fatalf("stdout empty, want NDJSON events; stderr=%q", stderr.String())
	}

	started := map[string]*domain.StepNode{}
	finished := map[string]*domain.StepNode{}
	var logChunks []*domain.LogChunk
	var pipelineDone *domain.CIEvent
	for i := range events {
		switch events[i].Type {
		case domain.CIEventNodeStarted:
			started[events[i].Node.ID] = events[i].Node
		case domain.CIEventNodeFinished:
			finished[events[i].Node.ID] = events[i].Node
		case domain.CIEventLogChunk:
			logChunks = append(logChunks, events[i].Log)
		case domain.CIEventPipelineDone:
			pipelineDone = &events[i]
		}
	}

	// Root + two children all started, with correct parent/depth/name.
	root, ok := started["1111111111111111"]
	if !ok {
		t.Fatalf("root node_started missing; started=%v", started)
	}
	if root.ParentID != "" || root.Depth != 0 || root.Name != "build" {
		t.Fatalf("root = %+v", root)
	}
	if lint, ok := started["2222222222222222"]; !ok || lint.ParentID != "1111111111111111" || lint.Depth != 1 || lint.Name != "lint" {
		t.Fatalf("lint node_started wrong: %+v (ok=%v)", lint, ok)
	}
	if testNode, ok := started["3333333333333333"]; !ok || testNode.ParentID != "1111111111111111" || testNode.Depth != 1 || testNode.Name != "test" {
		t.Fatalf("test node_started wrong: %+v (ok=%v)", testNode, ok)
	}

	// States: lint succeeded, test failed (with error), root succeeded.
	if n, ok := finished["2222222222222222"]; !ok || n.State != domain.StepStateSucceeded {
		t.Fatalf("lint finish = %+v (ok=%v)", n, ok)
	}
	if n, ok := finished["3333333333333333"]; !ok || n.State != domain.StepStateFailed {
		t.Fatalf("test finish = %+v (ok=%v)", n, ok)
	}
	if n, ok := finished["1111111111111111"]; !ok || n.State != domain.StepStateSucceeded {
		t.Fatalf("root finish = %+v (ok=%v)", n, ok)
	}

	// Logs attributed to the failed child span.
	if len(logChunks) == 0 {
		t.Fatal("no log_chunk emitted")
	}
	foundLog := false
	for _, lc := range logChunks {
		if lc.NodeID == "3333333333333333" && len(lc.Lines) == 2 {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatalf("log chunks = %+v, want one attributed to 3333… with 2 lines", logChunks)
	}

	// pipeline_done with root-derived status.
	if pipelineDone == nil || pipelineDone.Status != "success" {
		t.Fatalf("pipeline_done = %+v, want status success", pipelineDone)
	}

	// seq is strictly monotonic.
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("seq not monotonic: events[%d].Seq=%d <= events[%d].Seq=%d", i, events[i].Seq, i-1, events[i-1].Seq)
		}
	}

	// --- steps disabled: stdout must stay empty; stderr keeps the link ---
	cmd2 := exec.Command(bin,
		"--server", serverURL,
		"--token", adminToken,
		"--ui-url", "https://supv.example.com",
		"--config", missingConfig,
		"call", "foo",
	)
	var stdout2, stderr2 bytes.Buffer
	cmd2.Stdout = &stdout2
	cmd2.Stderr = &stderr2
	if err := cmd2.Run(); err != nil {
		t.Fatalf("dagger-kubernetes-ci (no steps): %v\n%s", err, stderr2.String())
	}
	if strings.TrimSpace(stdout2.String()) != "" {
		t.Fatalf("stdout = %q, want empty when --steps disabled", stdout2.String())
	}
	wantLink := fmt.Sprintf("Pipeline View: https://supv.example.com/pipelines/%s", ciStepsTraceID)
	if !strings.Contains(stderr2.String(), wantLink) {
		t.Fatalf("stderr = %q, want containing %q", stderr2.String(), wantLink)
	}
}
