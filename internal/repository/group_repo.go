package repository

import (
	"context"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GroupRepo is the Raft implementation of domain.GroupRepository.
type GroupRepo struct {
	store *RaftStore
}

var _ domain.GroupRepository = (*GroupRepo)(nil)

// NewGroupRepo returns a GroupRepo backed by store.
func NewGroupRepo(store *RaftStore) *GroupRepo {
	return &GroupRepo{store: store}
}

func (r *GroupRepo) Create(ctx context.Context, g *domain.Group) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = g.CreatedAt
	}
	return r.upsert(ctx, g, true)
}

func (r *GroupRepo) upsert(ctx context.Context, g *domain.Group, create bool) error {
	return r.store.applyCtx(ctx, kindUpsertGroup, cmdGroup{Group: *g, Create: create})
}

func (r *GroupRepo) Get(ctx context.Context, id string) (*domain.Group, error) {
	return r.store.fsmRead().readGroupByID(id)
}

func (r *GroupRepo) GetByName(ctx context.Context, name string) (*domain.Group, error) {
	return r.store.fsmRead().readGroupByName(name)
}

func (r *GroupRepo) List(ctx context.Context) ([]*domain.Group, error) {
	return r.store.fsmRead().listGroups(), nil
}

func (r *GroupRepo) Update(ctx context.Context, g *domain.Group) error {
	g.UpdatedAt = time.Now().UTC()
	return r.upsert(ctx, g, false)
}

func (r *GroupRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteGroup, id)
}

// SetMembers replaces the full membership of groupID with userIDs in one
// atomic log entry (no SQL transaction needed).
func (r *GroupRepo) SetMembers(ctx context.Context, groupID string, userIDs []string) error {
	return r.store.applyCtx(ctx, kindSetMembers, cmdSetMembers{GroupID: groupID, UserIDs: userIDs})
}

func (r *GroupRepo) Members(ctx context.Context, groupID string) ([]*domain.User, error) {
	return r.store.fsmRead().members(groupID), nil
}

func (r *GroupRepo) GroupsForUser(ctx context.Context, userID string) ([]*domain.Group, error) {
	return r.store.fsmRead().groupsForUser(userID), nil
}

// AllMemberships returns a map of groupID -> member userIDs (sorted).
func (r *GroupRepo) AllMemberships(ctx context.Context) (map[string][]string, error) {
	return r.store.fsmRead().allMemberships(), nil
}
