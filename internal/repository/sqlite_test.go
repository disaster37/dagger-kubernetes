package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// newTestDB opens a SQLite DB in a temp file, runs the migration, and returns
// it. The file is cleaned up via t.Cleanup.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := t.TempDir() + "/test.db"
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newID returns a fresh 32-char hex id (16 random bytes).
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("rand read: %v", err))
	}
	return hex.EncodeToString(b)
}

// seedUser inserts and returns a user with the given username (always RoleUser).
func seedUser(t *testing.T, db *sql.DB, username string) *domain.User {
	t.Helper()
	u := &domain.User{
		ID:       newID(),
		Username: username,
		Role:     domain.RoleUser,
	}
	if err := NewUserRepo(db).Create(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}
