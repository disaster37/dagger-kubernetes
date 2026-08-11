// dagger-cache is the local Dagger CI module for the dagger-kubernetes project.
//
// It delegates lint and build to the golang module and helm lint to the helm
// module from github.com/disaster37/dagger-library-go (pinned at 2.0.10). Test,
// UI, docker, and the helm template matrix are implemented locally because the
// upstream modules cannot express -race, the UI build, the Dockerfile smoke
// test, or helm template.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/dagger-cache/internal/dagger"
)

const (
	// chartDir is the single source of truth for the helm chart path.
	chartDir = "deploy/helm/dagger-kubernetes"

	// Pinned tool images and versions. Keep in sync with DAGGER.md.
	golangImage         = "golang:1.26"
	nodeImage           = "node:22-alpine"
	helmImage           = "alpine/helm:3.14.0"
	golangciLintVersion = "v2.12.2"
)

// binaries lists the Go binaries produced by Build.
var binaries = []struct {
	main string
	out  string
}{
	{main: "./cmd/api/", out: "bin/supervisor"},
	{main: "./cmd/ci/", out: "bin/dagger-cache-ci"},
}

// helmTemplateMatrix lists the --set combinations from the original CI.
var helmTemplateMatrix = [][]string{
	{},
	{"--set", "tools.otelCollector.enabled=false", "--set", "tools.registry.enabled=false"},
	{
		"--set", "tools.otelCollector.enabled=false",
		"--set", "tools.registry.enabled=false",
		"--set", "tools.tempo.enabled=false",
		"--set", "tools.loki.enabled=false",
		"--set", "tools.victoria.enabled=false",
		"--set", "tools.grafana.enabled=false",
	},
}

// DaggerCache is the root type for the dagger-cache module.
type DaggerCache struct {
	// Src is the repository root provided as source.
	Src *dagger.Directory
}

// New initializes the dagger-cache module with the repository root as source.
func New(
	// The repository root directory.
	src *dagger.Directory,
) *DaggerCache {
	return &DaggerCache{Src: src}
}

// Lint runs golangci-lint against the Go source.
//
// It delegates to the golang module with a custom base container that has
// golangci-lint preinstalled, preserving the CI version pin.
func (m *DaggerCache) Lint(ctx context.Context) (string, error) {
	install := fmt.Sprintf(
		"curl -sSfL https://github.com/golangci/golangci-lint/releases/download/%s/golangci-lint-%s-linux-amd64.tar.gz -o /tmp/golangci-lint.tar.gz && tar -C $(go env GOPATH)/bin -xzf /tmp/golangci-lint.tar.gz --strip-components=1 golangci-lint-%s-linux-amd64/golangci-lint && rm /tmp/golangci-lint.tar.gz",
		golangciLintVersion, strings.TrimPrefix(golangciLintVersion, "v"), strings.TrimPrefix(golangciLintVersion, "v"),
	)
	base := dag.Container().
		From(golangImage).
		WithExec([]string{"bash", "-c", install})

	out, err := dag.Golang(m.Src, dagger.GolangOpts{Base: base}).Lint(ctx)
	if err != nil {
		return "", fmt.Errorf("golangci-lint: %w", err)
	}
	return out, nil
}

// Test runs go vet and go test -race with coverage.
//
// Implemented locally because the upstream golang module hardcodes test flags
// (no -race, -vet=off). -race requires CGO, so a debian (not alpine) image is
// used and CGO_ENABLED is not set to 0.
func (m *DaggerCache) Test(ctx context.Context) (*dagger.File, error) {
	ctr := dag.Container().
		From(golangImage).
		WithMountedDirectory("/src", m.Src).
		WithWorkdir("/src").
		WithExec([]string{"go", "vet", "./..."}).
		WithExec([]string{
			"go", "test",
			"-race",
			"-coverprofile=coverage.out",
			"-covermode=atomic",
			"./...",
		})

	if _, err := ctr.Sync(ctx); err != nil {
		return nil, fmt.Errorf("go test: %w", err)
	}

	return ctr.File("coverage.out"), nil
}

// Ui builds the Vue 3 SPA and returns the dist/ directory.
//
// Implemented locally because the upstream golang module has no UI support.
// Mirrors the Dockerfile: npm ci || npm install, typecheck, build.
func (m *DaggerCache) Ui(ctx context.Context) (*dagger.Directory, error) {
	ctr := dag.Container().
		From(nodeImage).
		WithMountedDirectory("/ui", m.Src.Directory("ui")).
		WithWorkdir("/ui").
		WithExec([]string{"npm", "ci"}).
		WithExec([]string{"npm", "run", "typecheck"}).
		WithExec([]string{"npm", "run", "build"})

	if _, err := ctr.Sync(ctx); err != nil {
		return nil, fmt.Errorf("ui build: %w", err)
	}

	return ctr.Directory("dist"), nil
}

// Build compiles both Go binaries (supervisor and dagger-cache-ci) and returns
// a directory containing them under bin/.
//
// Delegates to the golang module's Build function (CGO_ENABLED=0, default
// ldflags ["-s","-w"]) for each binary, then merges both into one directory.
func (m *DaggerCache) Build(ctx context.Context) (*dagger.Directory, error) {
	g := dag.Golang(m.Src)

	bin := dag.Directory()
	for _, b := range binaries {
		out := g.Build(dagger.GolangBuildOpts{Main: b.main, Out: b.out})
		bin = bin.WithFile(b.out, out.File(b.out))
	}

	return bin, nil
}

// Docker builds the root Dockerfile and runs a smoke test (-h).
//
// Implemented locally because the upstream golang module has no Dockerfile
// support. The image entrypoint is `supervisor`, so `-h` exercises the
// urfave/cli help path.
func (m *DaggerCache) Docker(ctx context.Context) (*dagger.Container, error) {
	ctr := m.Src.DockerBuild().
		WithExec([]string{"-h"}, dagger.ContainerWithExecOpts{UseEntrypoint: true})

	if _, err := ctr.Sync(ctx); err != nil {
		return nil, fmt.Errorf("docker smoke test: %w", err)
	}

	return ctr, nil
}

// Helm lints the chart (delegated to the helm module) and runs the template
// matrix locally with the three --set combos from the original CI.
func (m *DaggerCache) Helm(ctx context.Context) error {
	chart := m.Src.Directory(chartDir)

	if _, err := dag.Helm(chart).Lint(ctx); err != nil {
		return fmt.Errorf("helm lint: %w", err)
	}

	// The subchart archives are gitignored, so fetch the dependencies first;
	// each template variant runs in a fresh container without charts/.
	base := dag.Container().
		From(helmImage).
		WithMountedDirectory("/src", m.Src).
		WithWorkdir("/src").
		WithExec([]string{"helm", "dependency", "update", chartDir})

	for i, sets := range helmTemplateMatrix {
		cmd := append([]string{"helm", "template", "dagger-kubernetes", chartDir, "--debug"}, sets...)
		if _, err := base.WithExec(cmd).Sync(ctx); err != nil {
			return fmt.Errorf("helm template variant %d: %w", i, err)
		}
	}

	return nil
}

// Publish builds the Docker image and pushes it to GHCR with the given
// version tag. Registry credentials are required for authentication.
//
// Returns the fully-qualified image reference with digest on success.
func (m *DaggerCache) Publish(
	ctx context.Context,
	// semver release tag (e.g. "v0.0.1-alpha4")
	// +required
	version string,
	// registry address for authentication (e.g. "ghcr.io")
	// +optional
	registry string,
	// registry username for authentication
	// +required
	registryUsername string,
	// registry password for authentication
	// +required
	registryPassword *dagger.Secret,
) (string, error) {
	const defaultRegistry = "ghcr.io"
	if registry == "" {
		registry = defaultRegistry
	}
	tag := strings.TrimPrefix(version, "v")
	addr := fmt.Sprintf("%s/%s:%s", registry, "disaster/dagger-kubernetes", tag)

	if _, err := m.Lint(ctx); err != nil {
		return "", fmt.Errorf("lint: %w", err)
	}
	if _, err := m.Test(ctx); err != nil {
		return "", fmt.Errorf("test: %w", err)
	}
	if _, err := m.Ui(ctx); err != nil {
		return "", fmt.Errorf("ui: %w", err)
	}

	ctr := m.Src.DockerBuild().
		WithRegistryAuth(defaultRegistry, registryUsername, registryPassword)

	digest, err := ctr.Publish(ctx, addr)
	if err != nil {
		return "", fmt.Errorf("docker publish: %w", err)
	}
	return digest, nil
}

// Ci runs the full pipeline: Lint, Test, Ui, Build, Docker, Helm.
//
// Returns a directory containing bin/supervisor, bin/dagger-cache-ci, and
// coverage.out.
func (m *DaggerCache) Ci(ctx context.Context) (*dagger.Directory, error) {
	if _, err := m.Lint(ctx); err != nil {
		return nil, err
	}

	coverage, err := m.Test(ctx)
	if err != nil {
		return nil, err
	}

	if _, err := m.Ui(ctx); err != nil {
		return nil, err
	}

	bin, err := m.Build(ctx)
	if err != nil {
		return nil, err
	}

	if _, err := m.Docker(ctx); err != nil {
		return nil, err
	}

	if err := m.Helm(ctx); err != nil {
		return nil, err
	}

	return bin.WithFile("coverage.out", coverage), nil
}
