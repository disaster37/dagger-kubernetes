package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var traceIDRe = regexp.MustCompile(`[a-f0-9]{32,}`)

func main() {
	serverURL := flag.String("server", "", "Dagger Cache server URL (required)")
	token := flag.String("token", "", "Dagger Cloud token (required)")
	uiURL := flag.String("ui-url", "", "UI URL for trace links")
	cacheRegistry := flag.String("cache-registry", "cache.reg/dagger-cache", "Cache registry host/repo")
	version := flag.String("version", "", "Dagger engine version")
	ciMode := flag.String("ci", "", "CI mode: gha, jenkins, drone")
	flag.Parse()

	if *serverURL == "" || *token == "" {
		fmt.Fprintf(os.Stderr, "Error: --server and --token required\n")
		os.Exit(1)
	}

	if *uiURL == "" {
		*uiURL = *serverURL
	}

	cmdArgs := flag.Args()
	if len(cmdArgs) == 0 {
		fmt.Fprintf(os.Stderr, "Error: no dagger command specified\n")
		os.Exit(1)
	}

	_ = os.Setenv("DAGGER_CLOUD_URL", *serverURL)
	_ = os.Setenv("DAGGER_CLOUD_TOKEN", *token)
	_ = os.Setenv("_EXPERIMENTAL_DAGGER_RUNNER_HOST", "dagger-cloud://self")

	if *version != "" {
		_ = os.Setenv("_EXPERIMENTAL_DAGGER_TAG", *version)
		vslug := strings.ReplaceAll(strings.ReplaceAll(*version, ".", "-"), "v", "")
		cacheRef := fmt.Sprintf("%s:V%s", *cacheRegistry, vslug)
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
		traceURL := fmt.Sprintf("%s/traces/%s", *uiURL, traceID)
		fmt.Fprintf(os.Stderr, "\nPipeline View: %s\n", traceURL)

		switch *ciMode {
		case "gha":
			emitGHAAnnotations(traceURL, traceID)
		case "jenkins":
			emitJenkinsStages(traceURL, traceID)
		case "drone":
			emitDroneAnnotations(traceURL)
		}
	}

	if err != nil {
		os.Exit(1)
	}
}

func extractTraceID(output string) string {
	match := traceIDRe.FindString(output)
	return match
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
