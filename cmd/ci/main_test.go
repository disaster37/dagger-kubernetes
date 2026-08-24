package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v2"
)

const testTraceID = "abcdef0123456789abcdef0123456789"

func newTestCLIContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	app := cli.NewApp()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.String("server", "", "")
	set.String("token", "", "")
	set.String("ui-url", "", "")
	set.String("config", "", "")
	set.String("cache-registry", "", "")
	set.String("version", "", "")
	set.String("ci", "", "")
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cli.NewContext(app, set, nil)
}

// stubDaggerOnPath installs a fake `dagger` executable on PATH that writes the
// given line to stderr and exits 0.
func stubDaggerOnPath(t *testing.T, stderrLine string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "dagger")
	content := fmt.Sprintf("#!/bin/sh\necho '%s' >&2\nexit 0\n", stderrLine)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write stub dagger: %v", err)
	}
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", dir, os.PathListSeparator, os.Getenv("PATH")))
}

// captureStderr swaps os.Stderr for the duration of fn and returns what was
// written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()

	_ = w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	return string(b)
}

func TestExtractTraceID(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "32 hex present",
			output: fmt.Sprintf("some log line with %s embedded", testTraceID),
			want:   testTraceID,
		},
		{
			name:   "multiple matches takes first",
			output: fmt.Sprintf("%s then 0123456789abcdef0123456789abcdef", testTraceID),
			want:   testTraceID,
		},
		{
			name:   "none",
			output: "no trace id here",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractTraceID(tt.output); got != tt.want {
				t.Fatalf("extractTraceID(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}

func TestResolveUIBase(t *testing.T) {
	tests := []struct {
		name            string
		uiURLFlag       string
		serverURLFlag   string
		configPublicURL string
		want            string
	}{
		{
			name:            "ui-url flag wins",
			uiURLFlag:       "https://ui.example.com",
			serverURLFlag:   "https://server.example.com",
			configPublicURL: "https://public.example.com",
			want:            "https://ui.example.com",
		},
		{
			name:            "config public_url wins over server flag",
			uiURLFlag:       "",
			serverURLFlag:   "https://server.example.com",
			configPublicURL: "https://public.example.com",
			want:            "https://public.example.com",
		},
		{
			name:          "server flag is last fallback",
			uiURLFlag:     "",
			serverURLFlag: "https://server.example.com",
			want:          "https://server.example.com",
		},
		{
			name: "all empty",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveUIBase(tt.uiURLFlag, tt.serverURLFlag, tt.configPublicURL); got != tt.want {
				t.Fatalf("resolveUIBase = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunPrintsPipelineView(t *testing.T) {
	stubDaggerOnPath(t, testTraceID)
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")
	ctx := newTestCLIContext(t,
		"--server", "https://supv.example.com",
		"--token", "tok",
		"--ui-url", "https://supv.example.com",
		"--config", missingConfig,
		"call", "foo",
	)

	stderr := captureStderr(t, func() {
		if err := run(ctx); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	want := fmt.Sprintf("Pipeline View: https://supv.example.com/pipelines/%s", testTraceID)
	if !strings.Contains(stderr, want) {
		t.Fatalf("stderr = %q, want containing %q", stderr, want)
	}
}

func TestRunNoTraceID(t *testing.T) {
	stubDaggerOnPath(t, "no trace id here")
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")
	ctx := newTestCLIContext(t,
		"--server", "https://supv.example.com",
		"--token", "tok",
		"--config", missingConfig,
		"call", "foo",
	)

	stderr := captureStderr(t, func() {
		if err := run(ctx); err != nil {
			t.Fatalf("run: %v", err)
		}
	})

	if strings.Contains(stderr, "Pipeline View:") {
		t.Fatalf("stderr = %q, want no Pipeline View line", stderr)
	}
}

func buildTestTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestProvisionCLILatest(t *testing.T) {
	tarball := buildTestTarGz(t, map[string]string{"dagger": "#!/bin/sh\necho hi\n"})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cli/versions/latest":
			if r.URL.Query().Get("os") != "linux" || r.URL.Query().Get("arch") != "amd64" {
				t.Errorf("unexpected query %q", r.URL.RawQuery)
			}
			_, _ = fmt.Fprintf(w, `{"version":"v0.21.8","url":"%s/api/v1/cli/v0.21.8?os=linux&arch=amd64"}`, srv.URL)
		case "/api/v1/cli/v0.21.8":
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q", got)
			}
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	binDir, cleanup, err := provisionCLI(context.Background(), srv.URL, "tok", "", "linux", "amd64")
	if err != nil {
		t.Fatalf("provisionCLI: %v", err)
	}
	defer cleanup()

	bin := filepath.Join(binDir, "dagger")
	st, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat dagger: %v", err)
	}
	if st.Mode()&0o111 == 0 {
		t.Fatal("dagger not executable")
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read dagger: %v", err)
	}
	if !strings.Contains(string(got), "echo hi") {
		t.Fatalf("content = %q", got)
	}
}

func TestProvisionCLIPinnedVersionURL(t *testing.T) {
	tarball := buildTestTarGz(t, map[string]string{"dagger": "binary"})
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	binDir, cleanup, err := provisionCLI(context.Background(), srv.URL, "tok", "v0.21.4", "linux", "arm64")
	if err != nil {
		t.Fatalf("provisionCLI: %v", err)
	}
	defer cleanup()

	if gotPath != "/api/v1/cli/v0.21.4?os=linux&arch=arm64" {
		t.Fatalf("path = %q, want /api/v1/cli/v0.21.4?os=linux&arch=arm64", gotPath)
	}
	if _, err := os.Stat(filepath.Join(binDir, "dagger")); err != nil {
		t.Fatalf("dagger missing: %v", err)
	}
}

func TestProvisionCLIDownloadNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, _, err := provisionCLI(context.Background(), srv.URL, "tok", "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want error")
	}
	if !strings.Contains(err.Error(), "server returned 404") {
		t.Fatalf("err = %q", err)
	}
}

func TestProvisionCLILatestNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := provisionCLI(context.Background(), srv.URL, "tok", "", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want error")
	}
	if !strings.Contains(err.Error(), "resolve latest cli version") {
		t.Fatalf("err = %q", err)
	}
}

func TestProvisionCLILatestInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	_, _, err := provisionCLI(context.Background(), srv.URL, "tok", "", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want decode error")
	}
}

func TestProvisionCLIInvalidGzip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "definitely not gzip")
	}))
	defer srv.Close()

	_, _, err := provisionCLI(context.Background(), srv.URL, "tok", "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want gunzip error")
	}
}

func TestProvisionCLIMissingDaggerEntry(t *testing.T) {
	tarball := buildTestTarGz(t, map[string]string{"other": "not dagger"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	_, _, err := provisionCLI(context.Background(), srv.URL, "tok", "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want missing-dagger error")
	}
	if !strings.Contains(err.Error(), "dagger binary not found") {
		t.Fatalf("err = %q", err)
	}
}

func TestExtractDaggerOversized(t *testing.T) {
	old := maxDaggerBinaryBytes
	maxDaggerBinaryBytes = 1024
	defer func() { maxDaggerBinaryBytes = old }()

	// A `dagger` entry larger than the (lowered) cap must be rejected.
	tarball := buildTestTarGz(t, map[string]string{"dagger": strings.Repeat("x", 2048)})

	binDir := t.TempDir()
	err := extractDagger(bytes.NewReader(tarball), binDir)
	if err == nil {
		t.Fatal("extractDagger = nil, want oversize error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %q", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "dagger")); !os.IsNotExist(statErr) {
		t.Fatal("oversized dagger binary left on disk")
	}
}

func TestProvisionCLINetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, _, err := provisionCLI(context.Background(), url, "tok", "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want network error")
	}
}

func TestProvisionCLIInvalidServerURL(t *testing.T) {
	_, _, err := provisionCLI(context.Background(), "://bad", "tok", "v0.21.8", "linux", "amd64")
	if err == nil {
		t.Fatal("provisionCLI = nil, want build-request error")
	}
}

func TestGetJSONBuildRequestError(t *testing.T) {
	err := getJSON(context.Background(), "://bad", "tok", &struct{}{})
	if err == nil {
		t.Fatal("getJSON = nil, want build-request error")
	}
}

func TestGetJSONNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	err := getJSON(context.Background(), url, "tok", &struct{}{})
	if err == nil {
		t.Fatal("getJSON = nil, want network error")
	}
}
