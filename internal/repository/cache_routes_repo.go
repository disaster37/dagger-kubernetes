package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// CacheRoutesRepo persists the object→backend routing table and in-flight
// upload sessions in SQLite (v3 schema).
type CacheRoutesRepo struct {
	db *sql.DB
}

// NewCacheRoutesRepo returns a CacheRoutesRepo backed by db.
func NewCacheRoutesRepo(db *sql.DB) *CacheRoutesRepo {
	return &CacheRoutesRepo{db: db}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// LookupManifest returns the route for repo+tag. ok=false when absent.
func (r *CacheRoutesRepo) LookupManifest(ctx context.Context, repo, tag string) (domain.CacheRoute, bool, error) {
	var cr domain.CacheRoute
	err := r.db.QueryRowContext(ctx,
		`SELECT repo, tag, digest, backend_id, stored_bytes, created_at, last_seen_at
		 FROM cache_object_routes WHERE repo = ? AND tag = ?`, repo, tag).
		Scan(&cr.Repo, &cr.Tag, &cr.Digest, &cr.BackendID, &cr.StoredBytes, &cr.CreatedAt, &cr.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CacheRoute{}, false, nil
	}
	if err != nil {
		return domain.CacheRoute{}, false, fmt.Errorf("lookup manifest route: %w", err)
	}
	return cr, true, nil
}

// UpsertManifest inserts or replaces the repo+tag → backend mapping.
func (r *CacheRoutesRepo) UpsertManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error {
	now := nowRFC3339()
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO cache_object_routes (repo, tag, digest, backend_id, stored_bytes, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repo, tag) DO UPDATE SET
		   digest = excluded.digest,
		   backend_id = excluded.backend_id,
		   stored_bytes = excluded.stored_bytes,
		   last_seen_at = excluded.last_seen_at`,
		repo, tag, digest, backendID, storedBytes, now, now)
	if err != nil {
		return fmt.Errorf("upsert manifest route: %w", err)
	}
	return nil
}

// LookupBlob returns a backend that holds digest. ok=false when absent.
func (r *CacheRoutesRepo) LookupBlob(ctx context.Context, digest string) (string, bool, error) {
	var backendID string
	err := r.db.QueryRowContext(ctx,
		`SELECT backend_id FROM cache_blob_routes WHERE digest = ? ORDER BY created_at DESC LIMIT 1`, digest).
		Scan(&backendID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lookup blob route: %w", err)
	}
	return backendID, true, nil
}

// UpsertBlob records that digest is present on backendID (idempotent).
func (r *CacheRoutesRepo) UpsertBlob(ctx context.Context, digest, backendID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO cache_blob_routes (digest, backend_id, created_at) VALUES (?, ?, ?)`,
		digest, backendID, nowRFC3339())
	if err != nil {
		return fmt.Errorf("upsert blob route: %w", err)
	}
	return nil
}

// LookupUpload returns the upload session for uuid. ok=false when absent.
func (r *CacheRoutesRepo) LookupUpload(ctx context.Context, uuid string) (domain.CacheUploadSession, bool, error) {
	var s domain.CacheUploadSession
	err := r.db.QueryRowContext(ctx,
		`SELECT upload_uuid, repo, backend_id, created_at FROM cache_upload_sessions WHERE upload_uuid = ?`, uuid).
		Scan(&s.UploadUUID, &s.Repo, &s.BackendID, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CacheUploadSession{}, false, nil
	}
	if err != nil {
		return domain.CacheUploadSession{}, false, fmt.Errorf("lookup upload session: %w", err)
	}
	return s, true, nil
}

// RecordUpload inserts an upload session.
func (r *CacheRoutesRepo) RecordUpload(ctx context.Context, uuid, repo, backendID string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO cache_upload_sessions (upload_uuid, repo, backend_id, created_at) VALUES (?, ?, ?, ?)`,
		uuid, repo, backendID, nowRFC3339())
	if err != nil {
		return fmt.Errorf("record upload session: %w", err)
	}
	return nil
}

// DeleteUpload removes an upload session (on completion or expiry).
func (r *CacheRoutesRepo) DeleteUpload(ctx context.Context, uuid string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM cache_upload_sessions WHERE upload_uuid = ?`, uuid)
	if err != nil {
		return fmt.Errorf("delete upload session: %w", err)
	}
	return nil
}

// BackendCharge returns the sum of stored_bytes for backendID.
func (r *CacheRoutesRepo) BackendCharge(ctx context.Context, backendID string) (int64, error) {
	var sum int64
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(stored_bytes), 0) FROM cache_object_routes WHERE backend_id = ?`, backendID).
		Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("backend charge: %w", err)
	}
	return sum, nil
}

// AllCharges returns stored_bytes summed per backend_id.
func (r *CacheRoutesRepo) AllCharges(ctx context.Context) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT backend_id, COALESCE(SUM(stored_bytes), 0) FROM cache_object_routes GROUP BY backend_id`)
	if err != nil {
		return nil, fmt.Errorf("all charges: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, fmt.Errorf("scan charge: %w", err)
		}
		out[id] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate charges: %w", err)
	}
	return out, nil
}

// DeleteManifestRoute removes a repo+tag route (used by purge).
func (r *CacheRoutesRepo) DeleteManifestRoute(ctx context.Context, repo, tag string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM cache_object_routes WHERE repo = ? AND tag = ?`, repo, tag)
	if err != nil {
		return fmt.Errorf("delete manifest route: %w", err)
	}
	return nil
}

// DeleteRoutesForBackend removes all manifest+blob routes for a backend
// (used when a backend is permanently removed).
func (r *CacheRoutesRepo) DeleteRoutesForBackend(ctx context.Context, backendID string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM cache_object_routes WHERE backend_id = ?`, backendID); err != nil {
		return fmt.Errorf("delete manifest routes for backend: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM cache_blob_routes WHERE backend_id = ?`, backendID); err != nil {
		return fmt.Errorf("delete blob routes for backend: %w", err)
	}
	return nil
}

// ReapUploadSessions deletes upload sessions older than maxAge (housekeeping).
func (r *CacheRoutesRepo) ReapUploadSessions(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	res, err := r.db.ExecContext(ctx, `DELETE FROM cache_upload_sessions WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reap upload sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reap upload sessions count: %w", err)
	}
	return int(n), nil
}
