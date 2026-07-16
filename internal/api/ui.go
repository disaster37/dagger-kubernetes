package api

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

	rel := strings.TrimPrefix(string(c.Path()), "/")
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
	c.SetStatusCode(consts.StatusOK)
	_, _ = c.Write(data)
	return nil
}

func contentTypeFor(rel string) string {
	if ct := mime.TypeByExtension(filepath.Ext(rel)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
