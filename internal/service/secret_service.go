package service

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	authcrypto "github.com/disaster37/go-auth/crypto"
	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// SecretService stores write-only AES-256-GCM sealed secrets with scope-bound
// AAD (org/project/environment/github_user/github_org_account).
type SecretService struct {
	repo   domain.SecretRepository
	key    []byte
	logger *logrus.Logger
}

var _ domain.TokenSealer = (*SecretService)(nil)

// NewSecretService returns a SecretService. key must be 32 bytes.
func NewSecretService(repo domain.SecretRepository, key []byte, logger *logrus.Logger) *SecretService {
	return &SecretService{repo: repo, key: key, logger: logger}
}

func secretAAD(scope domain.SecretScope, scopeID string) []byte {
	return []byte(fmt.Sprintf("%s|%s", scope, scopeID))
}

func (s *SecretService) sealer(scope domain.SecretScope, scopeID string) (*authcrypto.AESGCMSealer, error) {
	if scopeID == "" || strings.Contains(scopeID, "|") {
		return nil, fmt.Errorf("%w: invalid secret scope id", domain.ErrValidation)
	}
	sealer, err := authcrypto.NewAESGCMSealer(s.key, secretAAD(scope, scopeID))
	if err != nil {
		return nil, fmt.Errorf("create secret sealer: %w", err)
	}
	return sealer, nil
}

// openSealed decrypts a sealed value while enforcing this service's scope-bound
// AAD. go-auth's JSON envelope embeds its AAD (ctx) and OpenString trusts it,
// so opening the envelope under a different scope would otherwise succeed.
// Comparing the embedded context against the expected AAD before decryption
// keeps cross-scope substitution impossible.
func (s *SecretService) openSealed(scope domain.SecretScope, scopeID, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	sealer, err := s.sealer(scope, scopeID)
	if err != nil {
		return "", err
	}
	var envelope struct {
		Alg string `json:"alg"`
		Ctx string `json:"ctx"`
		Ct  string `json:"ct"`
	}
	if err := json.Unmarshal([]byte(ciphertext), &envelope); err == nil && envelope.Alg != "" && envelope.Ct != "" {
		aad, err := base64.StdEncoding.DecodeString(envelope.Ctx)
		if err != nil {
			return "", fmt.Errorf("decode sealed value context: %w", err)
		}
		expected := secretAAD(scope, scopeID)
		if subtle.ConstantTimeCompare(aad, expected) != 1 {
			return "", fmt.Errorf("open sealed value: %w", domain.ErrValidation)
		}
	}
	pt, err := sealer.OpenString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("open sealed value: %w", err)
	}
	return pt, nil
}

// Put seals and stores a secret value.
func (s *SecretService) Put(ctx context.Context, scope domain.SecretScope, scopeID, name, value string) (*domain.Secret, error) {
	if name == "" || scopeID == "" {
		return nil, fmt.Errorf("%w: secret name and scope id are required", domain.ErrValidation)
	}
	if scope == domain.SecretScopeEnvironment {
		orgID, envName, ok := strings.Cut(scopeID, "/")
		if !ok {
			return nil, fmt.Errorf("%w: environment scope id must be <orgID>/<envName>", domain.ErrValidation)
		}
		canonical, err := domain.EnvironmentScopeID(orgID, envName)
		if err != nil {
			return nil, err
		}
		scopeID = canonical
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
	pt, err := s.openSealed(scope, scopeID, sec.Ciphertext)
	if err != nil {
		return "", err
	}
	return pt, nil
}

// OpenRaw decrypts a sealed value stored outside the secret repository (e.g.
// a forge token or webhook secret on a ForgeSource).
func (s *SecretService) OpenRaw(scope domain.SecretScope, scopeID, ciphertext string) (string, error) {
	return s.openSealed(scope, scopeID, ciphertext)
}

// SealRaw seals a value stored outside the secret repository.
func (s *SecretService) SealRaw(scope domain.SecretScope, scopeID, plaintext string) (string, error) {
	sealer, err := s.sealer(scope, scopeID)
	if err != nil {
		return "", err
	}
	return sealer.SealString(plaintext)
}

// PutEnvironment seals and stores an environment-scoped secret under the
// tenant-isolated "<orgID>/<envName>" scope.
func (s *SecretService) PutEnvironment(ctx context.Context, orgID, envName, name, value string) (*domain.Secret, error) {
	scopeID, err := domain.EnvironmentScopeID(orgID, envName)
	if err != nil {
		return nil, err
	}
	return s.Put(ctx, domain.SecretScopeEnvironment, scopeID, name, value)
}

// ListEnvironment lists environment secret metadata.
func (s *SecretService) ListEnvironment(ctx context.Context, orgID, envName string) ([]*domain.Secret, error) {
	scopeID, err := domain.EnvironmentScopeID(orgID, envName)
	if err != nil {
		return nil, err
	}
	return s.List(ctx, domain.SecretScopeEnvironment, scopeID)
}

// DeleteEnvironment removes an environment secret.
func (s *SecretService) DeleteEnvironment(ctx context.Context, orgID, envName, name string) error {
	scopeID, err := domain.EnvironmentScopeID(orgID, envName)
	if err != nil {
		return err
	}
	return s.Delete(ctx, domain.SecretScopeEnvironment, scopeID, name)
}

// RevealEnvironment decrypts an environment secret.
func (s *SecretService) RevealEnvironment(ctx context.Context, orgID, envName, name string) (string, error) {
	scopeID, err := domain.EnvironmentScopeID(orgID, envName)
	if err != nil {
		return "", err
	}
	return s.Reveal(ctx, domain.SecretScopeEnvironment, scopeID, name)
}

// Resolve returns the effective secret value for a job: environment overrides
// project, which overrides org.
func (s *SecretService) Resolve(ctx context.Context, orgID, projectID, environment, name string) (string, error) {
	if environment != "" {
		if v, err := s.RevealEnvironment(ctx, orgID, environment, name); err == nil {
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

// ResolveJobSecrets resolves every named secret in names using Resolve; it
// returns an error if any declared secret is missing.
func (s *SecretService) ResolveJobSecrets(ctx context.Context, orgID, projectID, approvedEnvironment string, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		v, err := s.Resolve(ctx, orgID, projectID, approvedEnvironment, name)
		if err != nil {
			return nil, fmt.Errorf("resolve secret %s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// ResolveAllForCheck merges all Org secrets overridden by all Project secrets
// for non-fork check runs (never includes Environment secrets).
func (s *SecretService) ResolveAllForCheck(ctx context.Context, orgID, projectID string) (map[string]string, error) {
	out := make(map[string]string)
	orgSecrets, err := s.List(ctx, domain.SecretScopeOrg, orgID)
	if err != nil {
		return nil, err
	}
	for _, sec := range orgSecrets {
		v, err := s.Reveal(ctx, domain.SecretScopeOrg, orgID, sec.Name)
		if err != nil {
			return nil, err
		}
		out[sec.Name] = v
	}
	if projectID != "" {
		projectSecrets, err := s.List(ctx, domain.SecretScopeProject, projectID)
		if err != nil {
			return nil, err
		}
		for _, sec := range projectSecrets {
			v, err := s.Reveal(ctx, domain.SecretScopeProject, projectID, sec.Name)
			if err != nil {
				return nil, err
			}
			out[sec.Name] = v
		}
	}
	return out, nil
}
