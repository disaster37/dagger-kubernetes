package fleet

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/disaster/dagger-cache/internal/session"
)

func TestAcquireWithNoReplicas(t *testing.T) {
	provider := NewStubProvider()
	sessions := session.NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
		VersionRetention:      24 * time.Hour,
		MinReplicasPerVersion: 0,
	}, zap.NewNop())

	result, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if result.PodName != "dagger-engine-v0.21.4-0" {
		t.Fatalf("expected pod-0, got %s", result.PodName)
	}
}

func TestAcquireLeastPinned(t *testing.T) {
	provider := NewStubProvider()
	sessions := session.NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        5 * time.Minute,
		VersionRetention:      24 * time.Hour,
		MinReplicasPerVersion: 0,
	}, zap.NewNop())

	provider.EnsureStatefulSet("v0.21.4", "registry.dagger.io/engine:v0.21.4")
	provider.ScaleUp("v0.21.4", 2)

	sessions.Register("fp1", "v0.21.4", "dagger-engine-v0.21.4-0", "inst-1", "trace-1")

	result, err := m.Acquire(context.Background(), "v0.21.4")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if result.PodName != "dagger-engine-v0.21.4-1" {
		t.Fatalf("expected pod-1 (least pinned), got %s", result.PodName)
	}
}

func TestAcquireAtCapacity(t *testing.T) {
	provider := NewStubProvider()
	sessions := session.NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 2,
		MaxSessionsPerReplica: 1,
		ReplicaIdleTTL:        5 * time.Minute,
		VersionRetention:      24 * time.Hour,
		MinReplicasPerVersion: 0,
	}, zap.NewNop())

	provider.EnsureStatefulSet("v0.21.4", "")
	provider.ScaleUp("v0.21.4", 2)
	sessions.Register("fp1", "v0.21.4", "dagger-engine-v0.21.4-0", "inst-1", "trace-1")
	sessions.Register("fp2", "v0.21.4", "dagger-engine-v0.21.4-1", "inst-2", "trace-2")

	_, err := m.Acquire(context.Background(), "v0.21.4")
	if err == nil {
		t.Fatal("expected at-capacity error")
	}
}

func TestSweepScaleDown(t *testing.T) {
	provider := NewStubProvider()
	sessions := session.NewStore(5 * time.Minute)

	m := NewManager(provider, sessions, ManagerConfig{
		MaxReplicasPerVersion: 3,
		MaxSessionsPerReplica: 8,
		ReplicaIdleTTL:        0,
		VersionRetention:      24 * time.Hour,
		MinReplicasPerVersion: 0,
	}, zap.NewNop())

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
