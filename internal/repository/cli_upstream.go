package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// GitHubCLIUpstream is the Dagger GitHub-Releases-backed CLI upstream
// (release discovery + verified tarball/checksums downloads). Outbound HTTP
// uses the stdlib client (ADR-007).
type GitHubCLIUpstream struct {
	client       *http.Client
	releasesURL  string
	downloadBase string
	token        string
}

// GitHubCLIUpstreamConfig is the repository-side constructor config (mapped from
// domain.CLIUpstreamConfig in cmd/api).
type GitHubCLIUpstreamConfig struct {
	ReleasesURL  string
	DownloadBase string
	GitHubToken  string
	Timeout      time.Duration
}

// NewGitHubCLIUpstream builds a GitHubCLIUpstream. A non-positive Timeout
// falls back to 5 minutes (the config default).
func NewGitHubCLIUpstream(cfg GitHubCLIUpstreamConfig) *GitHubCLIUpstream {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &GitHubCLIUpstream{
		client:       &http.Client{Timeout: timeout},
		releasesURL:  cfg.ReleasesURL,
		downloadBase: cfg.DownloadBase,
		token:        cfg.GitHubToken,
	}
}

// maxReleasePages caps the number of GitHub release-list pages fetched. At 100
// per page this is 2000 tags — far above Dagger's real release count — but it
// bounds the loop against a misbehaving mirror that returns a self-referential
// or endless rel="next" Link header (CWE-400).
const maxReleasePages = 20

// List returns the upstream release tag names, following the pagination Link
// header until exhausted (bounded by maxReleasePages).
func (u *GitHubCLIUpstream) List(ctx context.Context) ([]string, error) {
	tags := make([]string, 0)
	next := fmt.Sprintf("%s?per_page=100&page=1", u.releasesURL)
	for page := 0; next != ""; page++ {
		if page >= maxReleasePages {
			return nil, fmt.Errorf("%w: release list exceeds %d pages", domain.ErrCLIUpstreamUnavailable, maxReleasePages)
		}
		resp, err := u.doRequest(ctx, next, true)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: read releases body: %v", domain.ErrCLIUpstreamUnavailable, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%w: releases api status %d", domain.ErrCLIUpstreamUnavailable, resp.StatusCode)
		}

		var releases []struct {
			TagName string `json:"tag_name"`
		}
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, fmt.Errorf("%w: decode releases: %v", domain.ErrCLIUpstreamUnavailable, err)
		}
		for _, r := range releases {
			if r.TagName != "" {
				tags = append(tags, r.TagName)
			}
		}
		next = u.nextLink(resp.Header.Get("Link"))
	}
	return tags, nil
}

// FetchChecksums fetches and parses <version>/checksums.txt into a
// filename -> sha256-hex map.
func (u *GitHubCLIUpstream) FetchChecksums(ctx context.Context, version string) (map[string]string, error) {
	reqURL := fmt.Sprintf("%s/%s/checksums.txt", u.downloadBase, version)
	resp, err := u.doRequest(ctx, reqURL, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, domain.ErrCLINotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: checksums status %d", domain.ErrCLIUpstreamUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read checksums: %v", domain.ErrCLIUpstreamUnavailable, err)
	}
	return parseChecksums(string(body)), nil
}

// FetchTarball streams the tarball for one os/arch. A 404 maps to
// domain.ErrCLINotFound; the returned length is -1 when unknown.
func (u *GitHubCLIUpstream) FetchTarball(ctx context.Context, version, osName, arch string) (io.ReadCloser, int64, error) {
	filename := domain.AssetFilename(version, osName, arch)
	reqURL := fmt.Sprintf("%s/%s/%s", u.downloadBase, version, filename)
	resp, err := u.doRequest(ctx, reqURL, false)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, domain.ErrCLINotFound
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: tarball status %d", domain.ErrCLIUpstreamUnavailable, resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

// doRequest builds and executes a GET, returning the response on success or a
// wrapped ErrCLIUpstreamUnavailable on network/request errors. withAuth
// controls whether the optional GitHub token is attached: it is only sent to
// the releases API host (the token exists to raise the API rate limit), never
// to the (potentially third-party/mirrored) download host, so a compromised or
// misconfigured download_base cannot capture the credential (CWE-522).
func (u *GitHubCLIUpstream) doRequest(ctx context.Context, target string, withAuth bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", domain.ErrCLIUpstreamUnavailable, err)
	}
	u.addHeaders(req, withAuth)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrCLIUpstreamUnavailable, err)
	}
	return resp, nil
}

// addHeaders sets the GitHub API Accept header and, when withAuth is true and a
// token is configured, the Authorization bearer header. Never logged by callers.
func (u *GitHubCLIUpstream) addHeaders(req *http.Request, withAuth bool) {
	req.Header.Set("Accept", "application/vnd.github+json")
	if withAuth && u.token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", u.token))
	}
}

// nextLink returns the rel="next" pagination URL from a Link header, but only
// when it is an absolute http(s) URL on the same host as the configured
// releases_url. This keeps the release-list fetch from being steered toward an
// arbitrary internal address by a compromised/misconfigured mirror (CWE-918).
func (u *GitHubCLIUpstream) nextLink(link string) string {
	next := parseNextLink(link)
	if next == "" {
		return ""
	}
	nu, err := url.Parse(next)
	if err != nil || nu.Host == "" || (nu.Scheme != "http" && nu.Scheme != "https") {
		return ""
	}
	base, err := url.Parse(u.releasesURL)
	if err != nil || base.Host == "" || !strings.EqualFold(base.Host, nu.Host) {
		return ""
	}
	return next
}

// parseNextLink extracts the URL of the rel="next" entry from a GitHub
// pagination Link header, or "" when absent/unparseable.
func parseNextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		rawURL := strings.TrimSpace(segments[0])
		if len(rawURL) < 2 || rawURL[0] != '<' || rawURL[len(rawURL)-1] != '>' {
			continue
		}
		urlPart := rawURL[1 : len(rawURL)-1]
		for _, param := range segments[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				return urlPart
			}
		}
	}
	return ""
}

// parseChecksums parses "sha256  filename" / "sha256 *filename" lines into a
// filename -> hex-digest map. Malformed lines are skipped.
func parseChecksums(content string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest, name := fields[0], strings.TrimPrefix(fields[1], "*")
		if name != "" && digest != "" {
			out[name] = digest
		}
	}
	return out
}
