package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// hexID returns a valid 16-char hex trace ID built from a single hex byte,
// so tests can use distinct, readable IDs that pass domain.ValidTraceID.
func hexID(c byte) string {
	return strings.Repeat(string(c), 16)
}

// --- fake trace meta repository ---------------------------------------------

type fakeTraceMetaRepo struct {
	mu     sync.Mutex
	traces map[string]*domain.TraceMeta
}

func newFakeTraceMetaRepo() *fakeTraceMetaRepo {
	return &fakeTraceMetaRepo{traces: make(map[string]*domain.TraceMeta)}
}

func (r *fakeTraceMetaRepo) UpsertProvision(context.Context, string, string, string) error {
	return nil
}
func (r *fakeTraceMetaRepo) UpsertIngest(_ context.Context, m *domain.TraceMeta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *m
	r.traces[m.TraceID] = &cp
	return nil
}
func (r *fakeTraceMetaRepo) Get(_ context.Context, traceID string) (*domain.TraceMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.traces[traceID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *m
	return &cp, nil
}
func (r *fakeTraceMetaRepo) List(context.Context, domain.TraceFilter) ([]*domain.TraceListResult, error) {
	return nil, nil
}
func (r *fakeTraceMetaRepo) ListBefore(_ context.Context, cutoff time.Time, protectRunning bool) ([]*domain.TraceMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.TraceMeta
	for _, m := range r.traces {
		key := m.StartedAt
		if key.IsZero() {
			key = m.UpdatedAt
		}
		if key.IsZero() || key.After(cutoff) {
			continue
		}
		if protectRunning && (m.Status == "" || m.Status == "running") {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TraceID < out[j].TraceID })
	return out, nil
}
func (r *fakeTraceMetaRepo) Delete(_ context.Context, traceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.traces, traceID)
	return nil
}
func (r *fakeTraceMetaRepo) MarkFailed(_ context.Context, traceID, reason string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.traces[traceID]
	if !ok {
		return false, nil
	}
	if m.Status == "success" || m.Status == "failed" {
		return false, nil
	}
	m.Status = "failed"
	m.FailureReason = reason
	return true, nil
}
func (r *fakeTraceMetaRepo) Stats(context.Context) (int, time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var oldest time.Time
	for _, m := range r.traces {
		key := m.StartedAt
		if key.IsZero() {
			key = m.UpdatedAt
		}
		if key.IsZero() {
			continue
		}
		if oldest.IsZero() || key.Before(oldest) {
			oldest = key
		}
	}
	return len(r.traces), oldest, nil
}

func (r *fakeTraceMetaRepo) put(t *testing.T, traceID, status string, startedAt time.Time) {
	t.Helper()
	if err := r.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID:   traceID,
		Status:    status,
		StartedAt: startedAt,
		UpdatedAt: startedAt,
	}); err != nil {
		t.Fatalf("put trace: %v", err)
	}
}

// --- helpers -----------------------------------------------------------------

func defaultHistoryGC() domain.HistoryGCConfig {
	return domain.HistoryGCConfig{
		Enabled:  false,
		MaxAge:   720 * time.Hour,
		Schedule: time.Hour,
	}
}

// newHistorySvc wires Loki/VM backends returning the given status codes (0 =
// backend absent).
func newHistorySvc(t *testing.T, repo *fakeTraceMetaRepo, gc domain.HistoryGCConfig, lokiStatus, vmStatus int) *HistoryPurgeService {
	t.Helper()
	var logs domain.LogRepository
	var metrics *repository.MetricsClient
	if lokiStatus != 0 {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(lokiStatus)
		}))
		t.Cleanup(srv.Close)
		logs = repository.NewLogsClient(srv.URL)
	}
	if vmStatus != 0 {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(vmStatus)
		}))
		t.Cleanup(srv.Close)
		metrics = repository.NewMetricsClient(srv.URL)
	}
	return NewHistoryPurgeService(repo, logs, metrics, gc, observ.NewTestLogger(), observ.NewMetrics(nil))
}

// --- tests -------------------------------------------------------------------

func TestPurgeSingleTrace(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	repo.put(t, hexID('a'), "success", time.Now().UTC())
	svc := newHistorySvc(t, repo, defaultHistoryGC(), http.StatusNoContent, http.StatusNoContent)

	res, err := svc.Purge(context.Background(), domain.HistoryPurgeRequest{TraceID: hexID('a')})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.PurgedTraces != 1 || res.LogsDeleted != 1 || res.MetricsDeleted != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.TraceIDs) != 1 || res.TraceIDs[0] != hexID('a') {
		t.Fatalf("trace_ids = %v", res.TraceIDs)
	}
	if _, err := repo.Get(context.Background(), hexID('a')); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trace should be gone: %v", err)
	}
}

func TestPurgeInvalidTraceID(t *testing.T) {
	svc := newHistorySvc(t, newFakeTraceMetaRepo(), defaultHistoryGC(), 0, 0)
	_, err := svc.Purge(context.Background(), domain.HistoryPurgeRequest{TraceID: "not-hex!"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

func TestHistoryPurgeAlreadyPurged(t *testing.T) {
	svc := newHistorySvc(t, newFakeTraceMetaRepo(), defaultHistoryGC(), http.StatusNoContent, http.StatusNoContent)
	res, err := svc.Purge(context.Background(), domain.HistoryPurgeRequest{TraceID: hexID('a')})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.AlreadyPurged != 1 || res.PurgedTraces != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestHistoryPurgeAll(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	base := time.Now().UTC()
	repo.put(t, hexID('a'), "success", base.Add(-3*time.Hour))
	repo.put(t, hexID('b'), "success", base.Add(-2*time.Hour))
	repo.put(t, hexID('c'), "success", base.Add(-time.Minute))
	repo.put(t, hexID('d'), "running", base.Add(-3*time.Hour))

	gc := defaultHistoryGC()
	gc.MaxAge = time.Hour
	svc := newHistorySvc(t, repo, gc, http.StatusNoContent, http.StatusNoContent)

	res, err := svc.PurgeAll(context.Background())
	if err != nil {
		t.Fatalf("PurgeAll: %v", err)
	}
	if res.PurgedTraces != 2 {
		t.Fatalf("purged = %d, want 2 (running protected, recent too new)", res.PurgedTraces)
	}
	if res.LogsDeleted != 2 || res.MetricsDeleted != 2 {
		t.Fatalf("telemetry counts = %+v", res)
	}
	if _, err := repo.Get(context.Background(), hexID('d')); err != nil {
		t.Fatalf("running trace should remain: %v", err)
	}
	if _, err := repo.Get(context.Background(), hexID('c')); err != nil {
		t.Fatalf("recent trace should remain: %v", err)
	}
}

func TestRunGCPurgesOldTraces(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	base := time.Now().UTC()
	for _, c := range []byte{'a', 'b', 'c'} {
		repo.put(t, hexID(c), "success", base.Add(-3*time.Hour))
	}

	gc := defaultHistoryGC()
	gc.Enabled = true
	gc.MaxAge = time.Hour
	svc := newHistorySvc(t, repo, gc, http.StatusNoContent, http.StatusNoContent)

	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTraces != 3 {
		t.Fatalf("purged_traces = %d, want 3", summary.PurgedTraces)
	}
	if summary.LogsDeleted != 3 || summary.MetricsDeleted != 3 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestRunGCProtectsRunning(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	base := time.Now().UTC()
	repo.put(t, hexID('a'), "running", base.Add(-3*time.Hour))
	repo.put(t, hexID('b'), "", base.Add(-3*time.Hour))
	repo.put(t, hexID('c'), "success", base.Add(-3*time.Hour))

	gc := defaultHistoryGC()
	gc.Enabled = true
	gc.MaxAge = time.Hour
	svc := newHistorySvc(t, repo, gc, http.StatusNoContent, http.StatusNoContent)

	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTraces != 1 {
		t.Fatalf("purged_traces = %d, want 1", summary.PurgedTraces)
	}
	if summary.SkippedRunning != 2 {
		t.Fatalf("skipped_running = %d, want 2", summary.SkippedRunning)
	}
	if _, err := repo.Get(context.Background(), hexID('a')); err != nil {
		t.Fatalf("running trace should remain: %v", err)
	}
}

func TestRunGCTelemetryErrorsContinue(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	base := time.Now().UTC()
	repo.put(t, hexID('a'), "success", base.Add(-3*time.Hour))

	gc := defaultHistoryGC()
	gc.Enabled = true
	gc.MaxAge = time.Hour
	// Loki 500, VM 204: the Loki failure must not abort the trace_meta delete.
	svc := newHistorySvc(t, repo, gc, http.StatusInternalServerError, http.StatusNoContent)

	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTraces != 1 {
		t.Fatalf("purged_traces = %d, want 1 (telemetry failure must not abort)", summary.PurgedTraces)
	}
	if summary.TelemetryErrors < 1 {
		t.Fatalf("telemetry_errors = %d, want >= 1", summary.TelemetryErrors)
	}
	if summary.LogsDeleted != 0 || summary.MetricsDeleted != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if _, err := repo.Get(context.Background(), hexID('a')); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trace_meta should still be deleted: %v", err)
	}
}

func TestRunGCEmptyCandidates(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	repo.put(t, hexID('a'), "success", time.Now().UTC())

	gc := defaultHistoryGC()
	gc.Enabled = true
	gc.MaxAge = time.Hour
	svc := newHistorySvc(t, repo, gc, http.StatusNoContent, http.StatusNoContent)

	summary, err := svc.RunGC(context.Background())
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if summary.PurgedTraces != 0 {
		t.Fatalf("purged_traces = %d, want 0", summary.PurgedTraces)
	}
	if summary.Message != "no traces older than max_age" {
		t.Fatalf("message = %q", summary.Message)
	}
}

func TestHistoryGCRulesReflectConfigAndLastRun(t *testing.T) {
	gc := defaultHistoryGC()
	gc.Enabled = true
	gc.MaxAge = 2 * time.Hour
	gc.Schedule = 30 * time.Minute

	svc := newHistorySvc(t, newFakeTraceMetaRepo(), gc, 0, 0)

	rules := svc.GCRules()
	if !rules.Enabled || rules.MaxAge != "2h0m0s" || rules.Schedule != "30m0s" {
		t.Fatalf("rules = %+v", rules)
	}
	if rules.LastRunAt != "" {
		t.Fatalf("last_run_at should be empty before first run, got %q", rules.LastRunAt)
	}

	if _, err := svc.RunGC(context.Background()); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	rules = svc.GCRules()
	if rules.LastRunAt == "" || rules.LastRunSummary == nil {
		t.Fatalf("rules after run = %+v", rules)
	}
	if rules.NextRunAt == "" {
		t.Fatal("next_run_at should be set")
	}
}

func TestHistoryStartGCSweeperDisabled(t *testing.T) {
	svc := newHistorySvc(t, newFakeTraceMetaRepo(), defaultHistoryGC(), 0, 0)
	stop := svc.StartGCSweeper(context.Background())
	stop() // no-op; must not panic
}

func TestHistoryStartGCSweeperEnabled(t *testing.T) {
	gc := defaultHistoryGC()
	gc.Enabled = true
	gc.Schedule = 10 * time.Millisecond

	svc := newHistorySvc(t, newFakeTraceMetaRepo(), gc, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := svc.StartGCSweeper(ctx)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if rules := svc.GCRules(); rules.LastRunAt != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not run within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockingTraceMetaRepo wraps a fake repo and blocks inside Delete so the
// purgeMu serialization can be observed deterministically.
type blockingTraceMetaRepo struct {
	*fakeTraceMetaRepo
	deleteStarted chan struct{}
	release       chan struct{}
}

func (r *blockingTraceMetaRepo) Delete(ctx context.Context, traceID string) error {
	if r.deleteStarted != nil {
		r.deleteStarted <- struct{}{}
		<-r.release
	}
	return r.fakeTraceMetaRepo.Delete(ctx, traceID)
}

func TestPurgeConcurrency(t *testing.T) {
	base := newFakeTraceMetaRepo()
	base.put(t, hexID('a'), "success", time.Now().UTC())
	repo := &blockingTraceMetaRepo{
		fakeTraceMetaRepo: base,
		deleteStarted:     make(chan struct{}, 1),
		release:           make(chan struct{}),
	}
	svc := NewHistoryPurgeService(repo, nil, nil, defaultHistoryGC(), observ.NewTestLogger(), observ.NewMetrics(nil))

	first := make(chan struct{})
	go func() {
		_, _ = svc.Purge(context.Background(), domain.HistoryPurgeRequest{TraceID: hexID('a')})
		close(first)
	}()

	select {
	case <-repo.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first purge never reached Delete")
	}

	second := make(chan struct{})
	go func() {
		_, _ = svc.Purge(context.Background(), domain.HistoryPurgeRequest{TraceID: hexID('a')})
		close(second)
	}()

	// The second purge must block on purgeMu while the first holds it.
	select {
	case <-second:
		t.Fatal("second purge completed while first held purgeMu")
	case <-time.After(50 * time.Millisecond):
	}

	close(repo.release)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("first purge did not finish after release")
	}
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second purge did not finish after release")
	}
}

func TestStatsCached(t *testing.T) {
	repo := newFakeTraceMetaRepo()
	base := time.Now().UTC()
	repo.put(t, hexID('a'), "success", base.Add(-2*time.Hour))
	repo.put(t, hexID('b'), "success", base)

	svc := newHistorySvc(t, repo, defaultHistoryGC(), 0, 0)

	first, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if first.TraceCount != 2 {
		t.Fatalf("trace_count = %d, want 2", first.TraceCount)
	}
	if first.OldestUpdatedAt == "" || first.CollectedAt == "" {
		t.Fatalf("stats = %+v", first)
	}

	second, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats 2: %v", err)
	}
	if first != second {
		t.Fatal("second Stats() should return the cached pointer")
	}

	// Add a trace and force TTL expiry: the next Stats() must re-probe.
	repo.put(t, hexID('c'), "success", base.Add(-time.Minute))
	svc.cachedAt = time.Now().Add(-2 * historyStatsTTL)
	third, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats 3: %v", err)
	}
	if third == first {
		t.Fatal("third Stats() should re-probe after TTL expiry")
	}
	if third.TraceCount != 3 {
		t.Fatalf("trace_count after expiry = %d, want 3", third.TraceCount)
	}
}
