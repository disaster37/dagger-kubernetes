package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// tokenCols is the canonical column list shared by all api_tokens queries.
const tokenCols = `id, user_id, token_hash, prefix, created_at, last_used_at`

// TokenRepo is the SQLite implementation of domain.APITokenRepository.
type TokenRepo struct {
	db *sql.DB
}

var _ domain.APITokenRepository = (*TokenRepo)(nil)

// NewTokenRepo returns a TokenRepo backed by db.
func NewTokenRepo(db *sql.DB) *TokenRepo {
	return &TokenRepo{db: db}
}

// Upsert replaces any existing token for the user (one token per user).
func (r *TokenRepo) Upsert(ctx context.Context, t *domain.APIToken) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO api_tokens(%s)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET token_hash = excluded.token_hash, prefix = excluded.prefix, created_at = excluded.created_at, last_used_at = excluded.last_used_at`, tokenCols),
		t.ID, t.UserID, t.TokenHash, t.Prefix, t.CreatedAt, nullTime(t.LastUsedAt))
	if err != nil {
		return fmt.Errorf("upsert api token for user %s: %w", t.UserID, err)
	}
	return nil
}

func (r *TokenRepo) GetByHash(ctx context.Context, tokenHash string) (*domain.APIToken, error) {
	t, err := scanToken(r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM api_tokens WHERE token_hash = ?`, tokenCols), tokenHash))
	if err != nil {
		return nil, fmt.Errorf("get api token by hash: %w", err)
	}
	return t, nil
}

func (r *TokenRepo) GetByUser(ctx context.Context, userID string) (*domain.APIToken, error) {
	t, err := scanToken(r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM api_tokens WHERE user_id = ?`, tokenCols), userID))
	if err != nil {
		return nil, fmt.Errorf("get api token for user %s: %w", userID, err)
	}
	return t, nil
}

func (r *TokenRepo) Delete(ctx context.Context, userID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete api token for user %s: %w", userID, err)
	}
	return nil
}

func (r *TokenRepo) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return fmt.Errorf("touch api token %s: %w", id, err)
	}
	return nil
}

func scanToken(row scanner) (*domain.APIToken, error) {
	t := &domain.APIToken{}
	var lastUsed sql.NullTime
	err := row.Scan(&t.ID, &t.UserID, &t.TokenHash, &t.Prefix, &t.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("api token: %w", domain.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		t.LastUsedAt = &lastUsed.Time
	}
	return t, nil
}
