package integration

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

var orgUIDRe = regexp.MustCompile(`^org-[0-9a-f]{16}$`)

// TestOrgTenancyIsolation exercises the organization root-namespace contract
// against real Raft-backed repositories: generated UIDs, per-org name
// uniqueness, membership isolation, fail-closed resolution, and admin-bound
// hard quotas.
func TestOrgTenancyIsolation(t *testing.T) {
	logger := observ.NewTestLogger()
	store := newIntegrationStore(t)
	ctx := context.Background()

	userRepo := repository.NewUserRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	projectRepo := repository.NewProjectRepo(store)
	orgRepo := repository.NewOrgRepo(store)
	tokenRepo := repository.NewTokenRepo(store)

	usersSvc := service.NewUserService(userRepo, groupRepo, logger)
	usersSvc.SetOrgRepository(orgRepo)
	groupSvc := service.NewGroupService(groupRepo, userRepo, logger)
	projectSvc := service.NewProjectService(projectRepo, groupRepo, logger)
	tokenSvc := service.NewTokenService(tokenRepo, logger, nil)
	orgSvc := service.NewOrgService(orgRepo, groupRepo, tokenSvc, logger).
		WithMetaStore(repository.NewMetaStore(store)).
		WithDefaultQuotas(domain.OrgQuotas{})

	boot, err := orgSvc.EnsureBootstrap(ctx, "Default")
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	if !orgUIDRe.MatchString(boot.ID) || boot.Name != domain.DefaultOrgName {
		t.Fatalf("bootstrap org = %+v", boot)
	}

	acme, err := orgSvc.Create(ctx, service.OrgInput{Name: "acme", DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("create acme: %v", err)
	}
	globex, err := orgSvc.Create(ctx, service.OrgInput{Name: "globex", DisplayName: "Globex"})
	if err != nil {
		t.Fatalf("create globex: %v", err)
	}
	for _, o := range []*domain.Org{acme, globex} {
		if !orgUIDRe.MatchString(o.ID) {
			t.Fatalf("generated UID %q does not match org-<16hex>", o.ID)
		}
		if o.ID == boot.ID {
			t.Fatal("organizations must not share a storage UID")
		}
	}

	// Identically named groups and projects are allowed in distinct orgs.
	for _, o := range []*domain.Org{acme, globex} {
		if _, err := groupSvc.Create(ctx, o.ID, service.GroupInput{Name: "platform", AgentAvailable: true}); err != nil {
			t.Fatalf("group in %s: %v", o.Name, err)
		}
		if _, err := projectSvc.Create(ctx, o.ID, "github.com/shared/repo", ""); err != nil {
			t.Fatalf("project in %s: %v", o.Name, err)
		}
	}
	ag, err := groupSvc.GetByName(ctx, acme.ID, "platform")
	if err != nil {
		t.Fatalf("acme group: %v", err)
	}
	gg, err := groupSvc.GetByName(ctx, globex.ID, "platform")
	if err != nil {
		t.Fatalf("globex group: %v", err)
	}
	if ag.ID == gg.ID {
		t.Fatal("per-org groups must be distinct rows")
	}
	ap, _ := projectRepo.GetByName(ctx, acme.ID, "github.com/shared/repo")
	gp, _ := projectRepo.GetByName(ctx, globex.ID, "github.com/shared/repo")
	if ap == nil || gp == nil || ap.ID == gp.ID {
		t.Fatalf("per-org projects must be distinct: acme=%+v globex=%+v", ap, gp)
	}

	// charlie belongs to all three orgs; resolution is ambiguous without a
	// selector and exact with one.
	charlie, err := usersSvc.Create(ctx, "charlie", "password123", domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []*domain.Org{boot, acme, globex} {
		if err := orgSvc.SetMember(ctx, o.ID, charlie.ID, domain.OrgMember); err != nil {
			t.Fatalf("add member %s: %v", o.Name, err)
		}
	}
	if _, err := orgSvc.ResolveForIdentity(ctx, &domain.Identity{UserID: charlie.ID, Role: domain.RoleUser}, ""); !errors.Is(err, domain.ErrAmbiguousOrg) {
		t.Fatalf("ambiguous resolution = %v", err)
	}
	resolved, err := orgSvc.ResolveForIdentity(ctx, &domain.Identity{UserID: charlie.ID, Role: domain.RoleUser}, acme.ID)
	if err != nil || resolved.ID != acme.ID {
		t.Fatalf("explicit resolution = %+v, %v", resolved, err)
	}

	// FSM naming is per-org, so engine StatefulSets cannot collide.
	names := map[string]bool{
		domain.EngineStsName(acme.ID, "v0.21.4"):   true,
		domain.EngineStsName(globex.ID, "v0.21.4"): true,
	}
	if len(names) != 2 {
		t.Fatal("engine StatefulSet names must be per-org")
	}

	// Removing charlie from acme strips only acme memberships.
	if err := groupSvc.SetMembers(ctx, acme.ID, ag.ID, []string{charlie.ID}); err != nil {
		t.Fatal(err)
	}
	if err := groupSvc.SetMembers(ctx, globex.ID, gg.ID, []string{charlie.ID}); err != nil {
		t.Fatal(err)
	}
	if err := orgSvc.RemoveMember(ctx, acme.ID, charlie.ID); err != nil {
		t.Fatal(err)
	}
	if gs, _ := groupSvc.GroupsForUserInOrg(ctx, acme.ID, charlie.ID); len(gs) != 0 {
		t.Fatalf("acme groups after removal = %d, want 0", len(gs))
	}
	if gs, _ := groupSvc.GroupsForUserInOrg(ctx, globex.ID, charlie.ID); len(gs) != 1 {
		t.Fatalf("globex groups after acme removal = %d, want 1", len(gs))
	}

	// Hard quotas bind administrators too.
	if _, err := orgSvc.UpdateQuotas(ctx, &domain.Identity{UserID: "admin", Role: domain.RoleAdmin}, acme.ID, domain.OrgQuotas{
		MaxProjects: domain.QuotaThreshold{Hard: 1},
	}); err != nil {
		t.Fatal(err)
	}
	qs := service.NewQuotaService(service.NewStore(0), groupRepo, logger).WithOrgRepository(orgRepo, projectRepo)
	if _, err := qs.CheckProjectCreate(ctx, &domain.Identity{UserID: "admin", Role: domain.RoleAdmin}, acme.ID); !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("admin hard quota = %v, want ErrOrgQuotaExceeded", err)
	}

	// Bootstrap org is undeletable, even with force.
	if err := orgSvc.Delete(ctx, boot.ID, true, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("bootstrap delete = %v, want ErrForbidden", err)
	}
}
