// dagger-kubernetes is the local Dagger CI module for the dagger-kubernetes project.
//
// It delegates lint and build to the golang module, helm lint to the helm
// module, and the publish push to the image module, all from
// github.com/disaster37/dagger-library-go (golang/helm pinned at 2.0.12, image
// at 2.0.19). Test, UI, docker, and the helm template matrix are implemented
// locally because the upstream modules cannot express -race, the UI build, the
// Dockerfile smoke test, or helm template.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/dagger-kubernetes/internal/dagger"
	"dagger/dagger-kubernetes/internal/ref"
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
	{main: "./cmd/ci/", out: "bin/dagger-kubernetes-ci"},
}

// helmVariant is one helm template matrix case: the --set arguments to render
// with, plus substrings that must appear in the rendered manifests. A variant
// with no expectations only has to render successfully; a variant with
// expectations additionally proves the rendered auto-wired URLs are correct
// (a successful render alone would mask a URL that silently points at a
// renamed — or nonexistent — dependency Service). Substrings stop right after
// the Service name (before the namespace) so they stay stable across
// environments, because helm template picks up the kubeconfig's namespace.
type helmVariant struct {
	sets   []string
	expect []string
}

// helmTemplateMatrix lists the --set combinations from the original CI.
// The TLS variants exercise the three data-plane server-certificate cases
// (embedded default, cert-manager via dataCert, custom secret via
// dataIngress.tls.secretName) plus the external-provider keypair rendering.
// The default and subchart-rename variants assert the rendered URLs: the
// default pins each dependency's fullname-derived default name (including
// <release>-victoria-server), and the rename variant renames
// tempo/loki/victoria/minio/opentelemetry-collector and, in lock step, sets
// the global.daggerKubernetes.serviceNames.* keys the collector exporters
// follow — proving the auto-wired URLs track each dependency's fullname rules.
var helmTemplateMatrix = []helmVariant{
	{
		sets: []string{},
		expect: []string{
			`collector_url: "http://dagger-kubernetes-opentelemetry-collector.`,
			`tempo_url: "http://dagger-kubernetes-tempo.`,
			`loki_url: "http://dagger-kubernetes-loki.`,
			`victoria_url: "http://dagger-kubernetes-victoria-server.`,
			`endpoint: "dagger-kubernetes-minio.`,
		},
	},
	{sets: []string{"--set", "supervisor.enabled=false"}},
	{sets: []string{"--set", "opentelemetry-collector.enabled=false", "--set", "minio.enabled=false"}},
	{
		sets: []string{
			"--set", "opentelemetry-collector.enabled=false",
			"--set", "minio.enabled=false",
			"--set", "tempo.enabled=false",
			"--set", "loki.enabled=false",
			"--set", "victoria.enabled=false",
			"--set", "grafana.enabled=false",
		},
	},
	{
		sets: []string{
			"--set", "dataIngress.enabled=true",
			"--set", "dataIngress.host=data.example.com",
			"--set", "dataCert.enabled=true",
			"--set", "dataCert.issuerName=letsencrypt-prod",
		},
	},
	{
		sets: []string{
			"--set", "dataIngress.enabled=true",
			"--set", "dataIngress.host=data.example.com",
			"--set", "dataIngress.tls.secretName=dataplane-certs",
		},
	},
	{
		sets: []string{
			"--set", "supervisor.dataplane.tls.provider=external",
			"--set", "supervisor.dataplane.tls.crt=EXTERNAL_CRT",
			"--set", "supervisor.dataplane.tls.key=EXTERNAL_KEY",
		},
	},
	{
		sets: []string{
			"--set", "tempo.fullnameOverride=my-tempo",
			"--set", "loki.fullnameOverride=my-loki",
			"--set", "victoria.server.fullnameOverride=my-victoria",
			"--set", "minio.fullnameOverride=my-minio",
			"--set", "opentelemetry-collector.fullnameOverride=my-otel",
			"--set", "global.daggerKubernetes.serviceNames.tempo=my-tempo",
			"--set", "global.daggerKubernetes.serviceNames.loki=my-loki",
			"--set", "global.daggerKubernetes.serviceNames.victoria=my-victoria",
		},
		expect: []string{
			`collector_url: "http://my-otel.`,
			`tempo_url: "http://my-tempo.`,
			`loki_url: "http://my-loki.`,
			`victoria_url: "http://my-victoria.`,
			`endpoint: "my-minio.`,
			`endpoint: http://my-tempo.`,
			`endpoint: http://my-loki.`,
			`endpoint: http://my-victoria.`,
		},
	},
}

// DaggerKubernetes is the root type for the dagger-kubernetes module.
type DaggerKubernetes struct {
	// Src is the repository root provided as source.
	Src *dagger.Directory
}

// New initializes the dagger-kubernetes module with the repository root as source.
func New(
	// The repository root directory.
	src *dagger.Directory,
) *DaggerKubernetes {
	return &DaggerKubernetes{Src: src}
}

// Lint runs golangci-lint against the Go source.
//
// It delegates to the golang module with a custom base container that has
// golangci-lint preinstalled, preserving the CI version pin.
func (m *DaggerKubernetes) Lint(ctx context.Context) (string, error) {
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
func (m *DaggerKubernetes) Test(ctx context.Context) (*dagger.File, error) {
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

// Ui builds the Nuxt 4 + Nuxt UI v4 SPA and returns the .output/public/ directory.
//
// Implemented locally because the upstream golang module has no UI support.
// Mirrors the Dockerfile: npm ci, typecheck, build (nuxt generate).
func (m *DaggerKubernetes) Ui(ctx context.Context) (*dagger.Directory, error) {
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

	return ctr.Directory(".output/public"), nil
}

// Build compiles both Go binaries (supervisor and dagger-kubernetes-ci) and returns
// a directory containing them under bin/.
//
// Delegates to the golang module's Build function (CGO_ENABLED=0, default
// ldflags ["-s","-w"]) for each binary, then merges both into one directory.
func (m *DaggerKubernetes) Build(ctx context.Context) (*dagger.Directory, error) {
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
func (m *DaggerKubernetes) Docker(ctx context.Context) (*dagger.Container, error) {
	ctr := m.Src.DockerBuild().
		WithExec([]string{"-h"}, dagger.ContainerWithExecOpts{UseEntrypoint: true})

	if _, err := ctr.Sync(ctx); err != nil {
		return nil, fmt.Errorf("docker smoke test: %w", err)
	}

	return ctr, nil
}

// Helm lints the chart (delegated to the helm module) and runs the template
// matrix locally: every variant must render, and variants carrying
// expectations must also contain the asserted URL substrings.
func (m *DaggerKubernetes) Helm(ctx context.Context) error {
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

	for i, variant := range helmTemplateMatrix {
		cmd := append([]string{"helm", "template", "dagger-kubernetes", chartDir, "--debug"}, variant.sets...)
		ctr := base.WithExec(cmd)
		if _, err := ctr.Sync(ctx); err != nil {
			return fmt.Errorf("helm template variant %d: %w", i, err)
		}
		if len(variant.expect) == 0 {
			continue
		}
		out, err := ctr.Stdout(ctx)
		if err != nil {
			return fmt.Errorf("helm template variant %d output: %w", i, err)
		}
		for _, want := range variant.expect {
			if !strings.Contains(out, want) {
				return fmt.Errorf("helm template variant %d: rendered output does not contain %q", i, want)
			}
		}
	}

	return nil
}

// Publish builds the root Docker image and pushes it to a container registry
// (GHCR by default) under the given tag. It reuses Docker (Dockerfile build +
// `-h` smoke test), optionally runs the full quality gate, then delegates the
// push to the disaster37/dagger-library-go image module (2.0.19). Returns the
// published image reference including its digest.
//
// For GHCR, pass --registry-username env:GHCR_USERNAME and
// --registry-password env:GHCR_TOKEN (a PAT with write:packages).
func (m *DaggerKubernetes) Publish(
	ctx context.Context,
	// Image tag to publish under (e.g. "dev", "v0.1.0", or a git SHA). The image
	// dependency normalizes semver tags ("v0.1.0" → "0.1.0"); non-semver tags
	// pass through verbatim.
	// +required
	tag string,
	// Registry host (no scheme) to push to (e.g. "ghcr.io").
	// +optional
	registry string,
	// Image repository (no registry host), e.g. "disaster37/dagger-kubernetes".
	// +optional
	image string,
	// Registry username (GHCR: your GitHub username), passed as a Secret.
	// +optional
	registryUsername *dagger.Secret,
	// Registry password/token (GHCR: a PAT with write:packages), as a Secret.
	// +optional
	registryPassword *dagger.Secret,
	// Run the full quality gate (Lint + Test + Ui) before pushing.
	// +optional
	gates bool,
) (string, error) {
	const (
		defaultRegistry = "ghcr.io"
		defaultImage    = "disaster37/dagger-kubernetes"
	)
	if registry == "" {
		registry = defaultRegistry
	}
	if image == "" {
		image = defaultImage
	}

	if err := ref.Validate(registry, image, tag); err != nil {
		return "", err
	}

	if (registryUsername == nil) != (registryPassword == nil) {
		return "", fmt.Errorf("registry credentials: registryUsername and registryPassword must be provided together")
	}

	if gates {
		if _, err := m.Lint(ctx); err != nil {
			return "", fmt.Errorf("lint: %w", err)
		}
		if _, err := m.Test(ctx); err != nil {
			return "", fmt.Errorf("test: %w", err)
		}
		if _, err := m.Ui(ctx); err != nil {
			return "", fmt.Errorf("ui: %w", err)
		}
	}

	ctr, err := m.Docker(ctx)
	if err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}

	build := dag.Image(dagger.ImageOpts{BuildContainer: ctr}).
		Build(m.Src, dagger.ImageBuildOpts{Dockerfile: "Dockerfile"})

	digest, err := build.Push(ctx, image, tag, registry, dagger.ImageBuildPushOpts{
		WithRegistryUsername: registryUsername,
		WithRegistryPassword: registryPassword,
	})
	if err != nil {
		return "", fmt.Errorf("docker publish: %w", err)
	}
	return digest, nil
}

// Ci runs the full pipeline: Lint, Test, Ui, Build, Docker, Helm.
//
// Returns a directory containing bin/supervisor, bin/dagger-kubernetes-ci, and
// coverage.out.
func (m *DaggerKubernetes) Ci(ctx context.Context) (*dagger.Directory, error) {
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
