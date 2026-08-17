package repository

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"

	// Register the pure-Go SQLite driver.
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// OpenSQLite opens (or creates) the SQLite database at path and configures
// WAL mode, busy_timeout, and foreign-key enforcement. The parent directory
// is created when missing.
func OpenSQLite(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}

	// The database holds password hashes, API-token hashes, and the JWT
	// signing secret: restrict it to the supervisor user (CWE-732). SQLite
	// creates files with the process umask (world-readable by default).
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chmod sqlite %s: %w", path, err)
	}
	// Best effort for the WAL sidecar files (they may not exist yet; SQLite
	// recreates them with the process umask on checkpoint).
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}

	// WAL allows concurrent readers; a small pool avoids write-lock churn.
	db.SetMaxOpenConns(4)
	return db, nil
}

// Migrate runs the embedded schema (idempotent). The schema_migrations table
// is created first so its absence does not break the version check; if v1 is
// not yet recorded, the full schema is applied inside a single transaction.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 1").Scan(&count); err != nil {
		return fmt.Errorf("check schema_migrations: %w", err)
	}
	if count == 0 {
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (1, ?)", time.Now().UTC()); err != nil {
			return fmt.Errorf("record migration: %w", err)
		}
	}

	// v2: add token_ciphertext column to api_tokens. Fresh v1 installs already
	// include the column (schema.sql), so gate the ALTER on PRAGMA table_info
	// (SQLite does not support ADD COLUMN IF NOT EXISTS).
	var v2Count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 2").Scan(&v2Count); err != nil {
		return fmt.Errorf("check schema_migrations v2: %w", err)
	}
	if v2Count == 0 {
		hasColumn, err := apiTokensHasColumn(ctx, tx, "token_ciphertext")
		if err != nil {
			return err
		}
		if !hasColumn {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE api_tokens ADD COLUMN token_ciphertext TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("alter api_tokens v2: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (2, ?)", time.Now().UTC()); err != nil {
			return fmt.Errorf("record migration v2: %w", err)
		}
	}

	// v3: add the routing-table tables (cache_object_routes, cache_blob_routes,
	// cache_upload_sessions). schema.sql is IF NOT EXISTS, so re-applying it
	// creates the new tables on existing databases.
	var v3Count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 3").Scan(&v3Count); err != nil {
		return fmt.Errorf("check schema_migrations v3: %w", err)
	}
	if v3Count == 0 {
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply schema v3: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (3, ?)", time.Now().UTC()); err != nil {
			return fmt.Errorf("record migration v3: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// apiTokensHasColumn reports whether the api_tokens table has the given
// column, using PRAGMA table_info (used by the v2 migration).
func apiTokensHasColumn(ctx context.Context, tx *sql.Tx, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(api_tokens)")
	if err != nil {
		return false, fmt.Errorf("inspect api_tokens columns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan api_tokens column: %w", err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate api_tokens columns: %w", err)
	}
	return false, nil
}

// scanner is implemented by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// nullString maps "" to NULL so nullable TEXT columns store SQL NULL rather
// than an empty string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTime maps a nil pointer to NULL for nullable DATETIME columns.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// boolToInt converts a bool to SQLite's integer representation.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// prefixedColumns returns cols with each column prefixed by the table alias
// (e.g. alias "tm" turns "trace_id, user_id" into "tm.trace_id, tm.user_id")
// for use in joined queries.
func prefixedColumns(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = fmt.Sprintf("%s.%s", alias, c)
	}
	return strings.Join(parts, ", ")
}

// checkUpdated returns domain.ErrNotFound when a statement affected no rows.
func checkUpdated(res sql.Result, entity, id string) error {
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s %s: %w", entity, id, domain.ErrNotFound)
	}
	return nil
}

// MetaStore reads/writes arbitrary key/value pairs in the meta table. Used
// for the auto-generated JWT secret and similar singletons.
type MetaStore struct {
	db *sql.DB
}

// NewMetaStore returns a MetaStore backed by db.
func NewMetaStore(db *sql.DB) *MetaStore {
	return &MetaStore{db: db}
}

// Get returns the value for key. A missing key yields domain.ErrNotFound.
func (m *MetaStore) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := m.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("meta %s: %w", key, domain.ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("meta get %s: %w", key, err)
	}
	return v, nil
}

// Set upserts the value for key.
func (m *MetaStore) Set(ctx context.Context, key, value string) error {
	_, err := m.db.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	if err != nil {
		return fmt.Errorf("meta set %s: %w", key, err)
	}
	return nil
}
