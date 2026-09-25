package repository

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

// escapeRepository validates and escapes an OCI repository name for a
// Distribution v2 request path. The name is split on "/" and each segment is
// escaped independently so the separators survive: escaping the whole string
// would turn "library/alpine" into "library%2Falpine", which registries treat
// as a single (nonexistent) repository and answer with 404. Empty, "." and
// ".." segments are rejected so a hostile repository cannot traverse out of
// /v2/ (CWE-22/CWE-918).
func escapeRepository(repo string) (string, error) {
	segments := strings.Split(repo, "/")
	for i, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid repository %q: empty or traversal segment", repo)
		}
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/"), nil
}

// escapeTag validates and escapes an image tag as a single path segment. A tag
// (unlike a repository) must not contain "/" and must not be a "."/".."
// traversal value; everything else is percent-escaped so query/fragment
// metacharacters cannot alter the request (CWE-22/CWE-918).
func escapeTag(tag string) (string, error) {
	if tag == "" || tag == "." || tag == ".." || strings.Contains(tag, "/") {
		return "", fmt.Errorf("invalid tag %q", tag)
	}
	return url.PathEscape(tag), nil
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
	scheme     string         // "http" (default) | "https"
	caPool     *x509.CertPool // nil = system trust pool
	httpClient *http.Client
}

var _ domain.DistributionClient = (*DistributionClient)(nil)

func NewDistributionClient(host string) *DistributionClient {
	return &DistributionClient{
		host:   host,
		scheme: "http",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			// A compromised or poisoned mirror must not be able to pivot the
			// supervisor to another host with a 3xx (CWE-918/CWE-601): the
			// distribution client only ever talks to the configured mirror.
			// Zot serves catalog/tags/manifests inline; blob redirects (S3
			// presigned URLs) are never fetched here.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
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

// NewDistributionClientForMirror returns a client for a configured image-cache
// mirror. When m.TLS is true the client dials https:// and, when roots is
// non-nil, verifies the mirror against roots; roots == nil uses the system
// trust pool. Plaintext mirrors (m.TLS == false) keep today's http:// behavior.
//
//nolint:gocritic // hugeParam: value param preserved for API stability
func NewDistributionClientForMirror(m domain.ImageCacheMirror, roots *x509.CertPool) domain.DistributionClient {
	c := NewDistributionClient(m.InternalAddr)
	if !m.TLS {
		return c
	}
	c.scheme = "https"
	c.caPool = roots
	c.httpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: c.caPool, MinVersion: tls.VersionTLS12},
	}
	return c
}

// LoadCertPool returns a CertPool seeded from the PEM CA bundle at path. An
// empty path returns (nil, nil). A read/parse failure returns an error so
// wiring can fail fast at startup.
func LoadCertPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path) //nolint:gosec // G304: path is the admin-configured image_cache.tls_ca_path, not user input.
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in CA bundle %q", path)
	}
	return pool, nil
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
	return fmt.Sprintf("%s://%s", c.scheme, c.host)
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
	repoPath, err := escapeRepository(repo)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/tags/list", c.baseURL(), repoPath), "")
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

// manifest is the subset of the OCI/Docker manifest needed to sum sizes and to
// resolve an image index to a representative child platform manifest.
type manifest struct {
	MediaType   string            `json:"mediaType"`
	Config      *descriptor       `json:"config"`
	Layers      []descriptor      `json:"layers"`
	Manifests   []descriptor      `json:"manifests"`
	Annotations map[string]string `json:"annotations"`
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    platform          `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"

// OCI image index / Docker manifest list media types, and the BuildKit
// attestation annotation used to skip SBOM/provenance child descriptors.
const (
	ociIndexMediaType           = "application/vnd.oci.image.index.v1+json"
	dockerManifestListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	annotationReferenceType     = "vnd.docker.reference.type"
	referenceTypeAttestation    = "attestation-manifest"

	// maxIndexDepth bounds how many nested index levels ManifestSize resolves
	// before reporting unknown. Docker Hub tags are a single index level;
	// the cap guards against pathological index-in-index chains.
	maxIndexDepth = 2
)

// isIndexManifest reports whether m is an image index / manifest list. It
// matches the two index media types and falls back to the structural signal
// (no config but children present) for registries that omit mediaType.
func isIndexManifest(m *manifest) bool {
	switch m.MediaType {
	case ociIndexMediaType, dockerManifestListMediaType:
		return true
	}
	return m.Config == nil && len(m.Manifests) > 0
}

// isAttestation reports whether a child descriptor points at an attestation
// manifest (BuildKit SBOM/provenance), which has no runnable config/layers and
// must not be chosen as the representative child.
func isAttestation(d *descriptor) bool {
	return d.Annotations[annotationReferenceType] == referenceTypeAttestation
}

// selectChildManifest picks a representative child descriptor to size. It
// prefers linux/amd64, skips attestation and "unknown"-platform entries, and
// falls back to the first usable non-attestation descriptor. ok is false when
// no usable child exists (empty index, or only attestation/unknown entries).
func selectChildManifest(m *manifest) (descriptor, bool) {
	var fallback descriptor
	haveFallback := false
	for _, d := range m.Manifests {
		if isAttestation(&d) {
			continue
		}
		if d.Platform.Architecture == "unknown" {
			continue
		}
		if !haveFallback {
			fallback = d
			haveFallback = true
		}
		if d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
			return d, true
		}
	}
	if haveFallback {
		return fallback, true
	}
	return descriptor{}, false
}

// directManifestSize sums a non-index manifest's config+layers. The -1
// "unknown" sentinel is set when the manifest has layers but their sizes are
// absent (the registry omitted descriptor sizes) — the existing behavior.
func directManifestSize(m *manifest) (size, layers int64) {
	layers = int64(len(m.Layers))
	for _, l := range m.Layers {
		size += l.Size
	}
	if m.Config != nil {
		size += m.Config.Size
	}
	if size == 0 && len(m.Layers) > 0 {
		size = -1
	}
	return size, layers
}

// getManifest fetches repo:tag's manifest, mapping 404 to ErrManifestNotFound
// and other non-2xx to ErrRegistryUnreachable. It returns the decoded manifest
// plus its digest (from Docker-Content-Digest, or computed from the body).
func (c *DistributionClient) getManifest(ctx context.Context, repo, tag string) (*manifest, string, error) {
	return c.getManifestRef(ctx, repo, tag, false)
}

// getManifestByDigest fetches repo's manifest by sha256 digest (used to resolve
// an image index to a representative child). The digest is validated before it
// is interpolated into the request path.
func (c *DistributionClient) getManifestByDigest(ctx context.Context, repo, digest string) (*manifest, string, error) {
	return c.getManifestRef(ctx, repo, digest, true)
}

// getManifestRef is the shared manifest GET. When refIsDigest is true, ref is
// validated with validDigest and path-escaped; otherwise it is escaped as a
// tag. Everything else (Accept header, status mapping, digest header
// validation with body-hash fallback) is common to both.
func (c *DistributionClient) getManifestRef(ctx context.Context, repo, ref string, refIsDigest bool) (*manifest, string, error) {
	repoPath, err := escapeRepository(repo)
	if err != nil {
		return nil, "", err
	}
	var refPath string
	if refIsDigest {
		if !validDigest(ref) {
			return nil, "", fmt.Errorf("invalid digest: must be sha256:<hex>")
		}
		refPath = url.PathEscape(ref)
	} else {
		refPath, err = escapeTag(ref)
		if err != nil {
			return nil, "", err
		}
	}
	resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), repoPath, refPath), manifestAccept)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		discard(resp)
		return nil, "", fmt.Errorf("%w: %s:%s", ErrManifestNotFound, repo, ref)
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
// layerCount). digest is always the TOP-LEVEL manifest digest (the index digest
// for a multi-arch tag), which the prune path uses to unlink the tag. For an
// image index / manifest list, sizeBytes/layerCount are resolved from a
// representative child platform manifest (linux/amd64 preferred, attestation
// and "unknown"-platform entries skipped); when no child resolves (empty index,
// only attestation entries, child fetch/decode failure, or nesting past
// maxIndexDepth) both are -1 (unknown). A child-resolution failure is NOT an
// error — the tag stays in the listing. Returns ErrManifestNotFound on 404 of
// the top-level manifest.
func (c *DistributionClient) ManifestSize(ctx context.Context, repo, tag string) (digest string, size, layers int64, err error) {
	m, digest, err := c.getManifest(ctx, repo, tag)
	if err != nil {
		return "", 0, 0, err
	}
	size, layers = c.manifestSize(ctx, repo, m, 0)
	return digest, size, layers, nil
}

// manifestSize resolves m's (size, layers). Index manifests are resolved to a
// representative child (descending through at most maxIndexDepth nested indexes,
// then -1/-1); direct manifests are summed in place.
func (c *DistributionClient) manifestSize(ctx context.Context, repo string, m *manifest, depth int) (size, layers int64) {
	if !isIndexManifest(m) {
		return directManifestSize(m)
	}
	if depth >= maxIndexDepth {
		return -1, -1
	}
	child, ok := selectChildManifest(m)
	if !ok {
		return -1, -1
	}
	cm, _, err := c.getManifestByDigest(ctx, repo, child.Digest)
	if err != nil {
		return -1, -1
	}
	return c.manifestSize(ctx, repo, cm, depth+1)
}

// DeleteManifest deletes a manifest by digest. Returns
// domain.ErrRegistryDeleteDisabled on 405/403.
func (c *DistributionClient) DeleteManifest(ctx context.Context, repo, digest string) error {
	if !validDigest(digest) {
		return fmt.Errorf("invalid digest: must be sha256:<hex>")
	}
	repoPath, err := escapeRepository(repo)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL(), repoPath, url.PathEscape(digest)), "")
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
