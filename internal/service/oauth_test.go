package service

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// conflictGroupRepo simulates a concurrent first-login race: Create persists a
// competing group (with a different quota) and reports domain.ErrConflict, so
// ensureGroupByName re-fetches and must reuse the winner's group unchanged.
type conflictGroupRepo struct {
	domain.GroupRepository // real repo used for GetByName/Create
	seeded                 *domain.Group
}

func (r *conflictGroupRepo) Create(ctx context.Context, g *domain.Group) error {
	seeded := &domain.Group{ID: newID(), Name: g.Name, MaxRunnerSessions: 99, AgentAvailable: true}
	if err := r.GroupRepository.Create(ctx, seeded); err != nil {
		return err
	}
	r.seeded = seeded
	return domain.ErrConflict
}

// getErrGroupRepo is a GroupRepository stub whose GetByName always fails with
// the configured non-ErrNotFound error. The embedded interface satisfies the
// remaining methods (never called).
type getErrGroupRepo struct {
	domain.GroupRepository
	err error
}

func (r *getErrGroupRepo) GetByName(_ context.Context, _ string) (*domain.Group, error) {
	return nil, r.err
}

func TestEnsureGroupByName(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, groups domain.GroupRepository) string
		wantNil     bool
		wantMax     int
		wantAgent   bool
		checkResult func(t *testing.T, groups domain.GroupRepository, g *domain.Group)
	}{
		{
			name: "existing group returned unchanged",
			setup: func(t *testing.T, groups domain.GroupRepository) string {
				t.Helper()
				if err := groups.Create(context.Background(), &domain.Group{ID: "existing", Name: "existing", MaxRunnerSessions: 7, AgentAvailable: false}); err != nil {
					t.Fatalf("seed group: %v", err)
				}
				return "existing"
			},
			wantMax:   7,
			wantAgent: false,
			checkResult: func(t *testing.T, groups domain.GroupRepository, g *domain.Group) {
				t.Helper()
				if g.ID != "existing" {
					t.Fatalf("group ID = %q, want existing", g.ID)
				}
			},
		},
		{
			name: "missing group created with default limit and agent available",
			setup: func(_ *testing.T, _ domain.GroupRepository) string {
				return "brand-new"
			},
			wantMax:   3,
			wantAgent: true,
			checkResult: func(t *testing.T, groups domain.GroupRepository, g *domain.Group) {
				t.Helper()
				if !ValidateGroupName(g.Name) {
					t.Fatalf("created group name %q invalid", g.Name)
				}
				got, err := groups.GetByName(context.Background(), "brand-new")
				if err != nil {
					t.Fatalf("GetByName after create: %v", err)
				}
				if got.ID != g.ID {
					t.Fatalf("persisted group ID = %q, want %q", got.ID, g.ID)
				}
			},
		},
		{
			name: "invalid name with space is skipped",
			setup: func(_ *testing.T, _ domain.GroupRepository) string {
				return "has space"
			},
			wantNil: true,
		},
		{
			name: "empty name is skipped",
			setup: func(_ *testing.T, _ domain.GroupRepository) string {
				return ""
			},
			wantNil: true,
		},
		{
			name: "too-long name is skipped",
			setup: func(_ *testing.T, _ domain.GroupRepository) string {
				return "a1234567890123456789012345678901234567890123456789012345678901234"
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := newServiceDB(t).groups
			name := tt.setup(t, groups)

			g, err := ensureGroupByName(context.Background(), groups, name, 3, testLogger())
			if err != nil {
				t.Fatalf("ensureGroupByName error = %v, want nil", err)
			}
			if (g == nil) != tt.wantNil {
				t.Fatalf("ensureGroupByName group = %v, wantNil %v", g, tt.wantNil)
			}
			if g == nil {
				return
			}
			if g.MaxRunnerSessions != tt.wantMax {
				t.Fatalf("MaxRunnerSessions = %d, want %d", g.MaxRunnerSessions, tt.wantMax)
			}
			if g.AgentAvailable != tt.wantAgent {
				t.Fatalf("AgentAvailable = %v, want %v", g.AgentAvailable, tt.wantAgent)
			}
			if tt.checkResult != nil {
				tt.checkResult(t, groups, g)
			}
		})
	}
}

func TestEnsureGroupByNameGetError(t *testing.T) {
	wantErr := errors.New("store unavailable")
	repo := &getErrGroupRepo{err: wantErr}

	g, err := ensureGroupByName(context.Background(), repo, "g", 0, testLogger())
	if g != nil {
		t.Fatalf("group = %v, want nil", g)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapping %v", err, wantErr)
	}
}

func TestEnsureGroupByNameConflictReusesWinner(t *testing.T) {
	backing := newServiceDB(t).groups
	repo := &conflictGroupRepo{GroupRepository: backing}

	g, err := ensureGroupByName(context.Background(), repo, "raced", 5, testLogger())
	if err != nil {
		t.Fatalf("ensureGroupByName: %v", err)
	}
	if g == nil {
		t.Fatal("group = nil, want winner's group")
	}
	if repo.seeded == nil {
		t.Fatal("Create was never called")
	}
	if g.ID != repo.seeded.ID {
		t.Fatalf("group ID = %q, want winner %q", g.ID, repo.seeded.ID)
	}
	if g.MaxRunnerSessions != 99 {
		t.Fatalf("MaxRunnerSessions = %d, want winner's 99 (quota must not be overwritten)", g.MaxRunnerSessions)
	}
}

func TestReconcileMembershipsAutoCreates(t *testing.T) {
	r := newServiceDB(t)
	logger := testLogger()
	ctx := context.Background()

	u := &domain.User{ID: "u1", Username: "alice", Role: domain.RoleUser, OAuthProvider: "oidc"}
	if err := r.users.Create(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := r.groups.Create(ctx, &domain.Group{ID: "pre", Name: "existing", MaxRunnerSessions: 7, AgentAvailable: true}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	gids, err := reconcileMemberships(ctx, r.groups, logger, u, []string{"existing", "brand-new", "has space"}, 3)
	if err != nil {
		t.Fatalf("reconcileMemberships: %v", err)
	}
	if len(gids) != 2 {
		t.Fatalf("group IDs = %v, want 2 (existing + auto-created)", gids)
	}

	// The pre-existing group's quota is untouched.
	existing, err := r.groups.GetByName(ctx, "existing")
	if err != nil {
		t.Fatalf("GetByName existing: %v", err)
	}
	if existing.MaxRunnerSessions != 7 {
		t.Fatalf("existing MaxRunnerSessions = %d, want 7", existing.MaxRunnerSessions)
	}

	// The missing group is auto-created with the default limit.
	created, err := r.groups.GetByName(ctx, "brand-new")
	if err != nil {
		t.Fatalf("GetByName brand-new: %v", err)
	}
	if created.MaxRunnerSessions != 3 {
		t.Fatalf("created MaxRunnerSessions = %d, want 3", created.MaxRunnerSessions)
	}
	if !created.AgentAvailable {
		t.Fatal("created group must have AgentAvailable = true")
	}

	// The user is a member of the existing and the auto-created groups only.
	member, err := r.groups.GroupsForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GroupsForUser: %v", err)
	}
	ids := map[string]bool{}
	for _, g := range member {
		ids[g.ID] = true
	}
	if !ids["pre"] || !ids[created.ID] {
		t.Fatalf("memberships = %v, want pre + auto-created", member)
	}
	if len(member) != 2 {
		t.Fatalf("memberships = %v, want exactly 2 (invalid name skipped)", member)
	}
}
