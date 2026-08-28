package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// OCI media type constants used by CLI cache manifests.
const (
	MediaTypeOCIImageManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCILayerGzip     = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaTypeOCIEmptyJSON     = "application/vnd.oci.empty.v1+json"
)

// Sentinel errors surfaced by the CLI addon. Live in domain so the handler can
// map them to HTTP statuses without importing repository/service.
var (
	ErrCLINotFound            = errors.New("dagger cli version not found")
	ErrCLIVersionNotAllowed   = errors.New("dagger cli version not allowed")
	ErrCLIChecksumMismatch    = errors.New("dagger cli checksum mismatch")
	ErrCLIUpstreamUnavailable = errors.New("dagger cli upstream unavailable")
)

// CLIArtifact describes one Dagger CLI tarball (cached or resolvable).
type CLIArtifact struct {
	Version  string `json:"version"`  // "v0.21.8"
	OS       string `json:"os"`       // "linux" | "darwin"
	Arch     string `json:"arch"`     // "amd64" | "arm64" | "armv7"
	Filename string `json:"filename"` // "dagger_v0.21.8_linux_amd64.tar.gz"
	URL      string `json:"url"`      // supervisor download URL (absolute)
	SHA256   string `json:"sha256"`   // hex digest of the tarball ("" = unverified)
	Size     int64  `json:"size"`     // bytes; -1 unknown
}

// CLIReleaseIndex lists upstream release version strings (e.g. ["v0.21.8", ...]).
type CLIReleaseIndex interface {
	List(ctx context.Context) ([]string, error)
}

// CLIUpstream fetches upstream release artifacts.
type CLIUpstream interface {
	CLIReleaseIndex
	// FetchChecksums returns filename -> sha256 hex from <version>/checksums.txt.
	FetchChecksums(ctx context.Context, version string) (map[string]string, error)
	// FetchTarball returns a stream of the tarball and its byte length.
	FetchTarball(ctx context.Context, version, osName, arch string) (io.ReadCloser, int64, error)
}

// CLICache stores verified CLI tarballs on the shared OCI registry so every
// supervisor pod in a multi-node Raft cluster can serve cached binaries.
type CLICache interface {
	// Has reports whether the artifact exists without a full download.
	Has(ctx context.Context, version, osName, arch string) (bool, error)
	// Get downloads the artifact from the registry to a temp file and returns
	// the local path. Returns ("", false) when not found.
	Get(ctx context.Context, version, osName, arch string) (path string, ok bool)
	// Put uploads the tarball to the registry after verifying sha256Hex.
	// Returns "" (no local path) on success.
	Put(ctx context.Context, version, osName, arch string, r io.Reader, sha256Hex string) (path string, err error)
	// Dir returns the cache root directory ("" for registry-backed cache).
	Dir() string
}

// CLIRegistryClient is the subset of the OCI registry client the CLI cache
// needs (blob/manifest push + pull). Implemented by repository.RegistryStatsClient.
type CLIRegistryClient interface {
	ManifestExists(ctx context.Context, repo, tag string) (bool, error)
	GetManifest(ctx context.Context, repo, tag string) (*CLIManifest, error)
	GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, int64, error)
	UploadBlob(ctx context.Context, repo string, body io.Reader) (digest string, size int64, err error)
	PutManifest(ctx context.Context, repo, tag string, manifest *CLIManifest) error
}

// CLIManifest is a minimal OCI manifest for a single-layer CLI tarball artifact.
type CLIManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        CLIManifestConfig  `json:"config"`
	Layers        []CLIManifestLayer `json:"layers"`
	Annotations   map[string]string  `json:"annotations,omitempty"`
}

// CLIManifestConfig is the config descriptor of a CLI manifest.
type CLIManifestConfig struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// CLIManifestLayer is a layer descriptor of a CLI manifest.
type CLIManifestLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// AssetFilename returns the upstream asset filename for a CLI tarball.
func AssetFilename(version, osName, arch string) string {
	return fmt.Sprintf("dagger_%s_%s_%s.tar.gz", version, osName, arch)
}
