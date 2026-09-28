package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// pvcName returns the StatefulSet PVC name of the test version
// (dagger-kubernetes-<stsName>-<ordinal>).
func pvcName(ordinal int) string {
	return fmt.Sprintf("dagger-kubernetes-%s-%d", domain.StsName("v0.19.0"), ordinal)
}

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
	if result.Message != "no running engine pods; nothing to prune" {
		t.Fatalf("message = %q, want the no-op message", result.Message)
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
	if again.PVCs == nil {
		t.Fatal("status pvcs must be [] (not null) so the JSON shape matches the documented type")
	}
	if _, ok := svc.Status("v0.20.0"); ok {
		t.Fatal("unknown version should be absent")
	}
}

// --- orphaned-PVC deletion --------------------------------------------------

func TestPurgeDeletesOrphanedPVCs(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {
				{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"},
				{Name: "p1", Ordinal: 1, PodIP: "10.0.0.2"},
			},
		},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {
				{Name: pvcName(0), Ordinal: 0},
				{Name: pvcName(1), Ordinal: 1},
				{Name: pvcName(2), Ordinal: 2},
			},
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted || result.Replicas != 2 {
		t.Fatalf("result = %+v, want completed/2", result)
	}
	// Only the orphaned PVC (ordinal 2) is reported; the PVCs of the running
	// runners (ordinals 0 and 1) are never touched.
	if len(result.PVCs) != 1 {
		t.Fatalf("pvcs = %+v, want only the orphaned ordinal 2", result.PVCs)
	}
	got := result.PVCs[0]
	if got.Ordinal != 2 || !got.Deleted || got.Error != "" {
		t.Fatalf("pvc = %+v, want ordinal 2 deleted", got)
	}
	if want := pvcName(2); got.PVCName != want {
		t.Fatalf("pvc name = %q, want %q", got.PVCName, want)
	}
}

func TestPurgeZeroReplicasDeletesAllPVCs(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {
				{Name: pvcName(0), Ordinal: 0},
				{Name: pvcName(1), Ordinal: 1},
			},
		},
	}
	pruner := &fakeEnginePruner{}
	svc := newPurgeService(provider, pruner)

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted || result.Replicas != 0 {
		t.Fatalf("result = %+v, want completed/0", result)
	}
	if len(result.PVCs) != 2 {
		t.Fatalf("pvcs = %+v, want both PVCs deleted", result.PVCs)
	}
	for i, pvc := range result.PVCs {
		if pvc.Ordinal != i || !pvc.Deleted {
			t.Fatalf("pvcs[%d] = %+v, want ordinal %d deleted", i, pvc, i)
		}
	}
	if len(pruner.calls) != 0 {
		t.Fatal("no pruner calls expected")
	}
}

func TestPurgeNoPVCs(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {
				{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"},
				{Name: "p1", Ordinal: 1, PodIP: "10.0.0.2"},
			},
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted {
		t.Fatalf("state = %q, want completed", result.State)
	}
	if result.PVCs == nil || len(result.PVCs) != 0 {
		t.Fatalf("pvcs = %+v, want [] (not null) so the JSON shape matches the documented type", result.PVCs)
	}
	if strings.Contains(result.Message, "PVC") {
		t.Fatalf("message = %q, want no PVC summary", result.Message)
	}
}

func TestPurgePVCListError(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		pvcErr:   errors.New("list boom"),
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err == nil {
		t.Fatal("expected error when listing PVCs fails")
	}
	if result == nil || result.State != purgeStateFailed {
		t.Fatalf("result = %+v, want failed", result)
	}
	if result.Message != "list engine PVCs failed" {
		t.Fatalf("message = %q, want precondition failure", result.Message)
	}
}

func TestPurgePVCDeleteError(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {{Name: pvcName(0), Ordinal: 0}},
		},
		deleteErr: map[string]error{pvcName(0): errors.New("delete boom")},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted {
		t.Fatalf("state = %q, want completed", result.State)
	}
	if len(result.PVCs) != 1 {
		t.Fatalf("pvcs = %+v, want 1 entry", result.PVCs)
	}
	got := result.PVCs[0]
	if got.Deleted || !strings.Contains(got.Error, "delete boom") {
		t.Fatalf("pvc = %+v, want per-PVC error", got)
	}
}

func TestPurgePVCNotFoundIdempotent(t *testing.T) {
	// DeletePVC of an already-gone PVC returns nil (NotFound is success), so
	// the re-run reports the PVC as deleted.
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {{Name: pvcName(0), Ordinal: 0}},
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if len(result.PVCs) != 1 || !result.PVCs[0].Deleted {
		t.Fatalf("pvcs = %+v, want the PVC reported as deleted", result.PVCs)
	}
}

func TestPurgePartialPVCDelete(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {
				{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"},
				{Name: "p1", Ordinal: 1, PodIP: "10.0.0.2"},
			},
		},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {
				{Name: pvcName(2), Ordinal: 2},
				{Name: pvcName(3), Ordinal: 3},
			},
		},
		deleteErr: map[string]error{pvcName(3): errors.New("delete boom")},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted {
		t.Fatalf("state = %q, want completed", result.State)
	}
	if len(result.PVCs) != 2 {
		t.Fatalf("pvcs = %+v, want 2 entries", result.PVCs)
	}
	if !result.PVCs[0].Deleted || result.PVCs[0].Ordinal != 2 {
		t.Fatalf("pvcs[0] = %+v, want ordinal 2 deleted", result.PVCs[0])
	}
	if result.PVCs[1].Deleted || result.PVCs[1].Error == "" || result.PVCs[1].Ordinal != 3 {
		t.Fatalf("pvcs[1] = %+v, want ordinal 3 with error", result.PVCs[1])
	}
	if !strings.Contains(result.Message, "1 of 2") {
		t.Fatalf("message = %q, want partial PVC summary", result.Message)
	}
}

func TestPurgeAllPVCFailures(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"}},
		},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {
				{Name: pvcName(1), Ordinal: 1},
				{Name: pvcName(2), Ordinal: 2},
			},
		},
		deleteErr: map[string]error{
			pvcName(1): errors.New("delete boom 1"),
			pvcName(2): errors.New("delete boom 2"),
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted {
		t.Fatalf("state = %q, want completed", result.State)
	}
	if len(result.PVCs) != 2 {
		t.Fatalf("pvcs = %+v, want 2 entries", result.PVCs)
	}
	for i, pvc := range result.PVCs {
		if pvc.Deleted || pvc.Error == "" {
			t.Fatalf("pvcs[%d] = %+v, want a per-PVC error", i, pvc)
		}
	}
	want := "pruned 1 of 1 pods; deleted 0 of 2 orphaned PVCs; 2 PVCs failed"
	if result.Message != want {
		t.Fatalf("message = %q, want %q", result.Message, want)
	}
}

func TestPurgeMessageIncludesPVCs(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {
				{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"},
				{Name: "p1", Ordinal: 1, PodIP: "10.0.0.2"},
			},
		},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {{Name: pvcName(2), Ordinal: 2}},
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	want := "pruned 2 of 2 pods; deleted 1 orphaned PVC(s)"
	if result.Message != want {
		t.Fatalf("message = %q, want %q", result.Message, want)
	}
}

func TestPurgeSkipsUnparseablePVCOrdinal(t *testing.T) {
	provider := &stubFleetProvider{
		versions: []string{"v0.19.0"},
		replicas: map[string][]domain.Replica{
			"v0.19.0": {{Name: "p0", Ordinal: 0, PodIP: "10.0.0.1"}},
		},
		pvcs: map[string][]domain.PVCInfo{
			"v0.19.0": {{Name: "unexpected-name", Ordinal: -1}},
		},
	}
	svc := newPurgeService(provider, &fakeEnginePruner{})

	result, err := svc.Purge(context.Background(), "v0.19.0")
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if result.State != purgeStateCompleted {
		t.Fatalf("state = %q, want completed", result.State)
	}
	if len(result.PVCs) != 0 {
		t.Fatalf("pvcs = %+v, want the unparseable PVC skipped", result.PVCs)
	}
}
