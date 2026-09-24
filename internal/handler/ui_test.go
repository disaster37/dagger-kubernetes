package handler

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// newUIFileEngine serves a MapFS through the same SPA path resolution as the
// embedded UI, so cache-header behavior can be tested without the real bundle.
func newUIFileEngine(fsys fstest.MapFS) *route.Engine {
	e := route.NewEngine(config.NewOptions(nil))
	e.NoRoute(func(_ context.Context, c *app.RequestContext) {
		serveUIPath(c, fsys, string(c.Path()))
	})
	return e
}

func TestContentTypeFor(t *testing.T) {
	tests := []struct {
		rel  string
		want string
	}{
		{rel: "favicon.ico", want: "image/x-icon"},
		{rel: "apple-touch-icon.png", want: "image/png"},
		{rel: "assets/chart.png", want: "image/png"},
		{rel: "FAVICON.ICO", want: "image/x-icon"},
		{rel: "favicon.svg", want: "image/svg+xml"},
		{rel: "blob.unknownext", want: "application/octet-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.rel, func(t *testing.T) {
			if got := contentTypeFor(tc.rel); got != tc.want {
				t.Fatalf("contentTypeFor(%q) = %q, want %q", tc.rel, got, tc.want)
			}
		})
	}
}

// TestServeIconContentType proves the favicon assets are both present in the
// embedded bundle and served with their pinned Content-Type (a 200 with the
// wrong type or a 404 would break non-SVG-favicon browsers).
func TestServeIconContentType(t *testing.T) {
	for _, name := range []string{"favicon.ico", "apple-touch-icon.png", "favicon.svg"} {
		if _, err := fs.Stat(uiAssets, filepath.Join("ui-dist", name)); err != nil {
			t.Fatalf("embedded bundle missing %s: %v", name, err)
		}
	}

	fsys := fstest.MapFS{
		"favicon.ico":          &fstest.MapFile{Data: []byte{0, 0, 1, 0}},
		"apple-touch-icon.png": &fstest.MapFile{Data: []byte("png")},
		"favicon.svg":          &fstest.MapFile{Data: []byte("<svg/>")},
	}
	e := newUIFileEngine(fsys)

	tests := []struct {
		path     string
		wantType string
	}{
		{path: "/favicon.ico", wantType: "image/x-icon"},
		{path: "/apple-touch-icon.png", wantType: "image/png"},
		{path: "/favicon.svg", wantType: "image/svg+xml"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp := ut.PerformRequest(e, "GET", tc.path, nil)
			if got := resp.Result().StatusCode(); got != http.StatusOK {
				t.Fatalf("status = %d, want %d", got, http.StatusOK)
			}
			if got := string(resp.Result().Header.Peek("Content-Type")); got != tc.wantType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantType)
			}
		})
	}
}

func TestServeFileCacheHeaders(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":          &fstest.MapFile{Data: []byte("<html></html>")},
		"assets/index-abc.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	e := newUIFileEngine(fsys)

	tests := []struct {
		name         string
		path         string
		wantCode     int
		wantContains []string
	}{
		{
			name:         "html shell revalidates",
			path:         "/index.html",
			wantCode:     http.StatusOK,
			wantContains: []string{"no-cache"},
		},
		{
			name:         "extension-less SPA route revalidates",
			path:         "/pipelines/123",
			wantCode:     http.StatusOK,
			wantContains: []string{"no-cache"},
		},
		{
			name:         "root revalidates",
			path:         "/",
			wantCode:     http.StatusOK,
			wantContains: []string{"no-cache"},
		},
		{
			name:         "hashed asset is immutable",
			path:         "/assets/index-abc.js",
			wantCode:     http.StatusOK,
			wantContains: []string{"immutable", "max-age=31536000"},
		},
		{
			name:     "missing file is 404",
			path:     "/assets/missing.js",
			wantCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := ut.PerformRequest(e, "GET", tc.path, nil)
			if got := resp.Result().StatusCode(); got != tc.wantCode {
				t.Fatalf("status = %d, want %d", got, tc.wantCode)
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			got := string(resp.Result().Header.Peek("Cache-Control"))
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("Cache-Control = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}
