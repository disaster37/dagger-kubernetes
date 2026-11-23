package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ForgeService manages forge integrations and their encrypted credentials.
type ForgeService struct {
	sources   domain.ForgeSourceRepository
	secrets   *SecretService
	providers map[domain.ForgeKind]domain.ForgeProvider
	publicURL string
	logger    *logrus.Logger
}

// NewForgeService returns a ForgeService.
func NewForgeService(sources domain.ForgeSourceRepository, secrets *SecretService, providers []domain.ForgeProvider, publicURL string, logger *logrus.Logger) *ForgeService {
	m := make(map[domain.ForgeKind]domain.ForgeProvider, len(providers))
	for _, p := range providers {
		m[p.Kind()] = p
	}
	return &ForgeService{sources: sources, secrets: secrets, providers: m, publicURL: strings.TrimSuffix(publicURL, "/"), logger: logger}
}

// ForgeSourceInput is the create/update payload.
type ForgeSourceInput struct {
	ID            string
	OrgID         string
	ProjectID     string
	Provider      domain.ForgeKind
	BaseURL       string
	Repo          string
	Token         string
	WebhookSecret string
}

// CreateSource seals credentials, stores the source, and registers a webhook.
func (s *ForgeService) CreateSource(ctx context.Context, in *ForgeSourceInput) (*domain.ForgeSource, error) {
	if in.OrgID == "" || in.Repo == "" {
		return nil, fmt.Errorf("%w: org and repo are required", domain.ErrValidation)
	}
	if _, ok := s.providers[in.Provider]; !ok {
		return nil, fmt.Errorf("%w: unsupported forge %q", domain.ErrValidation, in.Provider)
	}
	id := in.ID
	if id == "" {
		id = fmt.Sprintf("%s-%s", in.Provider, strings.ReplaceAll(in.Repo, "/", "-"))
	}
	tokenCT, err := s.secrets.SealRaw(domain.SecretScopeOrg, in.OrgID, in.Token)
	if err != nil {
		return nil, err
	}
	// Registering a webhook with an empty secret makes the forge send unsigned
	// deliveries, which fail closed on verification. Auto-generate one so the
	// remote webhook and the sealed value always agree.
	webhookSecret := in.WebhookSecret
	if webhookSecret == "" {
		webhookSecret, err = forgeRandomHex(32)
		if err != nil {
			return nil, fmt.Errorf("generate webhook secret: %w", err)
		}
		s.logger.WithField("source_id", id).Info("generated webhook secret for forge source")
	}
	secretCT, err := s.secrets.SealRaw(domain.SecretScopeOrg, in.OrgID, webhookSecret)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	src := &domain.ForgeSource{
		ID: id, OrgID: in.OrgID, ProjectID: in.ProjectID, Provider: in.Provider,
		BaseURL: in.BaseURL, Repo: in.Repo, TokenCiphertext: tokenCT,
		WebhookSecretCiphertext: secretCT, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.sources.Upsert(ctx, src); err != nil {
		return nil, err
	}
	callback := fmt.Sprintf("%s/v1/webhooks/%s/%s", s.publicURL, in.Provider, id)
	// Clear any stale hook with the same callback URL before registering, so a
	// recreate does not leave duplicates and pre-fix orphans are cleaned up.
	if err := s.providers[in.Provider].DeleteWebhook(ctx, src, in.Token, ""); err != nil {
		s.logger.WithError(err).WithField("source_id", id).Warn("forge webhook pre-clean failed")
	}
	if _, err := s.providers[in.Provider].RegisterWebhook(ctx, src, in.Token, callback, webhookSecret); err != nil {
		s.logger.WithError(err).WithField("source_id", id).Warn("forge webhook registration failed")
	}
	return src, nil
}

// forgeRandomHex returns n random bytes hex-encoded.
func forgeRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// List returns an org's forge sources.
func (s *ForgeService) List(ctx context.Context, orgID string) ([]*domain.ForgeSource, error) {
	return s.sources.ListByOrg(ctx, orgID)
}

// Delete removes a source and best-effort removes its remote webhook.
func (s *ForgeService) Delete(ctx context.Context, id string) error {
	src, err := s.sources.Get(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Already gone: keep delete idempotent (nothing to clean up).
			return nil
		}
		return err
	}
	if p, ok := s.providers[src.Provider]; ok {
		token, err := s.secrets.OpenRaw(domain.SecretScopeOrg, src.OrgID, src.TokenCiphertext)
		if err != nil {
			s.logger.WithError(err).WithField("source_id", id).Warn("forge webhook cleanup failed")
		} else if err := p.DeleteWebhook(ctx, src, token, ""); err != nil {
			s.logger.WithError(err).WithField("source_id", id).Warn("forge webhook cleanup failed")
		}
	}
	return s.sources.Delete(ctx, id)
}
