package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/disaster/dagger-kubernetes/config"
	"github.com/disaster/dagger-kubernetes/internal/domain"
)

var traceIDRe = regexp.MustCompile(`[a-f0-9]{32,}`)

func main() {
	app := &cli.App{
		Name:  "dagger-kubernetes-ci",
		Usage: "Dagger Kubernetes CI helper — runs a Dagger command against the Supervisor and prints the pipeline-view link",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server", Usage: "Dagger Kubernetes server URL (required unless server.public_url is set in --config)"},
			&cli.StringFlag{Name: "token", Usage: "Dagger Cloud token (required)"},
			&cli.StringFlag{Name: "ui-url", Usage: "UI base URL for pipeline-view links (overrides server.pipeline_url/server.public_url; links use /pipelines/<traceID>)"},
			&cli.StringFlag{Name: "config", Value: "config.app.yaml", Usage: "path to config file (provides server.pipeline_url/server.public_url fallbacks)"},
			&cli.StringFlag{Name: "cache-registry", Value: "cache.reg/dagger-cache", Usage: "Cache registry host/repo"},
			&cli.StringFlag{Name: "version", Usage: "Dagger engine version"},
			&cli.StringFlag{Name: "ci", Usage: "CI mode: gha, jenkins, drone"},
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
	var configPipelineURL, configPublicURL string
	if hasConfigFile {
		configPipelineURL = cfg.Server.PipelineURL
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

	uiURL := resolveUIBase(c.String("ui-url"), c.String("server"), configPipelineURL, configPublicURL)

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
// precedence: uiURLFlag > configPipelineURL > configPublicURL > serverURLFlag.
// The config precedence itself is delegated to domain.ResolvePipelineBase.
func resolveUIBase(uiURLFlag, serverURLFlag, configPipelineURL, configPublicURL string) string {
	if uiURLFlag != "" {
		return uiURLFlag
	}
	if base := domain.ResolvePipelineBase(configPublicURL, configPipelineURL); base != "" {
		return base
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
