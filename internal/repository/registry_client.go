package repository

import (
	"bytes"
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
	"strconv"
	"strings"
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

var _ domain.CLIRegistryClient = (*RegistryStatsClient)(nil)

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

// RegistryStatsClient is a minimal OCI Distribution v2 client used to probe
// the shared cache registry (catalog, tags, manifests, delete) over stdlib
// net/http. It talks to the *internal* registry address, never the public
// cache vhost.
type RegistryStatsClient struct {
	host       string // e.g. "localhost:5000" (cache.internal_addr) or derived from cache.registry
	username   string
	password   string
	httpClient *http.Client
}

var _ domain.RegistryClient = (*RegistryStatsClient)(nil)

func NewRegistryStatsClient(host string) *RegistryStatsClient {
	return &RegistryStatsClient{
		host: host,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// NewRegistryStatsClientWithAuth returns a client that sends Basic auth on
// every request (per-backend registry credentials).
func NewRegistryStatsClientWithAuth(host, username, password string) *RegistryStatsClient {
	c := NewRegistryStatsClient(host)
	c.username = username
	c.password = password
	return c
}

// Host returns the registry host the client talks to.
func (c *RegistryStatsClient) Host() string {
	return c.host
}

// baseURL returns the scheme-prefixed registry host root.
func (c *RegistryStatsClient) baseURL() string {
	return fmt.Sprintf("http://%s", c.host)
}

// do performs a request and maps transport errors to ErrRegistryUnreachable.
func (c *RegistryStatsClient) do(ctx context.Context, method, rawURL, accept string) (*http.Response, error) {
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
func (c *RegistryStatsClient) Ping(ctx context.Context) error {
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

// ProbeManifest performs a HEAD request for repo:ref. It reports
// (true, nil) when the manifest exists (200), (false, nil) when it is
// definitively absent (404, or 405 which some registries return for HEAD),
// and (false, ErrRegistryUnreachable) for transport errors or any other
// non-2xx status (401/403/5xx) so the router marks the backend down.
func (c *RegistryStatsClient) ProbeManifest(ctx context.Context, repo, ref string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(ref)), manifestAccept)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return false, nil
	}
	return false, fmt.Errorf("%w: probe status %d", ErrRegistryUnreachable, resp.StatusCode)
}

// ProbeBlob performs a HEAD request for repo's blob digest. It reports
// (true, nil) when the blob exists (200), (false, nil) when it is
// definitively absent (404, or 405 which some registries return for HEAD),
// and (false, ErrRegistryUnreachable) for transport errors or any other
// non-2xx status (401/403/5xx) so the router marks the backend down.
func (c *RegistryStatsClient) ProbeBlob(ctx context.Context, repo, digest string) (bool, error) {
	if !validDigest(digest) {
		return false, fmt.Errorf("invalid digest: must be sha256:<hex>")
	}
	resp, err := c.do(ctx, http.MethodHead, fmt.Sprintf("%s/v2/%s/blobs/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(digest)), "")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return false, nil
	}
	return false, fmt.Errorf("%w: probe status %d", ErrRegistryUnreachable, resp.StatusCode)
}

// Catalog returns the list of repositories. Returns ErrRegistryCatalogDisabled
// on 404/403, ErrRegistryUnreachable on transport error.
func (c *RegistryStatsClient) Catalog(ctx context.Context) ([]string, error) {
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
func (c *RegistryStatsClient) Tags(ctx context.Context, repo string) ([]string, error) {
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
func (c *RegistryStatsClient) getManifest(ctx context.Context, repo, tag string) (*manifest, string, error) {
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
// those sizes are absent it falls back to HEAD blob Content-Length and, if
// that also fails, returns -1. Returns ErrManifestNotFound on 404.
func (c *RegistryStatsClient) ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error) {
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

	// Some registries omit descriptor sizes; fall back to blob Content-Length.
	if size == 0 && len(m.Layers) > 0 {
		size = 0
		for _, l := range m.Layers {
			bs, err := c.BlobSize(ctx, repo, l.Digest)
			if err != nil {
				return digest, -1, layers, nil
			}
			size += bs
		}
	}

	return digest, size, layers, nil
}

// BlobSize returns a blob's size via HEAD /v2/<repo>/blobs/<digest>
// Content-Length. Returns 0 + error when the registry does not expose it.
func (c *RegistryStatsClient) BlobSize(ctx context.Context, repo, digest string) (int64, error) {
	if !validDigest(digest) {
		return 0, fmt.Errorf("invalid digest: must be sha256:<hex>")
	}
	resp, err := c.do(ctx, http.MethodHead, fmt.Sprintf("%s/v2/%s/blobs/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(digest)), "")
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("blob head status %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("blob head missing content-length")
}

// DeleteManifest deletes a manifest by digest. Returns
// domain.ErrRegistryDeleteDisabled on 405/403.
func (c *RegistryStatsClient) DeleteManifest(ctx context.Context, repo, digest string) error {
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

// createdAnnotation is the OCI annotation carrying a manifest's creation time.
const createdAnnotation = "org.opencontainers.image.created"

// ManifestCreated fetches the manifest for repo:tag and returns its creation
// time from the OCI created annotation. It returns a zero time (no error) when
// the annotation is absent, and ErrManifestNotFound on 404.
func (c *RegistryStatsClient) ManifestCreated(ctx context.Context, repo, tag string) (time.Time, error) {
	m, _, err := c.getManifest(ctx, repo, tag)
	if err != nil {
		return time.Time{}, err
	}
	if m.Annotations == nil {
		return time.Time{}, nil
	}
	raw, ok := m.Annotations[createdAnnotation]
	if !ok || raw == "" {
		return time.Time{}, nil
	}
	created, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, nil
	}
	return created, nil
}

// UploadBlob performs a monolithic blob upload to repo, returning the digest
// and byte count. Uses the OCI Distribution v2 blob upload flow:
// POST /v2/<repo>/blobs/uploads/ → PUT <location>?digest=sha256:<hex>.
func (c *RegistryStatsClient) UploadBlob(ctx context.Context, repo string, body io.Reader) (digest string, size int64, err error) {
	// Read the entire body into memory so we can compute sha256 and send it
	// as a monolithic upload (single PUT with digest query param).
	buf := new(bytes.Buffer)
	size, err = io.Copy(buf, body)
	if err != nil {
		return "", 0, fmt.Errorf("read body: %w", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	digest = fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))

	// Initiate a monolithic blob upload.
	resp, err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/v2/%s/blobs/uploads/", c.baseURL(), url.PathEscape(repo)), "")
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)

	if resp.StatusCode != http.StatusAccepted {
		return "", 0, fmt.Errorf("%w: initiate upload status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location == "" {
		return "", 0, fmt.Errorf("upload init missing Location header")
	}

	// PUT the blob with the digest.
	sep := "?"
	if strings.Contains(location, "?") {
		sep = "&"
	}
	putURL := fmt.Sprintf("%s%sdigest=%s", location, sep, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, putURL, buf)
	if err != nil {
		return "", 0, fmt.Errorf("build put request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err = c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrRegistryUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("%w: upload status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	// Verify the returned digest matches what we computed.
	if dgst := resp.Header.Get("Docker-Content-Digest"); dgst != "" && validDigest(dgst) {
		digest = dgst
	}
	return digest, size, nil
}

// PutManifest pushes an OCI manifest to repo:tag.
func (c *RegistryStatsClient) PutManifest(ctx context.Context, repo, tag string, manifest *domain.CLIManifest) error {
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(tag)), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build put request: %w", err)
	}
	req.Header.Set("Content-Type", domain.MediaTypeOCIImageManifest)
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRegistryUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: put manifest status %d", ErrRegistryUnreachable, resp.StatusCode)
	}
	return nil
}

// GetManifest fetches and decodes a CLI manifest for repo:tag.
// Returns ErrManifestNotFound on 404.
func (c *RegistryStatsClient) GetManifest(ctx context.Context, repo, tag string) (*domain.CLIManifest, error) {
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(tag)), manifestAccept)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		discard(resp)
		return nil, fmt.Errorf("%w: %s:%s", ErrManifestNotFound, repo, tag)
	}
	if resp.StatusCode != http.StatusOK {
		discard(resp)
		return nil, fmt.Errorf("%w: status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	body, err := readBounded(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m domain.CLIManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// GetBlob streams a blob from repo:digest, returning the body and Content-Length.
func (c *RegistryStatsClient) GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error) {
	if !validDigest(digest) {
		return nil, 0, fmt.Errorf("invalid digest: must be sha256:<hex>")
	}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/blobs/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(digest)), "")
	if err != nil {
		return nil, 0, err
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: %s:%s", ErrManifestNotFound, repo, digest)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: get blob status %d", ErrRegistryUnreachable, resp.StatusCode)
	}

	var size int64 = -1
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			size = n
		}
	}
	return resp.Body, size, nil
}

// ManifestExists is a HEAD-based check for a manifest (no body download).
func (c *RegistryStatsClient) ManifestExists(ctx context.Context, repo, tag string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), url.PathEscape(repo), url.PathEscape(tag)), manifestAccept)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	discard(resp)
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return false, nil
	}
	return false, fmt.Errorf("%w: probe status %d", ErrRegistryUnreachable, resp.StatusCode)
}
