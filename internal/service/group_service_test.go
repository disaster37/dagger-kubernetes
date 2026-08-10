package service

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func newGroupService(t *testing.T) (*GroupService, *UserService) {
	t.Helper()
	r := newServiceDB(t)
	gsvc := NewGroupService(r.groups, r.users, testLogger())
	usvc := NewUserService(r.users, r.groups, testLogger())
	return gsvc, usvc
}

func TestGroupServiceCreate(t *testing.T) {
	gsvc, _ := newGroupService(t)
	ctx := context.Background()

	g, err := gsvc.Create(ctx, GroupInput{Name: "Eng", MaxRunnerSessions: 8, AgentAvailable: true, AutoAssignPattern: `^github\.com/.*`})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if g.ID == "" || !g.AgentAvailable {
		t.Fatalf("bad group: %+v", g)
	}
}

func TestGroupServiceValidation(t *testing.T) {
	gsvc, _ := newGroupService(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   GroupInput
	}{
		{"empty name", GroupInput{Name: ""}},
		{"bad name", GroupInput{Name: "-bad"}},
		{"negative max", GroupInput{Name: "g", MaxRunnerSessions: -1}},
		{"bad pattern", GroupInput{Name: "g", AutoAssignPattern: "["}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := gsvc.Create(ctx, tc.in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestGroupServiceCRUD(t *testing.T) {
	gsvc, _ := newGroupService(t)
	ctx := context.Background()

	g, _ := gsvc.Create(ctx, GroupInput{Name: "G", AgentAvailable: true})

	got, err := gsvc.Get(ctx, g.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "G" {
		t.Fatalf("name = %q", got.Name)
	}

	updated, err := gsvc.Update(ctx, g.ID, GroupInput{Name: "G2", MaxRunnerSessions: 4, AgentAvailable: false})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "G2" || updated.MaxRunnerSessions != 4 || updated.AgentAvailable {
		t.Fatalf("update did not persist: %+v", updated)
	}

	list, _ := gsvc.List(ctx)
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}

	if err := gsvc.Delete(ctx, g.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := gsvc.Get(ctx, g.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
}

func TestGroupServiceSetMembers(t *testing.T) {
	gsvc, usvc := newGroupService(t)
	ctx := context.Background()

	g, _ := gsvc.Create(ctx, GroupInput{Name: "G", AgentAvailable: true})
	u1 := seedUserSvc(t, usvc, "u1")
	u2 := seedUserSvc(t, usvc, "u2")

	if err := gsvc.SetMembers(ctx, g.ID, []string{u1.ID, u2.ID, u1.ID}); err != nil {
		t.Fatalf("SetMembers: %v", err)
	}
	members, _ := gsvc.Members(ctx, g.ID)
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2 (dedup)", len(members))
	}

	groups, _ := gsvc.GroupsForUser(ctx, u1.ID)
	if len(groups) != 1 || groups[0].ID != g.ID {
		t.Fatalf("groups = %v", groups)
	}

	// Missing user ID -> error.
	if err := gsvc.SetMembers(ctx, g.ID, []string{"nope"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetMembers missing user: %v", err)
	}
	// Missing group -> error.
	if err := gsvc.SetMembers(ctx, "nope", []string{u1.ID}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetMembers missing group: %v", err)
	}
}
