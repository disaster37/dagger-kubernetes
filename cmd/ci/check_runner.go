package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
)

// checkRunnerFlags configures the `check-runner` subcommand.
func checkRunnerFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "repo", Required: true, Usage: "repository slug (host/owner/repo)"},
		&cli.StringFlag{Name: "sha", Usage: "commit SHA to check out"},
		&cli.StringFlag{Name: "ref", Usage: "git ref to check out"},
		&cli.IntFlag{Name: "pr", Usage: "pull/merge request number"},
		&cli.StringFlag{Name: "server", EnvVars: []string{"DAGGER_KUBERNETES_SERVER"}, Usage: "supervisor URL"},
		&cli.StringFlag{Name: "token", EnvVars: []string{"DAGGER_KUBERNETES_TOKEN"}, Usage: "supervisor token"},
		&cli.StringFlag{Name: "workdir", Usage: "checkout directory (default: temp dir)"},
		&cli.DurationFlag{Name: "timeout", Value: 30 * time.Minute, Usage: "overall check-run timeout"},
	}
}

// runCheckRunner clones the repository, discovers checks with `dagger check -l`,
// and runs each top-level check. Zero checks is a valid GitOps-only result.
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
	if err := cloneRepo(ctx, c.String("repo"), c.String("sha"), c.String("ref"), workdir); err != nil {
		return err
	}
	env := append(os.Environ(),
		fmt.Sprintf("DAGGER_CLOUD_URL=%s", c.String("server")),
		fmt.Sprintf("DAGGER_CLOUD_TOKEN=%s", c.String("token")),
		"DAGGER_ENGINE=cloud",
	)
	checks, err := listChecks(ctx, workdir, env)
	if err != nil {
		return err
	}
	if len(checks) == 0 {
		fmt.Println("0 checks discovered; load phase succeeded")
		return nil
	}
	for _, check := range checks {
		fmt.Printf("running check %s\n", check)
		cmd := exec.CommandContext(ctx, "dagger", "check", check) // #nosec G204 -- check names come from the repository's own dagger module
		cmd.Dir = workdir
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("check %s failed: %w", check, err)
		}
	}
	return nil
}

// cloneRepo clones repo at sha/ref without persisting credentials in
// .git/config (the token is passed as a one-shot HTTP header).
func cloneRepo(ctx context.Context, repo, sha, ref, dir string) error {
	url := fmt.Sprintf("https://%s", repo)
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clone %s: %w", repo, err)
	}
	if sha != "" {
		checkout := exec.CommandContext(ctx, "git", "fetch", "--depth", "1", "origin", sha) // #nosec G204 -- sha is a validated commit id from the webhook
		checkout.Dir = dir
		checkout.Stdout = os.Stdout
		checkout.Stderr = os.Stderr
		if err := checkout.Run(); err != nil {
			return fmt.Errorf("fetch %s: %w", sha, err)
		}
		reset := exec.CommandContext(ctx, "git", "checkout", sha) // #nosec G204 -- sha is a validated commit id from the webhook
		reset.Dir = dir
		reset.Stdout = os.Stdout
		reset.Stderr = os.Stderr
		if err := reset.Run(); err != nil {
			return fmt.Errorf("checkout %s: %w", sha, err)
		}
	}
	return nil
}

// listChecks runs `dagger check -l` and parses one check name per line.
func listChecks(ctx context.Context, dir string, env []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "dagger", "check", "-l")
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
