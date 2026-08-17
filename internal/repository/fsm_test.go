package repository

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// memSink is an in-memory raft.SnapshotSink for round-trip tests.
type memSink struct {
	bytes.Buffer
}

func (m *memSink) ID() string    { return "mem" }
func (m *memSink) Cancel() error { return nil }
func (m *memSink) Close() error  { return nil }

func newTestFSM(t *testing.T) *FSM {
	t.Helper()
	return NewFSM()
}

func applyCmd(t *testing.T, f *FSM, kind commandKind, payload any) error {
	t.Helper()
	_, err := f.applyCommand(mustCommand(t, kind, payload))
	return err
}

func TestFSMUpsertUserUniqueness(t *testing.T) {
	f := newTestFSM(t)

	u1 := &cmdUser{ID: "u1", Username: "Alice", Role: domain.RoleUser, Create: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := applyCmd(t, f, kindUpsertUser, u1); err != nil {
		t.Fatalf("upsert u1: %v", err)
	}

	// Case-insensitive duplicate username.
	dup := &cmdUser{ID: "u2", Username: "alice", Role: domain.RoleUser, Create: true}
	if err := applyCmd(t, f, kindUpsertUser, dup); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate username: %v, want ErrConflict", err)
	}

	// Update path (same ID) with a renamed username succeeds.
	renamed := &cmdUser{ID: "u1", Username: "Bob", Role: domain.RoleAdmin}
	if err := applyCmd(t, f, kindUpsertUser, renamed); err != nil {
		t.Fatalf("update u1: %v", err)
	}
	got, err := f.readUserByID("u1")
	if err != nil || got.Username != "Bob" || got.Role != domain.RoleAdmin {
		t.Fatalf("updated user = %+v err=%v", got, err)
	}
	if _, err := f.readUserByUsername("bob"); err != nil {
		t.Fatalf("readUserByUsername bob: %v", err)
	}

	// Rename onto another user's name collides.
	u3 := &cmdUser{ID: "u3", Username: "Carol", Role: domain.RoleUser, Create: true}
	applyCmd(t, f, kindUpsertUser, u3)
	collide := &cmdUser{ID: "u1", Username: "carol", Role: domain.RoleUser}
	if err := applyCmd(t, f, kindUpsertUser, collide); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("rename collision: %v, want ErrConflict", err)
	}

	// OAuth uniqueness.
	oa := &cmdUser{ID: "oa1", Username: "OA1", OAuthProvider: "github", OAuthID: "42", Create: true}
	applyCmd(t, f, kindUpsertUser, oa)
	oa2 := &cmdUser{ID: "oa2", Username: "OA2", OAuthProvider: "github", OAuthID: "42", Create: true}
	if err := applyCmd(t, f, kindUpsertUser, oa2); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("oauth duplicate: %v, want ErrConflict", err)
	}
	if _, err := f.readUserByOAuth("github", "42"); err != nil {
		t.Fatalf("readUserByOAuth: %v", err)
	}

	// Update a missing user is ErrNotFound (insert on a used id is ErrConflict).
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "ghost", Username: "ghost"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update missing: %v, want ErrNotFound", err)
	}
	if err := applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "dup", Create: true}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("insert existing id: %v, want ErrConflict", err)
	}
}

func TestFSMDeleteUserCascades(t *testing.T) {
	f := newTestFSM(t)
	u := &cmdUser{ID: "u", Username: "u", Create: true}
	g := &cmdGroup{Group: domain.Group{ID: "g", Name: "g"}, Create: true}
	applyCmd(t, f, kindUpsertUser, u)
	applyCmd(t, f, kindUpsertGroup, g)
	applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g", UserIDs: []string{"u"}})
	applyCmd(t, f, kindUpsertToken, &cmdToken{ID: "t", UserID: "u", TokenHash: "h"})
	applyCmd(t, f, kindUpsertTraceProvision, cmdUpsertTraceProvision{TraceID: "tr", UserID: "u", UpdatedAt: time.Now().UTC()})

	if err := applyCmd(t, f, kindDeleteUser, "u"); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := f.readUserByID("u"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("user should be gone: %v", err)
	}
	if len(f.members("g")) != 0 {
		t.Fatalf("memberships should cascade: %v", f.members("g"))
	}
	if _, err := f.readTokenByUser("u"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("token should cascade: %v", err)
	}
	if m, _ := f.readTrace("tr"); m.UserID != "" {
		t.Fatalf("trace.user_id should be nulled, got %q", m.UserID)
	}

	// Deleting a missing user is ErrNotFound.
	if err := applyCmd(t, f, kindDeleteUser, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestFSMSetMembersReplace(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g", Name: "g"}, Create: true})
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "u1", Create: true})
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "u2", Create: true})

	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g", UserIDs: []string{"u1", "u2"}}); err != nil {
		t.Fatalf("set members: %v", err)
	}
	if got := f.allMemberships()["g"]; len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Fatalf("memberships = %v", got)
	}

	// Full replace.
	applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g", UserIDs: []string{"u2"}})
	if got := f.allMemberships()["g"]; len(got) != 1 || got[0] != "u2" {
		t.Fatalf("replaced memberships = %v", got)
	}
	if len(f.groupsForUser("u1")) != 0 {
		t.Fatalf("u1 should have no groups")
	}

	// Unknown user rejected.
	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g", UserIDs: []string{"ghost"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown user: %v, want ErrNotFound", err)
	}
	// Unknown group rejected.
	if err := applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "ghost", UserIDs: []string{"u1"}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown group: %v, want ErrNotFound", err)
	}
}

func TestFSMGroupAndProjectUniqueness(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", Name: "Engines"}, Create: true})
	if err := applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g2", Name: "engines"}, Create: true}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate group: %v", err)
	}

	applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p1", Name: "github.com/acme/api", GroupID: "g1"}, Create: true})
	if err := applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p2", Name: "GITHUB.COM/ACME/API"}, Create: true}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate project: %v", err)
	}

	// Delete group nulls project.group_id.
	applyCmd(t, f, kindDeleteGroup, "g1")
	p, _ := f.readProjectByID("p1")
	if p.GroupID != "" {
		t.Fatalf("project.group_id = %q, want empty", p.GroupID)
	}
	if err := applyCmd(t, f, kindDeleteGroup, "g1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing group: %v", err)
	}

	// Delete project.
	applyCmd(t, f, kindDeleteProject, "p1")
	if _, err := f.readProjectByID("p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("project should be gone: %v", err)
	}
	if err := applyCmd(t, f, kindDeleteProject, "p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete missing project: %v", err)
	}
}

func TestFSMTokenOnePerUser(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "u1", Create: true})
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "u2", Create: true})

	applyCmd(t, f, kindUpsertToken, &cmdToken{ID: "t1", UserID: "u1", TokenHash: "h1", Prefix: "dct_aa"})
	got, err := f.readTokenByUser("u1")
	if err != nil || got.TokenHash != "h1" {
		t.Fatalf("token = %+v err=%v", got, err)
	}

	// Same user upsert replaces.
	applyCmd(t, f, kindUpsertToken, &cmdToken{ID: "t2", UserID: "u1", TokenHash: "h2", Prefix: "dct_bb"})
	got, _ = f.readTokenByUser("u1")
	if got.TokenHash != "h2" || got.ID != "t2" {
		t.Fatalf("token should be replaced: %+v", got)
	}
	if _, err := f.readTokenByHash("h1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old hash should be gone: %v", err)
	}
	if _, err := f.readTokenByHash("h2"); err != nil {
		t.Fatalf("new hash: %v", err)
	}

	// Hash collision across users rejected.
	if err := applyCmd(t, f, kindUpsertToken, &cmdToken{ID: "t3", UserID: "u2", TokenHash: "h2"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("hash collision: %v, want ErrConflict", err)
	}

	// Touch + delete.
	at := time.Now().UTC()
	applyCmd(t, f, kindTouchToken, cmdTouchToken{ID: "t2", At: at})
	got, _ = f.readTokenByUser("u1")
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) {
		t.Fatalf("last_used_at = %v", got.LastUsedAt)
	}
	applyCmd(t, f, kindDeleteToken, cmdDeleteToken{UserID: "u1"})
	if _, err := f.readTokenByUser("u1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("token should be deleted: %v", err)
	}
	// Delete absent token is a no-op.
	if err := applyCmd(t, f, kindDeleteToken, cmdDeleteToken{UserID: "u1"}); err != nil {
		t.Fatalf("delete absent token: %v", err)
	}
	// Touch absent token is a no-op.
	if err := applyCmd(t, f, kindTouchToken, cmdTouchToken{ID: "nope", At: at}); err != nil {
		t.Fatalf("touch absent token: %v", err)
	}
}

func TestFSMTraceCOALESCE(t *testing.T) {
	f := newTestFSM(t)

	// Provision: first writer wins for user_id; empty version does not wipe.
	now1 := time.Now().UTC()
	applyCmd(t, f, kindUpsertTraceProvision, cmdUpsertTraceProvision{TraceID: "t", UserID: "u1", Version: "v1", UpdatedAt: now1})
	applyCmd(t, f, kindUpsertTraceProvision, cmdUpsertTraceProvision{TraceID: "t", UserID: "u2", Version: "", UpdatedAt: now1.Add(time.Minute)})
	m, _ := f.readTrace("t")
	if m.UserID != "u1" || m.Version != "v1" {
		t.Fatalf("provision coalesce = %+v", m)
	}

	// Ingest: group_id set-once; others newer non-empty wins; user_id preserved.
	started := now1.Add(-time.Hour)
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{
		TraceID: "t", GroupID: "g1", ProjectName: "p1", Status: "success", Version: "v2",
		CIRepo: "r1", DurationMS: 100, StartedAt: started, UpdatedAt: now1.Add(2 * time.Minute),
	})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{
		TraceID: "t", GroupID: "g2", Status: "failed", DurationMS: 0, UpdatedAt: now1.Add(3 * time.Minute),
	})
	m, _ = f.readTrace("t")
	if m.UserID != "u1" {
		t.Fatalf("user_id should be preserved: %q", m.UserID)
	}
	if m.GroupID != "g1" {
		t.Fatalf("group_id should be set-once: %q", m.GroupID)
	}
	if m.Status != "failed" {
		t.Fatalf("status should update: %q", m.Status)
	}
	if m.ProjectName != "p1" || m.Version != "v2" || m.CIRepo != "r1" {
		t.Fatalf("ingest fields = %+v", m)
	}
	if m.DurationMS != 100 {
		t.Fatalf("duration_ms should keep 100, got %d", m.DurationMS)
	}
	if !m.StartedAt.Equal(started) {
		t.Fatalf("started_at = %v, want %v", m.StartedAt, started)
	}

	// Ingest on a new trace sets user_id from insert.
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "fresh", UserID: "u9", UpdatedAt: now1})
	m, _ = f.readTrace("fresh")
	if m.UserID != "u9" {
		t.Fatalf("insert user_id = %q", m.UserID)
	}

	// Missing trace read.
	if _, err := f.readTrace("nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("readTrace missing: %v", err)
	}
}

func TestFSMTraceListFilterAndSort(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g1", Name: "G1"}, Create: true})
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true})

	base := time.Now().UTC()
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "in-group", GroupID: "g1", UserID: "u1", StartedAt: base, UpdatedAt: base})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "own", UserID: "u1", StartedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute)})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "other", UserID: "u2", StartedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute)})

	// Admin sees all, newest first.
	all := f.listTraces(domain.TraceFilter{IncludeUnassigned: true, Limit: 100})
	if len(all) != 3 || all[0].TraceID != "other" || all[2].TraceID != "in-group" {
		t.Fatalf("admin list = %v", traceIDs(all))
	}
	if all[2].GroupName != "G1" || all[2].Username != "alice" {
		t.Fatalf("join names = %+v", all[2])
	}

	// u1 sees group + own, not u2's unassigned.
	scoped := f.listTraces(domain.TraceFilter{GroupIDs: []string{"g1"}, UserID: "u1", Limit: 100})
	if len(scoped) != 2 || scoped[0].TraceID != "own" || scoped[1].TraceID != "in-group" {
		t.Fatalf("scoped list = %v", traceIDs(scoped))
	}

	// Unassigned-only.
	unassigned := f.listTraces(domain.TraceFilter{UnassignedOnly: true, Limit: 100})
	if len(unassigned) != 2 {
		t.Fatalf("unassigned = %v", traceIDs(unassigned))
	}

	// Limit clamp.
	limited := f.listTraces(domain.TraceFilter{IncludeUnassigned: true, Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("limit=1 -> %d", len(limited))
	}
}

func traceIDs(rows []*domain.TraceListResult) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.TraceID
	}
	return out
}

func TestFSMMetaAndCacheRouting(t *testing.T) {
	f := newTestFSM(t)

	// Meta.
	applyCmd(t, f, kindSetMeta, cmdSetMeta{Key: "k", Value: "v1"})
	if v, _ := f.readMeta("k"); v != "v1" {
		t.Fatalf("meta = %q", v)
	}
	applyCmd(t, f, kindSetMeta, cmdSetMeta{Key: "k", Value: "v2"})
	if v, _ := f.readMeta("k"); v != "v2" {
		t.Fatalf("meta upsert = %q", v)
	}
	if _, err := f.readMeta("nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("readMeta missing: %v", err)
	}

	// Manifest routes.
	applyCmd(t, f, kindUpsertManifestRoute, &domain.CacheRoute{Repo: "r", Tag: "t", Digest: "d1", BackendID: "b1", StoredBytes: 10, CreatedAt: "2026-01-01T00:00:00Z", LastSeenAt: "2026-01-01T00:00:00Z"})
	applyCmd(t, f, kindUpsertManifestRoute, &domain.CacheRoute{Repo: "r", Tag: "t", Digest: "d2", BackendID: "b2", StoredBytes: 20, CreatedAt: "2026-02-01T00:00:00Z", LastSeenAt: "2026-02-01T00:00:00Z"})
	cr, ok := f.lookupManifestRoute("r", "t")
	if !ok || cr.BackendID != "b2" || cr.StoredBytes != 20 {
		t.Fatalf("manifest route = %+v ok=%v", cr, ok)
	}
	if cr.CreatedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("created_at should be set-once: %q", cr.CreatedAt)
	}

	// Blob routes (dedupe + most recent).
	applyCmd(t, f, kindUpsertBlobRoute, cmdUpsertBlobRoute{Digest: "dgst", BackendID: "b1", CreatedAt: "2026-01-01T00:00:00Z"})
	applyCmd(t, f, kindUpsertBlobRoute, cmdUpsertBlobRoute{Digest: "dgst", BackendID: "b1", CreatedAt: "2026-03-01T00:00:00Z"})
	applyCmd(t, f, kindUpsertBlobRoute, cmdUpsertBlobRoute{Digest: "dgst", BackendID: "b2", CreatedAt: "2026-02-01T00:00:00Z"})
	if backend, ok := f.lookupBlobRoute("dgst"); !ok || backend != "b2" {
		t.Fatalf("lookupBlobRoute = %q ok=%v, want b2", backend, ok)
	}

	// Charges.
	if charge := f.backendCharge("b2"); charge != 20 {
		t.Fatalf("backendCharge b2 = %d", charge)
	}
	charges := f.allCharges()
	if charges["b2"] != 20 {
		t.Fatalf("allCharges = %v", charges)
	}

	// Upload lifecycle.
	applyCmd(t, f, kindRecordUpload, &domain.CacheUploadSession{UploadUUID: "up", Repo: "r", BackendID: "b1", CreatedAt: "2026-01-01T00:00:00Z"})
	if _, ok := f.lookupUpload("up"); !ok {
		t.Fatal("upload should exist")
	}
	applyCmd(t, f, kindDeleteUpload, cmdDeleteUpload{UUID: "up"})
	if _, ok := f.lookupUpload("up"); ok {
		t.Fatal("upload should be gone")
	}

	// Delete manifest route + delete routes for backend.
	applyCmd(t, f, kindDeleteManifestRoute, cmdDeleteManifestRoute{Repo: "r", Tag: "t"})
	if _, ok := f.lookupManifestRoute("r", "t"); ok {
		t.Fatal("manifest route should be gone")
	}
	applyCmd(t, f, kindUpsertManifestRoute, &domain.CacheRoute{Repo: "r", Tag: "x", BackendID: "b1", CreatedAt: "2026-01-01T00:00:00Z", LastSeenAt: "2026-01-01T00:00:00Z"})
	applyCmd(t, f, kindDeleteRoutesForBackend, cmdDeleteRoutesForBackend{BackendID: "b1"})
	if _, ok := f.lookupManifestRoute("r", "x"); ok {
		t.Fatal("backend manifest routes should be gone")
	}
	if _, ok := f.lookupBlobRoute("dgst"); !ok {
		t.Fatal("b2 blob route should remain")
	}
}

func TestFSMReapUploads(t *testing.T) {
	f := newTestFSM(t)
	old := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	applyCmd(t, f, kindRecordUpload, &domain.CacheUploadSession{UploadUUID: "old", CreatedAt: old})
	applyCmd(t, f, kindRecordUpload, &domain.CacheUploadSession{UploadUUID: "fresh", CreatedAt: fresh})

	cutoff := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	f.state.mu.Lock()
	n, err := f.state.reapUploads(cutoff)
	f.state.mu.Unlock()
	if err != nil || n != 1 {
		t.Fatalf("reap = %d err=%v, want 1", n, err)
	}
	if _, ok := f.lookupUpload("old"); ok {
		t.Fatal("old upload should be reaped")
	}
	if _, ok := f.lookupUpload("fresh"); !ok {
		t.Fatal("fresh upload should remain")
	}

	// Invalid cutoff errors.
	if _, err := f.state.reapUploads("not-a-time"); err == nil {
		t.Fatal("expected error for invalid cutoff")
	}
}

func TestFSMUnknownKind(t *testing.T) {
	f := newTestFSM(t)
	if _, err := f.applyCommand(&command{Kind: 99}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
	// Malformed payload.
	if _, err := f.applyCommand(&command{Kind: kindUpsertUser, Data: []byte("{bad")}); err == nil {
		t.Fatal("expected error for malformed payload")
	}
}

func TestFSMSnapshotRestoreRoundTrip(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u", Username: "alice", PasswordHash: "hash", Create: true})
	applyCmd(t, f, kindUpsertGroup, &cmdGroup{Group: domain.Group{ID: "g", Name: "g"}, Create: true})
	applyCmd(t, f, kindUpsertProject, &cmdProject{Project: domain.Project{ID: "p", Name: "github.com/acme/api", GroupID: "g"}, Create: true})
	applyCmd(t, f, kindSetMembers, cmdSetMembers{GroupID: "g", UserIDs: []string{"u"}})
	applyCmd(t, f, kindUpsertToken, &cmdToken{ID: "t", UserID: "u", TokenHash: "h", TokenCiphertext: "ct"})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "tr", UserID: "u", GroupID: "g", ProjectName: "github.com/acme/api", UpdatedAt: time.Now().UTC()})
	applyCmd(t, f, kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	applyCmd(t, f, kindUpsertManifestRoute, &domain.CacheRoute{Repo: "r", Tag: "t", BackendID: "b1", StoredBytes: 5, CreatedAt: "2026-01-01T00:00:00Z", LastSeenAt: "2026-01-01T00:00:00Z"})
	applyCmd(t, f, kindUpsertBlobRoute, cmdUpsertBlobRoute{Digest: "dgst", BackendID: "b1", CreatedAt: "2026-01-01T00:00:00Z"})
	applyCmd(t, f, kindRecordUpload, &domain.CacheUploadSession{UploadUUID: "up", CreatedAt: "2026-01-01T00:00:00Z"})

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	snap.Release()

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	u, err := restored.readUserByID("u")
	if err != nil || u.PasswordHash != "hash" {
		t.Fatalf("user after restore = %+v err=%v (password hash must survive)", u, err)
	}
	if len(restored.allMemberships()["g"]) != 1 {
		t.Fatalf("membership after restore: %v", restored.allMemberships())
	}
	tok, err := restored.readTokenByUser("u")
	if err != nil || tok.TokenHash != "h" || tok.TokenCiphertext != "ct" {
		t.Fatalf("token after restore = %+v err=%v (hashes must survive)", tok, err)
	}
	if p, err := restored.readProjectByID("p"); err != nil || p.GroupID != "g" {
		t.Fatalf("project after restore = %+v err=%v", p, err)
	}
	if tr, err := restored.readTrace("tr"); err != nil || tr.GroupID != "g" {
		t.Fatalf("trace after restore = %+v err=%v", tr, err)
	}
	if cr, ok := restored.lookupManifestRoute("r", "t"); !ok || cr.StoredBytes != 5 {
		t.Fatalf("manifest route after restore = %+v ok=%v", cr, ok)
	}
	if b, ok := restored.lookupBlobRoute("dgst"); !ok || b != "b1" {
		t.Fatalf("blob route after restore = %q ok=%v", b, ok)
	}
	if v, _ := restored.readMeta("k"); v != "v" {
		t.Fatalf("meta after restore = %q", v)
	}
	if _, ok := restored.lookupUpload("up"); !ok {
		t.Fatal("upload after restore missing")
	}

	// Restore replaces (not merges).
	replaced := NewFSM()
	applyCmd(t, replaced, kindUpsertUser, &cmdUser{ID: "stale", Username: "stale", Create: true})
	if err := replaced.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore replace: %v", err)
	}
	if _, err := replaced.readUserByID("stale"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale user should be gone after restore: %v", err)
	}
	if _, err := replaced.readUserByID("u"); err != nil {
		t.Fatalf("restored user missing: %v", err)
	}

	// Corrupt snapshot.
	if err := NewFSM().Restore(io.NopCloser(strings.NewReader("{not-json"))); err == nil {
		t.Fatal("expected error for corrupt snapshot")
	}
}

// TestFSMRestoreConcurrentReads guards against regressions where Restore swaps
// the fsmState pointer, racing with lock-free read-helper dereferences. Run
// under -race in CI.
func TestFSMRestoreConcurrentReads(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sink := &memSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	payload := sink.Bytes()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = f.readMeta("k")
				_, _ = f.readUserByID("u")
				f.listUsers()
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if err := f.Restore(io.NopCloser(bytes.NewReader(payload))); err != nil {
			t.Fatalf("Restore: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

type failingSink struct{}

func (f failingSink) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (f failingSink) Close() error              { return nil }
func (f failingSink) ID() string                { return "failing" }
func (f failingSink) Cancel() error             { return nil }

func TestFSMSnapshotPersistError(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := snap.Persist(failingSink{}); err == nil {
		t.Fatal("expected error persisting to a failing sink")
	}
}
