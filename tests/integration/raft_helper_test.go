package integration

import (
	"context"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// newIntegrationStore builds a single-node in-memory Raft store for
// black-box integration tests.
func newIntegrationStore(t *testing.T) *repository.RaftStore {
	t.Helper()
	store, err := repository.NewInmemRaftStore("integration-test", observ.NewTestLogger(), 5*time.Second)
	if err != nil {
		t.Fatalf("NewInmemRaftStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WaitForLeader(ctx); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
