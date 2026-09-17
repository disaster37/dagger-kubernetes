package integration

import (
	"context"
	"encoding/json"
	"fmt"
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

const searchTraceID = "abcdef0123456789abcdef0123456789"

// fakeTempo serves a nested root -> child -> grandchild span tree for
// searchTraceID. Span IDs are base64 (Tempo form) so they match the Loki
// span_id labels the fake Loki returns.
func fakeTempo(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fmt.Sprintf("/api/traces/%s", searchTraceID) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"batches": [{
				"scopeSpans": [{
					"spans": [
						{"spanId":"cm9vdA==","traceId":"` + searchTraceID + `","name":"build","startTimeUnixNano":"1000000000","endTimeUnixNano":"9000000000","status":{"code":"STATUS_CODE_OK"}},
						{"spanId":"Y2hpbGQ=","parentSpanId":"cm9vdA==","traceId":"` + searchTraceID + `","name":"compile","startTimeUnixNano":"2000000000","endTimeUnixNano":"5000000000","status":{"code":"STATUS_CODE_OK"}},
						{"spanId":"Z3JhbmQ=","parentSpanId":"Y2hpbGQ=","traceId":"` + searchTraceID + `","name":"run","startTimeUnixNano":"3000000000","endTimeUnixNano":"4000000000","status":{"code":"STATUS_CODE_ERROR"}}
					]
				}]
			}]
		}`))
	}))
}

// fakeLoki serves span-correlated log lines for searchTraceID, honouring the
// start/end/limit query params so pagination can be exercised.
func fakeLoki(t *testing.T) *httptest.Server {
	t.Helper()
	base := time.Now().Add(-time.Hour).UnixNano()
	records := []struct {
		ts     int64
		spanID string
		line   string
	}{
		{base + 100, "cm9vdA==", "root line"},
		{base + 200, "Y2hpbGQ=", "compile error: boom"},
		{base + 300, "Z3JhbmQ=", "run error: crash"},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		start := parseInt64Default(q.Get("start"), 0)
		end := parseInt64Default(q.Get("end"), 0)
		limit := parseInt64Default(q.Get("limit"), 1000)

		type stream struct {
			spanID string
			values []string
		}
		var streams []stream
		index := map[string]int{}
		written := int64(0)
		for _, rec := range records {
			if rec.ts < start || rec.ts > end || written >= limit {
				continue
			}
			idx, ok := index[rec.spanID]
			if !ok {
				idx = len(streams)
				index[rec.spanID] = idx
				streams = append(streams, stream{spanID: rec.spanID})
			}
			streams[idx].values = append(streams[idx].values, fmt.Sprintf(`["%d",%q]`, rec.ts, rec.line))
			written++
		}

		var result []string
		for _, st := range streams {
			result = append(result, fmt.Sprintf(
				`{"stream":{"trace_id":%q,"span_id":%q},"values":[%s]}`,
				searchTraceID, st.spanID, strings.Join(st.values, ",")))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"result":[%s]}}`, strings.Join(result, ","))
	}))
}

func parseInt64Default(s string, def int64) int64 {
	if s == "" {
		return def
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}

// startPipelineSearchServer boots a supervisor wired to the fake Tempo + Loki
// backends and returns the control URL + admin token.
func startPipelineSearchServer(t *testing.T) (controlURL, adminToken string) {
	t.Helper()
	tempo := fakeTempo(t)
	t.Cleanup(tempo.Close)
	loki := fakeLoki(t)
	t.Cleanup(loki.Close)

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
	if err := traceMetaRepo.UpsertProvision(context.Background(), searchTraceID, admin.ID, "v0.19.0"); err != nil {
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
		Logs:   repository.NewLogsClient(loki.URL),
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

func searchRequest(t *testing.T, controlURL, token, query string) (*http.Response, domain.LogSearchPage) {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/traces/%s/search%s", controlURL, searchTraceID, query), http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET search: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp, domain.LogSearchPage{}
	}
	var page domain.LogSearchPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, page
}

// TestPipelineSearchEndpoint proves the end-to-end search contract against real
// Tempo/Loki clients: whole-trace keyword search, subtree scoping, pagination,
// invalid regex, and unknown span.
func TestPipelineSearchEndpoint(t *testing.T) {
	controlURL, token := startPipelineSearchServer(t)

	// Whole-trace keyword search.
	resp, page := searchRequest(t, controlURL, token, "?q=error")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("whole-trace status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(page.Entries) != 2 {
		t.Fatalf("whole-trace entries = %d, want 2", len(page.Entries))
	}

	// Scoped to the compile child: only its subtree logs (compile + run).
	resp, page = searchRequest(t, controlURL, token, "?span_id=Y2hpbGQ=&q=error")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scoped status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(page.Entries) != 2 {
		t.Fatalf("scoped entries = %d, want 2", len(page.Entries))
	}

	// Scoped to the run grandchild: only its own log.
	resp, page = searchRequest(t, controlURL, token, "?span_id=Z3JhbmQ=&q=error")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grandchild status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(page.Entries) != 1 || page.Entries[0].SpanID != "Z3JhbmQ=" {
		t.Fatalf("grandchild entries = %+v, want one run log", page.Entries)
	}

	// Pagination: limit=1 returns one entry + a cursor; the next page returns
	// the second entry.
	resp, first := searchRequest(t, controlURL, token, "?q=error&limit=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(first.Entries) != 1 {
		t.Fatalf("page1 entries = %d, want 1", len(first.Entries))
	}
	if first.Next == 0 {
		t.Fatal("page1 next = 0, want a cursor")
	}
	resp, second := searchRequest(t, controlURL, token, fmt.Sprintf("?q=error&limit=1&cursor=%d", first.Next))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if len(second.Entries) != 1 {
		t.Fatalf("page2 entries = %d, want 1", len(second.Entries))
	}
	if second.Entries[0].Line == first.Entries[0].Line {
		t.Fatalf("page2 repeated page1 entry %q", second.Entries[0].Line)
	}

	// Invalid regex -> 400.
	resp, _ = searchRequest(t, controlURL, token, "?mode=regex&q=%28%5B")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid regex status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unknown span -> 404.
	resp, _ = searchRequest(t, controlURL, token, "?span_id=bm9wZQ==")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown span status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
