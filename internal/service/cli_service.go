package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
)

// CLIService orchestrates on-the-fly Dagger CLI provisioning: allowlist-aware
// "latest" resolution, sha256-verified caching, streaming, and in-flight dedup.
type CLIService struct {
	resolver  domain.VersionResolver
	upstream  domain.CLIUpstream
	cache     domain.CLICache
	publicURL string
	logger    *logrus.Logger
	metrics   *observ.Metrics

	mu               sync.Mutex
	releases         []string
	releasesAt       time.Time
	releaseTTL       time.Duration
	releasesInflight *releaseInflight
	inflight         map[string]*cliInflight // key = version|os|arch
	meta             map[string]cliMeta      // key = version|os|arch
}

type cliInflight struct {
	done chan struct{}
	path string
	err  error
}

// cliMeta stores artifact metadata recorded after a successful Put so that
// EnsureCached can return a CLIArtifact without reading files from disk (the
// registry backend has no local path).
type cliMeta struct {
	sha256 string
	size   int64
}

// releaseInflight is the single-flight handle for the upstream release-list
// fetch, so a burst of concurrent callers on a cold cache triggers one
// upstream API call instead of one per request (mirrors the per-artifact
// inflight dedup).
type releaseInflight struct {
	done     chan struct{}
	releases []string
	err      error
}

// NewCLIService returns a CLIService.
func NewCLIService(
	resolver domain.VersionResolver,
	upstream domain.CLIUpstream,
	cache domain.CLICache,
	publicURL string,
	releaseTTL time.Duration,
	logger *logrus.Logger,
	metrics *observ.Metrics,
) *CLIService {
	return &CLIService{
		resolver: resolver,
		upstream: upstream,
		cache:    cache,
		// Trim a trailing slash so the emitted download URL never becomes
		// "...com//api/v1/cli/..." (server.public_url is validated as absolute
		// but a trailing slash is otherwise accepted).
		publicURL:  strings.TrimSuffix(publicURL, "/"),
		logger:     logger,
		metrics:    metrics,
		releaseTTL: releaseTTL,
		inflight:   make(map[string]*cliInflight),
		meta:       make(map[string]cliMeta),
	}
}

// ResolveLatest returns the highest allowed released version, ensuring it is
// cached. The release list is fetched through an in-memory TTL cache to absorb
// GitHub API rate limits.
func (s *CLIService) ResolveLatest(ctx context.Context, osName, arch string) (*domain.CLIArtifact, error) {
	releases, err := s.listReleases(ctx)
	if err != nil {
		return nil, err
	}

	var best *domain.Version
	for _, raw := range releases {
		v, err := domain.Parse(raw)
		if err != nil || !s.resolver.IsAllowed(v) {
			continue
		}
		if best == nil || v.Compare(best) > 0 {
			best = v
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: no released version satisfies floor %s and allowlist", domain.ErrCLINotFound, s.resolver.Floor())
	}
	return s.EnsureCached(ctx, best.String(), osName, arch)
}

// EnsureCached verifies the version is a full, allowed release and guarantees
// the tarball is in the cache. Concurrent requests for the same artifact share
// one upstream fetch.
func (s *CLIService) EnsureCached(ctx context.Context, version, osName, arch string) (*domain.CLIArtifact, error) {
	v, err := domain.Parse(version)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid version %q", domain.ErrCLIVersionNotAllowed, version)
	}
	if !domain.IsFullVersion(version) {
		return nil, fmt.Errorf("%w: version %q must be a full vX.Y.Z release", domain.ErrCLIVersionNotAllowed, version)
	}
	if !s.resolver.IsAllowed(v) {
		return nil, fmt.Errorf("%w: version %s not allowed (floor %s)", domain.ErrCLIVersionNotAllowed, v.String(), s.resolver.Floor())
	}
	// Normalize to the canonical "vX.Y.Z" form so the upstream asset filename
	// and download URL are consistent regardless of the input's "v" prefix
	// (e.g. a bare "0.21.8" still resolves the "dagger_v0.21.8_*" asset).
	version = v.String()

	// Lightweight existence check via Has (HEAD manifest) before doing a full
	// Get (which downloads the blob to a temp file).
	if ok, err := s.cache.Has(ctx, version, osName, arch); err != nil {
		s.incCache("error")
		return nil, fmt.Errorf("check cache: %w", err)
	} else if ok {
		s.incCache("hit")
		return s.artifactFromMeta(version, osName, arch), nil
	}

	key := fmt.Sprintf("%s|%s|%s", version, osName, arch)
	s.mu.Lock()
	// Double-check after acquiring the lock (Has may have returned true just
	// before we locked, and another goroutine may have already fetched).
	if path, ok := s.cache.Get(ctx, version, osName, arch); ok {
		s.mu.Unlock()
		s.incCache("hit")
		return s.artifact(version, osName, arch, path), nil
	}
	if inflight, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		<-inflight.done
		if inflight.err != nil {
			return nil, inflight.err
		}
		s.incCache("hit")
		return s.artifact(version, osName, arch, inflight.path), nil
	}
	inflight := &cliInflight{done: make(chan struct{})}
	s.inflight[key] = inflight
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		close(inflight.done)
		s.mu.Unlock()
	}()

	path, err := s.fetchAndCache(ctx, version, osName, arch)
	if err != nil {
		inflight.err = err
		s.incCache("error")
		return nil, err
	}
	inflight.path = path
	s.incCache("miss")
	// Use artifactFromMeta for registry-backed cache (path is ""), or artifact
	// for filesystem-backed cache (path is non-empty).
	if path == "" {
		return s.artifactFromMeta(version, osName, arch), nil
	}
	return s.artifact(version, osName, arch, path), nil
}

// Open ensures the artifact is cached and returns a seekable stream plus its
// byte length for the download endpoint.
func (s *CLIService) Open(ctx context.Context, version, osName, arch string) (io.ReadSeekCloser, int64, error) {
	if _, err := s.EnsureCached(ctx, version, osName, arch); err != nil {
		return nil, 0, err
	}
	path, ok := s.cache.Get(ctx, version, osName, arch)
	if !ok {
		return nil, 0, fmt.Errorf("%w: cached tarball disappeared", domain.ErrCLINotFound)
	}
	// #nosec G304 -- path is produced by the cache from validated version/os/arch within the cache dir.
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open cached tarball: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat cached tarball: %w", err)
	}
	return f, st.Size(), nil
}

// listReleases returns the TTL-cached upstream release list, using a
// single-flight so a burst of concurrent callers on a cold cache triggers one
// upstream API call instead of one per request.
func (s *CLIService) listReleases(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	if s.releases != nil && time.Since(s.releasesAt) < s.releaseTTL {
		releases := s.releases
		s.mu.Unlock()
		return releases, nil
	}
	if inf := s.releasesInflight; inf != nil {
		s.mu.Unlock()
		select {
		case <-inf.done:
			return inf.releases, inf.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	inf := &releaseInflight{done: make(chan struct{})}
	s.releasesInflight = inf
	s.mu.Unlock()

	releases, err := s.upstream.List(ctx)

	s.mu.Lock()
	if err == nil {
		s.releases = releases
		s.releasesAt = time.Now()
	}
	inf.releases, inf.err = releases, err
	close(inf.done)
	s.releasesInflight = nil
	s.mu.Unlock()
	return releases, err
}

// fetchAndCache fetches checksums + tarball and writes the verified tarball
// into the cache. The upstream metric is incremented once per overall fetch
// operation (success or error), not per individual HTTP call.
func (s *CLIService) fetchAndCache(ctx context.Context, version, osName, arch string) (string, error) {
	filename := domain.AssetFilename(version, osName, arch)

	sums, err := s.upstream.FetchChecksums(ctx, version)
	if err != nil {
		return "", s.failUpstream(err, version, osName, arch)
	}

	expected, ok := sums[filename]
	if !ok {
		return "", s.failUpstream(fmt.Errorf("%w: no checksum for %s", domain.ErrCLIUpstreamUnavailable, filename), version, osName, arch)
	}

	rc, size, err := s.upstream.FetchTarball(ctx, version, osName, arch)
	if err != nil {
		return "", s.failUpstream(err, version, osName, arch)
	}
	defer func() { _ = rc.Close() }()

	path, err := s.cache.Put(ctx, version, osName, arch, rc, expected)
	if err != nil {
		return "", s.failUpstream(err, version, osName, arch)
	}
	// Record metadata for artifactFromMeta (registry backend has no local path).
	s.recordMeta(version, osName, arch, expected, size)
	s.incUpstream("success")
	return path, nil
}

// failUpstream records an upstream error metric, logs it, and returns the
// original error so callers can use it in a return statement directly.
func (s *CLIService) failUpstream(err error, version, osName, arch string) error {
	s.incUpstream("error")
	if errors.Is(err, domain.ErrCLINotFound) {
		s.logger.WithFields(logrus.Fields{"version": version, "os": osName, "arch": arch}).Debug("cli version not found upstream")
	} else {
		s.logger.WithError(err).WithFields(logrus.Fields{"version": version, "os": osName, "arch": arch}).Error("cli upstream fetch failed")
	}
	return err
}

// artifact builds the CLIArtifact for a cached path, reading the sha256
// sidecar and file size best-effort (both degrade to ""/-1 on IO errors).
func (s *CLIService) artifact(version, osName, arch, path string) *domain.CLIArtifact {
	var size int64 = -1
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	sha := ""
	if sidecar, err := os.ReadFile(fmt.Sprintf("%s.sha256", path)); err == nil {
		sha = strings.TrimSpace(string(sidecar))
	}
	return &domain.CLIArtifact{
		Version:  version,
		OS:       osName,
		Arch:     arch,
		Filename: domain.AssetFilename(version, osName, arch),
		URL:      fmt.Sprintf("%s/api/v1/cli/%s?os=%s&arch=%s", s.publicURL, version, osName, arch),
		SHA256:   sha,
		Size:     size,
	}
}

// artifactFromMeta builds a CLIArtifact from in-memory metadata recorded after
// a successful Put. Used by EnsureCached when Has reports a hit but no local
// path exists (registry backend).
func (s *CLIService) artifactFromMeta(version, osName, arch string) *domain.CLIArtifact {
	key := fmt.Sprintf("%s|%s|%s", version, osName, arch)
	s.mu.Lock()
	m := s.meta[key]
	s.mu.Unlock()

	return &domain.CLIArtifact{
		Version:  version,
		OS:       osName,
		Arch:     arch,
		Filename: domain.AssetFilename(version, osName, arch),
		URL:      fmt.Sprintf("%s/api/v1/cli/%s?os=%s&arch=%s", s.publicURL, version, osName, arch),
		SHA256:   m.sha256,
		Size:     m.size,
	}
}

// recordMeta stores artifact metadata keyed by version|os|arch so that
// artifactFromMeta can return it without reading files from disk.
func (s *CLIService) recordMeta(version, osName, arch, sha256 string, size int64) {
	key := fmt.Sprintf("%s|%s|%s", version, osName, arch)
	s.mu.Lock()
	s.meta[key] = cliMeta{sha256: sha256, size: size}
	s.mu.Unlock()
}

func (s *CLIService) incCache(result string) {
	if s.metrics != nil {
		s.metrics.CLICacheTotal.WithLabelValues(result).Inc()
	}
}

func (s *CLIService) incUpstream(status string) {
	if s.metrics != nil {
		s.metrics.CLIUpstreamFetchTotal.WithLabelValues(status).Inc()
	}
}
