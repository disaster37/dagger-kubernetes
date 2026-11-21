package service

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

const (
	oauthBootID = "org-000000000000000a"
	oauthAcmeID = "org-000000000000000b"
)

// newOAuthOrgRepos builds a bootstrapped FSM-backed org+group repository pair
// with the bootstrap org and one extra org ("acme").
func newOAuthOrgRepos(t *testing.T) (domain.OrgRepository, domain.GroupRepository) {
	t.Helper()
	store := newServiceStore(t)
	orgRepo := repository.NewOrgRepo(store)
	groupRepo := repository.NewGroupRepo(store)
	ctx := context.Background()

	if _, err := orgRepo.Bootstrap(ctx, &domain.Org{
		ID:   oauthBootID,
		Name: domain.DefaultOrgName,
	}, "grp-bootstrap", domain.DefaultOrgDisplayName); err != nil {
		t.Fatalf("bootstrap org: %v", err)
	}
	if err := orgRepo.Create(ctx, &domain.Org{ID: oauthAcmeID, Name: "acme"}); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	return orgRepo, groupRepo
}

func TestOAuthGroupMappingTargetsOrg(t *testing.T) {
	orgs, groups := newOAuthOrgRepos(t)
	ctx := context.Background()
	logger := testLogger()

	ids, err := resolveOAuthGroupIDs(ctx, groups, nil, orgs, logger, []mappedGroup{{Name: "team", Org: "acme"}}, 3)
	if err != nil {
		t.Fatalf("resolveOAuthGroupIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("group IDs = %v, want 1", ids)
	}
	g, err := groups.GetByName(ctx, oauthAcmeID, "team")
	if err != nil {
		t.Fatalf("GetByName acme/team: %v", err)
	}
	if g.OrgID != oauthAcmeID {
		t.Fatalf("group OrgID = %q, want acme %q", g.OrgID, oauthAcmeID)
	}
}

func TestOAuthGroupMappingTargetsOrgByID(t *testing.T) {
	orgs, groups := newOAuthOrgRepos(t)
	ctx := context.Background()

	ids, err := resolveOAuthGroupIDs(ctx, groups, nil, orgs, testLogger(), []mappedGroup{{Name: "team", Org: oauthAcmeID}}, 3)
	if err != nil {
		t.Fatalf("resolveOAuthGroupIDs by ID: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("group IDs = %v, want 1", ids)
	}
	g, err := groups.GetByName(ctx, oauthAcmeID, "team")
	if err != nil || g.OrgID != oauthAcmeID {
		t.Fatalf("group = %+v err=%v, want in acme", g, err)
	}
}

func TestOAuthGroupMappingEmptyOrgUsesBootstrap(t *testing.T) {
	orgs, groups := newOAuthOrgRepos(t)
	ctx := context.Background()

	ids, err := resolveOAuthGroupIDs(ctx, groups, nil, orgs, testLogger(), []mappedGroup{{Name: "team", Org: ""}}, 3)
	if err != nil {
		t.Fatalf("resolveOAuthGroupIDs: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("group IDs = %v, want 1", ids)
	}
	g, err := groups.GetByName(ctx, oauthBootID, "team")
	if err != nil {
		t.Fatalf("GetByName bootstrap/team: %v", err)
	}
	if g.OrgID != oauthBootID {
		t.Fatalf("group OrgID = %q, want bootstrap %q", g.OrgID, oauthBootID)
	}
}

func TestOAuthGroupMappingUnknownOrgFailsClosed(t *testing.T) {
	orgs, groups := newOAuthOrgRepos(t)
	ctx := context.Background()

	_, err := resolveOAuthGroupIDs(ctx, groups, nil, orgs, testLogger(), []mappedGroup{{Name: "team", Org: "nope"}}, 3)
	if err == nil {
		t.Fatal("unknown target org must fail closed")
	}
	// Nothing may be created, anywhere.
	if _, gErr := groups.GetByName(ctx, oauthBootID, "team"); !errors.Is(gErr, domain.ErrNotFound) {
		t.Fatalf("bootstrap group exists after fail-closed: %v", gErr)
	}
	if _, gErr := groups.GetByName(ctx, oauthAcmeID, "team"); !errors.Is(gErr, domain.ErrNotFound) {
		t.Fatalf("acme group exists after fail-closed: %v", gErr)
	}
}

func TestOAuthGroupMappingMaxGroupsQuota(t *testing.T) {
	orgs, groups := newOAuthOrgRepos(t)
	ctx := context.Background()

	// cap acme at 1 group and pre-seed one group so it is already at the cap.
	if err := groups.Create(ctx, &domain.Group{ID: "g-acme-existing", OrgID: oauthAcmeID, Name: "existing"}); err != nil {
		t.Fatal(err)
	}
	acme, err := orgs.Get(ctx, oauthAcmeID)
	if err != nil {
		t.Fatal(err)
	}
	acme.Quotas = domain.OrgQuotas{MaxGroups: domain.QuotaThreshold{Hard: 1}}
	if err := orgs.Update(ctx, acme); err != nil {
		t.Fatal(err)
	}
	quota := NewQuotaService(&stubSessionStore{}, groups, testLogger()).WithOrgRepository(orgs, nil)

	_, err = resolveOAuthGroupIDs(ctx, groups, quota, orgs, testLogger(), []mappedGroup{{Name: "team", Org: "acme"}}, 3)
	if !errors.Is(err, domain.ErrOrgQuotaExceeded) {
		t.Fatalf("max_groups quota = %v, want ErrOrgQuotaExceeded", err)
	}
}
