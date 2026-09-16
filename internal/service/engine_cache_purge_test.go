package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// --- fakes -----------------------------------------------------------------

type pruneCall struct {
	podIP   string
	version string
}

// fakeEnginePruner records prune calls and can fail per pod IP or block until
// a gate is closed (for concurrency tests).
type fakeEnginePruner struct {
	mu      sync.Mutex
	calls   []pruneCall
	errs    map[string]error
	gate    chan struct{}
	started chan struct{}
}

func (f *fakeEnginePruner) PruneLocalCache(ctx context.Context, podIP, version string) error {
	f.mu.Lock()
	f.calls = append(f.calls, pruneCall{podIP: podIP, version: version})
	err := f.errs[podIP]
	gate, started := f.gate, f.started
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func newPurgeService(provider domain.FleetProvider, pruner domain.EnginePruner) *EngineCachePurgeService {
	return NewEngineCachePurgeService(provider, pruner, observ.NewTestLogger())
}

// --- tests -----------------------------------------------------------------

func TestPurgeFleetNotFound(t *testing.T) {
	provider := &stubFleetProvider{versions: []string{"v0.20.0"}}
	pruner := &fakeEnginePruner{}
	svc := newPurgeService(provider, pruner)

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if !errors.Is(err, domain.ErrEngineFleetNotFound) {
		t.Fatalf("err = %v, want ErrEngineFleetNotFound", err)
	}
	if result == nil || result.State != purgeStateFailed {
		t.Fatalf("result = %+v, want state failed", result)
	}
	if len(pruner.calls) != 0 {
		t.Fatal("pruner must not be called")
	}
}

func TestPurgeConcurrentRejected(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"}},
		},
	}
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	pruner := &fakeEnginePruner{gate: gate, started: started}
	svc := newPurgeService(provider, pruner)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := svc.Purge(context.Background(), "v0.19.0"); err != nil {
			t.Errorf("first purge: %v", err)
		}
	}()
	<-started

	if _, err := svc.Purge(context.Background(), "v0.19.0"); !errors.Is(err, domain.ErrPurgeInProgress) {
		t.Fatalf("second purge err = %v, want ErrPurgeInProgress", err)
	}

	close(gate)
	<-done
}

func TestPurgeHappyPathNRunning(t *testing.T) {
	replicas := []domain.Replica{
		{Name: "p2", Ordinal: 2, PodIP: "10.0.0.3"},
		{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"},
		{Name: "p1", Ordinal: 1, PodIP: "10.0.0.2"},
	}
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{"v0.19.0": replicas},
	}
	pruner := &fakeEnginePruner{}
	svc := newPurgeService(provider, pruner)

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted || result.Replicas != 3 {
		t.Fatalf("result = %+v, want completed/3", result)
	}
	if len(result.Pods) != 3 {
		t.Fatalf("pods = %d, want 3", len(result.Pods))
	}
	for i, p := range result.Pods {
		if p.Ordinal != i {
			t.Fatalf("pods[%d].Ordinal = %d, want %d (sorted by ordinal)", i, p.Ordinal, i)
		}
		if !p.Pruned {
			t.Fatalf("pods[%d] = %+v, want pruned", i, p)
		}
	}
	if len(pruner.calls) != 3 {
		t.Fatalf("pruner calls = %d, want 3", len(pruner.calls))
	}
	for _, c := range pruner.calls {
		if c.version != "v0.19.0" {
			t.Fatalf("pruner version = %q, want v0.19.0", c.version)
		}
	}
	// Per-pod isolation: each pod must be pruned by its own IP.
	seenIPs := make(map[string]bool, len(pruner.calls))
	for _, c := range pruner.calls {
		seenIPs[c.podIP] = true
	}
	for _, want := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if !seenIPs[want] {
			t.Fatalf("pruner did not see pod IP %s; calls = %v", want, pruner.calls)
		}
	}
}

func TestPurgePartialPruneFailure(t *testing.T) {
	replicas := []domain.Replica{
		{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"},
		{Name: "p1", Ordinal: 1, PodIP: "10.0.0.2"},
		{Name: "p2", Ordinal: 2, PodIP: "10.0.0.3"},
	}
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{"v0.19.0": replicas},
	}
	pruner := &fakeEnginePruner{errs: map[string]error{"10.0.0.3": errors.New("boom")}}
	svc := newPurgeService(provider, pruner)

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted {
		t.Fatalf("state = %q, want completed", result.State)
	}
	p2 := result.Pods[2]
	if p2.Pruned || p2.Error == "" {
		t.Fatalf("pods[2] = %+v, want pruned=false error set", p2)
	}
	if !strings.Contains(result.Message, "2 of 3") {
		t.Fatalf("message = %q, want partial summary", result.Message)
	}
}

func TestPurgeZeroReplicas(t *testing.T) {
	provider := &stubFleetProvider{versions: []string{"v0.19.0"}}
	pruner := &fakeEnginePruner{}
	svc := newPurgeService(provider, pruner)

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted || result.Replicas != 0 {
		t.Fatalf("result = %+v, want completed/0", result)
	}
	if result.Message == "" {
		t.Fatal("expected a no-op message")
	}
	if result.Pods == nil {
		t.Fatal("pods must be [] (not null) so the JSON shape matches the documented type")
	}
	if len(pruner.calls) != 0 {
		t.Fatal("no pruner calls expected")
	}
}

func TestPurgePrunerNilDisabled(t *testing.T) {
	provider := &stubFleetProvider{versions: []string{"v0.19.0"}}
	svc := newPurgeService(provider, nil)

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err == nil {
		t.Fatal("expected error for nil pruner")
	}
	if result == nil || result.State != purgeStateFailed {
		t.Fatalf("result = %+v, want failed", result)
	}
}

func TestStatusReflectsLastResult(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"}},
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	if _, ok := svc.Status("v0.19.0"); ok {
		t.Fatal("status before purge should be absent")
	}
	if _, err := svc.Purge(context.Background(), "v0.19.0"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	got, ok := svc.Status("v0.19.0")
	if !ok || got.State != purgeStateCompleted {
		t.Fatalf("status = %+v/%v, want completed/true", got, ok)
	}
	// Status returns a copy: mutating it must not affect the stored result.
	got.Pods[0].Pruned = false
	again, _ := svc.Status("v0.19.0")
	if !again.Pods[0].Pruned {
		t.Fatal("Status must return a copy of the stored result")
	}
	if _, ok := svc.Status("v0.20.0"); ok {
		t.Fatal("unknown version should be absent")
	}
}
