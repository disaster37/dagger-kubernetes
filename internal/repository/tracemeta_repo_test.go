package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestTraceMetaRepoProvisionSetOnce(t *testing.T) {
	db := newTestDB(t)
	repo := NewTraceMetaRepo(db)
	ctx := context.Background()

	u1 := seedUser(t, db, "u1")
	u2 := seedUser(t, db, "u2")

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

func TestTraceMetaRepoIngestGroupSetOnce(t *testing.T) {
	db := newTestDB(t)
	repo := NewTraceMetaRepo(db)
	ctx := context.Background()

	grepo := NewGroupRepo(db)
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
	db := newTestDB(t)
	repo := NewTraceMetaRepo(db)
	if _, err := repo.Get(context.Background(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
}

func TestTraceMetaRepoListScoping(t *testing.T) {
	db := newTestDB(t)
	repo := NewTraceMetaRepo(db)
	ctx := context.Background()

	grepo := NewGroupRepo(db)
	g1 := &domain.Group{ID: newID(), Name: "G1", AgentAvailable: true}
	grepo.Create(ctx, g1)

	u1 := seedUser(t, db, "u1") // member of g1
	u2 := seedUser(t, db, "u2") // not a member
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
	db := newTestDB(t)
	repo := NewTraceMetaRepo(db)
	ctx := context.Background()

	grepo := NewGroupRepo(db)
	g1 := &domain.Group{ID: newID(), Name: "G1", AgentAvailable: true}
	grepo.Create(ctx, g1)

	u1 := seedUser(t, db, "u1")
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
	db := newTestDB(t)
	repo := NewTraceMetaRepo(db)
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
