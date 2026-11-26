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

	"github.com/disaster/dagger-kubernetes/internal/domain"
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
		&cli.StringSliceFlag{Name: "filter", Usage: "typed check filter as key=value (repeatable; appended to dagger check as --key=value)"},
		&cli.StringSliceFlag{Name: "workflow-path", Usage: "workdir-relative directory to scan for workflow files (repeatable; default .dagger/workflows)"},
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
	env := append(runnerEnv(server, token, c.String("org"), defaultRunnerHost), binExtraEnv...)

	filters, err := checkFilterArgv(c.StringSlice("filter"))
	if err != nil {
		return err
	}

	workflowFiles, contextFiles := scanWorkflowFiles(workdir, workflowPaths(c.StringSlice("workflow-path")))
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
		result := runCheck(ctx, daggerBin, workdir, env, check, filters)
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
// filters are canonical "--key=value" argv entries appended after the check
// name (typed artifact/test filters).
func runCheck(ctx context.Context, daggerBin, dir string, env []string, check string, filters []string) checkResult {
	traceFile, err := os.CreateTemp("", "dagger-trace-*")
	if err != nil {
		return checkResult{Name: check, State: "failure", Summary: err.Error()}
	}
	tracePath := traceFile.Name()
	_ = traceFile.Close()
	defer func() { _ = os.Remove(tracePath) }()

	argv := append([]string{"check", check}, filters...)
	cmd := exec.CommandContext(ctx, daggerBin, silentArgs(argv)...) // #nosec G204 -- check names and validated filters come from the repository's own dagger module / run request
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

// defaultWorkflowPath is the built-in workflow discovery directory.
const defaultWorkflowPath = ".dagger/workflows"

// workflowPaths returns the configured discovery directories, falling back to
// the built-in default when none are supplied.
func workflowPaths(configured []string) []string {
	if len(configured) == 0 {
		return []string{defaultWorkflowPath}
	}
	return configured
}

// scanWorkflowFiles reads */*.y{a,}ml from each discovery directory and the
// jailed context files referenced by file()/next_semver(). Missing directories
// are skipped silently (a repository without workflows is valid). Paths are
// confined to workdir: an escaping entry is ignored rather than read.
func scanWorkflowFiles(workdir string, paths []string) (workflowFiles map[string]string, contextFiles map[string][]byte) {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return nil, nil
	}
	defer func() { _ = root.Close() }()

	workflowFiles = make(map[string]string)
	contextFiles = make(map[string][]byte)
	for _, path := range paths {
		dir, ok := confinedPath(workdir, path)
		if !ok {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
				continue
			}
			if _, exists := workflowFiles[name]; exists {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- directory entry under a confined workflow dir
			if err != nil {
				continue
			}
			workflowFiles[name] = string(data)
			for _, match := range workflowFileRefRe.FindAllStringSubmatch(string(data), -1) {
				ref := filepath.Clean(match[1])
				if _, ok := contextFiles[ref]; ok {
					continue
				}
				f, err := root.Open(ref)
				if err != nil {
					continue
				}
				content, err := io.ReadAll(io.LimitReader(f, 1<<20))
				_ = f.Close()
				if err == nil {
					contextFiles[ref] = content
				}
			}
		}
	}
	return workflowFiles, contextFiles
}

// confinedPath resolves rel under workdir and reports whether it stays inside
// it (a configured ".." path must never read outside the checkout).
func confinedPath(workdir, rel string) (string, bool) {
	base, err := filepath.Abs(workdir)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(filepath.Join(base, rel))
	if err != nil {
		return "", false
	}
	if abs != base && !strings.HasPrefix(abs, fmt.Sprintf("%s%c", base, os.PathSeparator)) {
		return "", false
	}
	return abs, true
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

// checkColumnSep splits a table row on runs of 2+ spaces or any tab. Check
// names never contain whitespace, so a single space inside a description is
// preserved.
var checkColumnSep = regexp.MustCompile(`\s{2,}|\t+`)

// checkSeparatorRow matches the dashed/equals header separator emitted by the
// CLI's table renderer.
var checkSeparatorRow = regexp.MustCompile(`^[-=\s]+$`)

// checkTable is the parsed `dagger check -l` output: an optional normalized
// header (uppercase column names), the header column start offsets, and the
// data rows split into columns. The header is nil when the output has none
// (e.g. a headerless fixture).
type checkTable struct {
	header  []string
	offsets []int
	rows    [][]string
}

// parseCheckTable splits `dagger check -l` output into a header + rows. It
// tolerates both the pre-1.0 NAME table and the v1.0 multi-column table
// (MODULE, GO-PACKAGE, …, CHECK, DESCRIPTION).
//
// Space-aligned tables are sliced by the header column offsets rather than by
// runs of spaces: the v1.0 table leaves whole column families empty (a Go
// module has no VITEST-* values), and collapsing their padding would shift
// every later column — including CHECK. Tab-separated output has no padding to
// lose, so it is split on runs of whitespace.
func parseCheckTable(out string) checkTable {
	var t checkTable
	tabular := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" || checkSeparatorRow.MatchString(line) {
			continue
		}
		if t.header == nil {
			fields := checkColumnSep.Split(strings.TrimSpace(line), -1)
			if !isCheckHeader(fields) {
				t.rows = append(t.rows, fields)
				continue
			}
			t.header = make([]string, len(fields))
			for i, f := range fields {
				t.header[i] = strings.ToUpper(strings.TrimSpace(f))
			}
			if !strings.Contains(line, "\t") {
				t.offsets = columnOffsets(line, fields)
			}
			tabular = len(t.offsets) == 0
			continue
		}
		if tabular {
			t.rows = append(t.rows, checkColumnSep.Split(strings.TrimSpace(line), -1))
			continue
		}
		t.rows = append(t.rows, splitByOffsets(line, t.offsets))
	}
	return t
}

// columnOffsets returns the start index of each header field in line, in order.
// Nil when a field cannot be located again (caller falls back to splitting).
func columnOffsets(line string, fields []string) []int {
	offsets := make([]int, 0, len(fields))
	cursor := 0
	for _, f := range fields {
		i := strings.Index(line[cursor:], f)
		if i < 0 {
			return nil
		}
		start := cursor + i
		offsets = append(offsets, start)
		cursor = start + len(f)
	}
	return offsets
}

// splitByOffsets slices line into the header's columns. Missing trailing
// columns (a short row) yield empty values.
func splitByOffsets(line string, offsets []int) []string {
	out := make([]string, len(offsets))
	for i, start := range offsets {
		end := len(line)
		if i+1 < len(offsets) {
			end = offsets[i+1]
		}
		if start >= len(line) || end <= start {
			continue
		}
		if end > len(line) {
			end = len(line)
		}
		out[i] = strings.TrimSpace(line[start:end])
	}
	return out
}

// isCheckHeader reports whether a row is the table header: it carries a CHECK
// column (v1.0) or starts with the legacy NAME column.
func isCheckHeader(fields []string) bool {
	for i, f := range fields {
		upper := strings.ToUpper(strings.TrimSpace(f))
		if upper == "CHECK" || (i == 0 && upper == "NAME") {
			return true
		}
	}
	return false
}

// nameIndex returns the column that holds the check name: the CHECK column in
// the v1.0 table, otherwise the first column.
func (t checkTable) nameIndex() int {
	for i, c := range t.header {
		if c == "CHECK" {
			return i
		}
	}
	return 0
}

// parseCheckList returns the check names from `dagger check -l` output. When the
// header contains a CHECK column, that column is used; otherwise the first
// column is used. Rows shorter than the name column are skipped rather than
// misparsed.
func parseCheckList(out string) []string {
	t := parseCheckTable(out)
	idx := t.nameIndex()
	checks := make([]string, 0, len(t.rows))
	for _, fields := range t.rows {
		if idx >= len(fields) {
			continue
		}
		if name := strings.TrimSpace(fields[idx]); name != "" {
			checks = append(checks, name)
		}
	}
	return checks
}

// listChecks runs `dagger check -l` and parses the check-name column.
func listChecks(ctx context.Context, daggerBin, dir string, env []string) ([]string, error) {
	args := silentArgs([]string{"check", "-l"})
	//nolint:gosec // intentional: shell out to the dagger CLI with fixed args
	cmd := exec.CommandContext(ctx, daggerBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("dagger check -l: %w", err)
	}
	return parseCheckList(string(out)), nil
}

// checkFilterArgv validates the repeatable --filter values and returns the
// canonical "--key=value" forms appended to `dagger check`.
func checkFilterArgv(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		argv, err := domain.CheckFilterArgv(value)
		if err != nil {
			return nil, err
		}
		out = append(out, argv)
	}
	return out, nil
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
