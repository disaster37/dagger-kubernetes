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
	"time"

	"github.com/urfave/cli/v2"

	"github.com/disaster/dagger-kubernetes/config"
	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var traceIDRe = regexp.MustCompile(`[a-f0-9]{32,}`)

// cliHTTPClient is used for the provisioning/download requests so a stalled
// supervisor cannot hang a CI job indefinitely (http.DefaultClient has no
// timeout). 5m mirrors the supervisor's cli.download_timeout default.
var cliHTTPClient = &http.Client{Timeout: 5 * time.Minute}

func main() {
	app := &cli.App{
		Name:  "dagger-kubernetes-ci",
		Usage: "Dagger Kubernetes CI helper — runs a Dagger command against the Supervisor and prints the pipeline-view link",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server", Usage: "Dagger Kubernetes server URL (required unless server.public_url is set in --config)"},
			&cli.StringFlag{Name: "token", Usage: "Dagger Cloud token (required)"},
			&cli.StringFlag{Name: "ui-url", Usage: "UI base URL for pipeline-view links (overrides server.public_url; links use /pipelines/<traceID>)"},
			&cli.StringFlag{Name: "config", Value: "config.app.yaml", Usage: "path to config file (provides server.public_url fallback)"},
			&cli.StringFlag{Name: "cache-registry", Value: "cache.reg/dagger-cache", Usage: "Cache registry host/repo"},
			&cli.StringFlag{Name: "version", Usage: "Dagger engine version"},
			&cli.StringFlag{Name: "ci", Usage: "CI mode: gha, jenkins, drone"},
			&cli.BoolFlag{Name: "cli", Usage: "Provision the Dagger CLI binary on the fly from the supervisor"},
			&cli.StringFlag{Name: "cli-version", Usage: "Dagger CLI version to provision (empty = latest allowed)"},
			&cli.StringFlag{Name: "cli-os", Value: "linux", Usage: "CLI target OS (linux, darwin)"},
			&cli.StringFlag{Name: "cli-arch", Value: "amd64", Usage: "CLI target architecture (amd64, arm64, armv7)"},
		},
		Action: run,
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
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

	cmdArgs := c.Args().Slice()
	if len(cmdArgs) == 0 {
		return fmt.Errorf("no dagger command specified")
	}

	_ = os.Setenv("DAGGER_CLOUD_URL", serverURL)
	_ = os.Setenv("DAGGER_CLOUD_TOKEN", token)
	_ = os.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "dagger-cloud://self")

	if version != "" {
		_ = os.Setenv("_EXPERIMENTAL_DAGGER_TAG", version)
		vslug := strings.ReplaceAll(strings.ReplaceAll(version, ".", "-"), "v", "")
		cacheRef := fmt.Sprintf("%s:V%s", cacheRegistry, vslug)
		_ = os.Setenv("_EXPERIMENTAL_DAGGER_CACHE_CONFIG", fmt.Sprintf("type=registry,ref=%s,mode=max", cacheRef))
	}

	//nolint:gosec // intentional: shell out to dagger CLI with user-supplied args
	cmd := exec.Command("dagger", cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout

	if c.Bool("cli") {
		binDir, cleanup, err := provisionCLI(context.Background(), serverURL, token, c.String("cli-version"), c.String("cli-os"), c.String("cli-arch"))
		if err != nil {
			return fmt.Errorf("provision dagger cli: %w", err)
		}
		defer cleanup()
		cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
	}

	var logBuf strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &logBuf)

	err = cmd.Run()
	logOutput := logBuf.String()

	traceID := extractTraceID(logOutput)

	if traceID != "" {
		traceURL, err := domain.PipelineViewURL(uiURL, traceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nPipeline View: <unavailable: %v>\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "\nPipeline View: %s\n", traceURL)

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

	return err
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
