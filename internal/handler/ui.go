package handler

import (
	"context"
	"embed"
	"io"
	"io/fs"
	"mime"
	"path/filepath"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

//go:embed all:ui-dist
var uiAssets embed.FS

// serveUI serves the embedded SPA. Paths without a file extension resolve to
// index.html (client-side routing); extension paths are served verbatim (404
// when missing).
func (s *Server) serveUI(_ context.Context, c *app.RequestContext) {
	sub, err := fs.Sub(uiAssets, "ui-dist")
	if err != nil {
		s.logger.WithError(err).Error("embedded UI not available")
		writeError(c, consts.StatusNotFound, "UI not available")
		return
	}

	serveUIPath(c, sub, string(c.Path()))
}

// serveUIPath resolves an SPA request path against sub and serves the file:
// extension-less paths (client-side routes) fall back to index.html.
func serveUIPath(c *app.RequestContext, sub fs.FS, path string) {
	rel := strings.TrimPrefix(path, "/")
	if rel == "" || filepath.Ext(rel) == "" {
		rel = "index.html"
	}

	if err := serveFile(c, sub, rel); err != nil {
		c.SetStatusCode(consts.StatusNotFound)
		_, _ = c.WriteString("not found")
	}
}

func serveFile(c *app.RequestContext, sub fs.FS, rel string) error {
	f, err := sub.Open(rel)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	c.Response.Header.Set("Content-Type", contentTypeFor(rel))
	// Vite emits content-hashed assets, so they can be cached forever; the
	// un-hashed HTML shell must revalidate so a new deploy's asset hashes are
	// picked up instead of a stale index.html.
	if strings.HasPrefix(rel, "assets/") {
		c.Response.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Response.Header.Set("Cache-Control", "no-cache")
	}
	c.SetStatusCode(consts.StatusOK)
	_, _ = c.Write(data)
	return nil
}

// iconContentTypes pins the favicon-related types explicitly: Go's builtin
// mime table has no ".ico" entry and the platform mime database (loaded from
// /etc/mime.types etc.) is absent in the minimal runtime image, so without
// this map favicon.ico would be served as application/octet-stream.
var iconContentTypes = map[string]string{
	".ico": "image/x-icon",
	".png": "image/png",
}

func contentTypeFor(rel string) string {
	ext := strings.ToLower(filepath.Ext(rel))
	if ct, ok := iconContentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
