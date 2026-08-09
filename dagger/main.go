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

	"dagger/dagger-cache/internal/dagger"
)

// chartDir is the single source of truth for the helm chart path.
const chartDir = "deploy/helm/dagger-kubernetes"

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

// Lint runs golangci-lint v2.12.2 against the Go source.
//
// It delegates to the golang module with a custom base container
// (golang:1.26 with golangci-lint v2.12.2 preinstalled) to preserve the CI pin.
func (m *DaggerCache) Lint(ctx context.Context) (string, error) {
	base := dag.Container().
		From("golang:1.26").
		WithExec([]string{
			"bash", "-c",
			fmt.Sprintf(
				"curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/main/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.12.2",
			),
		})

	g := dag.Golang(m.Src, dagger.GolangOpts{Base: base})

	out, err := g.Lint(ctx)
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
		From("golang:1.26").
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
// Mirrors the Dockerfile: node:22-alpine, npm ci || npm install, typecheck,
// build.
func (m *DaggerCache) Ui(ctx context.Context) (*dagger.Directory, error) {
	ctr := dag.Container().
		From("node:22-alpine").
		WithMountedDirectory("/ui", m.Src.Directory("ui")).
		WithWorkdir("/ui").
		WithExec([]string{"sh", "-c", "npm ci || npm install"}).
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

	supervisor := g.Build(dagger.GolangBuildOpts{
		Main: "./cmd/supervisor/",
		Out:  "bin/supervisor",
	})

	ci := g.Build(dagger.GolangBuildOpts{
		Main: "./cmd/dagger-cache-ci/",
		Out:  "bin/dagger-cache-ci",
	})

	bin := dag.Directory().
		WithFile("bin/supervisor", supervisor.File("bin/supervisor")).
		WithFile("bin/dagger-cache-ci", ci.File("bin/dagger-cache-ci"))

	return bin, nil
}

// Docker builds the root Dockerfile and runs a smoke test (-h).
//
// Implemented locally because the upstream golang module has no Dockerfile
// support. The image entrypoint is `supervisor`, so `-h` exercises the
// urfave/cli help path.
func (m *DaggerCache) Docker(ctx context.Context) (*dagger.Container, error) {
	ctr := m.Src.DockerBuild()

	ctr = ctr.WithExec([]string{"-h"}, dagger.ContainerWithExecOpts{UseEntrypoint: true})
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, fmt.Errorf("docker smoke test: %w", err)
	}

	return ctr, nil
}

// Helm lints the chart (delegated to the helm module) and runs the template
// matrix locally (alpine/helm:3.14.0) with the three --set combos from the
// original CI.
func (m *DaggerCache) Helm(ctx context.Context) error {
	chart := m.Src.Directory(chartDir)

	h := dag.Helm(chart)

	if _, err := h.Lint(ctx); err != nil {
		return fmt.Errorf("helm lint: %w", err)
	}

	templateMatrix := [][]string{
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

	for i, sets := range templateMatrix {
		cmd := []string{
			"helm", "template", "dagger-kubernetes", chartDir, "--debug",
		}
		cmd = append(cmd, sets...)

		ctr := dag.Container().
			From("alpine/helm:3.14.0").
			WithMountedDirectory("/src", m.Src).
			WithWorkdir("/src").
			WithExec(cmd)

		if _, err := ctr.Sync(ctx); err != nil {
			return fmt.Errorf("helm template variant %d: %w", i, err)
		}
	}

	return nil
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

	return dag.Directory().
		WithFile("bin/supervisor", bin.File("bin/supervisor")).
		WithFile("bin/dagger-cache-ci", bin.File("bin/dagger-cache-ci")).
		WithFile("coverage.out", coverage), nil
}
