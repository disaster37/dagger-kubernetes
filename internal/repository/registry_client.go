package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

// Sentinel errors for the OCI Distribution v2 client. Callers use
// errors.Is to branch on registry behaviour (catalog disabled, delete
// disabled, etc.). ErrRegistryCatalogDisabled and ErrManifestNotFound alias
// the domain sentinels (the canonical definitions) so errors.Is still
// matches when the service layer branches on the domain values.
var (
	ErrRegistryUnreachable     = errors.New("registry unreachable")
	ErrRegistryCatalogDisabled = domain.ErrRegistryCatalogDisabled
	ErrManifestNotFound        = domain.ErrManifestNotFound
)

// maxRegistryBody caps the size of a registry response body the client will
// decode (manifests, catalog, tags). A compromised or misbehaving registry
// could otherwise stream an arbitrarily large body and exhaust supervisor
// memory (CWE-400/CWE-770). 16 MiB is far above any legitimate OCI manifest
// or catalog payload while keeping allocations bounded.
const maxRegistryBody = 16 << 20

// digestRe constrains manifest/blob digest values before they are placed
// into registry request paths. Digests originate from registry responses
// (Docker-Content-Digest header or descriptor JSON) which an attacker who
// controls the registry could craft; url.PathEscape already neutralises
// path/query injection, but constraining the shape to sha256:<hex> is
// defense-in-depth (CWE-20/CWE-918) so a malicious digest can never reach
// a DELETE path.
var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// validDigest reports whether a digest has the sha256:<hex> shape required
// before it is interpolated into a registry URL path.
func validDigest(d string) bool {
	return digestRe.MatchString(d)
}

// readBounded reads at most maxRegistryBody+1 bytes from r and returns an
// error when the body exceeds maxRegistryBody, so a compromised registry
// cannot exhaust memory with an oversized response (CWE-400/CWE-770).
func readBounded(r io.Reader) ([]byte, error) {
	lr := io.LimitReader(r, maxRegistryBody+1)
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxRegistryBody {
		return nil, fmt.Errorf("%w: response body exceeds %d bytes", ErrRegistryUnreachable, maxRegistryBody)
	}
	return b, nil
}

// DistributionClient is a minimal OCI Distribution v2 client used to list and
// prune the local image mirrors (Zot serves this API) over stdlib net/http. It
// talks to the *internal* mirror address, never a public vhost.
type DistributionClient struct {
	host       string // e.g. "dagger-docker-io-mirror.dagger.svc:5000"
	username   string
	password   string
	httpClient *http.Client
}

var _ domain.DistributionClient = (*DistributionClient)(nil)

func NewDistributionClient(host string) *DistributionClient {
	return &DistributionClient{
		host: host,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NewDistributionClientWithAuth returns a client that sends Basic auth on
// every request.
func NewDistributionClientWithAuth(host, username, password string) *DistributionClient {
	c := NewDistributionClient(host)
	c.username = username
	c.password = password
	return c
}

// WithTimeout returns the client with the given total per-request timeout,
// overriding the 10s default. http.Client.Timeout covers connection,
// redirects, and reading the response body.
func (c *DistributionClient) WithTimeout(d time.Duration) *DistributionClient {
	c.httpClient.Timeout = d
	return c
}

// Host returns the registry host the client talks to.
func (c *DistributionClient) Host() string {
	return c.host
}

// baseURL returns the scheme-prefixed registry host root.
func (c *DistributionClient) baseURL() string {
	return fmt.Sprintf("http://%s", c.host)
}

// do performs a request and maps transport errors to ErrRegistryUnreachable.
func (c *DistributionClient) do(ctx context.Context, method, rawURL, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistryUnreachable, err)
	}
	return resp, nil
}

// discard drains a response body so its connection can be reused.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
}

// Ping probes registry reachability (GET /v2/). Returns nil if reachable.
func (c *DistributionClient) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/", c.baseURL()), "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}
	return nil
}

// Catalog returns the list of repositories. Returns ErrRegistryCatalogDisabled
// on 404/403, ErrRegistryUnreachable on transport error.
func (c *DistributionClient) Catalog(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/_catalog", c.baseURL()), "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		discard(resp)
		return nil, fmt.Errorf("%w: status %d", ErrRegistryCatalogDisabled, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp)
		return nil, fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	var body struct {
		Repositories []string `json:"repositories"`
	}
	raw, err := readBounded(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	return body.Repositories, nil
}

// Tags returns the tags for a repository.
func (c *DistributionClient) Tags(ctx context.Context, repo string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/tags/list", c.baseURL(), url.PathEscape(repo)), "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		discard(resp)
		return nil, fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	raw, err := readBounded(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tags: %w", err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode tags: %w", err)
	}
	return body.Tags, nil
}

// manifest is the subset of the OCI/Docker manifest needed to sum sizes.
type manifest struct {
	Config      *descriptor       `json:"config"`
	Layers      []descriptor      `json:"layers"`
	Annotations map[string]string `json:"annotations"`
}

type descriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"

// getManifest fetches repo:tag's manifest, mapping 404 to ErrManifestNotFound
// and other non-2xx to ErrRegistryUnreachable. It returns the decoded manifest
// plus its digest (from Docker-Content-Digest, or computed from the body).
func (c *DistributionClient) getManifest(ctx context.Context, repo, tag string) (*manifest, string, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(tag)), manifestAccept)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		discard(resp)
		return nil, "", fmt.Errorf("%w: %s:%s", ErrManifestNotFound, repo, tag)
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp)
		return nil, "", fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest: %w", err)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	// Only trust a registry-supplied digest when it has the expected
	// sha256:<hex> shape; otherwise compute it from the body. This prevents
	// a compromised registry from injecting an arbitrary value into the
	// digest that is later placed in a DELETE path (CWE-20/CWE-918).
	if digest != "" && !validDigest(digest) {
		digest = ""
	}
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("decode manifest: %w", err)
	}
	return &m, digest, nil
}

// ManifestSize fetches the manifest for repo:tag and returns (digest, sizeBytes,
// layerCount). sizeBytes is the sum of layer + config descriptor sizes; when
// those sizes are absent the listing reports -1 (unknown) rather than issuing a
// HEAD per blob. Returns ErrManifestNotFound on 404.
func (c *DistributionClient) ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error) {
	m, digest, err := c.getManifest(ctx, repo, tag)
	if err != nil {
		return "", 0, 0, err
	}

	layers = int64(len(m.Layers))
	for _, l := range m.Layers {
		size += l.Size
	}
	if m.Config != nil {
		size += m.Config.Size
	}

	// Some registries omit descriptor sizes; report unknown rather than
	// falling back to a HEAD per blob.
	if size == 0 && len(m.Layers) > 0 {
		size = -1
	}

	return digest, size, layers, nil
}

// DeleteManifest deletes a manifest by digest. Returns
// domain.ErrRegistryDeleteDisabled on 405/403.
func (c *DistributionClient) DeleteManifest(ctx context.Context, repo, digest string) error {
	if !validDigest(digest) {
		return fmt.Errorf("invalid digest: must be sha256:<hex>")
	}
	resp, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(digest)), "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)

	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: status %d", domain.ErrRegistryDeleteDisabled, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}
	return nil
}
