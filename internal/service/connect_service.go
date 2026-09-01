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
	cache           *Cache
	versionResolver domain.VersionResolver
	tokens          *TokenService
	logger          *logrus.Logger
}

// NewConnectService returns a ConnectService.
func NewConnectService(
	cfg *domain.Config,
	cache *Cache,
	vr domain.VersionResolver,
	tokens *TokenService,
	logger *logrus.Logger,
) *ConnectService {
	return &ConnectService{cfg: cfg, cache: cache, versionResolver: vr, tokens: tokens, logger: logger}
}

// ConnectEnv builds the snapshot. When reveal=true and the token is
// recoverable, the DAGGER_CLOUD_TOKEN value is populated with the plaintext.
func (s *ConnectService) ConnectEnv(ctx context.Context, userID, version string, reveal bool) (*domain.ConnectEnvSnapshot, error) {
	snap := &domain.ConnectEnvSnapshot{
		ServerURL:    s.cfg.Server.PublicURL,
		DataHostname: s.cfg.Server.DataHost,
		CacheBackend: s.cfg.Cache.Backend,
		VersionFloor: s.cfg.Version.Floor,
		Token:        s.tokenMeta(ctx, userID),
	}
	snap.AllowedVersions = s.allowedVersions()

	tokenValue := ""
	if reveal && snap.Token.Recoverable {
		pt, err := s.tokens.Reveal(ctx, userID)
		if err != nil {
			s.logger.WithError(err).Warn("connect: token reveal unavailable")
		} else {
			tokenValue = pt
		}
	}

	envs := []domain.ConnectEnvVar{
		{Name: "DAGGER_CLOUD_URL", Value: s.cfg.Server.PublicURL, Required: true, Description: "Control-plane URL the Dagger CLI talks to (replaces Dagger Cloud)."},
		{Name: "DAGGER_CLOUD_TOKEN", Value: tokenValue, Required: true, Secret: true, Description: "Your per-user API token (dct_...)."},
		{Name: "_EXPERIMENTAL_DAGGER_RUNNER_HOST", Value: "dagger-cloud://self", Required: true, Description: "Tells the CLI to provision a remote engine via the cloud driver."},
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

	if cc := s.cache.BuildCacheConfig("max"); cc != "" {
		envs = append(envs, domain.ConnectEnvVar{
			Name: "_EXPERIMENTAL_DAGGER_CACHE_CONFIG", Value: cc, Required: false,
			Description: "Remote shared cache (MagicCache) ref — one global cache shared across all engine versions.",
		})
	}

	snap.EnvVars = envs
	return snap, nil
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
