package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

const (
	// runnerHostEnvName is the Dagger CLI variable that selects the engine driver.
	runnerHostEnvName = "_EXPERIMENTAL_DAGGER_RUNNER_HOST"
	// defaultRunnerHost keeps the CLI pointed at this supervisor.
	defaultRunnerHost = "dagger-cloud://self"
	// xReleaseEnvName is the CLI's release-override env (stripped by the re-exec).
	xReleaseEnvName = "DAGGER_X_RELEASE"
	// xReleaseMinCLI is the first CLI that preserves runnerHostEnvName across
	// the --x-release re-exec (dagger PR 13752).
	xReleaseMinCLI = "v0.21.8"
)

// cliVersionRe matches the semver token in `dagger version` output, covering
// both the 0.21.x single-line form and the 1.0 multi-line `version: …` form.
var cliVersionRe = regexp.MustCompile(`v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)

// resolveCloneURL derives the git clone URL for a runner. An explicit
// --clone-url always wins. A repo that is already a URL is returned unchanged.
// A host-qualified slug (host/owner/repo) becomes https://<slug>.git. A bare
// owner/repo has no derivable host and returns "" so the caller fails with a
// clear error instead of cloning https://owner/repo.
func resolveCloneURL(repo, cloneURL string) string {
	if cloneURL != "" {
		return cloneURL
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	if strings.Contains(repo, "://") {
		return repo
	}
	if strings.Count(repo, "/") < 2 {
		return ""
	}
	return fmt.Sprintf("https://%s.git", strings.TrimSuffix(repo, ".git"))
}

// daggerEnvOpts is the resolved environment for one CLI invocation.
type daggerEnvOpts struct {
	Server        string
	Token         string
	RunnerHost    string // "" = do not export runnerHostEnvName
	EngineVersion string // "" = do not export _EXPERIMENTAL_DAGGER_TAG
	Org           string
}

// daggerEnv upserts keys into base (last value wins, no duplicates) and
// appends OTEL_EXPORTER_OTLP_TRACES_LIVE=1. The live span export is required
// for the pipeline view: without it the engine exports spans only at
// completion, so per-step state never appears while a run is live.
func daggerEnv(base []string, o daggerEnvOpts) []string {
	env := upsertEnv(base, "DAGGER_CLOUD_URL", o.Server)
	env = upsertEnv(env, "DAGGER_CLOUD_TOKEN", o.Token)
	env = upsertEnv(env, "OTEL_EXPORTER_OTLP_TRACES_LIVE", "1")
	if o.RunnerHost != "" {
		env = upsertEnv(env, runnerHostEnvName, o.RunnerHost)
	}
	if o.EngineVersion != "" {
		env = upsertEnv(env, "_EXPERIMENTAL_DAGGER_TAG", o.EngineVersion)
	}
	if o.Org != "" {
		env = upsertEnv(env, "DAGGER_ORG", o.Org)
	}
	return env
}

// upsertEnv replaces the first "key=value" entry in env (dropping any
// duplicates) or appends it when absent.
func upsertEnv(env []string, key, value string) []string {
	prefix := fmt.Sprintf("%s=", key)
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			if replaced {
				continue
			}
			out = append(out, fmt.Sprintf("%s=%s", key, value))
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, fmt.Sprintf("%s=%s", key, value))
	}
	return out
}

// runnerEnv builds the Dagger environment for a runner job. Both DAGGER_ENGINE
// and the runner host are set because 0.21.x ignores DAGGER_ENGINE; the
// live-trace and org vars wire the pipeline view.
func runnerEnv(server, token, org, runnerHost string) []string {
	env := daggerEnv(os.Environ(), daggerEnvOpts{Server: server, Token: token, Org: org, RunnerHost: runnerHost})
	return upsertEnv(env, "DAGGER_ENGINE", "cloud")
}

// withSilentFlag prepends --silent (before the subcommand) unless the caller
// already asked for it. Idempotent.
func withSilentFlag(args []string, silent bool) []string {
	if !silent || hasFlag(args, "--silent") || hasFlag(args, "-s") {
		return args
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, "--silent")
	return append(out, args...)
}

// silentArgs prepends --silent when DAGGER_SILENT parses true, matching the
// CLI's strconv.ParseBool semantics (an unparseable value is not silent).
// Idempotent.
func silentArgs(args []string) []string {
	enabled, err := strconv.ParseBool(os.Getenv("DAGGER_SILENT"))
	if err != nil || !enabled {
		return args
	}
	return withSilentFlag(args, true)
}

// resolveRunnerHost returns the host to export, or "" when the caller passed an// explicit engine selector (--engine / --engine=) that must win on its own.
// v1.0 remote execution (SSH, Kubernetes, cloud) is a CLI-level selector; the
// wrapper must never override it.
func resolveRunnerHost(requested string, args []string) string {
	if hasFlag(args, "--engine") {
		return ""
	}
	return requested
}

// hasFlag reports whether args contain flag, either bare ("--engine") or with
// an inline value ("--engine=ssh://…").
func hasFlag(args []string, flag string) bool {
	inline := fmt.Sprintf("%s=", flag)
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, inline) {
			return true
		}
	}
	return false
}

// xReleaseRequested reports whether the invocation will take the x-release
// path, via the --x-release flag or the DAGGER_X_RELEASE env var.
func xReleaseRequested(args []string, env []string) bool {
	if hasFlag(args, "--x-release") {
		return true
	}
	prefix := fmt.Sprintf("%s=", xReleaseEnvName)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix) != ""
		}
	}
	return false
}

// cliSemver extracts "vX.Y.Z[-pre]" from `dagger version` output. Empty on any
// failure (missing binary, timeout, unparseable output).
func cliSemver(ctx context.Context, bin string, env []string) string {
	versionCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(versionCtx, bin, "version") // #nosec G204 -- bin is the dagger CLI resolved from PATH
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return cliVersionRe.FindString(string(out))
}

// checkXReleaseRunnerHost returns a typed error when the effective CLI would
// drop the runner host on the --x-release re-exec. Before 0.21.8 the re-exec
// strips runnerHostEnvName, so the inner CLI falls back to a LOCAL engine; on a
// CI agent without Docker that hangs at "connecting to engine".
func checkXReleaseRunnerHost(ctx context.Context, logger *logrus.Logger, bin string, args, env []string, runnerHost string) error {
	if !xReleaseRequested(args, env) {
		return nil
	}
	if runnerHost == "" {
		// The caller chose an explicit engine selector; there is no host to
		// lose on the re-exec.
		return nil
	}
	raw := cliSemver(ctx, bin, env)
	if raw == "" {
		logger.WithField("reason", "cli_version_unknown").Warn("x-release runner host not verified")
		return nil
	}
	cliVer, err := domain.Parse(raw)
	if err != nil {
		logger.WithField("reason", "cli_version_unknown").Warn("x-release runner host not verified")
		return nil
	}
	minVer, err := domain.Parse(xReleaseMinCLI)
	if err != nil {
		return fmt.Errorf("parse minimum x-release CLI version: %w", err)
	}
	if cliVer.Compare(minVer) < 0 {
		return fmt.Errorf("--x-release with %s on dagger CLI %s drops %s before the re-exec (fixed in dagger %s); upgrade the CLI (--cli-version %s+ or the provisioned CLI) or drop --x-release",
			xReleaseEnvName, cliVer, runnerHostEnvName, xReleaseMinCLI, xReleaseMinCLI)
	}
	logger.WithFields(logrus.Fields{
		"cli_version": raw,
		"runner_host": runnerHost,
		"x_release":   true,
	}).Info("x-release preserves the runner host")
	return nil
}

// resolveDaggerBin returns the ABSOLUTE path to the dagger binary plus any
// extra environment needed to run it, and a cleanup func. exec.LookPath ignores
// cmd.Env, so a provisioned binary must be addressed by absolute path rather
// than via PATH.
func resolveDaggerBin(ctx context.Context, server, token string) (bin string, extraEnv []string, cleanup func(), err error) {
	if path, lerr := exec.LookPath("dagger"); lerr == nil {
		return path, nil, func() {}, nil
	}
	binDir, provisionCleanup, perr := provisionCLI(ctx, server, token, "", runtime.GOOS, runtime.GOARCH)
	if perr != nil {
		return "", nil, nil, fmt.Errorf("provision dagger cli: %w", perr)
	}
	extraEnv = []string{fmt.Sprintf("PATH=%s%c%s", binDir, os.PathListSeparator, os.Getenv("PATH"))}
	return filepath.Join(binDir, "dagger"), extraEnv, provisionCleanup, nil
}
