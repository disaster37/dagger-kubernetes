package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/disaster37/go-auth/apitoken"
	authcrypto "github.com/disaster37/go-auth/crypto"
	goauthdomain "github.com/disaster37/go-auth/domain"
	goauthobserv "github.com/disaster37/go-auth/observ"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TokenService implements API token generate/regenerate/revoke/validate on top
// of go-auth's apitoken.Service. The sealer transparently opens both go-auth
// JSON envelopes and legacy base64(nonce||ciphertext||tag) values.
type TokenService struct {
	tokens    domain.APITokenRepository
	svc       *apitoken.Service
	sealer    *authcrypto.AESGCMSealer
	sealerErr error
	logger    *logrus.Logger
}

// NewTokenService returns a TokenService. encKey is the AES-256 key used to
// reversibly encrypt token plaintexts (nil disables encryption).
func NewTokenService(tokens domain.APITokenRepository, logger *logrus.Logger, encKey []byte) *TokenService {
	var (
		sealer    *authcrypto.AESGCMSealer
		sealerErr error
	)
	if len(encKey) > 0 {
		s, err := authcrypto.NewAESGCMSealer(encKey, nil)
		if err != nil {
			sealerErr = err
		} else {
			sealer = s
		}
	}
	var cryptoSealer authcrypto.Sealer
	if sealer != nil {
		cryptoSealer = sealer
	}
	svc := apitoken.NewService(&apiTokenStoreAdapter{repo: tokens}, apitoken.Config{
		Prefix: apitoken.DefaultPrefix,
		Sealer: cryptoSealer,
		Logger: goauthobserv.NewLogrusAdapter(logger),
	})
	return &TokenService{tokens: tokens, svc: svc, sealer: sealer, sealerErr: sealerErr, logger: logger}
}

// Generate creates a new token for the user. Returns ErrTokenExists if the
// user already has one. The plaintext token is returned exactly once.
func (s *TokenService) Generate(ctx context.Context, userID string) (string, *domain.APIToken, error) {
	if s.sealerErr != nil {
		return "", nil, fmt.Errorf("encrypt token: %w", s.sealerErr)
	}
	pt, t, err := s.svc.Generate(ctx, userID)
	if err != nil {
		return "", nil, fromGoAuthErr(err)
	}
	return pt, fromGoAuthToken(t), nil
}

// GenerateFor issues a token with a dynamic prefix for a synthetic subject
// (used for org-scoped dag_<org>_ tokens). The plaintext is returned once.
func (s *TokenService) GenerateFor(ctx context.Context, subject, prefix string) (string, *domain.APIToken, error) {
	if s.sealerErr != nil {
		return "", nil, fmt.Errorf("encrypt token: %w", s.sealerErr)
	}
	var cryptoSealer authcrypto.Sealer
	if s.sealer != nil {
		cryptoSealer = s.sealer
	}
	svc := apitoken.NewService(&apiTokenStoreAdapter{repo: s.tokens}, apitoken.Config{
		Prefix: prefix,
		Sealer: cryptoSealer,
		Logger: goauthobserv.NewLogrusAdapter(s.logger),
	})
	pt, t, err := svc.Regenerate(ctx, subject)
	if err != nil {
		return "", nil, fromGoAuthErr(err)
	}
	return pt, fromGoAuthToken(t), nil
}

// Regenerate replaces the user's token; the old token is invalid immediately.
// The plaintext token is returned exactly once.
func (s *TokenService) Regenerate(ctx context.Context, userID string) (string, *domain.APIToken, error) {
	pt, t, err := s.svc.Regenerate(ctx, userID)
	if err != nil {
		return "", nil, fromGoAuthErr(err)
	}
	return pt, fromGoAuthToken(t), nil
}

// Revoke deletes the user's token (no error if none exists).
func (s *TokenService) Revoke(ctx context.Context, userID string) error {
	return fromGoAuthErr(s.svc.Revoke(ctx, userID))
}

// Meta returns the masked metadata for the user's token (ErrNotFound if none).
func (s *TokenService) Meta(ctx context.Context, userID string) (*domain.APIToken, error) {
	t, err := s.svc.Meta(ctx, userID)
	if err != nil {
		return nil, fromGoAuthErr(err)
	}
	return fromGoAuthToken(t), nil
}

// Reveal returns the plaintext token for the user. Returns ErrNotFound when no
// token exists and ErrTokenNotRecoverable when recovery is unavailable.
func (s *TokenService) Reveal(ctx context.Context, userID string) (string, error) {
	pt, err := s.svc.Reveal(ctx, userID)
	if err != nil {
		return "", fromGoAuthErr(err)
	}
	return pt, nil
}

// encKeyAvailable reports whether a usable encryption key is configured.
func (s *TokenService) encKeyAvailable() bool {
	return s.sealer != nil
}

// Validate looks up a token by its hash. On success it best-effort touches
// LastUsedAt (errors are logged, never returned).
func (s *TokenService) Validate(ctx context.Context, plaintext string) (*domain.APIToken, error) {
	t, err := s.svc.Validate(ctx, plaintext)
	if err != nil {
		return nil, fromGoAuthErr(err)
	}
	return fromGoAuthToken(t), nil
}

// ImportRaw upserts a token for a user using a known plaintext value (used by
// the legacy token importer).
func (s *TokenService) ImportRaw(ctx context.Context, userID, plaintext string) error {
	if s.sealerErr != nil {
		return fmt.Errorf("encrypt token: %w", s.sealerErr)
	}
	return fromGoAuthErr(s.svc.ImportRaw(ctx, userID, plaintext))
}

// HashAPIToken returns the SHA-256 hex of the plaintext token.
func HashAPIToken(plaintext string) string {
	return apitoken.Hash(plaintext)
}

// apiTokenStoreAdapter adapts the supervisor's APITokenRepository to the
// go-auth store.APITokenStore interface, translating the token type and the
// not-found/conflict sentinels.
type apiTokenStoreAdapter struct {
	repo domain.APITokenRepository
}

func (a *apiTokenStoreAdapter) Upsert(ctx context.Context, t *goauthdomain.APIToken) error {
	return toGoAuthErr(a.repo.Upsert(ctx, fromGoAuthToken(t)))
}

func (a *apiTokenStoreAdapter) GetByHash(ctx context.Context, tokenHash string) (*goauthdomain.APIToken, error) {
	t, err := a.repo.GetByHash(ctx, tokenHash)
	if err != nil {
		return nil, toGoAuthErr(err)
	}
	return toGoAuthToken(t), nil
}

func (a *apiTokenStoreAdapter) GetByUser(ctx context.Context, userID string) (*goauthdomain.APIToken, error) {
	t, err := a.repo.GetByUser(ctx, userID)
	if err != nil {
		return nil, toGoAuthErr(err)
	}
	return toGoAuthToken(t), nil
}

func (a *apiTokenStoreAdapter) Delete(ctx context.Context, userID string) error {
	return toGoAuthErr(a.repo.Delete(ctx, userID))
}

func (a *apiTokenStoreAdapter) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	return toGoAuthErr(a.repo.TouchLastUsed(ctx, id, at))
}

func toGoAuthToken(t *domain.APIToken) *goauthdomain.APIToken {
	if t == nil {
		return nil
	}
	return &goauthdomain.APIToken{
		ID:              t.ID,
		UserID:          t.UserID,
		TokenHash:       t.TokenHash,
		TokenCiphertext: t.TokenCiphertext,
		Prefix:          t.Prefix,
		CreatedAt:       t.CreatedAt,
		LastUsedAt:      t.LastUsedAt,
	}
}

func fromGoAuthToken(t *goauthdomain.APIToken) *domain.APIToken {
	if t == nil {
		return nil
	}
	return &domain.APIToken{
		ID:              t.ID,
		UserID:          t.UserID,
		TokenHash:       t.TokenHash,
		TokenCiphertext: t.TokenCiphertext,
		Prefix:          t.Prefix,
		CreatedAt:       t.CreatedAt,
		LastUsedAt:      t.LastUsedAt,
	}
}

// toGoAuthErr translates supervisor domain sentinels to go-auth sentinels.
func toGoAuthErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return goauthdomain.ErrNotFound
	case errors.Is(err, domain.ErrConflict):
		return goauthdomain.ErrConflict
	default:
		return err
	}
}

// fromGoAuthErr translates go-auth sentinels to supervisor domain sentinels.
func fromGoAuthErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, goauthdomain.ErrNotFound):
		return domain.ErrNotFound
	case errors.Is(err, goauthdomain.ErrTokenExists):
		return domain.ErrTokenExists
	case errors.Is(err, goauthdomain.ErrTokenNotRecoverable):
		return domain.ErrTokenNotRecoverable
	case errors.Is(err, goauthdomain.ErrUnauthenticated):
		return domain.ErrUnauthenticated
	default:
		return err
	}
}
