package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/disaster/dagger-kubernetes/config"
	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
	"github.com/disaster/dagger-kubernetes/internal/service"
)

var traceIDRe = regexp.MustCompile(`[a-f0-9]{32,}`)

// ciStepsHTTPTimeout bounds each supervisor poll so a stalled supervisor cannot
// hang the step-stream goroutine. 10s is well above normal supervisor latency
// yet caps the wrapper's worst-case post-dagger exit delay to ~2 polls.
const ciStepsHTTPTimeout = 10 * time.Second

// ciLogQueryLimit is the per-poll log query limit for the CI step stream.
const ciLogQueryLimit = 1000

// liveCaptureMaxBuf caps the pending-bytes buffer the liveCaptureWriter keeps
// while scanning for the trace id, so a pathological stderr stream cannot grow
// it unbounded (CWE-400).
const liveCaptureMaxBuf = 8192

// cliHTTPClient is used for the provisioning/download requests so a stalled
// supervisor cannot hang a CI job indefinitely (http.DefaultClient has no
// timeout). 5m mirrors the supervisor's cli.download_timeout default.
var cliHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// minStepsPollInterval is the floor for the step-stream poll cadence (flag or
// config): below it the poller would effectively hot-loop the supervisor's
// REST API (CWE-400). The first poll always fires immediately regardless.
const minStepsPollInterval = 100 * time.Millisecond

func main() {
	app := &cli.App{
		Name:   "dagger-kubernetes-ci",
		Usage:  "Dagger Kubernetes CI helper — runs a Dagger command against the Supervisor and prints the pipeline-view link",
		Flags:  ciFlags(),
		Action: run,
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// ciFlags returns the wrapper's CLI flags. Extracted so tests can build a
// cli.Context that behaves exactly like the real app's (including env-var
// sourcing and flag defaults).
func ciFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "server", Usage: "Dagger Kubernetes server URL (required unless server.public_url is set in --config)"},
		// The token may also be supplied via the DAGGER_KUBERNETES_TOKEN
		// environment variable: passing secrets through argv exposes them in
		// the process list (ps / /proc/<pid>/cmdline) to every local user on
		// shared CI agents (CWE-214/CWE-532), so env is the preferred source.
		&cli.StringFlag{Name: "token", EnvVars: []string{"DAGGER_KUBERNETES_TOKEN"}, Usage: "Dagger Cloud token (required; prefer the DAGGER_KUBERNETES_TOKEN env var to keep it out of process argv)"},
		&cli.StringFlag{Name: "ui-url", Usage: "UI base URL for pipeline-view links (overrides server.public_url; links use /pipelines/<traceID>)"},
		&cli.StringFlag{Name: "config", Value: "config.app.yaml", Usage: "path to config file (provides server.public_url fallback)"},
		&cli.StringFlag{Name: "cache-registry", Value: "cache.reg/dagger-cache", Usage: "Cache registry host/repo"},
		&cli.StringFlag{Name: "version", Usage: "Dagger engine version"},
		&cli.StringFlag{Name: "ci", Usage: "CI mode: gha, jenkins, drone"},
		&cli.BoolFlag{Name: "cli", Usage: "Provision the Dagger CLI binary on the fly from the supervisor"},
		&cli.StringFlag{Name: "cli-version", Usage: "Dagger CLI version to provision (empty = latest allowed)"},
		&cli.StringFlag{Name: "cli-os", Value: "linux", Usage: "CLI target OS (linux, darwin)"},
		&cli.StringFlag{Name: "cli-arch", Value: "amd64", Usage: "CLI target architecture (amd64, arm64, armv7)"},
		&cli.BoolFlag{Name: "steps", Usage: "stream nested Dagger steps as NDJSON events on stdout"},
		&cli.DurationFlag{Name: "steps-poll-interval", Usage: "poll cadence for the CI step stream (default from ci.jenkins.steps_poll_interval)"},
		&cli.IntFlag{Name: "steps-max-depth", Usage: "maximum nested step depth surfaced (0 = unlimited; default from ci.jenkins.steps_max_depth)"},
	}
}

func run(c *cli.Context) error {
	cfg, err := config.Load(c.String("config"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Only trust config values when the config file was actually present;
	// config.Load otherwise returns compiled-in defaults (e.g. the example
	// public_url) that must not silently become the wrapper's target.
	hasConfigFile := fileExists(c.String("config"))
	var configPublicURL string
	if hasConfigFile {
		configPublicURL = cfg.Server.PublicURL
	}

	serverURL := c.String("server")
	if serverURL == "" {
		serverURL = configPublicURL
	}
	token := c.String("token")

	if serverURL == "" || token == "" {
		return fmt.Errorf("--server and --token required")
	}

	uiURL := resolveUIBase(c.String("ui-url"), c.String("server"), configPublicURL)

	cacheRegistry := c.String("cache-registry")
	version := c.String("version")
	ciMode := c.String("ci")

	steps, pollInterval, maxDepth := resolveSteps(c, cfg)

	cmdArgs := c.Args().Slice()
	if len(cmdArgs) == 0 {
		return fmt.Errorf("no dagger command specified")
	}

	_ = os.Setenv("DAGGER_CLOUD_URL", serverURL)
	_ = os.Setenv("DAGGER_CLOUD_TOKEN", token)
	_ = os.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "dagger-cloud://self")
	// Live span export: without this the engine only exports spans at
	// completion, so per-step state would never appear while the run is live.
	_ = os.Setenv("OTEL_EXPORTER_OTLP_TRACES_LIVE", "1")

	if version != "" {
		_ = os.Setenv("_EXPERIMENTAL_DAGGER_TAG", version)
	}
	if c.IsSet("cache-registry") {
		cacheRef := fmt.Sprintf("%s:%s", cacheRegistry, "cache")
		_ = os.Setenv("_EXPERIMENTAL_DAGGER_CACHE_CONFIG", fmt.Sprintf("type=registry,ref=%s,mode=max", cacheRef))
	}

	//nolint:gosec // intentional: shell out to dagger CLI with user-supplied args
	cmd := exec.Command("dagger", cmdArgs...)
	cmd.Stdin = os.Stdin
	// In --steps mode stdout is reserved for the NDJSON event protocol, so the
	// dagger command's own stdout is redirected to stderr (alongside its
	// stderr) to keep the protocol channel clean.
	if steps {
		cmd.Stdout = os.Stderr
	} else {
		cmd.Stdout = os.Stdout
	}

	if c.Bool("cli") {
		binDir, cleanup, err := provisionCLI(context.Background(), serverURL, token, c.String("cli-version"), c.String("cli-os"), c.String("cli-arch"))
		if err != nil {
			return fmt.Errorf("provision dagger cli: %w", err)
		}
		defer cleanup()
		cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
	}

	var logBuf strings.Builder

	// Steps-mode plumbing. The captured trace id is shared between the
	// streamSteps goroutine and the final flush via capturedID (mutex-guarded);
	// stepsCh (buffered 1) wakes the goroutine once the id is known.
	var (
		stepsWG      sync.WaitGroup
		stepsCancel  context.CancelFunc
		stepsSrc     domain.TraceSnapshotSource
		stepsBuilder *service.StepEventBuilder
		stepsSink    domain.CIEventSink
		capturedMu   sync.Mutex
		capturedID   string
		stepsCh      = make(chan string, 1)
	)

	capture := &liveCaptureWriter{
		dst: io.MultiWriter(os.Stderr, &logBuf),
		onID: func(id string) {
			capturedMu.Lock()
			if capturedID == "" {
				capturedID = id
			}
			capturedMu.Unlock()
			select {
			case stepsCh <- id:
			default:
			}
		},
	}
	cmd.Stderr = capture

	logger := observ.NewLogger(cfg.LogLevel, cfg.LogFormat)

	if steps {
		ctx, cancel := context.WithCancel(context.Background())
		stepsCancel = cancel
		stepsSrc = repository.NewSupervisorTraceClient(serverURL, token, ciStepsHTTPTimeout)
		stepsBuilder = service.NewStepEventBuilder(maxDepth)
		stepsSink = service.NewNDJSONEventSink(os.Stdout)

		// A panic anywhere below must not strand the consumer without a
		// terminal event: the Jenkins shared library polls the NDJSON stream
		// until pipeline_done, so finalize on the way out (idempotent — the
		// normal flush at the end of run() may already have emitted it) and
		// re-panic.
		defer func() {
			if r := recover(); r != nil {
				// Stop the poller first so the final flush cannot race a
				// concurrent Advance/Emit from the stream goroutine.
				if stepsCancel != nil {
					stepsCancel()
				}
				stepsWG.Wait()
				status := "failed"
				msg := fmt.Sprintf("wrapper panic: %v", r)
				for _, e := range stepsBuilder.Finalize(status, msg) {
					_ = stepsSink.Emit(&e)
				}
				_ = stepsSink.Flush()
				panic(r)
			}
		}()

		stepsWG.Add(1)
		go func() {
			defer stepsWG.Done()
			var traceID string
			select {
			case traceID = <-stepsCh:
			case <-ctx.Done():
				return
			}
			if traceID == "" {
				return
			}
			streamSteps(ctx, stepsSrc, stepsBuilder, stepsSink, traceID, pollInterval, logger)
		}()
	}

	err = cmd.Run()

	if stepsCancel != nil {
		stepsCancel()
	}
	stepsWG.Wait()

	// Final flush: capture terminal state + pipeline_done once the dagger
	// command has exited and the poller has stopped. Errors here are non-fatal.
	if stepsBuilder != nil {
		capturedMu.Lock()
		id := capturedID
		capturedMu.Unlock()
		if id != "" {
			if perr := pollTraceOnce(stepsSrc, stepsBuilder, stepsSink, id, stepsBuilder.LogMark()); perr != nil {
				logger.WithError(perr).WithField("trace_id", id).Debug("final ci step flush failed")
			}
		}

		// Finalize guarantees a terminal event (closing any still-running nodes
		// and emitting exactly-one pipeline_done) even when no trace id was ever
		// captured, the root never resolved, or the final poll failed. It uses
		// the dagger exit status, which is authoritative for the build result.
		status := "success"
		var errMsg string
		if err != nil {
			status = "failed"
			errMsg = err.Error()
		}
		for _, e := range stepsBuilder.Finalize(status, errMsg) {
			if eerr := stepsSink.Emit(&e); eerr != nil {
				logger.WithError(eerr).Debug("final ci step event emit failed")
			}
		}
		if ferr := stepsSink.Flush(); ferr != nil {
			logger.WithError(ferr).Debug("final ci step sink flush failed")
		}
	}

	logOutput := logBuf.String()

	traceID := extractTraceID(logOutput)

	if traceID != "" {
		traceURL, err := domain.PipelineViewURL(uiURL, traceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nPipeline View: <unavailable: %v>\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "\nPipeline View: %s\n", traceURL)

			if !steps {
				switch ciMode {
				case "gha":
					emitGHAAnnotations(traceURL, traceID)
				case "jenkins":
					emitJenkinsStages(traceURL, traceID)
				case "drone":
					emitDroneAnnotations(traceURL)
				}
			}
		}
	}

	return err
}

// liveCaptureWriter scans an underlying writer line-by-line for the first
// trace id (traceIDRe) and records it via onID; it always passes bytes through
// to the underlying writer. Used to learn the trace id while dagger still runs.
type liveCaptureWriter struct {
	dst   io.Writer
	onID  func(string)
	found bool
	buf   []byte
}

// Write passes p through to dst unchanged, then scans the accumulated stream
// for the first trace id. The returned n/err are exactly dst's so callers see
// pass-through behaviour; scanning never affects them.
func (w *liveCaptureWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if !w.found {
		w.buf = append(w.buf, p...)
		if m := traceIDRe.Find(w.buf); m != nil {
			w.found = true
			w.buf = nil
			w.onID(string(m))
		} else if len(w.buf) > liveCaptureMaxBuf {
			// Keep only the tail: a trace id never spans more than the tail we
			// retain, and this bounds memory under a noisy stderr stream.
			w.buf = w.buf[len(w.buf)-liveCaptureMaxBuf/2:]
		}
	}
	return n, err
}

// clampPollInterval enforces the minimum step-stream poll cadence so a
// misconfigured flag or config value (e.g. 1ns) cannot hot-loop the
// supervisor's REST API (CWE-400).
func clampPollInterval(d time.Duration) time.Duration {
	if d < minStepsPollInterval {
		return minStepsPollInterval
	}
	return d
}

// streamSteps runs the poller: it polls GetTrace + QueryTraceLogs at interval,
// feeding StepEventBuilder.Advance into sink until ctx is cancelled. Errors are
// logged (logrus) and retried; they never fail the CI build. Returns when ctx
// is done.
//
// NOTE: the supervisor's GET /api/v1/traces/:id/live SSE "re-fetch" ping is
// intentionally not subscribed to in v1 — the interval poll alone provides the
// snapshot cadence, and a long-lived SSE client would add failure modes with
// little gain at a ~2s poll. This is a documented simplification (ADR-024).
func streamSteps(ctx context.Context, src domain.TraceSnapshotSource,
	builder *service.StepEventBuilder, sink domain.CIEventSink,
	traceID string, interval time.Duration, logger *logrus.Logger) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	interval = clampPollInterval(interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	poll := func() {
		if ctx.Err() != nil {
			return
		}
		if err := pollTraceOnce(src, builder, sink, traceID, builder.LogMark()); err != nil {
			logger.WithError(err).WithField("trace_id", traceID).Debug("ci step poll failed")
		}
	}

	poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

// pollTraceOnce performs a single snapshot poll and emits any new events.
// Extracted for unit testing and the final-flush path.
func pollTraceOnce(src domain.TraceSnapshotSource, builder *service.StepEventBuilder,
	sink domain.CIEventSink, traceID string, logFrom time.Time) error {
	trace, err := src.GetTrace(traceID)
	if err != nil {
		return fmt.Errorf("get trace: %w", err)
	}
	logs, err := src.QueryTraceLogs(traceID, logFrom, time.Now(), ciLogQueryLimit)
	if err != nil {
		return fmt.Errorf("query logs: %w", err)
	}
	events, err := builder.Advance(trace, logs)
	if err != nil {
		return fmt.Errorf("advance step snapshot: %w", err)
	}
	for _, e := range events {
		if err := sink.Emit(&e); err != nil {
			return fmt.Errorf("emit step event: %w", err)
		}
	}
	return nil
}

// resolveUIBase returns the effective pipeline-view base URL with the
// precedence: uiURLFlag > configPublicURL > serverURLFlag.
func resolveUIBase(uiURLFlag, serverURLFlag, configPublicURL string) string {
	if uiURLFlag != "" {
		return uiURLFlag
	}
	if configPublicURL != "" {
		return configPublicURL
	}
	return serverURLFlag
}

// fileExists reports whether path exists (used to distinguish a real config
// file from config.Load's compiled-in defaults).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// resolveSteps resolves the nested-step streaming settings: --steps is a plain
// bool flag (default false); the poll interval and depth clamp fall back to the
// ci.jenkins.* config values when not set explicitly.
func resolveSteps(c *cli.Context, cfg *domain.Config) (steps bool, pollInterval time.Duration, maxDepth int) {
	steps = c.Bool("steps")
	pollInterval = c.Duration("steps-poll-interval")
	if !c.IsSet("steps-poll-interval") {
		pollInterval = cfg.CI.Jenkins.StepsPollInterval
	}
	maxDepth = c.Int("steps-max-depth")
	if !c.IsSet("steps-max-depth") {
		maxDepth = cfg.CI.Jenkins.StepsMaxDepth
	}
	return steps, pollInterval, maxDepth
}

func extractTraceID(output string) string {
	return traceIDRe.FindString(output)
}

func emitGHAAnnotations(traceURL, traceID string) {
	fmt.Printf("::notice title=Dagger Pipeline::Pipeline View: %s\n", traceURL)

	summaryFile := os.Getenv("GITHUB_STEP_SUMMARY")
	if summaryFile != "" {
		//nolint:gosec // trusted env var set by GitHub Actions
		f, err := os.OpenFile(summaryFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err == nil {
			defer func() { _ = f.Close() }()
			_, _ = fmt.Fprintf(f, "## Dagger Pipeline\n\n")
			_, _ = fmt.Fprintf(f, "[Live Pipeline View](%s)\n\n", traceURL)
			_, _ = fmt.Fprintf(f, "| Trace ID | Status |\n|---|---|\n")
			_, _ = fmt.Fprintf(f, "| `%s` | View |\n", traceID)
		}
	}

	if os.Getenv("GITHUB_REPOSITORY") != "" {
		pollSummary(traceURL)
	}
}

func emitJenkinsStages(traceURL, traceID string) {
	fmt.Printf("[dagger-kubernetes] Pipeline View: %s\n", traceURL)
	stageName := fmt.Sprintf("dagger: %s", traceID[:12])
	fmt.Printf("stage('%s') { sh 'true' }\n", stageName)
}

func emitDroneAnnotations(traceURL string) {
	fmt.Printf("[dagger-kubernetes] Pipeline View: %s\n", traceURL)
}

func pollSummary(traceURL string) {
	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		if traceFinished(client, traceURL) {
			return
		}
	}
}

// traceFinished reports whether the trace at traceURL has reached a terminal
// status.
func traceFinished(client *http.Client, traceURL string) bool {
	resp, err := client.Get(traceURL)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false
	}

	status, _ := data["status"].(string)
	return status == "success" || status == "failed" || status == "canceled"
}

// cliLatestResponse is the subset of the supervisor /cli/versions/latest
// payload the wrapper consumes.
type cliLatestResponse struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// provisionCLI resolves (or pins) the Dagger CLI version, downloads the
// verified tarball from the supervisor, extracts `dagger`, and returns the
// directory to prepend to PATH plus a cleanup func.
func provisionCLI(ctx context.Context, serverURL, token, version, osName, arch string) (binDir string, cleanup func(), err error) {
	downloadURL := ""
	if version == "" {
		latestURL := fmt.Sprintf("%s/api/v1/cli/versions/latest?os=%s&arch=%s", serverURL, osName, arch)
		var latest cliLatestResponse
		if err := getJSON(ctx, latestURL, token, &latest); err != nil {
			return "", nil, fmt.Errorf("resolve latest cli version: %w", err)
		}
		downloadURL = latest.URL
	} else {
		downloadURL = fmt.Sprintf("%s/api/v1/cli/%s?os=%s&arch=%s", serverURL, version, osName, arch)
	}

	resp, err := doAuthenticatedGet(ctx, downloadURL, token)
	if err != nil {
		return "", nil, fmt.Errorf("download cli: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	binDir, err = os.MkdirTemp("", "dagger-cli-*")
	if err != nil {
		return "", nil, fmt.Errorf("create cli bin dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(binDir) }

	if err := extractDagger(resp.Body, binDir); err != nil {
		cleanup()
		return "", nil, err
	}
	return binDir, cleanup, nil
}

// doAuthenticatedGet performs an authenticated GET and returns the response
// body on 200, or an error on any failure (network, non-200 status).
func doAuthenticatedGet(ctx context.Context, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	return resp, nil
}

// getJSON performs an authenticated GET and decodes the JSON response into out.
func getJSON(ctx context.Context, url, token string, out any) error {
	resp, err := doAuthenticatedGet(ctx, url, token)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// maxDaggerBinaryBytes caps the decompressed size of the extracted `dagger`
// binary. The real binary is ~100 MB; 1 GiB is far above any legitimate build
// but prevents a decompression bomb from filling the runner's disk if the
// tarball is malicious (CWE-409). A var (not const) so tests can lower it.
var maxDaggerBinaryBytes int64 = 1 << 30

// extractDagger extracts the `dagger` executable from a gzip'd tar stream into
// binDir and makes it executable.
func extractDagger(r io.Reader, binDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gunzip cli tarball: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read cli tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "dagger" {
			continue
		}

		dst := filepath.Join(binDir, "dagger")
		// #nosec G304 G302 -- dst is a fixed "dagger" basename under the caller-owned binDir; 0755 is required for the executable.
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("create dagger binary: %w", err)
		}
		n, err := io.Copy(f, io.LimitReader(tr, maxDaggerBinaryBytes+1))
		if err != nil {
			_ = f.Close()
			_ = os.Remove(dst)
			return fmt.Errorf("extract dagger: %w", err)
		}
		if n > maxDaggerBinaryBytes {
			_ = f.Close()
			_ = os.Remove(dst)
			return fmt.Errorf("dagger binary exceeds %d bytes", maxDaggerBinaryBytes)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close dagger binary: %w", err)
		}
		return nil
	}
	return fmt.Errorf("dagger binary not found in tarball")
}
