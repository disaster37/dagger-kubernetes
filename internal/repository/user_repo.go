package repository

import (
	"context"
	"fmt"
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
	cu := cmdUserFrom(u)
	cu.Create = true
	resp, err := r.store.applyCtxResponse(ctx, kindUpsertUser, cu)
	if ferr := fsmError(resp, err); ferr != nil {
		return ferr
	}
	committed, err := userResponse(resp)
	if err != nil {
		return err
	}
	u.Revision = committed.Revision
	return nil
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

// Update persists u with u.Revision as an optimistic-concurrency precondition
// and advances u.Revision on success. A conflict yields ErrStaleUser with no
// effect.
func (r *UserRepo) Update(ctx context.Context, u *domain.User) error {
	u.UpdatedAt = time.Now().UTC()
	cu := cmdUserFrom(u)
	rev := u.Revision
	cu.ExpectedRevision = &rev
	resp, err := r.store.applyCtxResponse(ctx, kindUpsertUser, cu)
	if ferr := fsmError(resp, err); ferr != nil {
		return ferr
	}
	committed, err := userResponse(resp)
	if err != nil {
		return err
	}
	u.Revision = committed.Revision
	u.OAuthLoginVersion = committed.OAuthLoginVersion
	return nil
}

// Delete soft-deletes the account identified by id, recording actorID.
func (r *UserRepo) Delete(ctx context.Context, actorID, id string) error {
	return r.store.applyCtx(ctx, kindSoftDeleteUser, cmdSoftDeleteUser{
		ActorID: actorID,
		ID:      id,
		At:      time.Now().UTC(),
	})
}

// SetState applies an administrator lifecycle action.
func (r *UserRepo) SetState(ctx context.Context, actorID, id string, action domain.UserStateAction) (*domain.User, error) {
	resp, err := r.store.applyCtxResponse(ctx, kindUserState, cmdUserState{
		ActorID: actorID,
		ID:      id,
		Action:  action,
		At:      time.Now().UTC(),
	})
	if ferr := fsmError(resp, err); ferr != nil {
		return nil, ferr
	}
	u, err := userResponse(resp)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// CommitOAuth applies a revision-checked atomic OAuth account mutation. The
// FSM response is the committed snapshot; the store is never re-read after a
// successful commit (another write could have intervened).
func (r *UserRepo) CommitOAuth(ctx context.Context, mutation domain.OAuthUserMutation) (*domain.OAuthUserResult, error) {
	resp, err := r.store.applyCtxResponse(ctx, kindOAuthUserMutation, cmdOAuthUserMutation{
		Kind:             mutation.Kind,
		User:             cmdUserFrom(mutation.User),
		ExpectedRevision: mutation.ExpectedRevision,
		ManagedGroupIDs:  mutation.ManagedGroupIDs,
		FallbackGroupID:  mutation.FallbackGroupID,
		At:               time.Now().UTC(),
	})
	if ferr := fsmError(resp, err); ferr != nil {
		return nil, ferr
	}
	res, ok := resp.(*domain.OAuthUserResult)
	if !ok || res == nil {
		return nil, fmt.Errorf("oauth mutation: unexpected raft response %T", resp)
	}
	if res.User == nil {
		return nil, fmt.Errorf("oauth mutation: raft response missing user")
	}
	return res, nil
}

func (r *UserRepo) Count(ctx context.Context) (int, error) {
	return r.store.fsmRead().countUsers(), nil
}

// userResponse type-checks a committed user snapshot from the FSM.
func userResponse(resp interface{}) (*domain.User, error) {
	u, ok := resp.(*domain.User)
	if !ok || u == nil {
		return nil, fmt.Errorf("user mutation: unexpected raft response %T", resp)
	}
	return u, nil
}
