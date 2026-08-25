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
	"time"

	"github.com/urfave/cli/v2"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

const testTraceID = "abcdef0123456789abcdef0123456789"

func newTestCLIContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	app := cli.NewApp()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	// Apply the real app flags so the context behaves exactly like production:
	// defaults and env-var sources (e.g. DAGGER_KUBERNETES_TOKEN for --token)
	// are resolved the same way app.Run resolves them.
	for _, f := range ciFlags() {
		if err := f.Apply(set); err != nil {
			t.Fatalf("apply flag %v: %v", f.Names(), err)
		}
	}
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

// TestRunTokenFromEnv proves the supervisor token can be supplied via the
// DAGGER_KUBERNETES_TOKEN environment variable instead of argv: the CI
// integrations rely on it to keep the credential out of the process list
// (CWE-214/CWE-532).
func TestRunTokenFromEnv(t *testing.T) {
	stubDaggerOnPath(t, testTraceID)
	t.Setenv("DAGGER_KUBERNETES_TOKEN", "env-token")
	missingConfig := filepath.Join(t.TempDir(), "missing.yaml")
	ctx := newTestCLIContext(t,
		"--server", "https://supv.example.com",
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

// TestRunTokenFlagWinsOverEnv proves an explicit --token still takes
// precedence over the environment (urfave/cli source precedence).
func TestRunTokenFlagWinsOverEnv(t *testing.T) {
	t.Setenv("DAGGER_KUBERNETES_TOKEN", "env-token")
	ctx := newTestCLIContext(t, "--server", "https://supv.example.com", "--token", "flag-token", "call", "foo")
	if got := ctx.String("token"); got != "flag-token" {
		t.Fatalf("token = %q, want flag-token", got)
	}
}

func TestClampPollInterval(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want time.Duration
	}{
		{in: -time.Second, want: minStepsPollInterval},
		{in: 0, want: minStepsPollInterval},
		{in: time.Nanosecond, want: minStepsPollInterval},
		{in: 50 * time.Millisecond, want: minStepsPollInterval},
		{in: minStepsPollInterval, want: minStepsPollInterval},
		{in: 5 * time.Second, want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.in.String(), func(t *testing.T) {
			if got := clampPollInterval(tt.in); got != tt.want {
				t.Fatalf("clampPollInterval(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
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

// --- nested-step streaming (ADR-024) tests ---

type stubSnapshotSource struct {
	trace     *domain.TraceInfo
	traceErr  error
	logs      []domain.LogEntry
	logsErr   error
	lastStart time.Time
}

func (s *stubSnapshotSource) GetTrace(string) (*domain.TraceInfo, error) {
	return s.trace, s.traceErr
}

func (s *stubSnapshotSource) QueryTraceLogs(_ string, start, _ time.Time, _ int) ([]domain.LogEntry, error) {
	s.lastStart = start
	return s.logs, s.logsErr
}

type collectSink struct {
	events []domain.CIEvent
}

func (s *collectSink) Emit(e *domain.CIEvent) error {
	s.events = append(s.events, *e)
	return nil
}

func (s *collectSink) Flush() error { return nil }

func TestLiveCaptureWriterPassesThroughAndCaptures(t *testing.T) {
	var dst bytes.Buffer
	var gotID string
	w := &liveCaptureWriter{dst: &dst, onID: func(id string) { gotID = id }}

	// Feed the trace id split across writes to prove the buffer accumulates.
	for _, chunk := range []string{"prefix ", testTraceID[:16], testTraceID[16:], " suffix\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}

	if gotID != testTraceID {
		t.Fatalf("captured id = %q, want %q", gotID, testTraceID)
	}
	if dst.String() != "prefix "+testTraceID+" suffix\n" {
		t.Fatalf("dst = %q", dst.String())
	}

	// A later write must not re-trigger onID (found flag latched).
	calls := 0
	w.onID = func(string) { calls++ }
	if _, err := w.Write([]byte("more\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if calls != 0 {
		t.Fatalf("onID re-fired %d times after first capture", calls)
	}
}

func TestLiveCaptureWriterNoID(t *testing.T) {
	var dst bytes.Buffer
	fired := false
	w := &liveCaptureWriter{dst: &dst, onID: func(string) { fired = true }}
	if _, err := w.Write([]byte("no trace id here\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fired {
		t.Fatal("onID fired without a trace id")
	}
	if dst.String() != "no trace id here\n" {
		t.Fatalf("dst = %q", dst.String())
	}
}

func TestLiveCaptureWriterBoundedBuffer(t *testing.T) {
	var dst bytes.Buffer
	var gotID string
	w := &liveCaptureWriter{dst: &dst, onID: func(id string) { gotID = id }}

	// A large non-hex stream must not grow the scan buffer unbounded, and a
	// trace id arriving afterwards must still be captured.
	noise := strings.Repeat("z", liveCaptureMaxBuf*3)
	if _, err := w.Write([]byte(noise)); err != nil {
		t.Fatalf("Write noise: %v", err)
	}
	if _, err := w.Write([]byte(testTraceID)); err != nil {
		t.Fatalf("Write id: %v", err)
	}
	if gotID != testTraceID {
		t.Fatalf("captured id = %q, want %q", gotID, testTraceID)
	}
}

func TestResolveStepsFlagDefaultsFromConfig(t *testing.T) {
	cfg := &domain.Config{CI: domain.CIConfig{Jenkins: domain.JenkinsConfig{
		DynamicStages:     true,
		StepsPollInterval: 7 * time.Second,
		StepsMaxDepth:     3,
	}}}

	// Explicit flags win over config.
	ctx := newTestCLIContext(t, "--steps", "--steps-poll-interval", "5s", "--steps-max-depth", "2")
	steps, interval, depth := resolveSteps(ctx, cfg)
	if !steps || interval != 5*time.Second || depth != 2 {
		t.Fatalf("resolveSteps = (%v, %v, %v), want (true, 5s, 2)", steps, interval, depth)
	}

	// Without flags, poll/depth fall back to config; steps stays false.
	ctx = newTestCLIContext(t)
	steps, interval, depth = resolveSteps(ctx, cfg)
	if steps || interval != 7*time.Second || depth != 3 {
		t.Fatalf("resolveSteps = (%v, %v, %v), want (false, 7s, 3)", steps, interval, depth)
	}
}

func TestPollTraceOnceEmitsEvents(t *testing.T) {
	root := &domain.SpanNode{SpanID: "r", Name: "build", Status: "success", Children: []*domain.SpanNode{}}
	src := &stubSnapshotSource{
		trace: &domain.TraceInfo{TraceID: testTraceID, RootSpan: root, Status: "success"},
		logs:  []domain.LogEntry{{Timestamp: time.Unix(100, 0), SpanID: "r", Line: "hello"}},
	}
	b := service.NewStepEventBuilder(0)
	sink := &collectSink{}

	if err := pollTraceOnce(src, b, sink, testTraceID, time.Time{}); err != nil {
		t.Fatalf("pollTraceOnce: %v", err)
	}
	if len(sink.events) == 0 {
		t.Fatal("no events emitted")
	}
	// First event is root node_started, last is pipeline_done.
	if sink.events[0].Type != domain.CIEventNodeStarted {
		t.Fatalf("events[0].Type = %q, want node_started", sink.events[0].Type)
	}
	if sink.events[len(sink.events)-1].Type != domain.CIEventPipelineDone {
		t.Fatalf("last event = %q, want pipeline_done", sink.events[len(sink.events)-1].Type)
	}
	// The query start bound was passed through to the source.
	if !src.lastStart.IsZero() {
		t.Fatalf("lastStart = %v, want zero", src.lastStart)
	}
}

func TestPollTraceOnceGetTraceError(t *testing.T) {
	src := &stubSnapshotSource{traceErr: fmt.Errorf("network down")}
	b := service.NewStepEventBuilder(0)
	sink := &collectSink{}

	err := pollTraceOnce(src, b, sink, testTraceID, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "get trace") {
		t.Fatalf("err = %q, want get-trace error", err)
	}
}

func TestPollTraceOnceLogsError(t *testing.T) {
	src := &stubSnapshotSource{
		trace:   &domain.TraceInfo{TraceID: testTraceID, RootSpan: &domain.SpanNode{SpanID: "r", Status: "running", Children: []*domain.SpanNode{}}},
		logsErr: fmt.Errorf("loki down"),
	}
	b := service.NewStepEventBuilder(0)
	sink := &collectSink{}

	err := pollTraceOnce(src, b, sink, testTraceID, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "query logs") {
		t.Fatalf("err = %q, want query-logs error", err)
	}
}

func TestStreamStepsPollsUntilCancelled(t *testing.T) {
	root := &domain.SpanNode{SpanID: "r", Name: "build", Status: "running", Children: []*domain.SpanNode{}}
	src := &stubSnapshotSource{
		trace: &domain.TraceInfo{TraceID: testTraceID, RootSpan: root, Status: "running"},
	}
	b := service.NewStepEventBuilder(0)
	sink := &collectSink{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		streamSteps(ctx, src, b, sink, testTraceID, 10*time.Millisecond, observ.NewTestLogger())
		close(done)
	}()

	time.Sleep(70 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamSteps did not return after cancel")
	}

	if len(sink.events) == 0 {
		t.Fatal("streamSteps emitted no events")
	}
	if sink.events[0].Type != domain.CIEventNodeStarted {
		t.Fatalf("events[0].Type = %q, want node_started", sink.events[0].Type)
	}
}

type errSink struct{}

func (errSink) Emit(*domain.CIEvent) error { return fmt.Errorf("sink boom") }
func (errSink) Flush() error               { return nil }

func TestPollTraceOnceNilTraceError(t *testing.T) {
	// GetTrace succeeds with a nil trace -> Advance wraps the nil-trace error.
	src := &stubSnapshotSource{trace: nil}
	b := service.NewStepEventBuilder(0)

	err := pollTraceOnce(src, b, &collectSink{}, testTraceID, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "advance step snapshot") {
		t.Fatalf("err = %q, want advance error", err)
	}
}

func TestPollTraceOnceEmitError(t *testing.T) {
	root := &domain.SpanNode{SpanID: "r", Name: "build", Status: "success", Children: []*domain.SpanNode{}}
	src := &stubSnapshotSource{trace: &domain.TraceInfo{TraceID: testTraceID, RootSpan: root, Status: "success"}}
	b := service.NewStepEventBuilder(0)

	err := pollTraceOnce(src, b, errSink{}, testTraceID, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "emit step event") {
		t.Fatalf("err = %q, want emit error", err)
	}
}

func TestStreamStepsDefaultIntervalAndErrorRetry(t *testing.T) {
	// interval <= 0 falls back to the default; poll errors are logged, not
	// fatal, and the loop still returns cleanly on cancel.
	src := &stubSnapshotSource{traceErr: fmt.Errorf("network down")}
	b := service.NewStepEventBuilder(0)
	sink := &collectSink{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		streamSteps(ctx, src, b, sink, testTraceID, 0, observ.NewTestLogger())
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamSteps did not return after cancel")
	}

	if len(sink.events) != 0 {
		t.Fatalf("events = %d, want 0 (source always errors)", len(sink.events))
	}
}
