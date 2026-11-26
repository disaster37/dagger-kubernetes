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

	"dagger/dagger-kubernetes/internal/artifact"
	"dagger/dagger-kubernetes/internal/dagger"
	"dagger/dagger-kubernetes/internal/ref"
)

const (
	// chartDir is the single source of truth for the helm chart path.
	chartDir = "deploy/helm/dagger-kubernetes"

	// jenkinsLibsDir is the single source of truth for the Jenkins shared
	// library source directory packaged by JenkinsLibs.
	jenkinsLibsDir = "ci-integrations/jenkins"

	nodeImage = "node:22-alpine"
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
// with, plus substrings that must appear (expect) or must NOT appear (forbid)
// in the rendered manifests. A variant with no expectations only has to render
// successfully; a variant with expectations additionally proves the rendered
// auto-wired URLs are correct (a successful render alone would mask a URL that
// silently points at a renamed — or nonexistent — dependency Service).
// Substrings stop right after the Service name (before the namespace) so they
// stay stable across environments, because helm template picks up the
// kubeconfig's namespace. `forbid` proves a resource is NOT rendered (e.g. the
// shared-Gateway variant must not emit a Gateway).
type helmVariant struct {
	sets   []string
	expect []string
	forbid []string
}

// helmTemplateMatrix lists the --set combinations from the original CI.
// The TLS variants exercise the three data-plane server-certificate cases
// (embedded default, cert-manager via dataCert, custom secret via
// dataIngress.tls.secretName) plus the external-provider keypair rendering.
// The default and subchart-rename variants assert the rendered URLs: the
// default pins each dependency's fullname-derived default name (including
// <release>-victoria-server) — both the supervisor ConfigMap URLs and the
// collector's own exporter endpoints under their default Service names — and
// the rename variant renames tempo/loki/victoria/minio/opentelemetry-collector
// and, in lock step, sets the global.daggerKubernetes.serviceNames.* keys the
// collector exporters follow — proving the auto-wired URLs track each
// dependency's fullname rules. The final variant enables the optional
// dex/openldap subcharts (disabled by default) and asserts the Dex Deployment
// renders.
var helmTemplateMatrix = []helmVariant{
	{
		sets: []string{},
		expect: []string{
			`collector_url: "http://dagger-kubernetes-opentelemetry-collector.`,
			`tempo_url: "http://dagger-kubernetes-tempo.`,
			`loki_url: "http://dagger-kubernetes-loki.`,
			`victoria_url: "http://dagger-kubernetes-victoria-server.`,
			`endpoint: "dagger-kubernetes-minio.`,
			`endpoint: http://dagger-kubernetes-tempo.`,
			`endpoint: http://dagger-kubernetes-loki.`,
			`endpoint: http://dagger-kubernetes-victoria-server.`,
			`relay: |`,
			`- key: relay`,
			`path: relay.yaml`,
			`job_image: "ghcr.io/disaster37/dagger-kubernetes:`,
			`job_image_pull_secrets: []`,
			"app.kubernetes.io/component: supervisor",
			"targetPort: 8080",
			"targetPort: 8443",
			"targetPort: 8081",
			// Exact supervisor Service selector block: proves the Services
			// select on component: supervisor (not name+instance alone), the
			// regression this fix addresses.
			"selector:\n    app.kubernetes.io/name: dagger-kubernetes\n    app.kubernetes.io/instance: dagger-kubernetes\n    app.kubernetes.io/component: supervisor",
			// The public Minting CA trust-bundle ConfigMap is rendered by
			// default (supervisor.enabled) and wired through ca.minting_ca_configmap.
			`minting_ca_configmap: "dagger-kubernetes-dagger-kubernetes-minting-ca-bundle"`,
			"name: dagger-kubernetes-dagger-kubernetes-minting-ca-bundle",
		},
	},
	{
		// Pull secrets render as a plain string list of names (the config
		// contract), derived from supervisor.image.pullSecrets[].name.
		sets: []string{"--set", "supervisor.image.pullSecrets[0].name=ghcr-pull"},
		expect: []string{
			"job_image_pull_secrets:\n        - \"ghcr-pull\"",
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
	// Optional Dex/OpenLDAP subcharts (both disabled by default): prove the
	// condition-gated dependencies render when enabled (the default variant
	// proves they stay out when disabled).
	{
		sets:   []string{"--set", "dex.enabled=true", "--set", "openldap.enabled=true"},
		expect: []string{"name: dagger-kubernetes-dex"},
	},
	// F5 NGINX Ingress Controller data-plane passthrough: a TransportServer
	// replaces the data Ingress, so cert-manager renewal must come from
	// dataCert. Also proves the supervisor component selector keeps the
	// image-cache mirror pods out of the supervisor Services/PDB/ServiceMonitor.
	{
		sets: []string{
			"--set", "dataIngress.enabled=true",
			"--set", "dataIngress.host=data.example.com",
			"--set", "dataIngress.passthroughVia=transportServer",
			"--set", "dataCert.enabled=true",
			"--set", "imageCache.enabled=true",
			"--set", "serviceMonitor.enabled=true",
		},
		expect: []string{
			"kind: TransportServer",
			"protocol: TLS_PASSTHROUGH",
			"kind: Certificate",
			"app.kubernetes.io/component: supervisor",
			"app.kubernetes.io/component: image-cache",
			// Generous idle timeout so the controller's 10m default does not
			// cut long-lived idle engine sessions.
			"sessionParameters:",
			`timeout: "24h"`,
		},
	},
	// Gateway API exposition (NGINX Gateway Fabric): a dedicated Gateway with
	// HTTP/HTTPS/TLS listeners, HTTPRoute/TLSRoute/BackendTLSPolicy, and the
	// NGF v1alpha1 policies, alongside the dataCert Certificate and the
	// registry mirror. Proves the rendered URLs follow the gateway precedence
	// (public_url from gateway.control.host, data_hostname from
	// gateway.data.host, external_addr from gateway.registryMirror.host) and
	// that the data Cert SAN follows gateway.data.host.
	{
		sets: []string{
			"--set", "ingress.enabled=false",
			"--set", "gateway.enabled=true",
			"--set", `gateway.annotations.cert-manager\.io/cluster-issuer=letsencrypt-prod`,
			"--set", "gateway.control.host=supv.example.com",
			"--set", "gateway.data.enabled=true",
			"--set", "gateway.data.host=data.example.com",
			"--set", "dataCert.enabled=true",
			"--set", "imageCache.enabled=true",
			"--set", `imageCache.presets.registry\.dagger\.io.enabled=true`,
			"--set", "imageCache.tls.enabled=true",
			"--set", "imageCache.tls.secretName=mirror-tls",
			"--set", "imageCache.tls.caSecretName=mirror-ca",
			"--set", "gateway.registryMirror.enabled=true",
			"--set", "gateway.registryMirror.host=mirror.example.com",
			"--set", "dex.enabled=true",
			"--set", "dex.ingress.enabled=false",
			"--set", "gateway.dex.enabled=true",
			"--set", "gateway.dex.host=dex.example.com",
		},
		expect: []string{
			"kind: Gateway",
			`gatewayClassName: "nginx"`,
			"mode: Passthrough",
			"kind: HTTPRoute",
			// TLSRoute must be GA v1 (Gateway API v1.7.0 serves v1 only).
			"apiVersion: gateway.networking.k8s.io/v1\nkind: TLSRoute",
			"kind: BackendTLSPolicy",
			"kind: ClientSettingsPolicy",
			"kind: ProxySettingsPolicy",
			"kind: SnippetsPolicy",
			`context: "stream.server"`,
			`value: "proxy_timeout 24h;"`,
			`public_url: "https://supv.example.com"`,
			`data_hostname: "data.example.com"`,
			`external_addr: "mirror.example.com"`,
			`- "data.example.com"`,
			"-control.",
			// HTTP->HTTPS redirect route and the mirror BackendTLSPolicy
			// hostname (the mirror Service) plus the Dex route host.
			"statusCode: 301",
			"-registry-dagger-io-mirror.",
			"dex.example.com",
			// Control BackendTLSPolicy reads the Core ConfigMap bundle the
			// supervisor syncs; the mirror policy reads the explicit/default
			// mirror CA ConfigMap.
			`kind: "ConfigMap"`,
			`name: "dagger-kubernetes-dagger-kubernetes-minting-ca-bundle"`,
			`name: "mirror-ca"`,
		},
		forbid: []string{
			"gateway.networking.k8s.io/v1alpha2",
			"gateway.networking.k8s.io/v1alpha3",
			"gateway.networking.k8s.io/v1beta1",
		},
	},
	// Shared-Gateway mode: no dedicated Gateway is rendered; every route
	// attaches to the existing Gateway named by gateway.parentRef (cross
	// namespace), and the NGF-specific policies are disabled for portability
	// to other Gateway API implementations. The forbid list proves neither a
	// Gateway nor a SnippetsPolicy leaks into the shared-mode render.
	{
		sets: []string{
			"--set", "ingress.enabled=false",
			"--set", "gateway.enabled=true",
			"--set", "gateway.create=false",
			"--set", "gateway.parentRef.name=shared-gw",
			"--set", "gateway.parentRef.namespace=gateway-system",
			"--set", "gateway.nginxPolicies.enabled=false",
			"--set", "gateway.data.enabled=true",
			"--set", "gateway.data.host=data.example.com",
		},
		expect: []string{
			`name: "shared-gw"`,
			`namespace: "gateway-system"`,
			"kind: HTTPRoute",
			"kind: TLSRoute",
		},
		forbid: []string{"kind: Gateway", "kind: SnippetsPolicy", "gateway.networking.k8s.io/v1alpha2"},
	},
	// allowedRoutes.namespaces.from=Selector: the rendered Gateway listeners
	// carry a valid selector block (the gatewayAllowedRoutes helper indents it
	// correctly — a regression here fails rendering outright).
	{
		sets: []string{
			"--set", "gateway.enabled=true",
			"--set", "gateway.allowedRoutes.namespaces.from=Selector",
			"--set", "gateway.allowedRoutes.namespaces.selector.matchLabels.gateway=shared",
		},
		expect: []string{
			"from: Selector",
			"matchLabels:",
			"gateway: shared",
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
// Implemented locally (in the golang module's own container) because the
// module's Lint downloads golangci-lint through the Dagger core GitHub module
// when the binary is missing, and that module is incompatible with recent
// engines (OTel import cycle). Installing through the Go module proxy keeps
// the step deterministic and independent of the GitHub API (which rate-limits
// unauthenticated CI calls). The latest release is installed, so code must
// pass with the current latest golangci-lint.
func (m *DaggerKubernetes) Lint(ctx context.Context) (string, error) {
	ctr := dag.Golang(m.Src).
		Container().
		WithExec([]string{"go", "install", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"}).
		WithExec([]string{"golangci-lint", "run", "--timeout", "5m", "--output.text.colors"})

	out, err := ctr.Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("golangci-lint: %w", err)
	}
	return out, nil
}

// Test runs go vet and go test -race with coverage.
//
// Implemented locally because the upstream golang module hardcodes test flags
// (no -race, -vet=off). It reuses the golang module's own base container — the
// debian-based golang image resolved from go.mod, with the Go caches mounted
// and the source at /src — so the toolchain matches the module's other
// functions. -race requires CGO, which that image provides; CGO_ENABLED is
// left at its default.
func (m *DaggerKubernetes) Test(ctx context.Context) (*dagger.File, error) {
	ctr := dag.Golang(m.Src).
		Container().
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
// urfave/cli help path. version is baked into the UI footer through the
// Dockerfile's APP_VERSION build arg (empty = the Dockerfile default).
func (m *DaggerKubernetes) Docker(ctx context.Context, version string) (*dagger.Container, error) {
	opts := dagger.DirectoryDockerBuildOpts{}
	if version != "" {
		opts.BuildArgs = []dagger.BuildArg{{Name: "APP_VERSION", Value: version}}
	}
	ctr := m.Src.DockerBuild(opts).
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
	base := dag.Helm(m.Src).
		HelmContainer().
		WithExec([]string{"helm", "dependency", "update", chartDir})

	for i, variant := range helmTemplateMatrix {
		cmd := append([]string{"helm", "template", "dagger-kubernetes", chartDir, "--debug"}, variant.sets...)
		ctr := base.WithExec(cmd)
		if _, err := ctr.Sync(ctx); err != nil {
			return fmt.Errorf("helm template variant %d: %w", i, err)
		}
		if len(variant.expect) == 0 && len(variant.forbid) == 0 {
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
		for _, unwanted := range variant.forbid {
			if strings.Contains(out, unwanted) {
				return fmt.Errorf("helm template variant %d: rendered output unexpectedly contains %q", i, unwanted)
			}
		}
	}

	return nil
}

// Publish builds the root Docker image and pushes it to a container registry
// (GHCR by default) under the given tag. It reuses Docker (Dockerfile build +
// `-h` smoke test), optionally runs the full quality gate, then delegates the
// push to the disaster37/dagger-library-go image module (2.0.19). The tag is
// baked into the UI footer through the Dockerfile's APP_VERSION build arg.
// Returns the published image reference including its digest.
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

	ctr, err := m.Docker(ctx, tag)
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

// JenkinsLibs packages the Jenkins shared library (ci-integrations/jenkins/)
// as a deterministic, versioned tar.gz release artifact.
//
// Determinism comes from GNU tar flags --sort=name --mtime=@0 --owner=0
// --group=0 --numeric-owner plus gzip -n (no header timestamp/name), so
// identical input yields a byte-identical archive. version is validated by
// artifact.Filename before it reaches the shell command line.
func (m *DaggerKubernetes) JenkinsLibs(
	ctx context.Context,
	// Artifact version, e.g. "v0.1.0" or "dev"; embedded in the output
	// filename and validated to be filesystem/shell-safe.
	// +required
	version string,
) (*dagger.File, error) {
	name, err := artifact.Filename(version)
	if err != nil {
		return nil, err
	}

	src := m.Src.Directory(jenkinsLibsDir).WithoutFile(".git")
	entries, err := src.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jenkins libs: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s is empty: nothing to package (is the submodule initialized? run: git submodule update --init)", jenkinsLibsDir)
	}

	// The Jenkins library has no go.mod, so the golang module cannot resolve a
	// version from it; pass the repository's Go version so the module resolves
	// its own base image (no image override).
	goVersion, err := dag.Golang(m.Src).ModVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve go version: %w", err)
	}

	outPath := fmt.Sprintf("/out/%s", name)
	ctr := dag.Golang(src, dagger.GolangOpts{Version: goVersion}).
		Container().
		WithExec([]string{"bash", "-c", fmt.Sprintf(
			"set -o pipefail && mkdir -p /out && tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf - -C /src . | gzip -n > '%s'",
			outPath)})

	if _, err := ctr.Sync(ctx); err != nil {
		return nil, fmt.Errorf("package jenkins libs: %w", err)
	}
	return ctr.File(outPath), nil
}

// Ci runs the full pipeline: Lint, Test, Ui, Build, Docker, Helm, JenkinsLibs.
//
// Returns a directory containing bin/supervisor, bin/dagger-kubernetes-ci,
// coverage.out, and jenkins-libs-dev.tar.gz (the dev Jenkins library artifact
// proves packaging works on every run; only bin/ and coverage.out are
// uploaded by ci.yml).
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

	if _, err := m.Docker(ctx, ""); err != nil {
		return nil, err
	}

	if err := m.Helm(ctx); err != nil {
		return nil, err
	}

	libs, err := m.JenkinsLibs(ctx, "dev")
	if err != nil {
		return nil, err
	}

	return bin.WithFile("coverage.out", coverage).
		WithFile("jenkins-libs-dev.tar.gz", libs), nil
}
