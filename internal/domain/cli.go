package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// CLICache is a sha256-verified, atomic filesystem cache for tarballs.
type CLICache interface {
	// Get returns the cached tarball path, re-verifying its sha256 sidecar.
	Get(version, osName, arch string) (path string, ok bool)
	// Put streams r to a temp file, verifies sha256Hex, atomically renames.
	Put(version, osName, arch string, r io.Reader, sha256Hex string) (path string, err error)
	// Dir returns the cache root directory.
	Dir() string
}

// AssetFilename returns the upstream asset filename for a CLI tarball.
func AssetFilename(version, osName, arch string) string {
	return fmt.Sprintf("dagger_%s_%s_%s.tar.gz", version, osName, arch)
}
