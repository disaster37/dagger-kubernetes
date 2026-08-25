package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var (
	cliOSAllow   = map[string]bool{"linux": true, "darwin": true}
	cliArchAllow = map[string]bool{"amd64": true, "arm64": true, "armv7": true}
)

// cliGuard performs the common pre-checks for CLI endpoints: auth, enabled,
// and os/arch validation. Returns the parsed os/arch and whether to proceed.
func (s *Server) cliGuard(c *app.RequestContext) (osName, arch string, ok bool) {
	if !s.requireAuth(c) {
		return "", "", false
	}
	if s.cli == nil {
		writeError(c, consts.StatusNotFound, "cli provisioning disabled")
		return "", "", false
	}
	return s.parseCLIOSArch(c)
}

// handleCLILatest resolves the highest allowed released Dagger CLI version,
// ensures it is cached, and returns its artifact metadata.
func (s *Server) handleCLILatest(ctx context.Context, c *app.RequestContext) {
	osName, arch, ok := s.cliGuard(c)
	if !ok {
		return
	}
	art, err := s.cli.ResolveLatest(ctx, osName, arch)
	if err != nil {
		s.writeCLIError(c, err)
		return
	}
	writeJSON(c, art)
}

// handleCLIDownload streams the verified CLI tarball for a full, allowed
// version. Version validation (full vX.Y.Z, allowlist, floor) is delegated to
// CLIService.Open → EnsureCached, which returns ErrCLIVersionNotAllowed on
// invalid/disallowed versions.
func (s *Server) handleCLIDownload(ctx context.Context, c *app.RequestContext) {
	osName, arch, ok := s.cliGuard(c)
	if !ok {
		return
	}
	version := c.Param("version")

	rc, size, err := s.cli.Open(ctx, version, osName, arch)
	if err != nil {
		s.writeCLIError(c, err)
		return
	}

	c.Response.Header.Set("Content-Type", "application/gzip")
	c.Response.Header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", domain.AssetFilename(version, osName, arch)))
	c.SetStatusCode(consts.StatusOK)
	// Hertz closes the stream after reading it; do not close it here.
	c.SetBodyStream(rc, int(size))
}

// parseCLIOSArch validates the os/arch query params against the allowlist,
// defaulting to linux/amd64.
func (s *Server) parseCLIOSArch(c *app.RequestContext) (osName, arch string, ok bool) {
	osName = c.Query("os")
	if osName == "" {
		osName = "linux"
	}
	arch = c.Query("arch")
	if arch == "" {
		arch = "amd64"
	}
	if !cliOSAllow[osName] || !cliArchAllow[arch] {
		writeError(c, consts.StatusBadRequest, "invalid os or arch")
		return "", "", false
	}
	return osName, arch, true
}

// cliErrorStatus maps CLI sentinel errors to HTTP status codes and whether
// the error should be logged at ERROR level (vs DEBUG for expected not-found).
var cliErrorStatus = map[error]struct {
	status int
	log    bool
}{
	domain.ErrCLINotFound:            {consts.StatusNotFound, false},
	domain.ErrCLIVersionNotAllowed:   {consts.StatusBadRequest, false},
	domain.ErrCLIChecksumMismatch:    {consts.StatusBadGateway, true},
	domain.ErrCLIUpstreamUnavailable: {consts.StatusBadGateway, true},
}

// writeCLIError maps CLI addon sentinel errors to HTTP responses.
func (s *Server) writeCLIError(c *app.RequestContext, err error) {
	for sentinel, m := range cliErrorStatus {
		if errors.Is(err, sentinel) {
			if m.log {
				s.logger.WithError(err).Error(err.Error())
			}
			writeError(c, m.status, err.Error())
			return
		}
	}
	s.logger.WithError(err).Error("cli provisioning failed")
	writeError(c, consts.StatusBadGateway, "cli provisioning failed")
}
