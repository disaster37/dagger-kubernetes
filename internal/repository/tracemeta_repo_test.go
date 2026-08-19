package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestTraceMetaRepoProvisionSetOnce(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	u1 := seedUser(t, store, "u1")
	u2 := seedUser(t, store, "u2")

	if err := repo.UpsertProvision(ctx, "t1", u1.ID, "v0.21.4"); err != nil {
		t.Fatalf("UpsertProvision: %v", err)
	}
	// Second provision by a different user must not steal the trace, and an
	// empty version must not wipe the previously recorded one.
	if err := repo.UpsertProvision(ctx, "t1", u2.ID, ""); err != nil {
		t.Fatalf("UpsertProvision 2: %v", err)
	}
	got, _ := repo.Get(ctx, "t1")
	if got.UserID != u1.ID {
		t.Fatalf("user_id = %q, want %s (first writer wins)", got.UserID, u1.ID)
	}
	if got.Version != "v0.21.4" {
		t.Fatalf("version = %q, want v0.21.4 (set-once)", got.Version)
	}
}

func TestTraceMetaRepoValidatesTraceIDKey(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	t.Run("provision rejects invalid key", func(t *testing.T) {
		for _, id := range []string{`bad"id`, "has space", "", ".leading-dot"} {
			err := repo.UpsertProvision(ctx, id, "u1", "v0.21.4")
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("UpsertProvision(%q) err = %v, want ErrValidation", id, err)
			}
		}
	})

	t.Run("ingest rejects invalid key", func(t *testing.T) {
		for _, id := range []string{`bad"id`, "has space", "", "a/b"} {
			err := repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: id})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("UpsertIngest(%q) err = %v, want ErrValidation", id, err)
			}
		}
	})

	t.Run("broad-charset key still succeeds", func(t *testing.T) {
		if err := repo.UpsertProvision(ctx, "test-trace-001", "u1", "v0.21.4"); err != nil {
			t.Fatalf("UpsertProvision broad key: %v", err)
		}
		if err := repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "in-group_1.v2", Status: "success"}); err != nil {
			t.Fatalf("UpsertIngest broad key: %v", err)
		}
		if _, err := repo.Get(ctx, "test-trace-001"); err != nil {
			t.Fatalf("Get broad key: %v", err)
		}
	})
}

func TestTraceMetaRepoIngestGroupSetOnce(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	grepo := NewGroupRepo(store)
	g1 := &domain.Group{ID: newID(), Name: "G1", AgentAvailable: true}
	grepo.Create(ctx, g1)
	g2 := &domain.Group{ID: newID(), Name: "G2", AgentAvailable: true}
	grepo.Create(ctx, g2)

	// First ingest sets the group.
	if err := repo.UpsertIngest(ctx, &domain.TraceMeta{
		TraceID:     "t2",
		GroupID:     g1.ID,
		ProjectName: "github.com/acme/x",
		Status:      "success",
		Version:     "v0.21.4",
		CIRepo:      "github.com/acme/x",
		DurationMS:  1000,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertIngest 1: %v", err)
	}

	// Second ingest with a different group must NOT overwrite (set-once).
	if err := repo.UpsertIngest(ctx, &domain.TraceMeta{
		TraceID: "t2",
		GroupID: g2.ID,
		Status:  "failed",
	}); err != nil {
		t.Fatalf("UpsertIngest 2: %v", err)
	}
	got, _ := repo.Get(ctx, "t2")
	if got.GroupID != g1.ID {
		t.Fatalf("group_id = %q, want %s (set-once)", got.GroupID, g1.ID)
	}
	// Non-empty newer values do update.
	if got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.ProjectName != "github.com/acme/x" {
		t.Fatalf("project_name = %q", got.ProjectName)
	}
}

func TestTraceMetaRepoGetMissing(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	if _, err := repo.Get(context.Background(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
}

func TestTraceMetaRepoListScoping(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	grepo := NewGroupRepo(store)
	g1 := &domain.Group{ID: newID(), Name: "G1", AgentAvailable: true}
	grepo.Create(ctx, g1)

	u1 := seedUser(t, store, "u1") // member of g1
	u2 := seedUser(t, store, "u2") // not a member
	grepo.SetMembers(ctx, g1.ID, []string{u1.ID})

	// trace in g1
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "in-group", GroupID: g1.ID, UserID: u1.ID, ProjectName: "p1", StartedAt: time.Now().UTC()})
	// unassigned trace owned by u1
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "own-unassigned", UserID: u1.ID, ProjectName: "p2", StartedAt: time.Now().UTC()})
	// unassigned trace owned by u2 (should NOT be visible to u1)
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "other-unassigned", UserID: u2.ID, ProjectName: "p3", StartedAt: time.Now().UTC()})

	// Admin sees all.
	adminRes, err := repo.List(ctx, domain.TraceFilter{IncludeUnassigned: true, Limit: 100})
	if err != nil {
		t.Fatalf("admin List: %v", err)
	}
	if len(adminRes) != 3 {
		t.Fatalf("admin sees %d, want 3", len(adminRes))
	}

	// u1 sees group traces + own unassigned (2), not u2's unassigned.
	userRes, err := repo.List(ctx, domain.TraceFilter{GroupIDs: []string{g1.ID}, UserID: u1.ID, Limit: 100})
	if err != nil {
		t.Fatalf("user List: %v", err)
	}
	if len(userRes) != 2 {
		t.Fatalf("u1 sees %d, want 2", len(userRes))
	}
	seen := map[string]bool{}
	for _, r := range userRes {
		seen[r.TraceID] = true
	}
	if !seen["in-group"] || !seen["own-unassigned"] || seen["other-unassigned"] {
		t.Fatalf("u1 visibility = %v", seen)
	}

	// u2 (no groups) sees only own unassigned.
	u2Res, err := repo.List(ctx, domain.TraceFilter{GroupIDs: nil, UserID: u2.ID, Limit: 100})
	if err != nil {
		t.Fatalf("u2 List: %v", err)
	}
	if len(u2Res) != 1 || u2Res[0].TraceID != "other-unassigned" {
		t.Fatalf("u2 visibility = %v", u2Res)
	}
}

func TestTraceMetaRepoListUnassignedOnly(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	grepo := NewGroupRepo(store)
	g1 := &domain.Group{ID: newID(), Name: "G1", AgentAvailable: true}
	grepo.Create(ctx, g1)

	u1 := seedUser(t, store, "u1")
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "in-group", GroupID: g1.ID, UserID: u1.ID, StartedAt: time.Now().UTC()})
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "unassigned-1", UserID: u1.ID, StartedAt: time.Now().UTC()})
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "unassigned-2", StartedAt: time.Now().UTC()})

	// Admin "unassigned" view: only traces without a group.
	res, err := repo.List(ctx, domain.TraceFilter{UnassignedOnly: true, Limit: 100})
	if err != nil {
		t.Fatalf("List unassigned: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("unassigned view returned %d, want 2", len(res))
	}
	seen := map[string]bool{}
	for _, r := range res {
		seen[r.TraceID] = true
	}
	if !seen["unassigned-1"] || !seen["unassigned-2"] || seen["in-group"] {
		t.Fatalf("unassigned visibility = %v", seen)
	}
}

func TestTraceMetaRepoListLimitClamping(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: string(rune('a' + i)), StartedAt: time.Now().UTC()})
	}
	// default limit
	res, _ := repo.List(ctx, domain.TraceFilter{IncludeUnassigned: true})
	if len(res) != 5 {
		t.Fatalf("default limit returned %d, want 5", len(res))
	}
	// explicit small limit
	res, _ = repo.List(ctx, domain.TraceFilter{IncludeUnassigned: true, Limit: 2})
	if len(res) != 2 {
		t.Fatalf("limit=2 returned %d, want 2", len(res))
	}
	// over-max clamps to 500 (just verify no error)
	res, _ = repo.List(ctx, domain.TraceFilter{IncludeUnassigned: true, Limit: 1000})
	if len(res) != 5 {
		t.Fatalf("limit=1000 returned %d, want 5", len(res))
	}
}

func TestTraceMetaRepoDelete(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	if err := repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "abc123", Status: "success", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("UpsertIngest: %v", err)
	}
	if _, err := repo.Get(ctx, "abc123"); err != nil {
		t.Fatalf("Get before delete: %v", err)
	}

	if err := repo.Delete(ctx, "abc123"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, "abc123"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v, want ErrNotFound", err)
	}

	// Idempotent: deleting an absent row returns nil.
	if err := repo.Delete(ctx, "abc123"); err != nil {
		t.Fatalf("Delete again: %v", err)
	}
}

func TestTraceMetaRepoListBefore(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	base := time.Now().UTC()
	cutoff := base.Add(-time.Hour)

	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "old", Status: "success", StartedAt: base.Add(-2 * time.Hour), UpdatedAt: base.Add(-2 * time.Hour)})
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "old-running", Status: "running", StartedAt: base.Add(-2 * time.Hour), UpdatedAt: base.Add(-2 * time.Hour)})
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "future", Status: "success", StartedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)})

	protected, err := repo.ListBefore(ctx, cutoff, true)
	if err != nil {
		t.Fatalf("ListBefore: %v", err)
	}
	if len(protected) != 1 || protected[0].TraceID != "old" {
		t.Fatalf("protected = %v, want [old]", traceMetaIDs(protected))
	}

	all, err := repo.ListBefore(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("ListBefore(false): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unprotected = %v, want 2", traceMetaIDs(all))
	}
}

func TestTraceMetaRepoMarkFailedInvalidTraceID(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	for _, id := range []string{`bad"id`, "has space", "", ".leading-dot", "a/b"} {
		transitioned, err := repo.MarkFailed(ctx, id, "reason")
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("MarkFailed(%q) err = %v, want ErrValidation", id, err)
		}
		if transitioned {
			t.Fatalf("MarkFailed(%q) transitioned = true, want false", id)
		}
	}
}

func TestTraceMetaRepoMarkFailedBoundsReason(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	if err := repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "test-trace-001", Status: "running"}); err != nil {
		t.Fatalf("UpsertIngest: %v", err)
	}

	reason := strings.Repeat("r", maxReasonLen+100)
	transitioned, err := repo.MarkFailed(ctx, "test-trace-001", reason)
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !transitioned {
		t.Fatal("expected transition")
	}
	m, _ := repo.Get(ctx, "test-trace-001")
	if len(m.FailureReason) != maxReasonLen {
		t.Fatalf("failure_reason len = %d, want %d (bounded)", len(m.FailureReason), maxReasonLen)
	}
	if !strings.HasPrefix(reason, m.FailureReason) {
		t.Fatalf("failure_reason = %q, want prefix of original", m.FailureReason)
	}
}

func TestTraceMetaRepoMarkFailedTransitioned(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	if err := repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "test-trace-002", Status: "running"}); err != nil {
		t.Fatalf("UpsertIngest: %v", err)
	}

	transitioned, err := repo.MarkFailed(ctx, "test-trace-002", "client connection lost")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !transitioned {
		t.Fatal("first MarkFailed should transition")
	}
	m, _ := repo.Get(ctx, "test-trace-002")
	if m.Status != "failed" || m.FailureReason != "client connection lost" {
		t.Fatalf("trace = %+v", m)
	}

	// Idempotent: a terminal trace is untouched.
	transitioned, err = repo.MarkFailed(ctx, "test-trace-002", "other")
	if err != nil {
		t.Fatalf("second MarkFailed: %v", err)
	}
	if transitioned {
		t.Fatal("second MarkFailed should not transition")
	}
	m, _ = repo.Get(ctx, "test-trace-002")
	if m.FailureReason != "client connection lost" {
		t.Fatalf("failure_reason = %q, want original", m.FailureReason)
	}
}

func TestTraceMetaRepoStats(t *testing.T) {
	store := newTestRaftStore(t)
	repo := NewTraceMetaRepo(store)
	ctx := context.Background()

	base := time.Now().UTC()
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "a", StartedAt: base, UpdatedAt: base})
	repo.UpsertIngest(ctx, &domain.TraceMeta{TraceID: "b", StartedAt: base.Add(-3 * time.Hour), UpdatedAt: base.Add(-3 * time.Hour)})

	count, oldest, err := repo.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if !oldest.Equal(base.Add(-3 * time.Hour)) {
		t.Fatalf("oldest = %v, want %v", oldest, base.Add(-3*time.Hour))
	}
}
