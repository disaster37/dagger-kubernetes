package service

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ChecksService verifies forge webhooks and dispatches Cloud Checks runs.
type ChecksService struct {
	sources   domain.ForgeSourceRepository
	secrets   *SecretService
	projects  *ProjectService
	orgs      *OrgService
	runner    domain.CheckJobRunner
	providers map[domain.ForgeKind]domain.ForgeProvider
	logger    *logrus.Logger
}

// NewChecksService returns a ChecksService.
func NewChecksService(sources domain.ForgeSourceRepository, secrets *SecretService, projects *ProjectService, orgs *OrgService, runner domain.CheckJobRunner, providers []domain.ForgeProvider, logger *logrus.Logger) *ChecksService {
	m := make(map[domain.ForgeKind]domain.ForgeProvider, len(providers))
	for _, p := range providers {
		m[p.Kind()] = p
	}
	return &ChecksService{sources: sources, secrets: secrets, projects: projects, orgs: orgs, runner: runner, providers: m, logger: logger}
}

// HandleWebhook verifies and dispatches a forge webhook. HMAC failures return
// ErrForbidden and must never be recorded as token failures (shared NAT IPs).
func (s *ChecksService) HandleWebhook(ctx context.Context, provider domain.ForgeKind, sourceID string, headers map[string]string, body []byte) (*domain.WebhookEvent, error) {
	src, err := s.sources.Get(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if src.Provider != provider {
		return nil, fmt.Errorf("%w: provider mismatch", domain.ErrValidation)
	}
	p, ok := s.providers[provider]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported forge %q", domain.ErrValidation, provider)
	}
	secret, err := s.secrets.OpenRaw(domain.SecretScopeOrg, src.OrgID, src.WebhookSecretCiphertext)
	if err != nil {
		return nil, fmt.Errorf("open webhook secret: %w", err)
	}
	if !p.VerifyWebhook(src, secret, headers, body) {
		return nil, fmt.Errorf("%w: invalid webhook signature", domain.ErrForbidden)
	}
	ev, err := domain.ParseForgeWebhook(provider, forgeEventName(provider, headers), body)
	if err != nil {
		return nil, err
	}
	if ev.Repo == "" {
		ev.Repo = src.Repo
	}
	proj, err := s.projects.GetOrCreateByName(ctx, ev.Repo)
	if err != nil {
		return nil, err
	}
	spec := domain.CheckRunSpec{
		ID:        fmt.Sprintf("check-%s-%s", sourceID, ev.SHA),
		OrgID:     src.OrgID,
		ProjectID: proj.ID,
		SourceID:  src.ID,
		Provider:  provider,
		Repo:      ev.Repo,
		SHA:       ev.SHA,
		Ref:       ev.Ref,
		PRNumber:  ev.PRNumber,
		IsFork:    ev.IsFork,
	}
	if ev.IsFork {
		// Fork PRs hold for approval and never receive org/project secrets.
		// The hold is enforced here: the check-run Job is NOT created until an
		// org member/admin explicitly approves the run (CWE-862/863).
		spec.Environment = "waiting_approval"
	}
	if s.runner != nil {
		if spec.Environment == "waiting_approval" {
			s.logger.WithFields(logrus.Fields{
				"source_id": src.ID,
				"sha":       ev.SHA,
				"pr":        ev.PRNumber,
			}).Info("fork PR check run held for approval")
			return ev, nil
		}
		if err := s.runner.CancelSuperseded(ctx, proj.ID, ev.Ref); err != nil {
			s.logger.WithError(err).Warn("cancel superseded check runs failed")
		}
		if err := s.runner.Run(ctx, &spec); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// forgeEventName extracts the provider event name from headers.
func forgeEventName(provider domain.ForgeKind, headers map[string]string) string {
	switch provider {
	case domain.ForgeGitHub:
		return headers["X-GitHub-Event"]
	case domain.ForgeGitLab:
		return headers["X-Gitlab-Event"]
	case domain.ForgeGitea:
		return headers["X-Gitea-Event"]
	default:
		return ""
	}
}
