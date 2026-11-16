package service

import (
	"context"
	"fmt"
	"time"

	authcrypto "github.com/disaster37/go-auth/crypto"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// SecretService stores write-only AES-256-GCM sealed secrets with scope-bound
// AAD (org/project/environment).
type SecretService struct {
	repo   domain.SecretRepository
	key    []byte
	logger *logrus.Logger
}

// NewSecretService returns a SecretService. key must be 32 bytes.
func NewSecretService(repo domain.SecretRepository, key []byte, logger *logrus.Logger) *SecretService {
	return &SecretService{repo: repo, key: key, logger: logger}
}

func secretAAD(scope domain.SecretScope, scopeID string) []byte {
	return []byte(fmt.Sprintf("%s|%s", scope, scopeID))
}

func (s *SecretService) sealer(scope domain.SecretScope, scopeID string) (*authcrypto.AESGCMSealer, error) {
	sealer, err := authcrypto.NewAESGCMSealer(s.key, secretAAD(scope, scopeID))
	if err != nil {
		return nil, fmt.Errorf("create secret sealer: %w", err)
	}
	return sealer, nil
}

// Put seals and stores a secret value.
func (s *SecretService) Put(ctx context.Context, scope domain.SecretScope, scopeID, name, value string) (*domain.Secret, error) {
	if name == "" || scopeID == "" {
		return nil, fmt.Errorf("%w: secret name and scope id are required", domain.ErrValidation)
	}
	sealer, err := s.sealer(scope, scopeID)
	if err != nil {
		return nil, err
	}
	ct, err := sealer.SealString(value)
	if err != nil {
		return nil, fmt.Errorf("seal secret: %w", err)
	}
	now := time.Now().UTC()
	sec := &domain.Secret{ID: fmt.Sprintf("%s:%s:%s", scope, scopeID, name), Scope: scope, ScopeID: scopeID, Name: name, Ciphertext: ct, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.Upsert(ctx, sec); err != nil {
		return nil, err
	}
	return sec, nil
}

// List returns secret metadata (never plaintext).
func (s *SecretService) List(ctx context.Context, scope domain.SecretScope, scopeID string) ([]*domain.Secret, error) {
	return s.repo.List(ctx, scope, scopeID)
}

// Delete removes a secret.
func (s *SecretService) Delete(ctx context.Context, scope domain.SecretScope, scopeID, name string) error {
	return s.repo.Delete(ctx, scope, scopeID, name)
}

// Reveal decrypts a secret for injection into a job. Callers must enforce the
// approval/scope rules before calling.
func (s *SecretService) Reveal(ctx context.Context, scope domain.SecretScope, scopeID, name string) (string, error) {
	sec, err := s.repo.Get(ctx, scope, scopeID, name)
	if err != nil {
		return "", err
	}
	sealer, err := s.sealer(scope, scopeID)
	if err != nil {
		return "", err
	}
	pt, err := sealer.OpenString(sec.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("open secret: %w", err)
	}
	return pt, nil
}

// OpenRaw decrypts a sealed value stored outside the secret repository (e.g.
// a forge token or webhook secret on a ForgeSource).
func (s *SecretService) OpenRaw(scope domain.SecretScope, scopeID, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	sealer, err := s.sealer(scope, scopeID)
	if err != nil {
		return "", err
	}
	pt, err := sealer.OpenString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("open sealed value: %w", err)
	}
	return pt, nil
}

// SealRaw seals a value stored outside the secret repository.
func (s *SecretService) SealRaw(scope domain.SecretScope, scopeID, plaintext string) (string, error) {
	sealer, err := s.sealer(scope, scopeID)
	if err != nil {
		return "", err
	}
	return sealer.SealString(plaintext)
}

// Resolve returns the effective secret value for a job: environment overrides
// project, which overrides org.
func (s *SecretService) Resolve(ctx context.Context, orgID, projectID, environment, name string) (string, error) {
	if environment != "" {
		if v, err := s.Reveal(ctx, domain.SecretScopeEnvironment, environment, name); err == nil {
			return v, nil
		}
	}
	if projectID != "" {
		if v, err := s.Reveal(ctx, domain.SecretScopeProject, projectID, name); err == nil {
			return v, nil
		}
	}
	return s.Reveal(ctx, domain.SecretScopeOrg, orgID, name)
}
