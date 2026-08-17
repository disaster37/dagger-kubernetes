package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
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
	var before int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&before); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	// Running migrate again must not error (v1 + v2 already recorded).
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var after int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&after); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if after != before {
		t.Fatalf("migrations after second run = %d, want %d", after, before)
	}
}

// TestMigrateV2AddsColumn starts from a v1-only DB (no token_ciphertext) and
// asserts the v2 migration adds the column and records version 2.
func TestMigrateV2AddsColumn(t *testing.T) {
	path := t.TempDir() + "/v1.db"
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	// Simulate a pre-v2 install: api_tokens without token_ciphertext + v1
	// already recorded in schema_migrations.
	if _, err := db.ExecContext(ctx, `CREATE TABLE api_tokens (
		id           TEXT PRIMARY KEY,
		user_id      TEXT NOT NULL UNIQUE,
		token_hash   TEXT NOT NULL UNIQUE,
		prefix       TEXT NOT NULL DEFAULT '',
		created_at   DATETIME NOT NULL,
		last_used_at DATETIME
	)`); err != nil {
		t.Fatalf("create v1 api_tokens: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (1, ?)", time.Now().UTC()); err != nil {
		t.Fatalf("record v1: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	hasColumn := false
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(api_tokens)")
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == "token_ciphertext" {
			hasColumn = true
		}
	}
	if !hasColumn {
		t.Fatal("token_ciphertext column missing after v2 migration")
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 2").Scan(&n); err != nil {
		t.Fatalf("count v2: %v", err)
	}
	if n != 1 {
		t.Fatalf("v2 migration rows = %d, want 1", n)
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
