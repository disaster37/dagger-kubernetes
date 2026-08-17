package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// TokenService implements API token generate/regenerate/revoke/validate.
type TokenService struct {
	tokens domain.APITokenRepository
	encKey []byte // AES-256 key (32 bytes); nil = encryption disabled (pre-config)
	logger *logrus.Logger
}

// NewTokenService returns a TokenService. encKey is the AES-256 key used to
// reversibly encrypt token plaintexts (nil disables encryption).
func NewTokenService(tokens domain.APITokenRepository, logger *logrus.Logger, encKey []byte) *TokenService {
	return &TokenService{tokens: tokens, encKey: encKey, logger: logger}
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
	ct, err := encryptToken(s.encKey, plaintext)
	if err != nil {
		return "", nil, fmt.Errorf("encrypt token: %w", err)
	}
	t.TokenCiphertext = ct
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

// Reveal returns the plaintext token for the user. Returns ErrNotFound when no
// token exists, ErrTokenNotRecoverable when the ciphertext is empty (pre-v2
// token) or the encryption key is unavailable.
func (s *TokenService) Reveal(ctx context.Context, userID string) (string, error) {
	t, err := s.tokens.GetByUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if t.TokenCiphertext == "" {
		return "", domain.ErrTokenNotRecoverable
	}
	if len(s.encKey) == 0 {
		return "", domain.ErrTokenNotRecoverable
	}
	pt, err := decryptToken(s.encKey, t.TokenCiphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt token: %w", err)
	}
	return pt, nil
}

// encKeyAvailable reports whether a usable encryption key is configured.
// Kept unexported so ConnectService (same package) can compute Recoverable
// without leaking the key material.
func (s *TokenService) encKeyAvailable() bool {
	return len(s.encKey) > 0
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
// the legacy token importer). The plaintext is hashed via the same path and
// reversibly encrypted when a key is available so migrated tokens are also
// recoverable from the Connect-env UI.
func (s *TokenService) ImportRaw(ctx context.Context, userID, plaintext string) error {
	t := newAPIToken(userID, plaintext)
	ct, err := encryptToken(s.encKey, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}
	t.TokenCiphertext = ct
	if ct == "" {
		s.logger.Warn("api token imported without encryption key; token will not be recoverable")
	}
	return s.tokens.Upsert(ctx, t)
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

// newGCM returns an AES-GCM cipher for key, shared by encrypt/decrypt.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptToken AES-256-GCM encrypts plaintext with key and returns a base64
// string of nonce||ciphertext||tag. A nil/empty key disables encryption and
// returns ("", nil) — the pre-v2 behavior of not persisting the plaintext.
func encryptToken(key []byte, plaintext string) (string, error) {
	if len(key) == 0 {
		return "", nil // encryption disabled
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil) // nonce || ciphertext || tag
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decryptToken reverses encryptToken.
func decryptToken(key []byte, ctB64 string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
