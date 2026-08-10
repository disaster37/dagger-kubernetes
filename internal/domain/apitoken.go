package domain

import (
	"context"
	"time"
)

// APIToken is a per-user bearer token used by CI (DAGGER_CLOUD_TOKEN compatible).
// Only one token per user is allowed. The plaintext is returned exactly once
// at creation/regeneration; only the SHA-256 hash is persisted.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	TokenHash  string     `json:"-"`
	Prefix     string     `json:"prefix"` // first 12 chars of plaintext, e.g. "dct_ab12cd34"
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// APITokenRepository is the persistence interface for API tokens.
type APITokenRepository interface {
	Upsert(ctx context.Context, t *APIToken) error
	GetByHash(ctx context.Context, tokenHash string) (*APIToken, error)
	GetByUser(ctx context.Context, userID string) (*APIToken, error)
	Delete(ctx context.Context, userID string) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}
