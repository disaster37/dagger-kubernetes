package repository

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// OrgRepo is the Raft implementation of domain.OrgRepository.
type OrgRepo struct {
	store *RaftStore
}

var _ domain.OrgRepository = (*OrgRepo)(nil)

// NewOrgRepo returns an OrgRepo backed by store.
func NewOrgRepo(store *RaftStore) *OrgRepo {
	return &OrgRepo{store: store}
}

func (r *OrgRepo) Create(ctx context.Context, o *domain.Org) error {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = o.CreatedAt
	}
	if len(o.Features) == 0 {
		o.Features = append([]string(nil), domain.DefaultOrgFeatures...)
	}
	return r.store.applyCtx(ctx, kindUpsertOrg, cmdOrg{Org: *o, Create: true})
}

func (r *OrgRepo) Get(ctx context.Context, id string) (*domain.Org, error) {
	return r.store.fsmRead().readOrgByID(id)
}

func (r *OrgRepo) GetByName(ctx context.Context, name string) (*domain.Org, error) {
	return r.store.fsmRead().readOrgByName(name)
}

func (r *OrgRepo) List(ctx context.Context) ([]*domain.Org, error) {
	return r.store.fsmRead().listOrgs(), nil
}

func (r *OrgRepo) Update(ctx context.Context, o *domain.Org) error {
	o.UpdatedAt = time.Now().UTC()
	return r.store.applyCtx(ctx, kindUpsertOrg, cmdOrg{Org: *o, Create: false})
}

func (r *OrgRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteOrg, id)
}

func (r *OrgRepo) SetMember(ctx context.Context, orgID, userID string, role domain.OrgRole) error {
	return r.store.applyCtx(ctx, kindSetOrgMember, cmdOrgMember{
		OrgID: orgID, UserID: userID, Role: role, At: time.Now().UTC(),
	})
}

func (r *OrgRepo) RemoveMember(ctx context.Context, orgID, userID string) error {
	return r.store.applyCtx(ctx, kindRemoveOrgMember, cmdOrgMember{OrgID: orgID, UserID: userID})
}

func (r *OrgRepo) Members(ctx context.Context, orgID string) ([]*domain.OrgMembership, error) {
	return r.store.fsmRead().orgMemberships(orgID), nil
}

func (r *OrgRepo) OrgsForUser(ctx context.Context, userID string) ([]*domain.Org, error) {
	return r.store.fsmRead().orgsForUser(userID), nil
}

func (r *OrgRepo) AllMemberships(ctx context.Context) (map[string][]*domain.OrgMembership, error) {
	return r.store.fsmRead().allOrgMemberships(), nil
}

// --- FSM read helpers -------------------------------------------------------

func (f *FSM) readOrgByID(id string) (*domain.Org, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orgs[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyOrg(o), nil
}

func (f *FSM) readOrgByName(name string) (*domain.Org, error) {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.orgsByName[strings.ToLower(name)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return copyOrg(s.orgs[id]), nil
}

func (f *FSM) listOrgs() []*domain.Org {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Org, 0, len(s.orgs))
	for _, o := range s.orgs {
		out = append(out, copyOrg(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *FSM) orgMemberships(orgID string) []*domain.OrgMembership {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	members := s.orgMembers[orgID]
	out := make([]*domain.OrgMembership, 0, len(members))
	for uid, role := range members {
		out = append(out, &domain.OrgMembership{OrgID: orgID, UserID: uid, Role: role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}

func (f *FSM) orgsForUser(userID string) []*domain.Org {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Org, 0, len(s.orgMembersByUser[userID]))
	for oid := range s.orgMembersByUser[userID] {
		if o, ok := s.orgs[oid]; ok {
			out = append(out, copyOrg(o))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *FSM) allOrgMemberships() map[string][]*domain.OrgMembership {
	s := f.state
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]*domain.OrgMembership, len(s.orgMembers))
	for oid, members := range s.orgMembers {
		for uid, role := range members {
			out[oid] = append(out[oid], &domain.OrgMembership{OrgID: oid, UserID: uid, Role: role})
		}
	}
	return out
}

func copyOrg(o *domain.Org) *domain.Org {
	if o == nil {
		return nil
	}
	cp := *o
	cp.Features = append([]string(nil), o.Features...)
	return &cp
}
