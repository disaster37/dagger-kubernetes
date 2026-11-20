package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// ConnectService assembles the Dagger CLI connection environment snapshot.
type ConnectService struct {
	cfg             *domain.Config
	versionResolver domain.VersionResolver
	tokens          *TokenService
	orgs            *OrgService
	logger          *logrus.Logger
}

// NewConnectService returns a ConnectService.
func NewConnectService(
	cfg *domain.Config,
	vr domain.VersionResolver,
	tokens *TokenService,
	logger *logrus.Logger,
) *ConnectService {
	return &ConnectService{cfg: cfg, versionResolver: vr, tokens: tokens, logger: logger}
}

// WithOrgService wires org token support.
func (s *ConnectService) WithOrgService(orgs *OrgService) *ConnectService {
	s.orgs = orgs
	return s
}

// ConnectEnv builds the snapshot. tokenType is "personal" (dct_) or "org"
// (dag_<slug>_). When reveal=true and the token is recoverable, the
// DAGGER_CLOUD_TOKEN value is populated with the plaintext.
func (s *ConnectService) ConnectEnv(ctx context.Context, userID string, org *domain.Org, version, tokenType string, multiOrg, reveal bool) (*domain.ConnectEnvSnapshot, error) {
	snap := &domain.ConnectEnvSnapshot{
		ServerURL:    s.cfg.Server.PublicURL,
		DataHostname: s.cfg.Server.DataHost,
		VersionFloor: s.cfg.Version.Floor,
		Token:        s.tokenMeta(ctx, userID),
		TokenType:    tokenType,
	}
	if org != nil {
		snap.OrgName = org.Name
	}
	snap.AllowedVersions = s.allowedVersions()

	tokenValue := ""
	if tokenType == "org" && org != nil && s.orgs != nil {
		snap.Token = s.orgTokenMeta(ctx, org.ID)
		if reveal {
			pt, err := s.orgs.EnsureToken(ctx, org.ID)
			if err != nil {
				s.logger.WithError(err).Warn("connect: org token reveal unavailable")
			} else {
				tokenValue = pt
			}
		}
	} else if reveal && snap.Token.Recoverable {
		pt, err := s.tokens.Reveal(ctx, userID)
		if err != nil {
			s.logger.WithError(err).Warn("connect: token reveal unavailable")
		} else {
			tokenValue = pt
		}
	}

	envs := []domain.ConnectEnvVar{
		{Name: "DAGGER_ENGINE", Value: "cloud", Required: true, Description: "Selects the Dagger Cloud engine driver."},
		{Name: "DAGGER_CLOUD_URL", Value: s.cfg.Server.PublicURL, Required: true, Description: "Control-plane URL the Dagger CLI talks to (replaces Dagger Cloud)."},
		{Name: "DAGGER_CLOUD_EXTERNAL_URL", Value: s.cfg.Server.PublicURL, Required: false, Description: "Browser-facing control-plane URL."},
		{Name: "DAGGER_CLOUD_AUTH_URL", Value: s.cfg.Server.PublicURL, Required: false, Description: "OAuth/device-flow authorization URL."},
		{Name: "DAGGER_CLOUD_TOKEN", Value: tokenValue, Required: true, Secret: true, Description: "Your API token (dct_... or org-scoped dag_<org>_...)."},
		{Name: "_EXPERIMENTAL_DAGGER_RUNNER_HOST", Value: "dagger-cloud://self", Required: true, Description: "Tells the CLI to provision a remote engine via the cloud driver."},
	}

	if tokenType != "org" && multiOrg && org != nil {
		envs = append(envs, domain.ConnectEnvVar{
			Name: "DAGGER_ORG", Value: org.Name, Required: true,
			Description: "Active organization (sent as X-Dagger-Org by the CLI).",
		})
	}

	if version != "" {
		v, err := s.versionResolver.ResolveMinimal(version)
		if err != nil {
			return nil, fmt.Errorf("%w: parse version: %v", domain.ErrValidation, err)
		}
		if !s.versionResolver.IsAllowed(v) {
			return nil, fmt.Errorf("%w: version %s not allowed (floor %s)", domain.ErrValidation, v, s.versionResolver.Floor())
		}
		snap.SelectedVersion = v.String()
		envs = append(envs, domain.ConnectEnvVar{
			Name: "_EXPERIMENTAL_DAGGER_TAG", Value: v.String(), Required: false,
			Description: "Pins the engine version (recommended for cache locality).",
		})
	}

	snap.EnvVars = envs
	return snap, nil
}

func (s *ConnectService) orgTokenMeta(ctx context.Context, orgID string) domain.ConnectTokenMeta {
	if s.orgs == nil {
		return domain.ConnectTokenMeta{}
	}
	t, err := s.orgs.TokenMeta(ctx, orgID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ConnectTokenMeta{Exists: false}
		}
		return domain.ConnectTokenMeta{}
	}
	recoverable := t.TokenCiphertext != "" && s.tokens.encKeyAvailable()
	return domain.ConnectTokenMeta{Exists: true, Prefix: t.Prefix, Recoverable: recoverable}
}

func (s *ConnectService) tokenMeta(ctx context.Context, userID string) domain.ConnectTokenMeta {
	if userID == "" {
		return domain.ConnectTokenMeta{}
	}
	t, err := s.tokens.Meta(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ConnectTokenMeta{Exists: false}
		}
		s.logger.WithError(err).Warn("connect: token meta unavailable")
		return domain.ConnectTokenMeta{}
	}
	recoverable := t.TokenCiphertext != "" && s.tokens.encKeyAvailable()
	return domain.ConnectTokenMeta{Exists: true, Prefix: t.Prefix, Recoverable: recoverable}
}

func (s *ConnectService) allowedVersions() []string {
	releases := s.versionResolver.AllReleases()
	out := make([]string, 0, len(releases))
	for _, v := range releases {
		out = append(out, v.String())
	}
	return out
}
