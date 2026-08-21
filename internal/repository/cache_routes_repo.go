package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// CacheRoutesRepo persists the object→backend routing table and in-flight
// upload sessions in the Raft FSM (v3 schema equivalent).
type CacheRoutesRepo struct {
	store *RaftStore
}

var _ domain.CacheRoutesStore = (*CacheRoutesRepo)(nil)

// NewCacheRoutesRepo returns a CacheRoutesRepo backed by store.
func NewCacheRoutesRepo(store *RaftStore) *CacheRoutesRepo {
	return &CacheRoutesRepo{store: store}
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// LookupManifest returns the route for repo+tag. ok=false when absent.
func (r *CacheRoutesRepo) LookupManifest(ctx context.Context, repo, tag string) (domain.CacheRoute, bool, error) {
	cr, ok := r.store.fsmRead().lookupManifestRoute(repo, tag)
	return cr, ok, nil
}

// UpsertManifest inserts or replaces the repo+tag → backend mapping.
func (r *CacheRoutesRepo) UpsertManifest(ctx context.Context, repo, tag, digest, backendID string, storedBytes int64) error {
	now := nowRFC3339()
	return r.store.applyCtx(ctx, kindUpsertManifestRoute, &domain.CacheRoute{
		Repo:        repo,
		Tag:         tag,
		Digest:      digest,
		BackendID:   backendID,
		StoredBytes: storedBytes,
		CreatedAt:   now,
		LastSeenAt:  now,
	})
}

// LookupBlob returns a backend that holds digest. ok=false when absent.
func (r *CacheRoutesRepo) LookupBlob(ctx context.Context, digest string) (string, bool, error) {
	backendID, ok := r.store.fsmRead().lookupBlobRoute(digest)
	return backendID, ok, nil
}

// UpsertBlob records that digest is present on backendID (idempotent).
func (r *CacheRoutesRepo) UpsertBlob(ctx context.Context, digest, backendID string) error {
	return r.store.applyCtx(ctx, kindUpsertBlobRoute, cmdUpsertBlobRoute{
		Digest:    digest,
		BackendID: backendID,
		CreatedAt: nowRFC3339(),
	})
}

// LookupUpload returns the upload session for uuid. ok=false when absent.
func (r *CacheRoutesRepo) LookupUpload(ctx context.Context, uuid string) (domain.CacheUploadSession, bool, error) {
	sess, ok := r.store.fsmRead().lookupUpload(uuid)
	return sess, ok, nil
}

// RecordUpload inserts an upload session (INSERT OR REPLACE semantics).
func (r *CacheRoutesRepo) RecordUpload(ctx context.Context, uuid, repo, backendID string) error {
	return r.store.applyCtx(ctx, kindRecordUpload, &domain.CacheUploadSession{
		UploadUUID: uuid,
		Repo:       repo,
		BackendID:  backendID,
		CreatedAt:  nowRFC3339(),
	})
}

// DeleteUpload removes an upload session (on completion or expiry).
func (r *CacheRoutesRepo) DeleteUpload(ctx context.Context, uuid string) error {
	return r.store.applyCtx(ctx, kindDeleteUpload, cmdDeleteUpload{UUID: uuid})
}

// AllCharges returns stored_bytes summed per backend_id.
func (r *CacheRoutesRepo) AllCharges(ctx context.Context) (map[string]int64, error) {
	return r.store.fsmRead().allCharges(), nil
}

// DeleteManifestRoute removes a repo+tag route (used by purge).
func (r *CacheRoutesRepo) DeleteManifestRoute(ctx context.Context, repo, tag string) error {
	return r.store.applyCtx(ctx, kindDeleteManifestRoute, cmdDeleteManifestRoute{Repo: repo, Tag: tag})
}

// ReapUploadSessions deletes upload sessions older than maxAge (housekeeping).
// The FSM returns the count of reaped sessions as the apply response.
func (r *CacheRoutesRepo) ReapUploadSessions(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	resp, err := r.store.applyCtxResponse(ctx, kindReapUploads, cmdReapUploads{CutoffRFC3339: cutoff})
	if err != nil {
		return 0, err
	}
	n, ok := resp.(int)
	if !ok {
		return 0, fmt.Errorf("reap upload sessions: unexpected response type %T", resp)
	}
	return n, nil
}
