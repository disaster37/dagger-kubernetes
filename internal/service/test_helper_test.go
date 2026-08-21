package service

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// newServiceStore builds a single-node in-memory Raft store for service tests.
func newServiceStore(t *testing.T) *repository.RaftStore {
	t.Helper()
	store, err := repository.NewInmemRaftStore("service-test", observ.NewTestLogger(), 5*time.Second)
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

// newServiceDB opens a fresh in-memory Raft store and returns repos constructed
// from it. The store is cleaned up via t.Cleanup.
func newServiceDB(t *testing.T) *repos {
	t.Helper()
	store := newServiceStore(t)
	return &repos{
		users:     repository.NewUserRepo(store),
		groups:    repository.NewGroupRepo(store),
		projects:  repository.NewProjectRepo(store),
		tokens:    repository.NewTokenRepo(store),
		traceMeta: repository.NewTraceMetaRepo(store),
	}
}

type repos struct {
	users     domain.UserRepository
	groups    domain.GroupRepository
	projects  domain.ProjectRepository
	tokens    domain.APITokenRepository
	traceMeta domain.TraceMetaRepository
}

func testLogger() *logrus.Logger { return observ.NewTestLogger() }

// newRegistryClient constructs the concrete repository registry client for a
// backend. It is the factory wired into NewRegistryRouter by tests so the
// service layer never imports the repository layer.
func newRegistryClient(b domain.RegistryBackend) domain.RegistryClient {
	return repository.NewRegistryStatsClientWithAuth(b.InternalAddr, b.Username, b.Password)
}

// seedUserSvc creates a user (always RoleUser) via the UserService and returns it.
func seedUserSvc(t *testing.T, svc *UserService, username string) *domain.User {
	t.Helper()
	u, err := svc.Create(context.Background(), username, "password123", domain.RoleUser)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}
