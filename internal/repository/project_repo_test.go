package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestProjectRepoCRUD(t *testing.T) {
	db := newTestDB(t)
	repo := NewProjectRepo(db)
	ctx := context.Background()

	p := &domain.Project{
		ID:      newID(),
		Name:    "github.com/acme/api",
		GroupID: "",
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GroupID != "" {
		t.Fatalf("group_id = %q, want empty", got.GroupID)
	}

	byName, err := repo.GetByName(ctx, "GITHUB.COM/ACME/API")
	if err != nil {
		t.Fatalf("GetByName (case-insensitive): %v", err)
	}
	if byName.ID != p.ID {
		t.Fatal("id mismatch")
	}

	// Assign a group.
	grepo := NewGroupRepo(db)
	g := &domain.Group{ID: newID(), Name: "G", AgentAvailable: true}
	if err := grepo.Create(ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	got.GroupID = g.ID
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, _ := repo.Get(ctx, p.ID)
	if updated.GroupID != g.ID {
		t.Fatalf("group_id = %q, want %s", updated.GroupID, g.ID)
	}

	// Unassign.
	got.GroupID = ""
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update unassign: %v", err)
	}
	updated, _ = repo.Get(ctx, p.ID)
	if updated.GroupID != "" {
		t.Fatalf("group_id = %q, want empty", updated.GroupID)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}

	if err := repo.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, p.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err := repo.Delete(ctx, p.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete missing: %v", err)
	}
	if err := repo.Update(ctx, got); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Update missing: %v", err)
	}
}

func TestProjectRepoDuplicateName(t *testing.T) {
	db := newTestDB(t)
	repo := NewProjectRepo(db)
	ctx := context.Background()

	p := &domain.Project{ID: newID(), Name: "github.com/acme/api"}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dup := &domain.Project{ID: newID(), Name: "GITHUB.COM/ACME/API"}
	if err := repo.Create(ctx, dup); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestProjectRepoDeleteGroupNullsProjectGroup(t *testing.T) {
	db := newTestDB(t)
	prepo := NewProjectRepo(db)
	grepo := NewGroupRepo(db)
	ctx := context.Background()

	g := &domain.Group{ID: newID(), Name: "G", AgentAvailable: true}
	grepo.Create(ctx, g)
	p := &domain.Project{ID: newID(), Name: "github.com/acme/x", GroupID: g.ID}
	prepo.Create(ctx, p)

	if err := grepo.Delete(ctx, g.ID); err != nil {
		t.Fatalf("Delete group: %v", err)
	}
	got, _ := prepo.Get(ctx, p.ID)
	if got.GroupID != "" {
		t.Fatalf("group_id = %q, want empty after group delete", got.GroupID)
	}
}
