package service

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

// newServiceDB opens a fresh SQLite DB and returns it plus repos constructed
// from it. The DB is cleaned up via t.Cleanup.
func newServiceDB(t *testing.T) (*sql.DB, *repos) {
	t.Helper()
	path := t.TempDir() + "/test.db"
	db, err := repository.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, &repos{
		users:     repository.NewUserRepo(db),
		groups:    repository.NewGroupRepo(db),
		projects:  repository.NewProjectRepo(db),
		tokens:    repository.NewTokenRepo(db),
		traceMeta: repository.NewTraceMetaRepo(db),
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

// seedUserSvc creates a user via the UserService and returns it.
func seedUserSvc(t *testing.T, svc *UserService, username string, role domain.Role) *domain.User {
	t.Helper()
	u, err := svc.Create(context.Background(), username, "password123", role)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}
