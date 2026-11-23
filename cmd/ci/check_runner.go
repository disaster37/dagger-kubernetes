package main

import (
	"bytes"
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
)

// checkRunnerFlags configures the `check-runner` subcommand.
func checkRunnerFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "run-id", Usage: "check run id assigned by the supervisor"},
		&cli.StringFlag{Name: "org", Usage: "organization id"},
		&cli.StringFlag{Name: "project", Usage: "project id"},
		&cli.StringFlag{Name: "repo", Required: true, Usage: "repository slug (host/owner/repo)"},
		&cli.StringFlag{Name: "clone-url", Usage: "full git clone URL (defaults to https://<repo>)"},
		&cli.StringFlag{Name: "sha", Usage: "commit SHA to check out"},
		&cli.StringFlag{Name: "ref", Usage: "git ref to check out"},
		&cli.StringFlag{Name: "event", Usage: "forge event that triggered the run"},
		&cli.StringFlag{Name: "default-branch", Usage: "repository default branch"},
		&cli.StringFlag{Name: "target-branch", Usage: "pull request target branch"},
		&cli.StringFlag{Name: "tag", Usage: "release tag"},
		&cli.BoolFlag{Name: "is-release", Usage: "the run was triggered by a release"},
		&cli.IntFlag{Name: "pr", Usage: "pull/merge request number"},
		&cli.StringFlag{Name: "server", EnvVars: []string{"DAGGER_KUBERNETES_SERVER"}, Usage: "supervisor URL"},
		&cli.StringFlag{Name: "token", EnvVars: []string{"DAGGER_KUBERNETES_TOKEN", "DAGGER_CLOUD_TOKEN"}, Usage: "supervisor token"},
		&cli.StringFlag{Name: "run-token", EnvVars: []string{"DAGGER_RUN_TOKEN"}, Usage: "per-run callback token"},
		&cli.StringFlag{Name: "workdir", Usage: "checkout directory (default: temp dir)"},
		&cli.DurationFlag{Name: "timeout", Value: 30 * time.Minute, Usage: "overall check-run timeout"},
	}
}

type checkResult struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	TraceID string `json:"trace_id,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type loadReport struct {
	Checks        []string          `json:"checks"`
	WorkflowFiles map[string]string `json:"workflow_files,omitempty"`
	ContextFiles  map[string][]byte `json:"context_files,omitempty"`
}

type completeReport struct {
	Checks []checkResult `json:"checks"`
	Error  string        `json:"error,omitempty"`
}

var workflowFileRefRe = regexp.MustCompile(`(?:file|next_semver)\(\s*"([^"]+)"`)

// runCheckRunner clones the repository, discovers checks with `dagger check -l`,
// reports the load phase, runs each check, and reports completion.
func runCheckRunner(c *cli.Context) error {
	ctx, cancel := context.WithTimeout(c.Context, c.Duration("timeout"))
	defer cancel()

	workdir := c.String("workdir")
	if workdir == "" {
		dir, err := os.MkdirTemp("", "dagger-check-*")
		if err != nil {
			return fmt.Errorf("create workdir: %w", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		workdir = dir
	}
	cloneURL := resolveCloneURL(c.String("repo"), c.String("clone-url"))
	if cloneURL == "" {
		return fmt.Errorf("cannot derive clone URL from repo %q; pass --clone-url", c.String("repo"))
	}
	if err := cloneRepo(ctx, cloneURL, c.String("sha"), c.String("ref"), workdir); err != nil {
		return err
	}

	server := c.String("server")
	token := c.String("token")
	runToken := c.String("run-token")
	runID := c.String("run-id")

	daggerBin, binExtraEnv, binCleanup, err := resolveDaggerBin(ctx, server, token)
	if err != nil {
		return err
	}
	defer binCleanup()
	env := append(runnerEnv(server, token, c.String("org")), binExtraEnv...)

	workflowFiles, contextFiles := scanWorkflowFiles(workdir)
	checks, err := listChecks(ctx, daggerBin, workdir, env)
	if err != nil {
		// A repository without a Dagger module has zero checks; treat a
		// missing module as a valid GitOps-only result.
		checks = nil
	}
	if runID != "" {
		if err := postJSON(ctx, server, token, runToken, fmt.Sprintf("/api/v1/checks/runs/%s/load", runID), loadReport{
			Checks: checks, WorkflowFiles: workflowFiles, ContextFiles: contextFiles,
		}); err != nil {
			return fmt.Errorf("report load phase: %w", err)
		}
	}

	results := make([]checkResult, 0, len(checks))
	var runErr error
	for _, check := range checks {
		fmt.Printf("running check %s\n", check)
		result := runCheck(ctx, daggerBin, workdir, env, check)
		results = append(results, result)
		if result.State != "success" && runErr == nil {
			runErr = fmt.Errorf("check %s failed", check)
		}
	}
	if runID != "" {
		report := completeReport{Checks: results}
		if runErr != nil {
			report.Error = runErr.Error()
		}
		if err := postJSON(ctx, server, token, runToken, fmt.Sprintf("/api/v1/checks/runs/%s/complete", runID), report); err != nil {
			return fmt.Errorf("report completion: %w", err)
		}
	}
	return runErr
}

// runCheck executes one check and captures its trace ID and terminal state.
func runCheck(ctx context.Context, daggerBin, dir string, env []string, check string) checkResult {
	traceFile, err := os.CreateTemp("", "dagger-trace-*")
	if err != nil {
		return checkResult{Name: check, State: "failure", Summary: err.Error()}
	}
	tracePath := traceFile.Name()
	_ = traceFile.Close()
	defer func() { _ = os.Remove(tracePath) }()

	cmd := exec.CommandContext(ctx, daggerBin, "check", check) // #nosec G204 -- check names come from the repository's own dagger module
	cmd.Dir = dir
	cmd.Env = append(append([]string(nil), env...), fmt.Sprintf("DAGGER_KUBERNETES_TRACE_ID_FILE=%s", tracePath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	result := checkResult{Name: check, State: "success"}
	if runErr != nil {
		result.State = "failure"
		result.Summary = runErr.Error()
	}
	if data, err := os.ReadFile(tracePath); err == nil { // #nosec G304 -- path is a temp file created above
		result.TraceID = strings.TrimSpace(string(data))
	}
	return result
}

// scanWorkflowFiles reads .dagger/workflows/*.y{a,}ml and the jailed context
// files referenced by file()/next_semver().
func scanWorkflowFiles(workdir string) (workflowFiles map[string]string, contextFiles map[string][]byte) {
	dir := filepath.Join(workdir, ".dagger", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	workflowFiles = make(map[string]string)
	contextFiles = make(map[string][]byte)
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = root.Close() }()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- directory entry under the repo workflow dir
		if err != nil {
			continue
		}
		workflowFiles[name] = string(data)
		for _, match := range workflowFileRefRe.FindAllStringSubmatch(string(data), -1) {
			path := filepath.Clean(match[1])
			if _, ok := contextFiles[path]; ok {
				continue
			}
			f, err := root.Open(path)
			if err != nil {
				continue
			}
			content, err := io.ReadAll(io.LimitReader(f, 1<<20))
			_ = f.Close()
			if err == nil {
				contextFiles[path] = content
			}
		}
	}
	return workflowFiles, contextFiles
}

// cloneRepo clones repo at sha/ref without persisting credentials in
// .git/config. When GIT_CLONE_TOKEN is set, a 0700 GIT_ASKPASS helper is used
// so the token never touches .git/config or argv.
func cloneRepo(ctx context.Context, url, sha, ref, dir string) error {
	env, cleanup, err := gitAuthEnv()
	if err != nil {
		return err
	}
	defer cleanup()
	args := []string{"clone", "--depth", "1"}
	if branch := cloneBranch(ref); branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clone %s: %w", url, err)
	}
	if sha != "" {
		checkout := exec.CommandContext(ctx, "git", "fetch", "--depth", "1", "origin", sha) // #nosec G204 -- sha is a validated commit id from the webhook
		checkout.Dir = dir
		checkout.Env = env
		checkout.Stdout = os.Stdout
		checkout.Stderr = os.Stderr
		if err := checkout.Run(); err != nil {
			return fmt.Errorf("fetch %s: %w", sha, err)
		}
		reset := exec.CommandContext(ctx, "git", "checkout", sha) // #nosec G204 -- sha is a validated commit id from the webhook
		reset.Dir = dir
		reset.Env = env
		reset.Stdout = os.Stdout
		reset.Stderr = os.Stderr
		if err := reset.Run(); err != nil {
			return fmt.Errorf("checkout %s: %w", sha, err)
		}
	}
	return nil
}

// cloneBranch maps a full git ref to a --branch-able name. PR/merge-request
// refs (refs/pull/..., refs/merge-requests/...) are not clonable by name and
// return "" so the SHA checkout below handles them.
func cloneBranch(ref string) string {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return strings.TrimPrefix(ref, "refs/heads/")
	case strings.HasPrefix(ref, "refs/tags/"):
		return strings.TrimPrefix(ref, "refs/tags/")
	default:
		return ""
	}
}

// gitAuthEnv builds the git environment with a temporary GIT_ASKPASS helper
// when GIT_CLONE_TOKEN is set.
func gitAuthEnv() (env []string, cleanup func(), err error) {
	env = os.Environ()
	if os.Getenv("GIT_CLONE_TOKEN") == "" {
		return env, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "git-askpass-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create askpass dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is required for the askpass helper directory
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("chmod askpass dir: %w", err)
	}
	script := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$GIT_CLONE_TOKEN\"\n"), 0o700); err != nil { //nolint:gosec // the helper must be executable by the runner user only
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("write askpass helper: %w", err)
	}
	env = append(env, fmt.Sprintf("GIT_ASKPASS=%s", script), "GIT_TERMINAL_PROMPT=0")
	return env, func() { _ = os.RemoveAll(dir) }, nil
}

// listChecks runs `dagger check -l` and parses one check name per line.
func listChecks(ctx context.Context, daggerBin, dir string, env []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, daggerBin, "check", "-l")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("dagger check -l: %w", err)
	}
	var checks []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Name") || strings.HasPrefix(line, "---") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			checks = append(checks, fields[0])
		}
	}
	return checks, nil
}

// postJSON posts payload to the supervisor and requires a 2xx response.
func postJSON(ctx context.Context, server, token, runToken, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s%s", strings.TrimSuffix(server, "/"), path), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	if runToken != "" {
		req.Header.Set("X-Dagger-Run-Token", runToken)
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("post report: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("report rejected: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
