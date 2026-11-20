package service

import (
	"context"
	"errors"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestResolveForIdentityMatrix(t *testing.T) {
	const (
		bootID = "org-0000000000000001"
		acmeID = "org-1111111111111111"
		betaID = "org-2222222222222222"
		zetaID = "org-3333333333333333"
	)
	newSvc := func(t *testing.T) *OrgService {
		t.Helper()
		repo := newFakeOrgRepo()
		for _, o := range []*domain.Org{
			{ID: bootID, Name: domain.DefaultOrgName},
			{ID: acmeID, Name: "acme"},
			{ID: betaID, Name: "beta"},
			{ID: zetaID, Name: "zeta"},
		} {
			if err := repo.Create(context.Background(), o); err != nil {
				t.Fatalf("create org: %v", err)
			}
		}
		return NewOrgService(repo, &fakeGroupRepo{}, nil, testLogger())
	}

	t.Run("bootstrap not ready", func(t *testing.T) {
		svc := NewOrgService(newFakeOrgRepo(), &fakeGroupRepo{}, nil, testLogger())
		_, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u1"}, "")
		if !errors.Is(err, domain.ErrBootstrapNotReady) {
			t.Fatalf("err = %v, want ErrBootstrapNotReady", err)
		}
	})

	t.Run("org token matching selector", func(t *testing.T) {
		svc := newSvc(t)
		org, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "org:" + acmeID, OrgID: acmeID}, "acme")
		if err != nil || org.ID != acmeID {
			t.Fatalf("org = %+v err=%v", org, err)
		}
	})

	t.Run("org token mismatched selector", func(t *testing.T) {
		svc := newSvc(t)
		_, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "org:" + acmeID, OrgID: acmeID}, "beta")
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("user zero orgs", func(t *testing.T) {
		svc := newSvc(t)
		_, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u0"}, "")
		if !errors.Is(err, domain.ErrNoOrgMembership) {
			t.Fatalf("err = %v, want ErrNoOrgMembership", err)
		}
	})

	t.Run("user single org auto-selects", func(t *testing.T) {
		svc := newSvc(t)
		if err := svc.orgs.SetMember(context.Background(), acmeID, "u1", domain.OrgMember); err != nil {
			t.Fatal(err)
		}
		org, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u1"}, "")
		if err != nil || org.ID != acmeID {
			t.Fatalf("org = %+v err=%v", org, err)
		}
	})

	t.Run("user three orgs ambiguous", func(t *testing.T) {
		svc := newSvc(t)
		for _, id := range []string{acmeID, betaID, zetaID} {
			if err := svc.orgs.SetMember(context.Background(), id, "u3", domain.OrgMember); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u3"}, ""); !errors.Is(err, domain.ErrAmbiguousOrg) {
			t.Fatalf("err = %v, want ErrAmbiguousOrg", err)
		}
		org, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u3"}, "beta")
		if err != nil || org.ID != betaID {
			t.Fatalf("explicit beta = %+v err=%v", org, err)
		}
	})

	t.Run("user cannot select foreign org", func(t *testing.T) {
		svc := newSvc(t)
		if err := svc.orgs.SetMember(context.Background(), acmeID, "u1", domain.OrgMember); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u1"}, "globex"); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("missing org as user = %v, want ErrForbidden", err)
		}
		if _, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "u1"}, "zeta"); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("foreign org = %v, want ErrForbidden", err)
		}
	})

	t.Run("admin single cluster org auto-selects", func(t *testing.T) {
		repo := newFakeOrgRepo()
		if err := repo.Create(context.Background(), &domain.Org{ID: bootID, Name: domain.DefaultOrgName}); err != nil {
			t.Fatal(err)
		}
		svc := NewOrgService(repo, &fakeGroupRepo{}, nil, testLogger())
		org, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "admin", Role: domain.RoleAdmin}, "")
		if err != nil || org.ID != bootID {
			t.Fatalf("org = %+v err=%v", org, err)
		}
	})

	t.Run("admin two cluster orgs ambiguous", func(t *testing.T) {
		svc := newSvc(t)
		if _, err := svc.ResolveForIdentity(context.Background(), &domain.Identity{UserID: "admin", Role: domain.RoleAdmin}, ""); !errors.Is(err, domain.ErrAmbiguousOrg) {
			t.Fatalf("err = %v, want ErrAmbiguousOrg", err)
		}
	})
}

func TestOrgServiceCreateReservedSlugsAndFleetGuard(t *testing.T) {
	repo := newFakeOrgRepo()
	svc := NewOrgService(repo, &fakeGroupRepo{}, nil, testLogger())
	for _, name := range []string{"default", "_shared", "all", "org-0123456789abcdef"} {
		if _, err := svc.Create(context.Background(), OrgInput{Name: name}); err == nil {
			t.Fatalf("slug %q must be rejected", name)
		}
	}
	if err := repo.Create(context.Background(), &domain.Org{ID: "org-0000000000000001", Name: "existing"}); err != nil {
		t.Fatal(err)
	}
	shared := NewOrgService(repo, &fakeGroupRepo{}, nil, testLogger()).WithFleetIsolation("shared")
	if _, err := shared.Create(context.Background(), OrgInput{Name: "second"}); !errors.Is(err, domain.ErrSharedFleetMultiOrg) {
		t.Fatalf("shared fleet second org = %v, want ErrSharedFleetMultiOrg", err)
	}
}
