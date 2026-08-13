package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newAttributionForTest(t *testing.T) (*AttributionService, *GroupService, *UserService, *repos) {
	t.Helper()
	r := newServiceDB(t)
	psvc := NewProjectService(r.projects, r.groups, testLogger())
	gsvc := NewGroupService(r.groups, r.users, testLogger())
	usvc := NewUserService(r.users, r.groups, testLogger())
	asvc := NewAttributionService(psvc, r.groups, r.traceMeta, testLogger())
	return asvc, gsvc, usvc, r
}

func TestAttributionProvision(t *testing.T) {
	asvc, _, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	asvc.Provision(ctx, "t1", u.ID)
	meta, err := asvc.traceMeta.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.UserID != u.ID {
		t.Fatalf("user_id = %q, want %s", meta.UserID, u.ID)
	}

	// Second provision by a different user does not steal.
	u2 := seedUserSvc(t, usvc, "u2")
	asvc.Provision(ctx, "t1", u2.ID)
	meta, _ = asvc.traceMeta.Get(ctx, "t1")
	if meta.UserID != u.ID {
		t.Fatalf("user_id = %q, want %s (first writer wins)", meta.UserID, u.ID)
	}

	// Empty traceID is a no-op.
	asvc.Provision(ctx, "", u.ID)
}

func TestAttributionIngestExplicitAssignment(t *testing.T) {
	asvc, gsvc, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")
	g, _ := gsvc.Create(ctx, GroupInput{Name: "G1", AgentAvailable: true})
	proj, _ := asvc.projects.Create(ctx, "github.com/acme/api", g.ID)

	asvc.Ingest(ctx, "t1", u.ID, "github.com/acme/api", "", "github", "v0.21.4", "success", 1000, time.Now().UTC())

	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.GroupID != g.ID {
		t.Fatalf("group_id = %q, want %s (explicit assignment)", meta.GroupID, g.ID)
	}
	if meta.ProjectName != "github.com/acme/api" {
		t.Fatalf("project_name = %q", meta.ProjectName)
	}
	_ = proj
}

func TestAttributionIngestRegexAutoAssign(t *testing.T) {
	asvc, gsvc, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	// Two groups with patterns; first by id order should win.
	g1, _ := gsvc.Create(ctx, GroupInput{Name: "G1", AgentAvailable: true, AutoAssignPattern: `^github\.com/acme/.*`})
	g2, _ := gsvc.Create(ctx, GroupInput{Name: "G2", AgentAvailable: true, AutoAssignPattern: `^github\.com/.*`})

	// Ensure g1 wins by id order. If g2's id is lower, swap patterns.
	// (We rely on Create generating ids; to make the test deterministic we
	// re-create so g1 has the lower id by insertion order — but ids are random.
	// Instead, verify the assigned group is one of the two and the project is
	// assigned.)
	asvc.Ingest(ctx, "t1", u.ID, "github.com/acme/api", "", "github", "v0.21.4", "success", 1000, time.Now().UTC())

	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.GroupID == "" {
		t.Fatal("auto-assign should set a group")
	}
	if meta.GroupID != g1.ID && meta.GroupID != g2.ID {
		t.Fatalf("group_id = %q, want one of g1/g2", meta.GroupID)
	}
	// First-match-by-id-order: the group with the lower id whose pattern matches.
	// Both patterns match "github.com/acme/api"; the lower-id group should win.
	expected := g1.ID
	if g2.ID < g1.ID {
		expected = g2.ID
	}
	if meta.GroupID != expected {
		t.Fatalf("group_id = %q, want %s (first match by id order)", meta.GroupID, expected)
	}
}

func TestAttributionIngestInvalidRegexSkipped(t *testing.T) {
	asvc, gsvc, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	// Group with an invalid pattern is skipped (no panic); a valid group matches.
	// Note: GroupService rejects invalid patterns at Create time, so we insert
	// directly via the repo to simulate a bad row.
	gBad := &domain.Group{ID: newID(), Name: "BadPat", AgentAvailable: true, AutoAssignPattern: "["}
	_ = asvc.groups.Create(ctx, gBad)
	gGood, _ := gsvc.Create(ctx, GroupInput{Name: "GoodPat", AgentAvailable: true, AutoAssignPattern: `^github\.com/.*`})

	asvc.Ingest(ctx, "t1", u.ID, "github.com/acme/api", "", "github", "v0.21.4", "success", 0, time.Now().UTC())
	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.GroupID == "" {
		t.Fatal("expected auto-assign via good pattern")
	}
	if meta.GroupID != gGood.ID {
		t.Fatalf("group_id = %q, want %s", meta.GroupID, gGood.ID)
	}
}

func TestAttributionIngestGroupSetOnce(t *testing.T) {
	asvc, gsvc, usvc, r := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")
	g1, _ := gsvc.Create(ctx, GroupInput{Name: "G1", AgentAvailable: true})
	asvc.projects.Create(ctx, "github.com/acme/api", g1.ID)

	asvc.Ingest(ctx, "t1", u.ID, "github.com/acme/api", "", "github", "v0.21.4", "success", 100, time.Now().UTC())

	// Reassign the project to a different group; the existing trace's group
	// must NOT change (set-once).
	g2, _ := gsvc.Create(ctx, GroupInput{Name: "G2", AgentAvailable: true})
	proj, _ := r.projects.GetByName(ctx, "github.com/acme/api")
	asvc.projects.Assign(ctx, proj.ID, g2.ID)

	asvc.Ingest(ctx, "t1", u.ID, "github.com/acme/api", "", "github", "v0.21.4", "failed", 200, time.Now().UTC())
	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.GroupID != g1.ID {
		t.Fatalf("group_id = %q, want %s (set-once)", meta.GroupID, g1.ID)
	}
	if meta.Status != "failed" {
		t.Fatalf("status = %q, want failed (updated)", meta.Status)
	}
}

func TestAttributionIngestNoCIRepo(t *testing.T) {
	asvc, _, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")
	asvc.Ingest(ctx, "t1", u.ID, "", "", "", "v0.21.4", "success", 0, time.Now().UTC())
	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.GroupID != "" {
		t.Fatalf("group_id = %q, want empty (no ci_repo)", meta.GroupID)
	}
	if meta.UserID != u.ID {
		t.Fatalf("user_id = %q", meta.UserID)
	}
}

// TestAttributionIngestGitRemoteFallback verifies that a local (non-CI) run,
// which emits "dagger.io/git.remote" but not "dagger.io/ci.repo", still gets
// an identity persisted (project_name + ci_repo) and can auto-assign a group.
func TestAttributionIngestGitRemoteFallback(t *testing.T) {
	asvc, gsvc, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")
	g, _ := gsvc.Create(ctx, GroupInput{Name: "G1", AgentAvailable: true, AutoAssignPattern: `^github\.com/.*`})

	asvc.Ingest(ctx, "t1", u.ID, "", "github.com/acme/api", "", "v0.21.4", "success", 0, time.Now().UTC())

	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.ProjectName != "github.com/acme/api" {
		t.Fatalf("project_name = %q, want github.com/acme/api", meta.ProjectName)
	}
	if meta.CIRepo != "github.com/acme/api" {
		t.Fatalf("ci_repo = %q, want github.com/acme/api", meta.CIRepo)
	}
	if meta.GroupID != g.ID {
		t.Fatalf("group_id = %q, want %s (auto-assigned via git remote)", meta.GroupID, g.ID)
	}
}

// TestAttributionIngestCIRepoPrecedence verifies the CI repo slug wins over
// the git remote when both are present (existing behavior preserved).
func TestAttributionIngestCIRepoPrecedence(t *testing.T) {
	asvc, _, usvc, _ := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	asvc.Ingest(ctx, "t1", u.ID, "github.com/ci/slug", "github.com/git/remote", "github", "v0.21.4", "success", 0, time.Now().UTC())

	meta, _ := asvc.traceMeta.Get(ctx, "t1")
	if meta.ProjectName != "github.com/ci/slug" {
		t.Fatalf("project_name = %q, want github.com/ci/slug (ci.repo wins)", meta.ProjectName)
	}
	if meta.CIRepo != "github.com/ci/slug" {
		t.Fatalf("ci_repo = %q, want github.com/ci/slug", meta.CIRepo)
	}
}

// TestAttributionIngestBoundsFields verifies hostile OTLP values are bounded
// before persistence (CWE-770): oversized trace IDs are dropped and
// oversized span fields are treated as absent.
func TestAttributionIngestBoundsFields(t *testing.T) {
	asvc, _, usvc, r := newAttributionForTest(t)
	ctx := context.Background()
	u := seedUserSvc(t, usvc, "u1")

	// Oversized trace ID: dropped entirely.
	asvc.Ingest(ctx, strings.Repeat("t", maxIngestFieldLen+1), u.ID, "github.com/acme/api", "", "", "", "", 0, time.Time{})
	if _, err := asvc.traceMeta.Get(ctx, strings.Repeat("t", maxIngestFieldLen+1)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("oversized trace_id should not be persisted: %v", err)
	}

	// Oversized ci_repo: treated as absent (no project row created).
	asvc.Ingest(ctx, "t-big-repo", u.ID, strings.Repeat("r", maxIngestFieldLen+1), "", "github", "v0.21.4", "success", 0, time.Time{})
	meta, err := asvc.traceMeta.Get(ctx, "t-big-repo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meta.CIRepo != "" || meta.ProjectName != "" {
		t.Fatalf("oversized ci_repo should be dropped, got %q/%q", meta.CIRepo, meta.ProjectName)
	}
	projects, _ := r.projects.List(ctx)
	if len(projects) != 0 {
		t.Fatalf("no project should be created for oversized ci_repo, got %d", len(projects))
	}
}
