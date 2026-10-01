package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const execTraceID = "0123456789abcdef0123456789abcdef"

// dagCallWithExec is the verbatim dagger.io/dag.call value emitted by engine
// v0.21.8 for a Container.withExec call whose args are
// `sh -c "echo hello-from-issue60; exit 0"`.
const dagCallWithExec = "ChV4eGgzOmI0M2Q2MWM2YWI0YTE5NDkSDQoJQ29udGFpbmVyGAEaCHdpdGhFeGVjIjkKBGFyZ3MSMUIvCgQ6AnNoCgQ6Ai1jCiE6H2VjaG8gaGVsbG8tZnJvbS1pc3N1ZTYwOyBleGl0IDBKFXh4aDM6ZTNiYTg1ZjU1NzFkN2I4MlIHdjAuMjEuOA=="

// fakeExecTempo serves a root -> {exec, plain} span tree for execTraceID. The
// exec span carries Dagger exec attributes (argv array, cwd, env, secret env)
// and a "Container exited" event with the exit code; the plain span carries
// none.
func fakeExecTempo(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fmt.Sprintf("/api/traces/%s", execTraceID) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"batches": [{
				"scopeSpans": [{
					"spans": [
						{"spanId":"cm9vdA==","traceId":%q,"name":"build","startTimeUnixNano":"1000000000","endTimeUnixNano":"9000000000","status":{"code":"STATUS_CODE_OK"}},
						{"spanId":"ZXhlYw==","parentSpanId":"cm9vdA==","traceId":%q,"name":"exec.run","startTimeUnixNano":"2000000000","endTimeUnixNano":"5000000000","status":{"code":"STATUS_CODE_OK"},
						 "attributes":[
							{"key":"dagger.io/exec.args","value":{"arrayValue":{"values":[{"stringValue":"go"},{"stringValue":"build"},{"stringValue":"./..."}]}}},
							{"key":"dagger.io/exec.cwd","value":{"stringValue":"/src"}},
							{"key":"dagger.io/exec.env","value":{"arrayValue":{"values":[{"stringValue":"FOO=bar"},{"stringValue":"DB_PASSWORD=supersecret123"}]}}},
							{"key":"dagger.io/exec.secret.env","value":{"arrayValue":{"values":[{"stringValue":"DB_PASSWORD"}]}}}
						 ],
						 "events":[
							{"name":"Container exited","timeUnixNano":"5000000000","attributes":[{"key":"exit.code","value":{"intValue":"0"}}]}
						 ]},
						{"spanId":"ZGFnY2FsbA==","parentSpanId":"cm9vdA==","traceId":%q,"name":"Container.withExec","startTimeUnixNano":"5500000000","endTimeUnixNano":"5800000000","status":{"code":"STATUS_CODE_OK"},
						 "attributes":[
							{"key":"dagger.io/dag.call","value":{"stringValue":%q}}
						 ]},
						{"spanId":"cGxhaW4=","parentSpanId":"cm9vdA==","traceId":%q,"name":"compile","startTimeUnixNano":"6000000000","endTimeUnixNano":"7000000000","status":{"code":"STATUS_CODE_OK"}}
					]
				}]
			}]
		}`, execTraceID, execTraceID, execTraceID, dagCallWithExec, execTraceID)
	}))
}

// startPipelineExecServer boots a supervisor wired to the fake Tempo backend
// and returns the control URL + admin token.
func startPipelineExecServer(t *testing.T) (controlURL, adminToken string) {
	t.Helper()
	tempo := fakeExecTempo(t)
	t.Cleanup(tempo.Close)

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
	adminToken, _, err = tokensSvc.Generate(context.Background(), admin.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := traceMetaRepo.UpsertProvision(context.Background(), execTraceID, admin.ID, "v0.19.0"); err != nil {
		t.Fatalf("seed trace meta: %v", err)
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
		Attribution: attributionSvc, TraceMeta: traceMetaRepo,
		Traces: repository.NewSpanTreeReconstructor(tempo.URL),
		JWT:    jwtSvc,
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

	return fmt.Sprintf("http://localhost%s", controlAddr), adminToken
}

// TestPipelineExecEndpoint proves the end-to-end exec contract against a real
// Tempo client: the exec span's JSON carries command/args/env/exit_code, a
// non-exec span omits exec, and secret values never appear in the response.
func TestPipelineExecEndpoint(t *testing.T) {
	controlURL, token := startPipelineExecServer(t)

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/traces/%s", controlURL, execTraceID), http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET trace: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// The fixture declares DB_PASSWORD as a secret AND gives it a value in the
	// env array ("supersecret123", which legitimately appears inside the raw
	// dagger.io/exec.env attribute). The derived secret entry must carry no
	// value: its JSON shape is {"name":"DB_PASSWORD","is_secret":true}, so the
	// leaked shape {"name":"DB_PASSWORD","value":...} must be absent.
	if strings.Contains(string(raw), `DB_PASSWORD","value"`) {
		t.Fatal("secret value leaked into the derived exec view")
	}

	var trace domain.TraceInfo
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if trace.RootSpan == nil {
		t.Fatal("nil root span")
	}

	byID := map[string]*domain.SpanNode{}
	var walk func(n *domain.SpanNode)
	walk = func(n *domain.SpanNode) {
		if n == nil {
			return
		}
		byID[n.SpanID] = n
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(trace.RootSpan)

	execSpan := byID["ZXhlYw=="]
	if execSpan == nil || execSpan.Exec == nil {
		t.Fatalf("exec span missing exec view-model: %+v", execSpan)
	}
	if execSpan.Exec.Command != "go" {
		t.Fatalf("command = %q, want go", execSpan.Exec.Command)
	}
	if len(execSpan.Exec.Args) != 3 || execSpan.Exec.Args[2] != "./..." {
		t.Fatalf("args = %v, want [go build ./...]", execSpan.Exec.Args)
	}
	if execSpan.Exec.Cwd != "/src" {
		t.Fatalf("cwd = %q, want /src", execSpan.Exec.Cwd)
	}
	if execSpan.Exec.ExitCode == nil || *execSpan.Exec.ExitCode != 0 {
		t.Fatalf("exit_code = %v, want 0", execSpan.Exec.ExitCode)
	}
	var secret bool
	for _, e := range execSpan.Exec.Env {
		if e.Name == "DB_PASSWORD" {
			secret = e.IsSecret && e.Value == ""
		}
	}
	if !secret {
		t.Fatalf("DB_PASSWORD not rendered as secret: %+v", execSpan.Exec.Env)
	}

	// The real engine carries exec argv in dagger.io/dag.call, not in a
	// dedicated argv attribute; the endpoint must derive it from the call.
	dagCallSpan := byID["ZGFnY2FsbA=="]
	if dagCallSpan == nil || dagCallSpan.Exec == nil {
		t.Fatalf("dag.call span missing exec view-model: %+v", dagCallSpan)
	}
	if dagCallSpan.Exec.Command != "sh" {
		t.Fatalf("dag.call command = %q, want sh", dagCallSpan.Exec.Command)
	}
	if len(dagCallSpan.Exec.Args) == 0 {
		t.Fatal("dag.call args empty, want non-empty")
	}

	plainSpan := byID["cGxhaW4="]
	if plainSpan == nil {
		t.Fatal("plain span missing")
	}
	if plainSpan.Exec != nil {
		t.Fatalf("plain span exec = %+v, want nil", plainSpan.Exec)
	}
}
