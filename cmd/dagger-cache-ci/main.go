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
)

var traceIDRe = regexp.MustCompile(`[a-f0-9]{32,}`)

func main() {
	app := &cli.App{
		Name:  "dagger-cache-ci",
		Usage: "Dagger Cache CI helper — runs a Dagger command against the Supervisor and prints the pipeline-view link",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server", Usage: "Dagger Cache server URL (required)"},
			&cli.StringFlag{Name: "token", Usage: "Dagger Cloud token (required)"},
			&cli.StringFlag{Name: "ui-url", Usage: "UI URL for trace links"},
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
	serverURL := c.String("server")
	token := c.String("token")

	if serverURL == "" || token == "" {
		return fmt.Errorf("--server and --token required")
	}

	uiURL := c.String("ui-url")
	if uiURL == "" {
		uiURL = serverURL
	}

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

	err := cmd.Run()
	logOutput := logBuf.String()

	traceID := extractTraceID(logOutput)

	if traceID != "" {
		traceURL := fmt.Sprintf("%s/traces/%s", uiURL, traceID)
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

	return err
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
	fmt.Printf("[dagger-cache] Pipeline View: %s\n", traceURL)
	stageName := fmt.Sprintf("dagger: %s", traceID[:12])
	fmt.Printf("stage('%s') { sh 'true' }\n", stageName)
}

func emitDroneAnnotations(traceURL string) {
	fmt.Printf("[dagger-cache] Pipeline View: %s\n", traceURL)
}

func pollSummary(traceURL string) {
	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		resp, err := client.Get(traceURL)
		if err != nil {
			continue
		}
		var data map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			_ = resp.Body.Close()
			continue
		}
		_ = resp.Body.Close()

		status, _ := data["status"].(string)
		if status == "success" || status == "failed" || status == "canceled" {
			return
		}
	}
}
