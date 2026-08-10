package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newProjectService(t *testing.T) (*ProjectService, *GroupService) {
	t.Helper()
	r := newServiceDB(t)
	psvc := NewProjectService(r.projects, r.groups, testLogger())
	gsvc := NewGroupService(r.groups, r.users, testLogger())
	return psvc, gsvc
}

func TestProjectServiceCRUD(t *testing.T) {
	psvc, gsvc := newProjectService(t)
	ctx := context.Background()

	g, _ := gsvc.Create(ctx, GroupInput{Name: "G", AgentAvailable: true})

	p, err := psvc.Create(ctx, "github.com/acme/api", g.ID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.GroupID != g.ID {
		t.Fatalf("group_id = %q", p.GroupID)
	}

	// Create with non-existent group fails.
	if _, err := psvc.Create(ctx, "github.com/acme/x", "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Create bad group: %v", err)
	}
	// Empty name fails.
	if _, err := psvc.Create(ctx, "", ""); err == nil {
		t.Fatal("expected empty-name error")
	}

	got, _ := psvc.Get(ctx, p.ID)
	if got.Name != "github.com/acme/api" {
		t.Fatalf("name = %q", got.Name)
	}

	// Unassign.
	updated, err := psvc.Assign(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("Assign unassign: %v", err)
	}
	if updated.GroupID != "" {
		t.Fatalf("group_id = %q, want empty", updated.GroupID)
	}
	// Assign to a new group.
	updated, err = psvc.Assign(ctx, p.ID, g.ID)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if updated.GroupID != g.ID {
		t.Fatalf("group_id = %q", updated.GroupID)
	}
	// Assign to missing group fails.
	if _, err := psvc.Assign(ctx, p.ID, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Assign bad group: %v", err)
	}
	// Assign missing project fails.
	if _, err := psvc.Assign(ctx, "nope", g.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Assign bad project: %v", err)
	}

	list, _ := psvc.List(ctx)
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}

	if err := psvc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := psvc.Get(ctx, p.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
}

func TestProjectServiceGetOrCreateByName(t *testing.T) {
	psvc, _ := newProjectService(t)
	ctx := context.Background()

	p1, err := psvc.GetOrCreateByName(ctx, "github.com/acme/api")
	if err != nil {
		t.Fatalf("GetOrCreateByName 1: %v", err)
	}
	p2, err := psvc.GetOrCreateByName(ctx, "github.com/acme/api")
	if err != nil {
		t.Fatalf("GetOrCreateByName 2: %v", err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("should return same project: %s vs %s", p1.ID, p2.ID)
	}
}

// TestProjectServiceNameTooLong verifies project names are length-bounded
// (CWE-770) for both explicit creation and OTLP auto-creation.
func TestProjectServiceNameTooLong(t *testing.T) {
	psvc, _ := newProjectService(t)
	ctx := context.Background()

	long := strings.Repeat("a", maxProjectNameLen+1)
	if _, err := psvc.Create(ctx, long, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create long name: %v, want ErrValidation", err)
	}
	if _, err := psvc.GetOrCreateByName(ctx, long); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("GetOrCreateByName long name: %v, want ErrValidation", err)
	}
}
