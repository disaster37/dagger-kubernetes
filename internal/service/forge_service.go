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
	sources      domain.ForgeSourceRepository
	secrets      *SecretService
	providers    map[domain.ForgeKind]domain.ForgeProvider
	accounts     domain.GitHubAccountSourceRepository
	repoSettings domain.RepoSettingRepository
	publicURL    string
	logger       *logrus.Logger
}

// NewForgeService returns a ForgeService.
func NewForgeService(sources domain.ForgeSourceRepository, secrets *SecretService, providers []domain.ForgeProvider, publicURL string, logger *logrus.Logger) *ForgeService {
	m := make(map[domain.ForgeKind]domain.ForgeProvider, len(providers))
	for _, p := range providers {
		m[p.Kind()] = p
	}
	return &ForgeService{sources: sources, secrets: secrets, providers: m, publicURL: strings.TrimSuffix(publicURL, "/"), logger: logger}
}

// WithGitHubSync enables the account-source + repo-setting mirroring used by
// the Cloud CLI integration model.
func (s *ForgeService) WithGitHubSync(accounts domain.GitHubAccountSourceRepository, repoSettings domain.RepoSettingRepository) *ForgeService {
	s.accounts = accounts
	s.repoSettings = repoSettings
	return s
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
//
//nolint:gocyclo // create + GitHub sync branches
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
	repo := in.Repo
	if in.Provider == domain.ForgeGitHub {
		repo = repoTail(in.Repo)
	}
	baseURL := in.BaseURL
	accountBase := baseURL
	if in.Provider == domain.ForgeGitHub && accountBase == "" {
		accountBase = "https://api.github.com"
	}
	tokenCT, err := s.secrets.SealRaw(domain.SecretScopeOrg, in.OrgID, in.Token)
	if err != nil {
		return nil, err
	}
	var acct *domain.GitHubAccountSource
	if in.Provider == domain.ForgeGitHub && in.Token != "" && s.accounts != nil {
		owner, _ := splitOwnerRepo(repo)
		if owner != "" {
			acct, err = s.upsertAccountForSource(ctx, in.OrgID, accountBase, owner, in.Token)
			if err != nil {
				return nil, err
			}
		}
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
		BaseURL: baseURL, Repo: repo, TokenCiphertext: tokenCT,
		WebhookSecretCiphertext: secretCT, CreatedAt: now, UpdatedAt: now,
	}
	if acct != nil {
		src.AccountSourceID = acct.ID
	}
	if err := s.sources.Upsert(ctx, src); err != nil {
		return nil, err
	}
	if acct != nil && s.repoSettings != nil {
		canonical := domain.CanonicalForgeRepo(domain.ForgeGitHub, accountBase, repo)
		setting := &domain.RepoSetting{
			OrgID: in.OrgID, Repo: canonical, Provider: domain.ForgeGitHub,
			SourceID: acct.ID, ForgeSourceID: id,
			RunChecksOnDefaultBranch: true, RunChecksOnPullRequest: true, RunChecksOnBranchPush: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if existing, err := s.repoSettings.Get(ctx, in.OrgID, canonical); err == nil {
			setting.CreatedAt = existing.CreatedAt
			setting.ClaimedByUserID = existing.ClaimedByUserID
		}
		if err := s.repoSettings.BatchUpsert(ctx, in.OrgID, []domain.RepoSettingWrite{{Setting: setting}}); err != nil {
			return nil, err
		}
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

// upsertAccountForSource mirrors a per-repo GitHub PAT into the parent
// org-account model so CLI integration/checks commands see it.
func (s *ForgeService) upsertAccountForSource(ctx context.Context, orgID, baseURL, owner, token string) (*domain.GitHubAccountSource, error) {
	acctID := domain.GitHubAccountSourceID(orgID, baseURL, owner)
	scopeID, err := domain.GitHubOrgAccountScopeID(orgID, acctID)
	if err != nil {
		return nil, err
	}
	ct, err := s.secrets.SealRaw(domain.SecretScopeGitHubOrgAccount, scopeID, token)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	acct := &domain.GitHubAccountSource{
		ID: acctID, OrgID: orgID, Owner: owner, OwnerType: "PAT", BaseURL: baseURL,
		TokenCiphertext: ct, Status: domain.GitHubConnectionActive, CreatedAt: now, UpdatedAt: now,
	}
	if existing, err := s.accounts.Get(ctx, acctID); err == nil {
		acct.CreatedAt = existing.CreatedAt
		acct.ConnectedByUserID = existing.ConnectedByUserID
	}
	if err := s.accounts.Upsert(ctx, acct); err != nil {
		return nil, err
	}
	return acct, nil
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
	if s.repoSettings != nil {
		settings, err := s.repoSettings.ListBySource(ctx, id)
		if err != nil {
			return err
		}
		for _, setting := range settings {
			if setting.ForgeSourceID != id {
				continue
			}
			if err := s.repoSettings.Delete(ctx, setting.OrgID, setting.Repo, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		}
	}
	return s.sources.Delete(ctx, id)
}
