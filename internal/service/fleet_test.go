package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func TestAcquireWithNoReplicas(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	result, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if result.PodName != "dagger-engine-v0-21-4-0" {
		t.Fatalf("expected pod-0, got %s", result.PodName)
	}
}

func TestAcquireLeastPinned(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	provider.EnsureStatefulSet("v0.21.4", "registry.dagger.io/engine:v0.21.4")
	provider.ScaleUp("v0.21.4", 2)

	sessions.Register("fp1", "v0.21.4", "dagger-engine-v0-21-4-0", "inst-1", "trace-1", "")

	result, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if result.PodName != "dagger-engine-v0-21-4-1" {
		t.Fatalf("expected pod-1 (least pinned), got %s", result.PodName)
	}
}

func TestAcquireAtCapacity(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 2,
		MaxSessionsPerReplica: 1,
		ReplicaIdleTTL:        5 * time.Minute,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	provider.EnsureStatefulSet("v0.21.4", "")
	provider.ScaleUp("v0.21.4", 2)
	sessions.Register("fp1", "v0.21.4", "dagger-engine-v0-21-4-0", "inst-1", "trace-1", "")
	sessions.Register("fp2", "v0.21.4", "dagger-engine-v0-21-4-1", "inst-2", "trace-2", "")

	_, err := m.Acquire(context.Background(), "v0.21.4")
	if err == nil {
		t.Fatal("expected at-capacity error")
	}
}

func TestAcquireMultipleVersions(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	// Acquire for v0.21.4 (no existing replicas → creates first)
	result1, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire v0.21.4: %v", err)
	}
	if result1.PodName != "dagger-engine-v0-21-4-0" {
		t.Fatalf("expected pod-0, got %s", result1.PodName)
	}

	// Acquire for v0.22.0 (different version → separate statefulset)
	result2, err := m.Acquire(context.Background(), "v0.22.0")
	if err != nil {
		t.Fatalf("Acquire v0.22.0: %v", err)
	}
	if result2.PodName != "dagger-engine-v0-22-0-0" {
		t.Fatalf("expected pod-0 for v0.22.0, got %s", result2.PodName)
	}

	// Acquire again for v0.21.4 (should use existing replica)
	result3, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire v0.21.4 again: %v", err)
	}
	if result3.PodName != "dagger-engine-v0-21-4-0" {
		t.Fatalf("expected existing pod-0, got %s", result3.PodName)
	}

	// Fill up sessions on v0.21.4 pod-0 to max (maxSessionsPerReplica=8)
	for i := 0; i < 8; i++ {
		sessions.Register(fmt.Sprintf("fp-fill-%d", i), "v0.21.4", "dagger-engine-v0-21-4-0", fmt.Sprintf("inst-fill-%d", i), fmt.Sprintf("trace-fill-%d", i), "")
	}

	// Next acquire should scale up a new replica (pod-1)
	result4, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire after filling: %v", err)
	}
	if result4.PodName != "dagger-engine-v0-21-4-1" {
		t.Fatalf("expected new pod-1, got %s", result4.PodName)
	}

	// Fill pod-1 too
	for i := 0; i < 8; i++ {
		sessions.Register(fmt.Sprintf("fp-fill2-%d", i), "v0.21.4", "dagger-engine-v0-21-4-1", fmt.Sprintf("inst-fill2-%d", i), fmt.Sprintf("trace-fill2-%d", i), "")
	}

	// Next acquire should scale up pod-2
	result5, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire after filling second: %v", err)
	}
	if result5.PodName != "dagger-engine-v0-21-4-2" {
		t.Fatalf("expected new pod-2, got %s", result5.PodName)
	}
}

func TestSweepScaleDown(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        0,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	provider.EnsureStatefulSet("v0.21.4", "")
	provider.ScaleUp("v0.21.4", 2)

	if err := m.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	replicas, _ := provider.GetReplicas("v0.21.4")
	if len(replicas) != 1 {
		t.Fatalf("expected 1 replica after first sweep (one removed), got %d", len(replicas))
	}

	if err := m.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}

	replicas, _ = provider.GetReplicas("v0.21.4")
	if len(replicas) != 0 {
		t.Fatalf("expected 0 replicas after second sweep, got %d", len(replicas))
	}
}

func TestAllFleetInfoEmptyReturnsEmptySlice(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	infos, err := m.AllFleetInfo()
	if err != nil {
		t.Fatalf("AllFleetInfo: %v", err)
	}
	if infos == nil {
		t.Fatal("infos must not be nil (JSON must marshal to [], not null)")
	}
	if len(infos) != 0 {
		t.Fatalf("expected empty slice, got %d entries", len(infos))
	}

	raw, err := json.Marshal(infos)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("empty fleet must marshal to [], got %s", string(raw))
	}
}

func TestAllFleetInfoJSONTags(t *testing.T) {
	provider := repository.NewStubProvider()
	sessions := NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
	}, observ.NewTestLogger(), observ.NewMetrics(nil))

	ctx := context.Background()
	_, err := m.Acquire(ctx, "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	infos, err := m.AllFleetInfo()
	if err != nil {
		t.Fatalf("AllFleetInfo: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 version, got %d", len(infos))
	}

	raw, err := json.Marshal(infos)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(decoded))
	}

	info := decoded[0]

	// Assert camelCase keys exist.
	for _, key := range []string{"version", "stsName", "replicas", "readyReplicas", "ordinals"} {
		if _, ok := info[key]; !ok {
			t.Errorf("missing key %q in %s", key, string(raw))
		}
	}

	// Assert PascalCase keys are absent.
	for _, key := range []string{"Version", "STSName", "Replicas", "ReadyReplicas", "Ordinals"} {
		if _, ok := info[key]; ok {
			t.Errorf("PascalCase key %q should be absent in %s", key, string(raw))
		}
	}

	// Assert nested replica keys.
	ordinals, ok := info["ordinals"].([]any)
	if !ok || len(ordinals) == 0 {
		t.Fatal("expected non-empty ordinals array")
	}
	replica := ordinals[0].(map[string]any)
	for _, key := range []string{"name", "ordinal", "version", "podIP", "ready", "startedAt", "pinnedSessions"} {
		if _, ok := replica[key]; !ok {
			t.Errorf("missing replica key %q in %s", key, string(raw))
		}
	}
	for _, key := range []string{"Name", "Ordinal", "PodIP", "Ready", "StartedAt", "PinnedSessions"} {
		if _, ok := replica[key]; ok {
			t.Errorf("PascalCase replica key %q should be absent in %s", key, string(raw))
		}
	}
}
