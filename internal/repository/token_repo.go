package repository

import (
	"context"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TokenRepo is the Raft implementation of domain.APITokenRepository.
type TokenRepo struct {
	store *RaftStore
}

var _ domain.APITokenRepository = (*TokenRepo)(nil)

// NewTokenRepo returns a TokenRepo backed by store.
func NewTokenRepo(store *RaftStore) *TokenRepo {
	return &TokenRepo{store: store}
}

// Upsert replaces any existing token for the user (one token per user).
func (r *TokenRepo) Upsert(ctx context.Context, t *domain.APIToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	return r.store.applyCtx(ctx, kindUpsertToken, cmdTokenFrom(t))
}

func (r *TokenRepo) GetByHash(ctx context.Context, tokenHash string) (*domain.APIToken, error) {
	return r.store.fsmRead().readTokenByHash(tokenHash)
}

func (r *TokenRepo) GetByUser(ctx context.Context, userID string) (*domain.APIToken, error) {
	return r.store.fsmRead().readTokenByUser(userID)
}

func (r *TokenRepo) Delete(ctx context.Context, userID string) error {
	return r.store.applyCtx(ctx, kindDeleteToken, cmdDeleteToken{UserID: userID})
}

func (r *TokenRepo) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	return r.store.applyCtx(ctx, kindTouchToken, cmdTouchToken{ID: id, At: at})
}
