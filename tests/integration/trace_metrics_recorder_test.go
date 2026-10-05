package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

// fakeRecorderVM answers instant queries with a finite vector and captures
// import bodies.
type fakeRecorderVM struct {
	mu      sync.Mutex
	imports []string
}

func (f *fakeRecorderVM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/query":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[100,"1.5"]}]}}`))
		case "/api/v1/import":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.imports = append(f.imports, string(body))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeRecorderVM) imported() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.imports, "\n")
}

func newRecorder(t *testing.T, vmURL string, store *repository.RaftStore) *service.TraceMetricsRecorder {
	t.Helper()
	return service.NewTraceMetricsRecorder(
		repository.NewMetricsClient(vmURL),
		repository.NewTraceMetaRepo(store),
		"dagger-kubernetes",
		time.Minute, time.Minute, 24*time.Hour,
		func() bool { return true },
		observ.NewTestLogger(),
	)
}

// TestTraceMetricsRecorderWritesTaggedSamples proves the recorder samples a
// running trace and imports dagger_engine_* samples tagged with trace_id and
// version.
func TestTraceMetricsRecorderWritesTaggedSamples(t *testing.T) {
	vm := &fakeRecorderVM{}
	srv := httptest.NewServer(vm.handler())
	defer srv.Close()

	store := newIntegrationStore(t)
	repo := repository.NewTraceMetaRepo(store)
	const traceID = "abcdef0123456789abcdef0123456789"
	if err := repo.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID: traceID, Version: "v0.21.4", Status: "running", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	newRecorder(t, srv.URL, store).Run(context.Background())

	body := vm.imported()
	for _, want := range []string{"dagger_engine_cpu", "dagger_engine_memory", `"trace_id":"` + traceID + `"`, `"version":"v0.21.4"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("import body missing %q:\n%s", want, body)
		}
	}
}

// TestTraceMetricsRecorderSkipsTerminalTrace proves a terminal trace is not
// sampled.
func TestTraceMetricsRecorderSkipsTerminalTrace(t *testing.T) {
	vm := &fakeRecorderVM{}
	srv := httptest.NewServer(vm.handler())
	defer srv.Close()

	store := newIntegrationStore(t)
	repo := repository.NewTraceMetaRepo(store)
	if err := repo.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID: "abcdef0123456789abcdef0123456789", Version: "v0.21.4", Status: "success", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	newRecorder(t, srv.URL, store).Run(context.Background())
	if body := vm.imported(); body != "" {
		t.Fatalf("import body = %q, want none for a terminal trace", body)
	}
}

// TestTraceMetricsRecorderSkipsNonHexTraceID proves a non-hex trace id is not
// sampled (it could not be cleaned by DeleteTraceSeries).
func TestTraceMetricsRecorderSkipsNonHexTraceID(t *testing.T) {
	vm := &fakeRecorderVM{}
	srv := httptest.NewServer(vm.handler())
	defer srv.Close()

	store := newIntegrationStore(t)
	repo := repository.NewTraceMetaRepo(store)
	if err := repo.UpsertIngest(context.Background(), &domain.TraceMeta{
		TraceID: "not-hex-trace-id", Version: "v0.21.4", Status: "running", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	newRecorder(t, srv.URL, store).Run(context.Background())
	if body := vm.imported(); body != "" {
		t.Fatalf("import body = %q, want none for a non-hex trace id", body)
	}
}
