package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
)

// workflowRunnerFlags configures the `workflow-runner` subcommand.
func workflowRunnerFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "run-id", Required: true, Usage: "workflow run id"},
		&cli.StringFlag{Name: "org", Usage: "organization id"},
		&cli.StringFlag{Name: "project", Usage: "project id"},
		&cli.StringFlag{Name: "job", Required: true, Usage: "job name"},
		&cli.StringFlag{Name: "call", Required: true, Usage: "dagger call expression"},
		&cli.StringFlag{Name: "repo", Usage: "repository slug"},
		&cli.StringFlag{Name: "clone-url", Usage: "full git clone URL (defaults to https://<repo>)"},
		&cli.StringFlag{Name: "sha", Usage: "commit SHA to check out"},
		&cli.StringFlag{Name: "ref", Usage: "git ref to check out"},
		&cli.StringSliceFlag{Name: "artifact", Usage: "artifact to upload as name=path (repeatable)"},
		&cli.StringSliceFlag{Name: "uses-artifact", Usage: "artifact key to download before running (repeatable)"},
		&cli.StringFlag{Name: "server", EnvVars: []string{"DAGGER_KUBERNETES_SERVER"}, Usage: "supervisor URL"},
		&cli.StringFlag{Name: "token", EnvVars: []string{"DAGGER_KUBERNETES_TOKEN", "DAGGER_CLOUD_TOKEN"}, Usage: "supervisor token"},
		&cli.StringFlag{Name: "workdir", Usage: "checkout directory (default: temp dir)"},
		&cli.DurationFlag{Name: "timeout", Value: 30 * time.Minute, Usage: "overall workflow job timeout"},
	}
}

// runWorkflowRunner clones the repo, downloads inputs, runs the Dagger call,
// uploads declared artifacts, and reports completion.
func runWorkflowRunner(c *cli.Context) error {
	ctx, cancel := context.WithTimeout(c.Context, c.Duration("timeout"))
	defer cancel()

	workdir := c.String("workdir")
	if workdir == "" {
		dir, err := os.MkdirTemp("", "dagger-workflow-*")
		if err != nil {
			return fmt.Errorf("create workdir: %w", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		workdir = dir
	}
	cloneURL := c.String("clone-url")
	if cloneURL == "" {
		cloneURL = fmt.Sprintf("https://%s", c.String("repo"))
	}
	if err := cloneRepo(ctx, cloneURL, c.String("sha"), c.String("ref"), workdir); err != nil {
		return err
	}
	server := c.String("server")
	token := c.String("token")
	for _, key := range c.StringSlice("uses-artifact") {
		if err := downloadArtifact(ctx, server, token, key, workdir); err != nil {
			return err
		}
	}
	env := append(os.Environ(),
		fmt.Sprintf("DAGGER_CLOUD_URL=%s", server),
		fmt.Sprintf("DAGGER_CLOUD_TOKEN=%s", token),
		"DAGGER_ENGINE=cloud",
	)
	if _, err := exec.LookPath("dagger"); err != nil {
		binDir, cleanup, perr := provisionCLI(ctx, server, token, "", runtime.GOOS, runtime.GOARCH)
		if perr != nil {
			return fmt.Errorf("provision dagger cli: %w", perr)
		}
		defer cleanup()
		env = append(env, fmt.Sprintf("PATH=%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH")))
	}
	args := strings.Fields(c.String("call"))
	cmd := exec.CommandContext(ctx, "dagger", append([]string{"call"}, args...)...) // #nosec G204 -- call expression comes from the repository's own workflow
	cmd.Dir = workdir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	artifacts := make([]string, 0)
	for _, spec := range c.StringSlice("artifact") {
		name, path, ok := strings.Cut(spec, "=")
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(workdir, filepath.Clean(path))) // #nosec G304 -- path comes from the repository's own workflow
		if err != nil {
			continue
		}
		key, err := uploadArtifact(ctx, server, token, c.String("org"), c.String("project"), c.String("run-id"), name, data)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, key)
	}
	report := map[string]any{"outputs": map[string]string{}, "artifacts": artifacts}
	if runErr != nil {
		report["error"] = runErr.Error()
	}
	if err := postJSON(ctx, server, token, fmt.Sprintf("/api/v1/workflows/runs/%s/jobs/%s/complete", c.String("run-id"), c.String("job")), report); err != nil {
		return fmt.Errorf("report workflow job completion: %w", err)
	}
	return runErr
}

// downloadArtifact fetches an artifact key into workdir.
func downloadArtifact(ctx context.Context, server, token, key, workdir string) error {
	u := fmt.Sprintf("%s/api/v1/artifacts/download?key=%s", strings.TrimSuffix(server, "/"), url.QueryEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("build artifact request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download artifact: status %d", resp.StatusCode)
	}
	name := filepath.Base(key)
	dst := filepath.Join(workdir, name)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- dst is a basename under the caller-owned workdir
	if err != nil {
		return fmt.Errorf("create artifact file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	return nil
}

// uploadArtifact stores an artifact and returns its key.
func uploadArtifact(ctx context.Context, server, token, org, project, runID, name string, data []byte) (string, error) {
	body, err := json.Marshal(map[string]any{
		"org": org, "project_id": project, "run_id": runID, "name": name, "data": data,
	})
	if err != nil {
		return "", fmt.Errorf("marshal artifact: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/artifacts", strings.TrimSuffix(server, "/")), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build artifact upload: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("upload artifact: status %d", resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode artifact response: %w", err)
	}
	return out.Key, nil
}
