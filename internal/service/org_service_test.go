package service

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

type fakeOrgRepo struct {
	orgs    map[string]*domain.Org
	members map[string]map[string]domain.OrgRole
}

func newFakeOrgRepo() *fakeOrgRepo {
	return &fakeOrgRepo{orgs: map[string]*domain.Org{}, members: map[string]map[string]domain.OrgRole{}}
}

func (f *fakeOrgRepo) Create(_ context.Context, o *domain.Org) error {
	if _, ok := f.orgs[o.ID]; ok {
		return domain.ErrConflict
	}
	cp := *o
	f.orgs[o.ID] = &cp
	return nil
}
func (f *fakeOrgRepo) Get(_ context.Context, id string) (*domain.Org, error) {
	o, ok := f.orgs[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *o
	return &cp, nil
}
func (f *fakeOrgRepo) GetByName(_ context.Context, name string) (*domain.Org, error) {
	for _, o := range f.orgs {
		if o.Name == name {
			cp := *o
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (f *fakeOrgRepo) List(context.Context) ([]*domain.Org, error) {
	out := make([]*domain.Org, 0, len(f.orgs))
	for _, o := range f.orgs {
		cp := *o
		out = append(out, &cp)
	}
	return out, nil
}
func (f *fakeOrgRepo) Update(_ context.Context, o *domain.Org) error {
	cp := *o
	f.orgs[o.ID] = &cp
	return nil
}
func (f *fakeOrgRepo) Delete(_ context.Context, id string) error {
	delete(f.orgs, id)
	return nil
}
func (f *fakeOrgRepo) SetMember(_ context.Context, orgID, userID string, role domain.OrgRole) error {
	if f.members[orgID] == nil {
		f.members[orgID] = map[string]domain.OrgRole{}
	}
	f.members[orgID][userID] = role
	return nil
}
func (f *fakeOrgRepo) RemoveMember(_ context.Context, orgID, userID string) error {
	delete(f.members[orgID], userID)
	return nil
}
func (f *fakeOrgRepo) Members(_ context.Context, orgID string) ([]*domain.OrgMembership, error) {
	out := make([]*domain.OrgMembership, 0, len(f.members[orgID]))
	for uid, role := range f.members[orgID] {
		out = append(out, &domain.OrgMembership{OrgID: orgID, UserID: uid, Role: role})
	}
	return out, nil
}
func (f *fakeOrgRepo) OrgsForUser(_ context.Context, userID string) ([]*domain.Org, error) {
	var out []*domain.Org
	for oid, members := range f.members {
		if _, ok := members[userID]; ok {
			if o, ok := f.orgs[oid]; ok {
				cp := *o
				out = append(out, &cp)
			}
		}
	}
	return out, nil
}
func (f *fakeOrgRepo) AllMemberships(context.Context) (map[string][]*domain.OrgMembership, error) {
	return nil, nil
}

type fakeGroupRepo struct{ groups map[string]*domain.Group }

func (f *fakeGroupRepo) Create(context.Context, *domain.Group) error { return nil }
func (f *fakeGroupRepo) Get(context.Context, string) (*domain.Group, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeGroupRepo) GetByName(context.Context, string) (*domain.Group, error) {
	return nil, domain.ErrNotFound
}
func (f *fakeGroupRepo) List(context.Context) ([]*domain.Group, error) { return nil, nil }
func (f *fakeGroupRepo) Update(context.Context, *domain.Group) error   { return nil }
func (f *fakeGroupRepo) Delete(context.Context, string) error          { return nil }
func (f *fakeGroupRepo) SetMembers(context.Context, string, []string) error {
	return nil
}
func (f *fakeGroupRepo) Members(context.Context, string) ([]*domain.User, error) { return nil, nil }
func (f *fakeGroupRepo) GroupsForUser(_ context.Context, userID string) ([]*domain.Group, error) {
	var out []*domain.Group
	for _, g := range f.groups {
		if g.ID == userID {
			out = append(out, g)
		}
	}
	return out, nil
}
func (f *fakeGroupRepo) AllMemberships(context.Context) (map[string][]string, error) {
	return nil, nil
}

func TestOrgServiceCreateAndMembers(t *testing.T) {
	repo := newFakeOrgRepo()
	svc := NewOrgService(repo, &fakeGroupRepo{}, nil, logrus.New())
	o, err := svc.Create(context.Background(), OrgInput{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Name != "acme" || o.ID != "acme" {
		t.Fatalf("org = %+v", o)
	}
	if err := svc.SetMember(context.Background(), o.ID, "u1", domain.OrgAdmin); err != nil {
		t.Fatal(err)
	}
	if !svc.IsOrgAdmin(context.Background(), o.ID, "u1") {
		t.Fatal("expected org admin")
	}
	orgs, err := svc.OrgsForUser(context.Background(), "u1")
	if err != nil || len(orgs) != 1 {
		t.Fatalf("orgs = %v err=%v", orgs, err)
	}
}

func TestOrgServiceRejectsInvalidName(t *testing.T) {
	svc := NewOrgService(newFakeOrgRepo(), &fakeGroupRepo{}, nil, logrus.New())
	if _, err := svc.Create(context.Background(), OrgInput{Name: "Bad Name"}); err == nil {
		t.Fatal("expected validation error")
	}
}
