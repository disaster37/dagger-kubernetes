package repository

import (
	"context"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// UserRepo is the Raft implementation of domain.UserRepository.
type UserRepo struct {
	store *RaftStore
}

var _ domain.UserRepository = (*UserRepo)(nil)

// NewUserRepo returns a UserRepo backed by store.
func NewUserRepo(store *RaftStore) *UserRepo {
	return &UserRepo{store: store}
}

func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.CreatedAt
	}
	return r.upsert(ctx, u, true)
}

func (r *UserRepo) upsert(ctx context.Context, u *domain.User, create bool) error {
	cu := cmdUserFrom(u)
	cu.Create = create
	return r.store.applyCtx(ctx, kindUpsertUser, cu)
}

func (r *UserRepo) Get(ctx context.Context, id string) (*domain.User, error) {
	return r.store.fsmRead().readUserByID(id)
}

func (r *UserRepo) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	return r.store.fsmRead().readUserByUsername(username)
}

func (r *UserRepo) GetByOAuth(ctx context.Context, provider, oauthID string) (*domain.User, error) {
	return r.store.fsmRead().readUserByOAuth(provider, oauthID)
}

func (r *UserRepo) List(ctx context.Context) ([]*domain.User, error) {
	return r.store.fsmRead().listUsers(), nil
}

func (r *UserRepo) Update(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = time.Now().UTC()
	return r.upsert(ctx, u, false)
}

func (r *UserRepo) Delete(ctx context.Context, id string) error {
	return r.store.applyCtx(ctx, kindDeleteUser, id)
}

func (r *UserRepo) Count(ctx context.Context) (int, error) {
	return r.store.fsmRead().countUsers(), nil
}
