package handler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenNetHTTPSymbols lists stdlib net/http symbols that construct, mount,
// or run an HTTP server. Outbound client symbols (http.Client, http.Request,
// http.NewRequestWithContext, http.Status*, etc.) are allowed per ADR-007.
var forbiddenNetHTTPSymbols = map[string]string{
	"Server":            "use github.com/cloudwego/hertz/pkg/app/server (*server.Hertz) instead of http.Server",
	"ListenAndServe":    "use Hertz server.Default(...).Run() instead of http.ListenAndServe",
	"ListenAndServeTLS": "use Hertz server.Default(..., server.WithTLS(...)).Run() instead of http.ListenAndServeTLS",
	"Serve":             "use Hertz server.Default(server.WithListener(...)).Run() instead of http.Serve",
	"ServeTLS":          "use Hertz server.Default(server.WithListener(...), server.WithTLS(...)).Run() instead of http.ServeTLS",
	"NewServeMux":       "use Hertz route registration (h.GET/POST/Use) instead of http.NewServeMux",
	"ServeMux":          "use Hertz route.Engine / *server.Hertz instead of http.ServeMux",
	"DefaultServeMux":   "use Hertz route registration instead of http.DefaultServeMux",
	"Handle":            "use Hertz route registration instead of http.Handle",
	"HandleFunc":        "use Hertz app.HandlerFunc route registration instead of http.HandleFunc",
	"Handler":           "use Hertz app.HandlerFunc (or adaptor.HertzHandler for promhttp) instead of http.Handler",
	"HandlerFunc":       "use Hertz app.HandlerFunc instead of http.HandlerFunc",
	"FileServer":        "use Hertz static/UI serving (internal/handler/ui.go) instead of http.FileServer",
	"FileServerFS":      "use Hertz static/UI serving (internal/handler/ui.go) instead of http.FileServerFS",
	"StripPrefix":       "use Hertz routing instead of http.StripPrefix",
	"TimeoutHandler":    "use Hertz middleware/timeouts instead of http.TimeoutHandler",
	"MaxBytesHandler":   "use readBoundedBody / Hertz body limits instead of http.MaxBytesHandler",
}

// forbiddenHTTPUtilSymbols lists net/http/httputil server-side proxy symbols.
var forbiddenHTTPUtilSymbols = map[string]string{
	"NewSingleHostReverseProxy": "use github.com/hertz-contrib/reverseproxy instead of httputil.NewSingleHostReverseProxy",
	"ReverseProxy":              "use github.com/hertz-contrib/reverseproxy instead of httputil.ReverseProxy",
}

// repoRoot resolves the repository root directory from the location of this
// test file so the scan is independent of the current working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("go.mod not found at resolved repo root %s: %v", root, err)
	}
	return root
}

// TestProductionHTTPServerUsesHertzOnly scans all production Go files in the
// repository and fails if any file constructs or runs a stdlib net/http server
// or httputil reverse proxy (ADR-001, ADR-002, ADR-007).
func TestProductionHTTPServerUsesHertzOnly(t *testing.T) {
	root := repoRoot(t)
	scanRoots := []string{"cmd", "config", "internal", "scripts"}

	var violations []string
	var scannedFiles int

	for _, dir := range scanRoots {
		absDir := filepath.Join(root, dir)
		if _, err := os.Stat(absDir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(absDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				name := d.Name()
				if name == "vendor" || name == "ui-dist" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") ||
				strings.HasSuffix(name, ".gen.go") {
				return nil
			}

			src, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)

			scannedFiles++
			fileViolations, err := checkSourceForStdlibHTTPServer(rel, src)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			violations = append(violations, fileViolations...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if scannedFiles == 0 {
		t.Fatal("expected to scan production .go files, but scanned 0")
	}
	if len(violations) > 0 {
		t.Fatalf("found %d non-Hertz HTTP server violation(s) in production code:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// checkSourceForStdlibHTTPServer parses src as a Go file and returns human-
// readable violation strings if it uses forbidden stdlib HTTP server symbols.
func checkSourceForStdlibHTTPServer(relPath string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	for _, cg := range file.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "// Code generated ") && strings.HasSuffix(c.Text, " DO NOT EDIT.") {
				return nil, nil
			}
		}
	}

	httpAliases := make(map[string]bool)
	httputilAliases := make(map[string]bool)
	var violations []string

	isHandlerPkg := strings.HasPrefix(filepath.ToSlash(relPath), "internal/handler/")

	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		pos := fset.Position(imp.Pos())

		var localName string
		if imp.Name != nil {
			localName = imp.Name.Name
		} else {
			parts := strings.Split(importPath, "/")
			localName = parts[len(parts)-1]
		}

		switch importPath {
		case "net/http":
			if isHandlerPkg {
				violations = append(violations, fmt.Sprintf(
					"%s:%d: internal/handler must not import %q; use github.com/cloudwego/hertz packages",
					relPath, pos.Line, importPath,
				))
			}
			if localName != "_" {
				httpAliases[localName] = true
			}
		case "net/http/httputil":
			violations = append(violations, fmt.Sprintf(
				"%s:%d: forbidden import %q; use github.com/hertz-contrib/reverseproxy",
				relPath, pos.Line, importPath,
			))
			if localName != "_" {
				httputilAliases[localName] = true
			}
		case "net/http/httptest", "net/http/fcgi", "net/http/cgi":
			violations = append(violations, fmt.Sprintf(
				"%s:%d: forbidden server/test import %q in production code",
				relPath, pos.Line, importPath,
			))
		}
	}

	if len(httpAliases) == 0 && len(httputilAliases) == 0 {
		return violations, nil
	}

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		pos := fset.Position(sel.Pos())
		sym := sel.Sel.Name

		if httpAliases[pkgIdent.Name] {
			if reason, forbidden := forbiddenNetHTTPSymbols[sym]; forbidden {
				violations = append(violations, fmt.Sprintf(
					"%s:%d: forbidden stdlib HTTP server symbol %s.%s (%s)",
					relPath, pos.Line, pkgIdent.Name, sym, reason,
				))
			}
		}
		if httputilAliases[pkgIdent.Name] {
			if reason, forbidden := forbiddenHTTPUtilSymbols[sym]; forbidden {
				violations = append(violations, fmt.Sprintf(
					"%s:%d: forbidden stdlib reverse proxy symbol %s.%s (%s)",
					relPath, pos.Line, pkgIdent.Name, sym, reason,
				))
			}
		}
		return true
	})

	return violations, nil
}

func TestCheckSourceForStdlibHTTPServerDetectsViolations(t *testing.T) {
	tests := []struct {
		name           string
		relPath        string
		src            string
		wantViolations []string
	}{
		{
			name:    "http.Server composite literal is rejected",
			relPath: "internal/service/bad.go",
			src: `package service
import "net/http"
func f() *http.Server { return &http.Server{Addr: ":8080"} }
`,
			wantViolations: []string{"forbidden stdlib HTTP server symbol http.Server"},
		},
		{
			name:    "http.ListenAndServe is rejected",
			relPath: "cmd/api/bad.go",
			src: `package main
import "net/http"
func f() error { return http.ListenAndServe(":8080", nil) }
`,
			wantViolations: []string{"forbidden stdlib HTTP server symbol http.ListenAndServe"},
		},
		{
			name:    "http.Serve is rejected",
			relPath: "cmd/api/bad.go",
			src: `package main
import (
	"net"
	"net/http"
)
func f(ln net.Listener) error { return http.Serve(ln, nil) }
`,
			wantViolations: []string{"forbidden stdlib HTTP server symbol http.Serve"},
		},
		{
			name:    "http.NewServeMux and http.HandlerFunc are rejected",
			relPath: "internal/service/bad.go",
			src: `package service
import "net/http"
func f() {
	mux := http.NewServeMux()
	mux.Handle("/x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
}
`,
			wantViolations: []string{
				"forbidden stdlib HTTP server symbol http.NewServeMux",
				"forbidden stdlib HTTP server symbol http.HandlerFunc",
			},
		},
		{
			name:    "httputil.NewSingleHostReverseProxy is rejected",
			relPath: "internal/service/bad.go",
			src: `package service
import (
	"net/http/httputil"
	"net/url"
)
func f(u *url.URL) *httputil.ReverseProxy {
	return httputil.NewSingleHostReverseProxy(u)
}
`,
			wantViolations: []string{
				`forbidden import "net/http/httputil"`,
				"forbidden stdlib reverse proxy symbol httputil.ReverseProxy",
				"forbidden stdlib reverse proxy symbol httputil.NewSingleHostReverseProxy",
			},
		},
		{
			name:    "aliased net/http import is still caught",
			relPath: "cmd/api/bad.go",
			src: `package main
import stdhttp "net/http"
func f() error { return stdhttp.ListenAndServeTLS(":8443", "c", "k", nil) }
`,
			wantViolations: []string{"forbidden stdlib HTTP server symbol stdhttp.ListenAndServeTLS"},
		},
		{
			name:    "net/http import inside internal/handler is rejected",
			relPath: "internal/handler/bad.go",
			src: `package handler
import "net/http"
var _ = http.StatusOK
`,
			wantViolations: []string{`internal/handler must not import "net/http"`},
		},
		{
			name:    "outbound http.Client in internal/repository is allowed (ADR-007)",
			relPath: "internal/repository/client.go",
			src: `package repository
import (
	"context"
	"net/http"
	"time"
)
func doReq(ctx context.Context, url string) (*http.Response, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
`,
			wantViolations: nil,
		},
		{
			name:    "raw L4 net/tls server in internal/handler is allowed (ADR-002)",
			relPath: "internal/handler/l4.go",
			src: `package handler
import (
	"crypto/tls"
	"net"
)
func acceptL4(ln net.Listener, cfg *tls.Config) (net.Conn, error) {
	raw, err := ln.Accept()
	if err != nil {
		return nil, err
	}
	return tls.Server(raw, cfg), nil
}
`,
			wantViolations: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkSourceForStdlibHTTPServer(tc.relPath, []byte(tc.src))
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if len(tc.wantViolations) == 0 {
				if len(got) != 0 {
					t.Fatalf("expected no violations, got: %v", got)
				}
				return
			}
			joined := strings.Join(got, "\n")
			for _, want := range tc.wantViolations {
				if !strings.Contains(joined, want) {
					t.Fatalf("violations = %q, want substring %q", joined, want)
				}
			}
		})
	}
}
