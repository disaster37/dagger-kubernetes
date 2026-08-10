package service

import (
	"context"
	"errors"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// stubUserRepo is an in-memory UserRepository for service tests.
type stubUserRepo struct {
	users   map[string]*domain.User
	byName  map[string]*domain.User
	byOAuth map[string]*domain.User
}

func newStubUserRepo(users ...*domain.User) *stubUserRepo {
	r := &stubUserRepo{
		users:   make(map[string]*domain.User),
		byName:  make(map[string]*domain.User),
		byOAuth: make(map[string]*domain.User),
	}
	for _, u := range users {
		r.users[u.ID] = u
		r.byName[u.Username] = u
		if u.OAuthProvider != "" {
			r.byOAuth[u.OAuthProvider+":"+u.OAuthID] = u
		}
	}
	return r
}

func (r *stubUserRepo) Create(_ context.Context, u *domain.User) error {
	if _, ok := r.byName[u.Username]; ok {
		return errors.New("duplicate username")
	}
	r.users[u.ID] = u
	r.byName[u.Username] = u
	if u.OAuthProvider != "" {
		r.byOAuth[u.OAuthProvider+":"+u.OAuthID] = u
	}
	return nil
}
func (r *stubUserRepo) Get(_ context.Context, id string) (*domain.User, error) {
	u, ok := r.users[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}
func (r *stubUserRepo) GetByUsername(_ context.Context, name string) (*domain.User, error) {
	u, ok := r.byName[name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}
func (r *stubUserRepo) GetByOAuth(_ context.Context, provider, oauthID string) (*domain.User, error) {
	u, ok := r.byOAuth[provider+":"+oauthID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}
func (r *stubUserRepo) List(context.Context) ([]*domain.User, error) {
	out := make([]*domain.User, 0, len(r.users))
	for _, u := range r.users {
		out = append(out, u)
	}
	return out, nil
}
func (r *stubUserRepo) Update(_ context.Context, u *domain.User) error {
	if _, ok := r.users[u.ID]; !ok {
		return domain.ErrNotFound
	}
	r.users[u.ID] = u
	return nil
}
func (r *stubUserRepo) Delete(_ context.Context, id string) error {
	if _, ok := r.users[id]; !ok {
		return domain.ErrNotFound
	}
	delete(r.users, id)
	return nil
}
func (r *stubUserRepo) Count(context.Context) (int, error) { return len(r.users), nil }

// stubGroupRepo is an in-memory GroupRepository for service tests.
type stubGroupRepo struct {
	groups      map[string]*domain.Group
	byName      map[string]*domain.Group
	memberships map[string]map[string]bool // groupID -> set of userID
}

func newStubGroupRepo(groups ...*domain.Group) *stubGroupRepo {
	r := &stubGroupRepo{
		groups:      make(map[string]*domain.Group),
		byName:      make(map[string]*domain.Group),
		memberships: make(map[string]map[string]bool),
	}
	for _, g := range groups {
		r.groups[g.ID] = g
		r.byName[g.Name] = g
		r.memberships[g.ID] = make(map[string]bool)
	}
	return r
}

func (r *stubGroupRepo) Create(_ context.Context, g *domain.Group) error {
	if _, ok := r.byName[g.Name]; ok {
		return errors.New("duplicate group name")
	}
	r.groups[g.ID] = g
	r.byName[g.Name] = g
	r.memberships[g.ID] = make(map[string]bool)
	return nil
}
func (r *stubGroupRepo) Get(_ context.Context, id string) (*domain.Group, error) {
	g, ok := r.groups[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return g, nil
}
func (r *stubGroupRepo) GetByName(_ context.Context, name string) (*domain.Group, error) {
	g, ok := r.byName[name]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return g, nil
}
func (r *stubGroupRepo) List(context.Context) ([]*domain.Group, error) {
	out := make([]*domain.Group, 0, len(r.groups))
	for _, g := range r.groups {
		out = append(out, g)
	}
	return out, nil
}
func (r *stubGroupRepo) Update(_ context.Context, g *domain.Group) error {
	if _, ok := r.groups[g.ID]; !ok {
		return domain.ErrNotFound
	}
	r.groups[g.ID] = g
	return nil
}
func (r *stubGroupRepo) Delete(_ context.Context, id string) error {
	delete(r.groups, id)
	delete(r.memberships, id)
	return nil
}
func (r *stubGroupRepo) SetMembers(_ context.Context, groupID string, userIDs []string) error {
	if _, ok := r.groups[groupID]; !ok {
		return domain.ErrNotFound
	}
	r.memberships[groupID] = make(map[string]bool)
	for _, uid := range userIDs {
		r.memberships[groupID][uid] = true
	}
	return nil
}
func (r *stubGroupRepo) Members(_ context.Context, groupID string) ([]*domain.User, error) {
	m, ok := r.memberships[groupID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	out := []*domain.User{}
	// We don't have a user repo here; return placeholder users.
	for uid := range m {
		out = append(out, &domain.User{ID: uid})
	}
	return out, nil
}
func (r *stubGroupRepo) GroupsForUser(_ context.Context, userID string) ([]*domain.Group, error) {
	var out []*domain.Group
	for gid, members := range r.memberships {
		if members[userID] {
			if g, ok := r.groups[gid]; ok {
				out = append(out, g)
			}
		}
	}
	return out, nil
}
func (r *stubGroupRepo) AllMemberships(context.Context) (map[string][]string, error) {
	out := make(map[string][]string)
	for gid, members := range r.memberships {
		for uid := range members {
			out[gid] = append(out[gid], uid)
		}
	}
	return out, nil
}

// stubTokenRepo is an in-memory APITokenRepository for service tests.
type stubTokenRepo struct {
	byUser map[string]*domain.APIToken
	byHash map[string]*domain.APIToken
}

func newStubTokenRepo() *stubTokenRepo {
	return &stubTokenRepo{byUser: make(map[string]*domain.APIToken), byHash: make(map[string]*domain.APIToken)}
}

func (r *stubTokenRepo) Upsert(_ context.Context, t *domain.APIToken) error {
	if old, ok := r.byUser[t.UserID]; ok {
		delete(r.byHash, old.TokenHash)
	}
	r.byUser[t.UserID] = t
	r.byHash[t.TokenHash] = t
	return nil
}
func (r *stubTokenRepo) GetByHash(_ context.Context, hash string) (*domain.APIToken, error) {
	t, ok := r.byHash[hash]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return t, nil
}
func (r *stubTokenRepo) GetByUser(_ context.Context, userID string) (*domain.APIToken, error) {
	t, ok := r.byUser[userID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return t, nil
}
func (r *stubTokenRepo) Delete(_ context.Context, userID string) error {
	if t, ok := r.byUser[userID]; ok {
		delete(r.byHash, t.TokenHash)
	}
	delete(r.byUser, userID)
	return nil
}
func (r *stubTokenRepo) TouchLastUsed(_ context.Context, id string, at time.Time) error {
	for _, t := range r.byUser {
		if t.ID == id {
			t.LastUsedAt = &at
		}
	}
	return nil
}

// stubLegacyValidator is a flat-file-style TokenValidator stub.
type stubLegacyValidator struct {
	valid map[string]bool
}

func (v *stubLegacyValidator) ValidateToken(token string) (string, error) {
	if v.valid[token] {
		return token, nil
	}
	return "", errors.New("invalid")
}

// stubSessionStore is an in-memory SessionStore for quota tests.
type stubSessionStore struct {
	leases []*domain.Lease
}

func (s *stubSessionStore) Register(certFP, version, replicaPod, instanceID, traceID, userID string) *domain.Lease {
	l := &domain.Lease{CertFP: certFP, Version: version, ReplicaPod: replicaPod, InstanceID: instanceID, TraceID: traceID, UserID: userID}
	s.leases = append(s.leases, l)
	return l
}
func (s *stubSessionStore) Get(certFP string) (*domain.Lease, error) {
	for _, l := range s.leases {
		if l.CertFP == certFP {
			return l, nil
		}
	}
	return nil, errors.New("not found")
}
func (s *stubSessionStore) Touch(string) error                 { return nil }
func (s *stubSessionStore) IncInFlight(string) error           { return nil }
func (s *stubSessionStore) DecInFlight(string) error           { return nil }
func (s *stubSessionStore) Remove(string)                      {}
func (s *stubSessionStore) PinnedSessionsOnReplica(string) int { return 0 }
func (s *stubSessionStore) SetGroupID(certFP, groupID string) {
	for _, l := range s.leases {
		if l.CertFP == certFP {
			l.GroupID = groupID
		}
	}
}
func (s *stubSessionStore) CountByUser(userID string) int {
	n := 0
	for _, l := range s.leases {
		if l.UserID == userID {
			n++
		}
	}
	return n
}
func (s *stubSessionStore) List() []*domain.Lease {
	out := make([]*domain.Lease, len(s.leases))
	for i, l := range s.leases {
		cp := *l
		out[i] = &cp
	}
	return out
}
