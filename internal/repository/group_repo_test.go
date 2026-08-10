package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestGroupRepoCRUD(t *testing.T) {
	db := newTestDB(t)
	repo := NewGroupRepo(db)
	ctx := context.Background()

	g := &domain.Group{
		ID:                newID(),
		Name:              "Engines",
		Description:       "engine team",
		MaxRunnerSessions: 8,
		AgentAvailable:    true,
		AutoAssignPattern: `^github\.com/acme/.*`,
	}
	if err := repo.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, g.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.AgentAvailable {
		t.Fatal("agent_available should be true")
	}
	if got.MaxRunnerSessions != 8 {
		t.Fatalf("max = %d", got.MaxRunnerSessions)
	}

	byName, err := repo.GetByName(ctx, "engines")
	if err != nil {
		t.Fatalf("GetByName (case-insensitive): %v", err)
	}
	if byName.ID != g.ID {
		t.Fatal("id mismatch")
	}

	got.Description = "updated"
	got.AgentAvailable = false
	got.MaxRunnerSessions = 4
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, _ := repo.Get(ctx, g.ID)
	if updated.AgentAvailable {
		t.Fatal("agent_available should be false after update")
	}
	if updated.MaxRunnerSessions != 4 {
		t.Fatalf("max = %d, want 4", updated.MaxRunnerSessions)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}

	if err := repo.Delete(ctx, g.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.Get(ctx, g.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if err := repo.Delete(ctx, g.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Delete missing: %v", err)
	}
	if err := repo.Update(ctx, got); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Update missing: %v", err)
	}
}

func TestGroupRepoDuplicateName(t *testing.T) {
	db := newTestDB(t)
	repo := NewGroupRepo(db)
	ctx := context.Background()

	g := &domain.Group{ID: newID(), Name: "TeamA", AgentAvailable: true}
	if err := repo.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dup := &domain.Group{ID: newID(), Name: "teama"}
	if err := repo.Create(ctx, dup); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestGroupRepoMembership(t *testing.T) {
	db := newTestDB(t)
	grepo := NewGroupRepo(db)
	ctx := context.Background()

	g := &domain.Group{ID: newID(), Name: "G1", AgentAvailable: true}
	if err := grepo.Create(ctx, g); err != nil {
		t.Fatalf("Create group: %v", err)
	}
	u1 := seedUser(t, db, "u1", domain.RoleUser)
	u2 := seedUser(t, db, "u2", domain.RoleUser)

	if err := grepo.SetMembers(ctx, g.ID, []string{u1.ID, u2.ID}); err != nil {
		t.Fatalf("SetMembers: %v", err)
	}

	members, err := grepo.Members(ctx, g.ID)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}

	groups, err := grepo.GroupsForUser(ctx, u1.ID)
	if err != nil {
		t.Fatalf("GroupsForUser: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("groups = %v", groups)
	}

	// Replace membership: drop u1, keep u2.
	if err := grepo.SetMembers(ctx, g.ID, []string{u2.ID}); err != nil {
		t.Fatalf("SetMembers replace: %v", err)
	}
	members, _ = grepo.Members(ctx, g.ID)
	if len(members) != 1 || members[0].ID != u2.ID {
		t.Fatalf("after replace members = %v", members)
	}
	groups, _ = grepo.GroupsForUser(ctx, u1.ID)
	if len(groups) != 0 {
		t.Fatalf("u1 should have no groups, got %v", groups)
	}

	// AllMemberships
	all, err := grepo.AllMemberships(ctx)
	if err != nil {
		t.Fatalf("AllMemberships: %v", err)
	}
	if len(all[g.ID]) != 1 || all[g.ID][0] != u2.ID {
		t.Fatalf("all memberships = %v", all)
	}
}

func TestGroupRepoDeleteCascadesMemberships(t *testing.T) {
	db := newTestDB(t)
	grepo := NewGroupRepo(db)
	ctx := context.Background()

	g := &domain.Group{ID: newID(), Name: "G", AgentAvailable: true}
	grepo.Create(ctx, g)
	u := seedUser(t, db, "u", domain.RoleUser)
	grepo.SetMembers(ctx, g.ID, []string{u.ID})

	if err := grepo.Delete(ctx, g.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	groups, _ := grepo.GroupsForUser(ctx, u.ID)
	if len(groups) != 0 {
		t.Fatalf("memberships should cascade-delete, got %v", groups)
	}
}

func TestGroupRepoDeleteUserCascadesMemberships(t *testing.T) {
	db := newTestDB(t)
	grepo := NewGroupRepo(db)
	urepo := NewUserRepo(db)
	ctx := context.Background()

	g := &domain.Group{ID: newID(), Name: "G", AgentAvailable: true}
	grepo.Create(ctx, g)
	u := seedUser(t, db, "u", domain.RoleUser)
	grepo.SetMembers(ctx, g.ID, []string{u.ID})

	if err := urepo.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete user: %v", err)
	}
	members, _ := grepo.Members(ctx, g.ID)
	if len(members) != 0 {
		t.Fatalf("memberships should cascade on user delete, got %v", members)
	}
}
