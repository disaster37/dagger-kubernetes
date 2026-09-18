package handler

import (
	"context"
	"net/http"
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
