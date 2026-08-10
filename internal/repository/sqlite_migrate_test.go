package repository

import (
	"context"
	"os"
	"testing"
)

func TestOpenSQLiteAndMigrate(t *testing.T) {
	db := newTestDB(t)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := newTestDB(t)
	// Running migrate again must not error (v1 already recorded).
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrations = %d, want 1", n)
	}
}

// TestOpenSQLiteFilePermissions verifies the database file is restricted to
// the owner (CWE-732): it holds password hashes, token hashes, and the JWT
// secret.
func TestOpenSQLiteFilePermissions(t *testing.T) {
	path := t.TempDir() + "/perm.db"
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db file perm = %o, want 600", perm)
	}
}

func TestMetaStore(t *testing.T) {
	db := newTestDB(t)
	ms := NewMetaStore(db)
	ctx := context.Background()

	if _, err := ms.Get(ctx, "missing"); err == nil {
		t.Fatal("expected ErrNotFound for missing key")
	}

	if err := ms.Set(ctx, "k", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := ms.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v1" {
		t.Fatalf("got %q, want v1", got)
	}

	if err := ms.Set(ctx, "k", "v2"); err != nil {
		t.Fatalf("Set upsert: %v", err)
	}
	got, err = ms.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if got != "v2" {
		t.Fatalf("got %q, want v2", got)
	}
}
