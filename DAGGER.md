# Dagger CI

## Overview

The CI pipeline for this repository is a **local Dagger module** in [`dagger/`](./dagger) (module name `dagger-kubernetes`). It delegates lint and build to the [`golang`](https://github.com/disaster37/dagger-library-go) module, helm lint to the [`helm`](https://github.com/disaster37/dagger-library-go) module, and the registry push to the [`image`](https://github.com/disaster37/dagger-library-go) module. **Dependency pins** (each independent, in [`dagger/dagger.json`](./dagger/dagger.json)): `golang` + `helm` at `2.0.12`, `image` at `2.0.19`. Test, UI, docker, and the helm template matrix are implemented locally because the upstream modules cannot express them.

> **Note on the dependencies:** all dependencies load **remotely** from `github.com/disaster37/dagger-library-go`, pinned per-dependency in `dagger/dagger.json`. The formerly vendored copy at `dagger/deps/golang/` **no longer exists** — do not reference it.

| Function | Delegated / Local | Why |
|----------|-------------------|-----|
| `lint`   | Delegated to `golang` module `Lint` | Upstream provides golangci-lint; custom base image pins v2.12.2 |
| `build`  | Delegated to `golang` module `Build` ×2 | Upstream handles CGO_ENABLED=0, ldflags, cross-compile |
| `helm`   | Lint delegated to `helm` module; template matrix local | Upstream `Lint` = `helm dependency update` + `helm lint`; no `helm template` support. The local template matrix also covers the three data-plane TLS cases (embedded default, `dataCert.enabled`, `dataIngress.tls.secretName`) plus the external-provider keypair rendering, a `minio.enabled=false` variant that renders the chart without the bundled S3 object store, and a subchart-`fullnameOverride` variant that renames tempo/loki/victoria/minio/opentelemetry-collector (in lock step with the `global.daggerKubernetes.serviceNames.*` collector-exporter keys). The default and subchart-rename variants additionally assert URL substrings in the rendered manifests, so the matrix proves the auto-wired service URLs follow each dependency's own fullname rules instead of only checking that rendering succeeds. |
| `test`   | Local | Upstream hardcodes flags (no `-race`, `-vet=off`); `-race` requires CGO |
| `ui`     | Local | Local Nuxt 4 + Nuxt UI v4 build (upstream has no UI support) |
| `docker` | Local | Upstream has no Dockerfile support |
| `publish` | Delegated to the `image` module `Build`+`Push` (build container injected) | Build/push to GHCR |

## Prerequisites

- **Dagger CLI** `0.21.8` (pinned in CI via `DAGGER_VERSION`; newer versions may work).
- A running **Docker daemon** (Dagger uses it as the build engine).

Install the CLI:

```bash
curl -fsSL https://dl.dagger.io/dagger/install.sh | DAGGER_VERSION=0.21.8 sh
```

The installer places the binary in `./bin` by default (override with
`BIN_DIR=/some/dir`); add it to your `PATH` or invoke it as `./bin/dagger`.

## Full CI

```bash
dagger call -m ./dagger --src . ci export --path out
```

Outputs:

| Path | Contents |
|------|----------|
| `out/bin/supervisor` | Supervisor binary |
| `out/bin/dagger-kubernetes-ci` | CI helper binary |
| `out/coverage.out` | Go test coverage profile |

## Individual functions

| Function | Command | Returns |
|----------|---------|---------|
| `lint` | `dagger call -m ./dagger --src . lint` | golangci-lint stdout |
| `test` | `dagger call -m ./dagger --src . test export --path coverage.out` | `coverage.out` file |
| `ui` | `dagger call -m ./dagger --src . ui export --path ui-dist` | `.output/public/` directory |
| `build` | `dagger call -m ./dagger --src . build export --path .` | `bin/` directory with both binaries (the returned directory already contains `bin/`, so export to `.`) |
| `docker` | `dagger call -m ./dagger --src . docker` | built `Container` |
| `helm` | `dagger call -m ./dagger --src . helm` | (no return value; fails on error) |
| `publish` | `dagger call -m ./dagger --src . publish --tag dev --registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN` | image reference + digest (string) |

## Publishing the image (GHCR)

```bash
export GHCR_USERNAME="<github-username>"
export GHCR_TOKEN="<PAT with write:packages>"

# Publish the mutable "last dev version" image:
dagger call -m ./dagger --src . publish \
  --tag dev \
  --registry-username env:GHCR_USERNAME \
  --registry-password env:GHCR_TOKEN

# Same, but run the full quality gate (lint + test -race + UI) first:
dagger call -m ./dagger --src . publish --tag dev --gates=true \
  --registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN

# Anonymous smoke publish to a throwaway registry (no credentials):
dagger call -m ./dagger --src . publish \
  --registry ttl.sh --image smoke/dagger-kubernetes --tag 1h
```

Flags:

| Flag | Required | Default | Meaning |
|------|----------|---------|---------|
| `--tag` | yes | — | Image tag (`dev`, `v0.1.0`, git SHA, …) |
| `--registry` | no | `ghcr.io` | Bare registry host (no scheme) |
| `--image` | no | `disaster37/dagger-kubernetes` | Repository path (no registry host) |
| `--registry-username` / `--registry-password` | for GHCR | none (anonymous) | Credentials as Secrets; pass **both** or neither |
| `--gates` | no | `false` | Run Lint + Test + Ui before pushing |

Secrets are passed with the `env:` prefix (`env:GHCR_USERNAME`), which resolves
the value from the local environment without printing it. The PAT needs the
`write:packages` scope.

**Semver normalization:** the `image` module normalizes semver tags —
`--tag v0.1.0` publishes as `0.1.0` (leading `v` stripped). Non-semver tags
(`dev`, `1h`, a git SHA) pass through verbatim. Note this differs from
`release.yml`, which pushes `ghcr.io/…:v0.1.0` *with* the `v`; `release.yml`
stays the canonical release path, `publish` is the dev/adhoc path.

**Mutable tags:** `dev` and `latest` are mutable by design — the cluster runs
`imagePullPolicy: Always` and expects re-publishes under the same tag. Treat
published semver tags as immutable (GHCR does not enforce this).

## Direct module usage (bypassing local module)

The dependency modules can be called directly at their pinned tags. Because the
repo root has no Go files, `--main` is required:

```bash
dagger call -m github.com/disaster37/dagger-library-go/golang@2.0.12 --src . ci --main ./cmd/api --out bin/supervisor export --path .
```

> **Caveat:** upstream `test` runs without `-race` (hardcoded flags). The local module's `Test` is used instead for parity with the original CI. Upstream `Build` omits `-trimpath` (release builds still use the Dockerfile / `release.yml` path, unaffected).

The upstream `helm` module:

```bash
dagger call -m github.com/disaster37/dagger-library-go/helm@2.0.12 --src deploy/helm/dagger-kubernetes lint
```

## Secrets / env

No secrets are required for CI. `publish` needs a registry username/password for GHCR (passed as `--registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN`; a PAT with `write:packages`). An optional `DAGGER_CLOUD_TOKEN` enables Dagger Cloud trace observability.

## Troubleshooting

- **Engine startup on first run:** Dagger pulls the engine image on the first invocation; subsequent runs are faster.
- **`helm dependency update` needs network:** The chart depends on 6 public Helm charts (see `Chart.yaml`); ensure outbound network access is available.
- **golangci-lint version drift:** The local module pins golangci-lint **v2.12.2** via a custom base image. Bump deliberately when upgrading.
- **GHCR push fails with 401/403:** the PAT behind `env:GHCR_TOKEN` needs the `write:packages` scope (and the username must match the token owner). Both credentials must be passed together — see the next bullet.
- **`publish` with only one credential:** the `image` module **silently skips auth** when only one of `WithRegistryUsername`/`WithRegistryPassword` is set; the local module therefore rejects a lone credential up front (`registryUsername and registryPassword must be provided together`). Pass both, or neither for anonymous registries (ttl.sh).
- **Mutable `dev`/`latest` tags:** re-publishing over them is expected (`imagePullPolicy: Always`); do not re-publish a published semver tag.
- **`go test` panics with `DAGGER_SESSION_PORT` unset:** the generated SDK panics at `init()` without a Dagger session, so tests must not import `package main` or `internal/dagger`. Pure logic lives in `dagger/internal/ref` (stdlib only) and runs under plain `go test`.
- **Dependency pins:** dependencies are remote at `github.com/disaster37/dagger-library-go/{golang,helm}@2.0.12` and `.../image@2.0.19`, pinned in `dagger/dagger.json` (`dagger/deps/` no longer exists). Bump each independently; regenerate with `dagger develop -m ./dagger`.
- **UI build is a static Nuxt SPA:** `ui` runs `nuxt generate` with `ssr: false` and returns `.output/public/` (not `dist/`). `app.buildAssetsDir` is pinned to `/assets/` so the Go handler's immutable-cache path (`internal/handler/ui.go`) stays unchanged. The build needs Node `20.19+`/`22.12+`; the module uses `node:22-alpine`.
