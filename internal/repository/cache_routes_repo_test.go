package repository

import (
	"context"
	"testing"
	"time"
)

func newRoutesRepo(t *testing.T) *CacheRoutesRepo {
	t.Helper()
	db, err := OpenSQLite(t.TempDir() + "/routes.db")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewCacheRoutesRepo(db)
}

func TestManifestRouteRoundTrip(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	if err := r.UpsertManifest(ctx, "dagger-cache", "v0-21-4", digestRepeat("a"), "reg-1", 42); err != nil {
		t.Fatalf("UpsertManifest: %v", err)
	}
	got, ok, err := r.LookupManifest(ctx, "dagger-cache", "v0-21-4")
	if err != nil || !ok {
		t.Fatalf("LookupManifest: ok=%v err=%v", ok, err)
	}
	if got.Repo != "dagger-cache" || got.Tag != "v0-21-4" || got.Digest != digestRepeat("a") || got.BackendID != "reg-1" || got.StoredBytes != 42 {
		t.Fatalf("route = %+v", got)
	}
	if got.CreatedAt == "" || got.LastSeenAt == "" {
		t.Fatalf("timestamps empty: %+v", got)
	}
}

func TestManifestRouteLookupMiss(t *testing.T) {
	r := newRoutesRepo(t)
	_, ok, err := r.LookupManifest(context.Background(), "repo", "tag")
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want miss", ok, err)
	}
}

func TestManifestRouteConflictUpsert(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	if err := r.UpsertManifest(ctx, "repo", "tag", "", "reg-1", 1); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _, _ := r.LookupManifest(ctx, "repo", "tag")

	// Replace backend + digest + stored_bytes.
	if err := r.UpsertManifest(ctx, "repo", "tag", digestRepeat("b"), "reg-2", 2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, ok, _ := r.LookupManifest(ctx, "repo", "tag")
	if !ok {
		t.Fatal("expected route after conflict upsert")
	}
	if got.BackendID != "reg-2" || got.Digest != digestRepeat("b") || got.StoredBytes != 2 {
		t.Fatalf("route = %+v", got)
	}
	if got.CreatedAt != first.CreatedAt {
		t.Fatalf("created_at changed on upsert: %q -> %q", first.CreatedAt, got.CreatedAt)
	}
}

func TestBlobRouteDedupeAndLookup(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()
	dgst := digestRepeat("a")

	if err := r.UpsertBlob(ctx, dgst, "reg-1"); err != nil {
		t.Fatalf("UpsertBlob: %v", err)
	}
	if err := r.UpsertBlob(ctx, dgst, "reg-1"); err != nil {
		t.Fatalf("UpsertBlob duplicate: %v", err)
	}
	got, ok, err := r.LookupBlob(ctx, dgst)
	if err != nil || !ok || got != "reg-1" {
		t.Fatalf("LookupBlob = %q ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := r.LookupBlob(ctx, digestRepeat("b")); ok {
		t.Fatal("expected miss for unknown digest")
	}
}

func TestUploadSessionLifecycle(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	if err := r.RecordUpload(ctx, "uuid-1", "dagger-cache", "reg-1"); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}
	s, ok, err := r.LookupUpload(ctx, "uuid-1")
	if err != nil || !ok {
		t.Fatalf("LookupUpload: ok=%v err=%v", ok, err)
	}
	if s.UploadUUID != "uuid-1" || s.Repo != "dagger-cache" || s.BackendID != "reg-1" {
		t.Fatalf("session = %+v", s)
	}
	if err := r.DeleteUpload(ctx, "uuid-1"); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}
	if _, ok, _ := r.LookupUpload(ctx, "uuid-1"); ok {
		t.Fatal("expected miss after delete")
	}
	if _, ok, _ := r.LookupUpload(ctx, "missing"); ok {
		t.Fatal("expected miss for unknown uuid")
	}
}

func TestBackendChargeAndAllCharges(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	if err := r.UpsertManifest(ctx, "r", "a", "", "reg-1", 10); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := r.UpsertManifest(ctx, "r", "b", "", "reg-1", 20); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := r.UpsertManifest(ctx, "r", "c", "", "reg-2", 30); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	charge, err := r.BackendCharge(ctx, "reg-1")
	if err != nil || charge != 30 {
		t.Fatalf("BackendCharge reg-1 = %d err=%v", charge, err)
	}
	all, err := r.AllCharges(ctx)
	if err != nil {
		t.Fatalf("AllCharges: %v", err)
	}
	if all["reg-1"] != 30 || all["reg-2"] != 30 {
		t.Fatalf("charges = %v", all)
	}
}

func TestDeleteManifestRoute(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	if err := r.UpsertManifest(ctx, "r", "t", "", "reg-1", 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := r.DeleteManifestRoute(ctx, "r", "t"); err != nil {
		t.Fatalf("DeleteManifestRoute: %v", err)
	}
	if _, ok, _ := r.LookupManifest(ctx, "r", "t"); ok {
		t.Fatal("expected miss after delete")
	}
}

func TestDeleteRoutesForBackend(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	if err := r.UpsertManifest(ctx, "r", "t1", "", "reg-1", 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := r.UpsertManifest(ctx, "r", "t2", "", "reg-2", 1); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := r.UpsertBlob(ctx, digestRepeat("a"), "reg-1"); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := r.UpsertBlob(ctx, digestRepeat("b"), "reg-2"); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}

	if err := r.DeleteRoutesForBackend(ctx, "reg-1"); err != nil {
		t.Fatalf("DeleteRoutesForBackend: %v", err)
	}
	if _, ok, _ := r.LookupManifest(ctx, "r", "t1"); ok {
		t.Fatal("reg-1 manifest route should be gone")
	}
	if _, ok, _ := r.LookupManifest(ctx, "r", "t2"); !ok {
		t.Fatal("reg-2 manifest route should remain")
	}
	if _, ok, _ := r.LookupBlob(ctx, digestRepeat("a")); ok {
		t.Fatal("reg-1 blob route should be gone")
	}
	if _, ok, _ := r.LookupBlob(ctx, digestRepeat("b")); !ok {
		t.Fatal("reg-2 blob route should remain")
	}
}

func TestReapUploadSessions(t *testing.T) {
	r := newRoutesRepo(t)
	ctx := context.Background()

	// Insert one stale session directly (older than the reap cutoff).
	stale := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO cache_upload_sessions (upload_uuid, repo, backend_id, created_at) VALUES (?, ?, ?, ?)`,
		"stale", "dagger-cache", "reg-1", stale); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	if err := r.RecordUpload(ctx, "fresh", "dagger-cache", "reg-1"); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}

	n, err := r.ReapUploadSessions(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ReapUploadSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	if _, ok, _ := r.LookupUpload(ctx, "fresh"); !ok {
		t.Fatal("fresh session should remain")
	}
	if _, ok, _ := r.LookupUpload(ctx, "stale"); ok {
		t.Fatal("stale session should be reaped")
	}
}
