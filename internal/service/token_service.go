package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TokenService implements API token generate/regenerate/revoke/validate.
type TokenService struct {
	tokens domain.APITokenRepository
	logger *logrus.Logger
}

// NewTokenService returns a TokenService.
func NewTokenService(tokens domain.APITokenRepository, logger *logrus.Logger) *TokenService {
	return &TokenService{tokens: tokens, logger: logger}
}

// Generate creates a new token for the user. Returns ErrTokenExists if the
// user already has one. The plaintext token is returned exactly once.
func (s *TokenService) Generate(ctx context.Context, userID string) (string, *domain.APIToken, error) {
	if _, err := s.tokens.GetByUser(ctx, userID); err == nil {
		return "", nil, domain.ErrTokenExists
	} else if !isNotFound(err) {
		return "", nil, err
	}
	return s.upsertNew(ctx, userID)
}

// Regenerate replaces the user's token hash; the old token is invalid
// immediately. The plaintext token is returned exactly once.
func (s *TokenService) Regenerate(ctx context.Context, userID string) (string, *domain.APIToken, error) {
	return s.upsertNew(ctx, userID)
}

func (s *TokenService) upsertNew(ctx context.Context, userID string) (string, *domain.APIToken, error) {
	plaintext := newPlaintextToken()
	t := newAPIToken(userID, plaintext)
	if err := s.tokens.Upsert(ctx, t); err != nil {
		return "", nil, err
	}
	s.logger.WithFields(logrus.Fields{
		"token_id": t.ID,
		"user_id":  userID,
	}).Info("api token issued")
	return plaintext, t, nil
}

// Revoke deletes the user's token (no error if none exists).
func (s *TokenService) Revoke(ctx context.Context, userID string) error {
	return s.tokens.Delete(ctx, userID)
}

// Meta returns the masked metadata for the user's token (ErrNotFound if none).
func (s *TokenService) Meta(ctx context.Context, userID string) (*domain.APIToken, error) {
	return s.tokens.GetByUser(ctx, userID)
}

// Validate looks up a token by its hash. On success it best-effort touches
// LastUsedAt (errors are logged, never returned, so auth never fails on a
// touch error).
func (s *TokenService) Validate(ctx context.Context, plaintext string) (*domain.APIToken, error) {
	if plaintext == "" {
		return nil, domain.ErrUnauthenticated
	}
	t, err := s.tokens.GetByHash(ctx, HashAPIToken(plaintext))
	if err != nil {
		return nil, domain.ErrUnauthenticated
	}
	if err := s.tokens.TouchLastUsed(ctx, t.ID, time.Now().UTC()); err != nil {
		s.logger.WithError(err).WithField("token_id", t.ID).Warn("touch last_used failed")
	}
	return t, nil
}

// ImportRaw upserts a token for a user using a known plaintext value (used by
// the legacy token importer). The plaintext is hashed via the same path.
func (s *TokenService) ImportRaw(ctx context.Context, userID, plaintext string) error {
	return s.tokens.Upsert(ctx, newAPIToken(userID, plaintext))
}

// newAPIToken builds an APIToken for userID from a plaintext value, storing
// only the hash and a short display prefix.
func newAPIToken(userID, plaintext string) *domain.APIToken {
	return &domain.APIToken{
		ID:        newID(),
		UserID:    userID,
		TokenHash: HashAPIToken(plaintext),
		Prefix:    plaintext[:min(12, len(plaintext))],
	}
}

// newPlaintextToken returns a new token of the form dct_<32 random bytes hex>.
func newPlaintextToken() string {
	return fmt.Sprintf("dct_%s", randomHex(32))
}

// HashAPIToken returns the SHA-256 hex of the plaintext token.
func HashAPIToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
