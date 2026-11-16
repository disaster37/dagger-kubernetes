package service

import (
	"context"
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
	secretCT, err := s.secrets.SealRaw(domain.SecretScopeOrg, in.OrgID, in.WebhookSecret)
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
	if _, err := s.providers[in.Provider].RegisterWebhook(ctx, src, callback, in.WebhookSecret); err != nil {
		s.logger.WithError(err).WithField("source_id", id).Warn("forge webhook registration failed")
	}
	return src, nil
}

// List returns an org's forge sources.
func (s *ForgeService) List(ctx context.Context, orgID string) ([]*domain.ForgeSource, error) {
	return s.sources.ListByOrg(ctx, orgID)
}

// Delete removes a source.
func (s *ForgeService) Delete(ctx context.Context, id string) error {
	return s.sources.Delete(ctx, id)
}
