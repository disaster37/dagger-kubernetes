package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

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

// runnerEnv builds the Dagger environment for a runner job. Both DAGGER_ENGINE
// and _EXPERIMENTAL_DAGGER_RUNNER_HOST are set because 0.21.x ignores
// DAGGER_ENGINE; the live-trace and org vars wire the pipeline view.
func runnerEnv(server, token, org string) []string {
	env := append(os.Environ(),
		fmt.Sprintf("DAGGER_CLOUD_URL=%s", server),
		fmt.Sprintf("DAGGER_CLOUD_TOKEN=%s", token),
		"DAGGER_ENGINE=cloud",
		"_EXPERIMENTAL_DAGGER_RUNNER_HOST=dagger-cloud://self",
		"OTEL_EXPORTER_OTLP_TRACES_LIVE=1",
	)
	if org != "" {
		env = append(env, fmt.Sprintf("DAGGER_ORG=%s", org))
	}
	return env
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
