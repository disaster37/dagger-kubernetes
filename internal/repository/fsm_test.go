package repository

import (
	"bytes"
	"errors"
	"fmt"
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
	all := f.listTraces(&domain.TraceFilter{IncludeUnassigned: true, Limit: 100})
	if len(all) != 3 || all[0].TraceID != "other" || all[2].TraceID != "in-group" {
		t.Fatalf("admin list = %v", traceIDs(all))
	}
	if all[2].GroupName != "G1" || all[2].Username != "alice" {
		t.Fatalf("join names = %+v", all[2])
	}

	// u1 sees group + own, not u2's unassigned.
	scoped := f.listTraces(&domain.TraceFilter{GroupIDs: []string{"g1"}, UserID: "u1", Limit: 100})
	if len(scoped) != 2 || scoped[0].TraceID != "own" || scoped[1].TraceID != "in-group" {
		t.Fatalf("scoped list = %v", traceIDs(scoped))
	}

	// Unassigned-only.
	unassigned := f.listTraces(&domain.TraceFilter{UnassignedOnly: true, Limit: 100})
	if len(unassigned) != 2 {
		t.Fatalf("unassigned = %v", traceIDs(unassigned))
	}

	// Limit clamp.
	limited := f.listTraces(&domain.TraceFilter{IncludeUnassigned: true, Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("limit=1 -> %d", len(limited))
	}
}

func TestFSMTraceListTextFilters(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u1", Username: "alice", Create: true})
	applyCmd(t, f, kindUpsertUser, &cmdUser{ID: "u2", Username: "bob", Create: true})

	base := time.Now().UTC()
	ingest := func(id, userID, ciRepo, project string) {
		t.Helper()
		applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{
			TraceID: id, UserID: userID, CIRepo: ciRepo, ProjectName: project,
			StartedAt: base, UpdatedAt: base,
		})
	}
	ingest("t1", "u1", "github.com/org/repo", "org-project")
	ingest("t2", "u2", "gitlab.com/other/thing", "other-project")
	ingest("t3", "", "", "bare-project")

	cases := []struct {
		name   string
		filter domain.TraceFilter
		want   []string
	}{
		{"repo matches ci_repo", domain.TraceFilter{CIRepo: "gitlab.com", IncludeUnassigned: true}, []string{"t2"}},
		{"repo matches project fallback", domain.TraceFilter{CIRepo: "org-project", IncludeUnassigned: true}, []string{"t1"}},
		{"repo case-insensitive", domain.TraceFilter{CIRepo: "GITHUB.COM/ORG", IncludeUnassigned: true}, []string{"t1"}},
		{"repo matches both fields", domain.TraceFilter{CIRepo: "project", IncludeUnassigned: true}, []string{"t1", "t2", "t3"}},
		{"username substring", domain.TraceFilter{Username: "bob", IncludeUnassigned: true}, []string{"t2"}},
		{"username case-insensitive", domain.TraceFilter{Username: "ALICE", IncludeUnassigned: true}, []string{"t1"}},
		{"combined AND", domain.TraceFilter{CIRepo: "gitlab", Username: "alice", IncludeUnassigned: true}, nil},
		{"combined AND match", domain.TraceFilter{CIRepo: "gitlab", Username: "bob", IncludeUnassigned: true}, []string{"t2"}},
		{"no match", domain.TraceFilter{CIRepo: "nope", Username: "nope", IncludeUnassigned: true}, nil},
		{"empty filters unfiltered", domain.TraceFilter{IncludeUnassigned: true}, []string{"t1", "t2", "t3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.filter.Limit = 100
			got := traceIDs(f.listTraces(&tc.filter))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			wantSet := make(map[string]bool, len(tc.want))
			for _, w := range tc.want {
				wantSet[w] = true
			}
			for _, g := range got {
				if !wantSet[g] {
					t.Fatalf("unexpected trace %q in %v (want %v)", g, got, tc.want)
				}
			}
		})
	}
}

func TestFSMTraceListNormalizesStatus(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "empty", StartedAt: now, UpdatedAt: now})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "legacy-unset", Status: "unset", StartedAt: now, UpdatedAt: now})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "running", Status: "running", StartedAt: now, UpdatedAt: now})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "success", Status: "success", StartedAt: now, UpdatedAt: now})

	rows := f.listTraces(&domain.TraceFilter{IncludeUnassigned: true, Limit: 100})
	want := map[string]string{
		"empty":        "running",
		"legacy-unset": "running",
		"running":      "running",
		"success":      "success",
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		if r.Status != want[r.TraceID] {
			t.Fatalf("status[%s] = %q, want %q", r.TraceID, r.Status, want[r.TraceID])
		}
	}
}

func traceIDs(rows []*domain.TraceListResult) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.TraceID
	}
	return out
}

func traceMetaIDs(rows []*domain.TraceMeta) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.TraceID
	}
	return out
}

func TestFSMDeleteTrace(t *testing.T) {
	f := newTestFSM(t)
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "abc123", Status: "success", StartedAt: time.Now().UTC()})
	if _, err := f.readTrace("abc123"); err != nil {
		t.Fatalf("readTrace before delete: %v", err)
	}

	if err := applyCmd(t, f, kindDeleteTrace, cmdDeleteTrace{TraceID: "abc123"}); err != nil {
		t.Fatalf("delete trace: %v", err)
	}
	if _, err := f.readTrace("abc123"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("readTrace after delete: %v, want ErrNotFound", err)
	}
	// Re-apply is idempotent (delete on a missing key is a no-op).
	if err := applyCmd(t, f, kindDeleteTrace, cmdDeleteTrace{TraceID: "abc123"}); err != nil {
		t.Fatalf("re-delete trace: %v", err)
	}
}

func TestFSMListTracesBefore(t *testing.T) {
	f := newTestFSM(t)
	base := time.Now().UTC()
	cutoff := base.Add(-time.Hour)

	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "oldest", Status: "success", StartedAt: base.Add(-5 * time.Hour), UpdatedAt: base.Add(-5 * time.Hour)})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "old-running", Status: "running", StartedAt: base.Add(-3 * time.Hour), UpdatedAt: base.Add(-3 * time.Hour)})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "old-empty", StartedAt: base.Add(-4 * time.Hour), UpdatedAt: base.Add(-4 * time.Hour)})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "future", Status: "success", StartedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "unknown-age", Status: "success"})

	// protectRunning excludes "" and "running"; unknown-age + future skipped.
	protected := f.listTracesBefore(cutoff, true)
	if len(protected) != 1 || protected[0].TraceID != "oldest" {
		t.Fatalf("protected = %v, want [oldest]", traceMetaIDs(protected))
	}

	// Without protection, running + empty-status traces are included; future
	// and unknown-age still skipped. Sorted oldest-first.
	unprotected := f.listTracesBefore(cutoff, false)
	want := []string{"oldest", "old-empty", "old-running"}
	if len(unprotected) != len(want) {
		t.Fatalf("unprotected = %v, want %v", traceMetaIDs(unprotected), want)
	}
	for i, id := range want {
		if unprotected[i].TraceID != id {
			t.Fatalf("unprotected order = %v, want %v", traceMetaIDs(unprotected), want)
		}
	}
}

func TestFSMTraceStats(t *testing.T) {
	f := newTestFSM(t)
	base := time.Now().UTC()
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "new", StartedAt: base, UpdatedAt: base})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "old", StartedAt: base.Add(-2 * time.Hour), UpdatedAt: base.Add(-2 * time.Hour)})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "no-time"})

	count, oldest := f.traceStats()
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if !oldest.Equal(base.Add(-2 * time.Hour)) {
		t.Fatalf("oldest = %v, want %v", oldest, base.Add(-2*time.Hour))
	}

	// Empty FSM: zero count and zero oldest.
	empty := NewFSM()
	count, oldest = empty.traceStats()
	if count != 0 || !oldest.IsZero() {
		t.Fatalf("empty stats = (%d, %v), want (0, zero)", count, oldest)
	}
}

func TestFSMMeta(t *testing.T) {
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
}

// TestFSMReservedCacheRouteKindsReplayNoOp ensures the removed registry-cache
// routing commands (persisted as command.Kind bytes in pre-upgrade Raft logs)
// replay as decode-only no-ops instead of erroring with "unknown raft command
// kind" (D1). Payloads are raw maps so the test does not depend on the deleted
// payload types.
func TestFSMReservedCacheRouteKindsReplayNoOp(t *testing.T) {
	legacy := []struct {
		kind    commandKind
		payload map[string]any
	}{
		{kindUpsertManifestRoute, map[string]any{"repo": "r", "tag": "t", "digest": "d", "backend_id": "b"}},
		{kindDeleteManifestRoute, map[string]any{"repo": "r", "tag": "t"}},
		{kindUpsertBlobRoute, map[string]any{"digest": "d", "backend_id": "b", "created_at": "2026-01-01T00:00:00Z"}},
		{kindRecordUpload, map[string]any{"upload_uuid": "up", "repo": "r", "backend_id": "b"}},
		{kindDeleteUpload, map[string]any{"uuid": "up"}},
		{kindReapUploads, map[string]any{"cutoff": "2026-01-01T00:00:00Z"}},
		{kindTouchManifestRoute, map[string]any{"repo": "r", "tag": "t", "at": "2026-01-01T00:00:00Z"}},
	}

	for _, tc := range legacy {
		t.Run(fmt.Sprintf("kind-%d", tc.kind), func(t *testing.T) {
			f := newTestFSM(t)
			cmd, err := newCommand(tc.kind, tc.payload)
			if err != nil {
				t.Fatalf("newCommand: %v", err)
			}
			resp, err := f.applyCommand(cmd)
			if err != nil {
				t.Fatalf("applyCommand(kind %d): %v", tc.kind, err)
			}
			if resp != nil {
				t.Fatalf("resp = %v, want nil no-op", resp)
			}
		})
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

func TestMarkTraceFailed(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()

	seed := func(id, status string) {
		applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: id, Status: status, UpdatedAt: now})
	}
	mark := func(id, reason string) bool {
		resp, err := f.applyCommand(mustCommand(t, kindMarkTraceFailed, cmdMarkTraceFailed{
			TraceID: id, Reason: reason, UpdatedAt: now.Add(time.Minute),
		}))
		if err != nil {
			t.Fatalf("markTraceFailed: %v", err)
		}
		return resp.(bool)
	}

	t.Run("missing trace", func(t *testing.T) {
		if mark("nope", "r") {
			t.Fatal("missing trace should not transition")
		}
		if _, err := f.readTrace("nope"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("missing trace should not exist: %v", err)
		}
	})

	t.Run("empty status transitions", func(t *testing.T) {
		seed("empty", "")
		if !mark("empty", "client connection lost") {
			t.Fatal("empty status should transition")
		}
		m, _ := f.readTrace("empty")
		if m.Status != "failed" || m.FailureReason != "client connection lost" {
			t.Fatalf("trace = %+v", m)
		}
	})

	t.Run("running transitions", func(t *testing.T) {
		seed("running", "running")
		if !mark("running", "client connection lost") {
			t.Fatal("running should transition")
		}
		m, _ := f.readTrace("running")
		if m.Status != "failed" {
			t.Fatalf("status = %q", m.Status)
		}
	})

	t.Run("success unchanged", func(t *testing.T) {
		seed("success", "success")
		if mark("success", "r") {
			t.Fatal("success should not transition")
		}
		m, _ := f.readTrace("success")
		if m.Status != "success" || m.FailureReason != "" {
			t.Fatalf("trace = %+v", m)
		}
	})

	t.Run("failed unchanged keeps reason", func(t *testing.T) {
		applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "failed", Status: "failed", FailureReason: "prior", UpdatedAt: now})
		if mark("failed", "new reason") {
			t.Fatal("failed should not transition")
		}
		m, _ := f.readTrace("failed")
		if m.Status != "failed" || m.FailureReason != "prior" {
			t.Fatalf("trace = %+v (existing reason must be preserved)", m)
		}
	})

	t.Run("updated_at advanced on transition", func(t *testing.T) {
		seed("time", "")
		mark("time", "r")
		m, _ := f.readTrace("time")
		if !m.UpdatedAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("updated_at = %v, want %v", m.UpdatedAt, now.Add(time.Minute))
		}
	})
}

func TestUpsertTraceIngestClearsFailureReason(t *testing.T) {
	f := newTestFSM(t)
	now := time.Now().UTC()

	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "running", UpdatedAt: now})
	applyCmd(t, f, kindMarkTraceFailed, cmdMarkTraceFailed{TraceID: "t", Reason: "client connection lost", UpdatedAt: now.Add(time.Minute)})
	m, _ := f.readTrace("t")
	if m.Status != "failed" || m.FailureReason != "client connection lost" {
		t.Fatalf("after mark failed: %+v", m)
	}

	// A late OTLP finish is authoritative and supersedes the disconnect reason.
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "success", UpdatedAt: now.Add(2 * time.Minute)})
	m, _ = f.readTrace("t")
	if m.Status != "success" {
		t.Fatalf("status = %q, want success", m.Status)
	}
	if m.FailureReason != "" {
		t.Fatalf("failure_reason = %q, want empty (OTLP finish supersedes)", m.FailureReason)
	}
}

func TestUpsertTraceIngestRunningDoesNotResurrectFailed(t *testing.T) {
	// A non-terminal OTLP status ("running", the default for an in-flight
	// root span) must NOT resurrect a disconnect-failed trace or clear its
	// failure_reason. Otherwise a late-arriving "running" span (buffered/
	// retried OTLP, or an engine-emitted root span without a status code)
	// would defeat the disconnect detection (CWE-346).
	f := newTestFSM(t)
	now := time.Now().UTC()

	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "running", UpdatedAt: now})
	applyCmd(t, f, kindMarkTraceFailed, cmdMarkTraceFailed{TraceID: "t", Reason: "client connection lost", UpdatedAt: now.Add(time.Minute)})
	m, _ := f.readTrace("t")
	if m.Status != "failed" || m.FailureReason != "client connection lost" {
		t.Fatalf("after mark failed: %+v", m)
	}

	// A late OTLP ingest with the non-terminal "running" status must NOT
	// overwrite the terminal "failed" status or clear the failure reason.
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "running", UpdatedAt: now.Add(2 * time.Minute)})
	m, _ = f.readTrace("t")
	if m.Status != "failed" {
		t.Fatalf("status = %q, want failed (non-terminal OTLP must not resurrect)", m.Status)
	}
	if m.FailureReason != "client connection lost" {
		t.Fatalf("failure_reason = %q, want preserved", m.FailureReason)
	}

	// A terminal OTLP finish still supersedes (regression guard).
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "success", UpdatedAt: now.Add(3 * time.Minute)})
	m, _ = f.readTrace("t")
	if m.Status != "success" || m.FailureReason != "" {
		t.Fatalf("after terminal OTLP: %+v (want success/empty reason)", m)
	}
}

func TestUpsertTraceIngestRunningDoesNotDowngradeSuccess(t *testing.T) {
	// A non-terminal OTLP status must not downgrade an existing terminal
	// status either (defense-in-depth for the same CWE-346 vector).
	f := newTestFSM(t)
	now := time.Now().UTC()

	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "success", UpdatedAt: now})
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "running", UpdatedAt: now.Add(time.Minute)})
	m, _ := f.readTrace("t")
	if m.Status != "success" {
		t.Fatalf("status = %q, want success (non-terminal must not downgrade)", m.Status)
	}
}

func TestUpsertTraceIngestRunningPromotesEmptyStatus(t *testing.T) {
	// A non-terminal OTLP status still promotes a non-terminal existing
	// status (the normal "" -> "running" path during a live run).
	f := newTestFSM(t)
	now := time.Now().UTC()

	applyCmd(t, f, kindUpsertTraceProvision, cmdUpsertTraceProvision{TraceID: "t", UserID: "u", UpdatedAt: now})
	m, _ := f.readTrace("t")
	if m.Status != "" {
		t.Fatalf("after provision: status = %q, want empty", m.Status)
	}
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "t", Status: "running", UpdatedAt: now.Add(time.Minute)})
	m, _ = f.readTrace("t")
	if m.Status != "running" {
		t.Fatalf("after running ingest: status = %q, want running", m.Status)
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
	applyCmd(t, f, kindUpsertTraceIngest, &domain.TraceMeta{TraceID: "tr-del", UserID: "u", UpdatedAt: time.Now().UTC()})
	applyCmd(t, f, kindDeleteTrace, cmdDeleteTrace{TraceID: "tr-del"})
	applyCmd(t, f, kindSetMeta, cmdSetMeta{Key: "k", Value: "v"})

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
	if _, err := restored.readTrace("tr-del"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted trace should not survive restore: %v", err)
	}
	if v, _ := restored.readMeta("k"); v != "v" {
		t.Fatalf("meta after restore = %q", v)
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

// TestFSMOldSnapshotWithRemovedCacheKeysRestores verifies the D1 compatibility
// guarantee: a pre-upgrade snapshot that still carries the removed
// registry-cache keys (object_routes/blob_routes/uploads) restores cleanly —
// encoding/json ignores unknown keys, so the cache-route data is silently
// dropped while every surviving key restores.
func TestFSMOldSnapshotWithRemovedCacheKeysRestores(t *testing.T) {
	old := `{
	  "users": [{"id":"u","username":"alice","role":"admin","password_hash":"hash",
	             "created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],
	  "groups": [],
	  "memberships": [],
	  "tokens": [],
	  "projects": [],
	  "traces": [],
	  "meta": {"k":"v"},
	  "object_routes": [{"repo":"a","tag":"b","backend_id":"x",
	                     "created_at":"2026-01-01T00:00:00Z","last_seen_at":"2026-01-01T00:00:00Z"}],
	  "blob_routes": [{"digest":"sha256:abc","backend_id":"x","created_at":"2026-01-01T00:00:00Z"}],
	  "uploads": [{"upload_uuid":"uuid","created_at":"2026-01-01T00:00:00Z"}]
	}`
	f := NewFSM()
	if err := f.Restore(io.NopCloser(strings.NewReader(old))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	u, err := f.readUserByID("u")
	if err != nil || u.Username != "alice" || u.PasswordHash != "hash" {
		t.Fatalf("user after restore = %+v err=%v (known keys must survive)", u, err)
	}
	if v, _ := f.readMeta("k"); v != "v" {
		t.Fatalf("meta after restore = %q", v)
	}
}

// TestUserOAuthFieldsRoundTrip verifies that the new OAuth user fields
// (OAuthGroups, OAuthAdmin) survive the cmdUser conversion (both directions)
// and a full upsert → snapshot → restore cycle (plan §7, FSM persistence of the
// admin_groups feature).
func TestUserOAuthFieldsRoundTrip(t *testing.T) {
	f := newTestFSM(t)

	now := time.Now().UTC()
	u := &domain.User{
		ID:            "u1",
		Username:      "alice",
		Role:          domain.RoleAdmin,
		OAuthProvider: "oidc",
		OAuthID:       "alice-sub",
		OAuthGroups:   []string{"HM_ADM_Outils", "devs"},
		OAuthAdmin:    true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	// Conversion round-trip: domain -> cmdUser -> domain.
	cu := cmdUserFrom(u)
	back := cu.toDomain()
	if len(back.OAuthGroups) != 2 || back.OAuthGroups[0] != "HM_ADM_Outils" || back.OAuthGroups[1] != "devs" {
		t.Fatalf("cmdUserFrom->toDomain lost OAuthGroups: %v", back.OAuthGroups)
	}
	if !back.OAuthAdmin {
		t.Fatal("cmdUserFrom->toDomain lost OAuthAdmin")
	}

	// FSM persistence: upsert -> read. cmdUserFrom never sets Create, so it
	// must be flipped for the insert.
	cu.Create = true
	if err := applyCmd(t, f, kindUpsertUser, cu); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := f.readUserByID("u1")
	if err != nil {
		t.Fatalf("readUserByID: %v", err)
	}
	if len(got.OAuthGroups) != 2 || !got.OAuthAdmin {
		t.Fatalf("stored user lost OAuth fields: groups=%v admin=%v", got.OAuthGroups, got.OAuthAdmin)
	}

	// Snapshot/restore round-trip.
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
	rp, err := restored.readUserByID("u1")
	if err != nil {
		t.Fatalf("readUserByID after restore: %v", err)
	}
	if len(rp.OAuthGroups) != 2 || rp.OAuthGroups[0] != "HM_ADM_Outils" || rp.OAuthGroups[1] != "devs" {
		t.Fatalf("OAuthGroups lost in snapshot/restore: %v", rp.OAuthGroups)
	}
	if !rp.OAuthAdmin {
		t.Fatal("OAuthAdmin lost in snapshot/restore")
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
