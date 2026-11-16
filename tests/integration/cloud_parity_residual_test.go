package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/handler"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

type parityServer struct {
	base    string
	store   *repository.RaftStore
	admin   string
	users   *service.UserService
	groups  *service.GroupService
	tokens  *service.TokenService
	orgs    *service.OrgService
	secrets *service.SecretService
	checks  *service.ChecksService
	flows   *service.WorkflowService
}

func newParityServer(t *testing.T, graphqlCfg domain.GraphQLConfig, otlp *service.OTLPStreamService, remote *service.RemoteCacheService, checkRunner domain.CheckJobRunner, flowRunner domain.WorkflowJobRunner) *parityServer {
	t.Helper()
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	ctx := context.Background()

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	tokenRepo := repository.NewTokenRepo(store)
	traceMetaRepo := repository.NewTraceMetaRepo(store)
	orgRepo := repository.NewOrgRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	secretRepo := repository.NewSecretRepo(store)
	forgeRepo := repository.NewForgeSourceRepo(store)
	envRepo := repository.NewEnvironmentRepo(store)
	workflowRepo := repository.NewWorkflowRepo(store)
	checkRunRepo := repository.NewCheckRunRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	groupsSvc := service.NewGroupService(groupRepo, userRepo, logger)
	tokensSvc := service.NewTokenService(tokenRepo, logger, []byte("0123456789abcdef0123456789abcdef"))
	jwtSvc := service.NewJWTService([]byte("cloud-parity-secret-32-bytes-ok!!"), 15*time.Minute, 168*time.Hour)
	authSvc := service.NewAuthService(usersSvc, groupRepo, tokensSvc, jwtSvc, nil, logger)
	deviceSvc := service.NewDeviceAuthService(service.NewMemoryDeviceAuthStore(), usersSvc, groupRepo, jwtSvc, 10*time.Minute, 5*time.Second, logger)
	orgsSvc := service.NewOrgService(orgRepo, groupRepo, tokensSvc, logger)
	projectsSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	secretSvc := service.NewSecretService(secretRepo, []byte("0123456789abcdef0123456789abcdef"), logger)
	graphql := repository.NewCloudGraphQL(userRepo, orgRepo, traceMetaRepo, checkRunRepo, graphqlCfg)

	admin, err := usersSvc.Create(ctx, "admin", "password123", domain.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := tokensSvc.Generate(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := orgsSvc.EnsureDefault(ctx); err != nil {
		t.Fatal(err)
	}

	workflowSvc := service.NewWorkflowService(workflowRepo, envRepo, orgsSvc, projectsSvc, forgeRepo, repository.NewWorkflowFileParser(), repository.NewWorkflowExprEvaluator(), nil, flowRunner, secretSvc, service.WorkflowServiceConfig{DefaultTimeout: time.Minute, MaxParallel: 1}, logger)
	forgeProviders := []domain.ForgeProvider{repository.NewGitHubForge(""), repository.NewGitLabForge(""), repository.NewGiteaForge("")}
	checksSvc := service.NewChecksService(forgeRepo, secretSvc, projectsSvc, orgsSvc, checkRunner, checkRunRepo, workflowSvc, forgeProviders, "https://example.com", logger)

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
		PipelineURL:      "https://example.com",
	}, &handler.Deps{
		Logger: logger, Metrics: observ.NewMetrics(nil), MintingCA: mintingCA,
		FleetManager: fleetManager, Sessions: sessions, SessionRegistry: repository.NewSessionRepo(store),
		VersionResolver: versionResolver, Auth: authSvc, Users: usersSvc, Groups: groupsSvc,
		Tokens: tokensSvc, TraceMeta: traceMetaRepo, JWT: jwtSvc, DeviceAuth: deviceSvc,
		CloudGraphQL: graphql, OTLPStream: otlp, RemoteCache: remote,
		Orgs: orgsSvc, Projects: projectsSvc, Secrets: secretSvc, Forge: service.NewForgeService(forgeRepo, secretSvc, nil, "https://example.com", logger),
		Checks: checksSvc, Workflows: workflowSvc, Environments: service.NewEnvironmentService(envRepo, logger),
	})
	serverTLS, _ := mintingCA.TLSCertificate()
	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(startCtx, serverTLS); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	time.Sleep(300 * time.Millisecond)
	return &parityServer{
		base: fmt.Sprintf("http://%s", control.Addr().String()), store: store, admin: adminToken,
		users: usersSvc, groups: groupsSvc, tokens: tokensSvc, orgs: orgsSvc, secrets: secretSvc,
		checks: checksSvc, flows: workflowSvc,
	}
}

func (p *parityServer) do(t *testing.T, method, path string, body any) (resp *http.Response, data []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, p.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.admin))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, data
}

func (p *parityServer) doWithRunToken(t *testing.T, path string, body any, runToken string) (resp *http.Response, data []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(http.MethodPost, p.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.admin))
	req.Header.Set("X-Dagger-Run-Token", runToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, data
}

func TestGraphQLSecurityLimits(t *testing.T) {
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 3, MaxSelections: 5, MaxQueryBytes: 80, MaxParallelism: 10}, nil, nil, nil, nil)
	resp, body := srv.do(t, http.MethodPost, "/query", map[string]any{"query": `{ __schema { types { name } } }`}) //nolint:bodyclose // srv.do closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if bytes.Contains(body, []byte(`"types"`)) {
		t.Fatalf("introspection must be disabled: %s", body)
	}
	_, body = srv.do(t, http.MethodPost, "/query", map[string]any{"query": `{ user { orgs { traces { nodes { id } } } } }`}) //nolint:bodyclose // srv.do closes the body
	if !bytes.Contains(body, []byte("errors")) {
		t.Fatalf("deep query must be rejected: %s", body)
	}
}

func TestOTLPStreamIncrementalAndFailureRecovery(t *testing.T) {
	archive := repository.NewMemoryOTLPArchive()
	otlp := service.NewOTLPStreamService(archive, nil, observ.NewTestLogger())
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, otlp, nil, nil, nil)
	traceID := "0123456789abcdef0123456789abcdef"
	for i := 0; i < 3; i++ {
		otlp.Ingest(context.Background(), traceID, "traces", []byte{byte(i)})
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/traces/%s?listen=false", srv.base, traceID), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", srv.admin))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var seqs []uint64
	for {
		frame, err := domain.ReadOTLPFrame(resp.Body)
		if err != nil {
			break
		}
		if frame.Type == domain.FrameTerminal {
			break
		}
		seqs = append(seqs, frame.Seq)
	}
	if len(seqs) != 3 {
		t.Fatalf("seqs = %v", seqs)
	}
	for i, seq := range seqs {
		if seq != uint64(i) {
			t.Fatalf("seq[%d] = %d", i, seq)
		}
	}
}

func TestRemoteCachePackRange206AndRevocationClose(t *testing.T) {
	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		spec := strings.TrimPrefix(rangeHdr, "bytes=")
		parts := strings.SplitN(spec, "-", 2)
		start, _ := strconv.Atoi(parts[0])
		end, _ := strconv.Atoi(parts[1])
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer s3srv.Close()
	u, _ := url.Parse(s3srv.URL)
	store, err := repository.NewS3RemoteCacheStore(u.Host, "us-east-1", "key", "secret", false, "", "bucket", "prefix", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	index, err := repository.OpenBboltCacheIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	packKey := "prefix/acme/packs/packdigest.pack"
	if err := index.Put("acme", &repository.CacheIndexEntry{Digest: "sha256:member", PackDigest: "packdigest", PackKey: packKey, PackOffset: 10, Size: 5}); err != nil {
		t.Fatal(err)
	}
	remote := service.NewRemoteCacheService(index, store, time.Hour, 1<<30, 4, 4<<20, time.Minute, observ.NewMetrics(nil), observ.NewTestLogger())
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, remote, nil, nil)

	wsURL := "ws" + strings.TrimPrefix(srv.base, "http") + "/v1/engine"
	header := http.Header{"Authorization": []string{fmt.Sprintf("Bearer %s", srv.admin)}, "X-Dagger-Org": []string{"acme"}}
	conn, dialResp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatal(err)
	}
	if dialResp != nil {
		_ = dialResp.Body.Close()
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, mustEnvelope(t, domain.RCTypeHello, domain.Hello{Version: "v1"})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, mustEnvelope(t, domain.RCTypeMerge, domain.Merge{SessionID: "s", Digests: []string{"sha256:member"}})); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var merged domain.Merged
	if _, err := domain.DecodeRemoteCacheEnvelope(raw, &merged); err != nil {
		t.Fatal(err)
	}
	loc, ok := merged.Locations["sha256:member"]
	if !ok || loc.Headers["Range"] != "bytes=10-14" {
		t.Fatalf("location = %+v", loc)
	}
	req, _ := http.NewRequest(http.MethodGet, loc.URL, http.NoBody)
	req.Header.Set("Range", loc.Headers["Range"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abcde" {
		t.Fatalf("body = %q", body)
	}

	// Revoke the token and wait for the 4001 close on the next ping.
	if err := srv.tokens.Revoke(context.Background(), srv.adminUserID(t)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		if ce, ok := err.(*websocket.CloseError); ok && ce.Code == 4001 {
			return
		}
		t.Fatalf("close error = %v", err)
	}
}

func mustEnvelope(t *testing.T, typ string, payload any) []byte {
	t.Helper()
	raw, err := domain.NewRemoteCacheEnvelope(typ, payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (p *parityServer) adminUserID(t *testing.T) string {
	t.Helper()
	users, err := p.users.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Username == "admin" {
			return u.ID
		}
	}
	t.Fatal("admin user not found")
	return ""
}

type recordingCheckRunner struct {
	mu   sync.Mutex
	runs []*domain.CheckRunSpec
}

func (r *recordingCheckRunner) Run(_ context.Context, spec *domain.CheckRunSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, spec)
	return nil
}

func (r *recordingCheckRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func (r *recordingCheckRunner) tokenFor(runID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, spec := range r.runs {
		if spec.ID == runID {
			return spec.RunToken
		}
	}
	return ""
}

func (r *recordingCheckRunner) CancelSuperseded(context.Context, string, string) error { return nil }
func (r *recordingCheckRunner) JobStatus(context.Context, string) (domain.JobState, error) {
	return domain.JobStateActive, nil
}

type recordingWorkflowRunner struct {
	mu    sync.Mutex
	specs []*domain.WorkflowCallSpec
}

func (r *recordingWorkflowRunner) RunCall(_ context.Context, spec *domain.WorkflowCallSpec) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = append(r.specs, spec)
	return map[string]string{"tag": "v1.2.3"}, nil
}

func (r *recordingWorkflowRunner) snapshot() []*domain.WorkflowCallSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*domain.WorkflowCallSpec(nil), r.specs...)
}

func (r *recordingWorkflowRunner) tokenFor(runID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, spec := range r.specs {
		if spec.RunID == runID {
			return spec.RunToken
		}
	}
	return ""
}

func signGitHub(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestEndToEndWebhookChecksAndWorkflowApproval(t *testing.T) {
	checkRunner := &recordingCheckRunner{}
	flowRunner := &recordingWorkflowRunner{}
	srv := newParityServer(t, domain.GraphQLConfig{MaxDepth: 15, MaxSelections: 250, MaxQueryBytes: 65536, MaxParallelism: 10}, nil, nil, checkRunner, flowRunner)
	ctx := context.Background()

	if _, err := srv.orgs.Create(ctx, service.OrgInput{Name: "acme", DisplayName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	groupRepo := repository.NewGroupRepo(srv.store)
	group := &domain.Group{ID: "release-approvers", OrgID: "acme", Name: "release-approvers", AgentAvailable: true}
	if err := groupRepo.Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	approver, err := srv.users.Create(ctx, "approver", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := groupRepo.SetMembers(ctx, group.ID, []string{approver.ID}); err != nil {
		t.Fatal(err)
	}
	approverToken, _, err := srv.tokens.Generate(ctx, approver.ID)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := srv.users.Create(ctx, "outsider", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	outsiderToken, _, err := srv.tokens.Generate(ctx, outsider.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Protected environment + secrets.
	if resp, body := srv.do(t, http.MethodPut, "/api/v1/orgs/acme/environments/prod", map[string]any{ //nolint:bodyclose // srv.do closes the body
		"approver_group_ids": []string{group.ID}, "prevent_self_approval": true, "approval_timeout": "24h",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("environment put status = %d body=%s", resp.StatusCode, body)
	}
	if _, err := srv.secrets.Put(ctx, domain.SecretScopeOrg, "acme", "ORG_KEY", "org-value"); err != nil {
		t.Fatal(err)
	}
	proj, err := service.NewProjectService(repository.NewProjectRepo(srv.store), groupRepo, observ.NewTestLogger()).GetOrCreateByName(ctx, "acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.secrets.Put(ctx, domain.SecretScopeProject, proj.ID, "PROJECT_KEY", "project-value"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.secrets.PutEnvironment(ctx, "acme", "prod", "PROD_KEY", "prod-value"); err != nil {
		t.Fatal(err)
	}

	// Forge source with a sealed webhook secret.
	webhookSecret := "webhook-secret"
	tokenCT, _ := srv.secrets.SealRaw(domain.SecretScopeOrg, "acme", "forge-token")
	secretCT, _ := srv.secrets.SealRaw(domain.SecretScopeOrg, "acme", webhookSecret)
	src := &domain.ForgeSource{ID: "src-1", OrgID: "acme", Provider: domain.ForgeGitHub, Repo: "acme/repo", TokenCiphertext: tokenCT, WebhookSecretCiphertext: secretCT}
	if err := repository.NewForgeSourceRepo(srv.store).Upsert(ctx, src); err != nil {
		t.Fatal(err)
	}

	// Fork PR webhook holds for approval.
	forkBody := []byte(`{"ref":"refs/pull/1/head","after":"fork-sha","repository":{"full_name":"acme/repo"},"pull_request":{"number":1,"head":{"sha":"fork-sha","repo":{"id":1}},"base":{"repo":{"id":2}}}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.base+"/v1/webhooks/github/src-1", bytes.NewReader(forkBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signGitHub(webhookSecret, forkBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fork webhook status = %d", resp.StatusCode)
	}
	if checkRunner.count() != 0 {
		t.Fatal("fork PR must not launch a check job")
	}
	_, body := srv.do(t, http.MethodGet, "/api/v1/orgs/acme/checks?status=waiting_approval", nil) //nolint:bodyclose // srv.do closes the body
	var forkRuns []*domain.CheckRunRecord
	if err := json.Unmarshal(body, &forkRuns); err != nil || len(forkRuns) != 1 {
		t.Fatalf("fork runs = %s err=%v", body, err)
	}
	if _, body := srv.do(t, http.MethodPost, fmt.Sprintf("/api/v1/checks/runs/%s/approve", forkRuns[0].ID), map[string]any{"include_secrets": false}); !bytes.Contains(body, []byte(`"status":"running"`)) { //nolint:bodyclose // srv.do closes the body
		t.Fatalf("approve fork = %s", body)
	}
	if checkRunner.count() != 1 {
		t.Fatalf("check runs = %d", len(checkRunner.runs))
	}

	// Default-branch push: load + complete starts the after_checks workflow.
	pushBody := []byte(`{"ref":"refs/heads/main","after":"main-sha","repository":{"full_name":"acme/repo","default_branch":"main"}}`)
	req, _ = http.NewRequest(http.MethodPost, srv.base+"/v1/webhooks/github/src-1", bytes.NewReader(pushBody))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signGitHub(webhookSecret, pushBody))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_, body = srv.do(t, http.MethodGet, "/api/v1/orgs/acme/checks?status=running", nil) //nolint:bodyclose // srv.do closes the body
	var pushRuns []*domain.CheckRunRecord
	if err := json.Unmarshal(body, &pushRuns); err != nil || len(pushRuns) == 0 {
		t.Fatalf("push runs = %s err=%v", body, err)
	}
	runID := pushRuns[0].ID
	workflowYAML := `name: release
on:
  push: true
after_checks: true
jobs:
  build:
    call: build
    outputs:
      tag: "v1.2.3"
  deploy-prod:
    call: deploy
    needs: [build]
    environment: prod
    secrets: [PROD_KEY]
  trigger-infra:
    trigger:
      repo: acme/infra
      workflow: deploy
      wait: false
`
	checkToken := checkRunner.tokenFor(runID)
	if checkToken == "" {
		t.Fatal("push check run did not receive a per-run token")
	}
	if _, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/load", runID), map[string]any{ //nolint:bodyclose // srv.doWithRunToken closes the body
		"checks": []string{"test"}, "workflow_files": map[string]string{"release.yaml": workflowYAML},
	}, checkToken); !bytes.Contains(body, []byte(`"checks"`)) {
		t.Fatalf("load = %s", body)
	}
	if _, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/checks/runs/%s/complete", runID), map[string]any{ //nolint:bodyclose // srv.doWithRunToken closes the body
		"checks": []map[string]any{{"name": "test", "state": "success", "trace_id": "trace-1"}},
	}, checkToken); !bytes.Contains(body, []byte(`"status":"success"`)) {
		t.Fatalf("complete = %s", body)
	}

	// The workflow run dispatches build and pauses deploy-prod on its
	// environment gate once build completes.
	_, body = srv.do(t, http.MethodGet, "/api/v1/orgs/acme/workflows/runs", nil) //nolint:bodyclose // srv.do closes the body
	var runs []*domain.WorkflowRun
	if err := json.Unmarshal(body, &runs); err != nil || len(runs) == 0 {
		t.Fatalf("workflow runs = %s err=%v", body, err)
	}
	run := runs[0]
	if run.Jobs["build"].Status != "running" {
		t.Fatalf("build status = %+v", run.Jobs["build"])
	}
	workflowToken := flowRunner.tokenFor(run.ID)
	if workflowToken == "" {
		t.Fatal("workflow run did not receive a per-run token")
	}
	if _, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/workflows/runs/%s/jobs/build/complete", run.ID), map[string]any{ //nolint:bodyclose // srv.doWithRunToken closes the body
		"outputs": map[string]string{"tag": "v1.2.3"},
	}, workflowToken); !bytes.Contains(body, []byte(`"waiting_approval"`)) {
		t.Fatalf("build complete = %s", body)
	}
	_, body = srv.do(t, http.MethodGet, fmt.Sprintf("/api/v1/workflows/runs/%s", run.ID), nil) //nolint:bodyclose // srv.do closes the body
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatalf("get run = %s err=%v", body, err)
	}
	if run.Jobs["deploy-prod"].Status != "waiting_approval" {
		t.Fatalf("deploy status = %+v", run.Jobs["deploy-prod"])
	}

	// Non-approver is rejected; approver succeeds and injects the env secret.
	req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/workflows/runs/%s/approve/deploy-prod", srv.base, run.ID), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", outsiderToken))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outsider approve status = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/workflows/runs/%s/approve/deploy-prod", srv.base, run.ID), http.NoBody)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", approverToken))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	approvedBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approver status = %d body=%s", resp.StatusCode, approvedBody)
	}
	var approved domain.WorkflowRun
	if err := json.Unmarshal(approvedBody, &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Jobs["deploy-prod"].Status != "running" {
		t.Fatalf("deploy = %+v", approved.Jobs["deploy-prod"])
	}
	foundSecret := false
	for _, spec := range flowRunner.snapshot() {
		if spec.JobName == "deploy-prod" && spec.Secrets["PROD_KEY"] == "prod-value" {
			foundSecret = true
		}
	}
	if !foundSecret {
		t.Fatalf("deploy did not receive the environment secret: %+v", flowRunner.snapshot())
	}
	// Complete deploy-prod so the run finishes.
	if _, body := srv.doWithRunToken(t, fmt.Sprintf("/api/v1/workflows/runs/%s/jobs/deploy-prod/complete", run.ID), map[string]any{ //nolint:bodyclose // srv.doWithRunToken closes the body
		"outputs": map[string]string{},
	}, workflowToken); !bytes.Contains(body, []byte(`"success"`)) {
		t.Fatalf("deploy complete = %s", body)
	}
}
