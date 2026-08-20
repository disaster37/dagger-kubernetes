package main

import (
	"flag"
	"fmt"
	"io"
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
		name              string
		uiURLFlag         string
		serverURLFlag     string
		configPipelineURL string
		configPublicURL   string
		want              string
	}{
		{
			name:              "ui-url flag wins",
			uiURLFlag:         "https://ui.example.com",
			serverURLFlag:     "https://server.example.com",
			configPipelineURL: "https://pipeline.example.com",
			configPublicURL:   "https://public.example.com",
			want:              "https://ui.example.com",
		},
		{
			name:              "config pipeline_url wins over public_url and server",
			uiURLFlag:         "",
			serverURLFlag:     "https://server.example.com",
			configPipelineURL: "https://pipeline.example.com",
			configPublicURL:   "https://public.example.com",
			want:              "https://pipeline.example.com",
		},
		{
			name:              "config public_url wins over server flag",
			uiURLFlag:         "",
			serverURLFlag:     "https://server.example.com",
			configPipelineURL: "",
			configPublicURL:   "https://public.example.com",
			want:              "https://public.example.com",
		},
		{
			name:              "server flag is last fallback",
			uiURLFlag:         "",
			serverURLFlag:     "https://server.example.com",
			configPipelineURL: "",
			configPublicURL:   "",
			want:              "https://server.example.com",
		},
		{
			name: "all empty",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveUIBase(tt.uiURLFlag, tt.serverURLFlag, tt.configPipelineURL, tt.configPublicURL); got != tt.want {
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
