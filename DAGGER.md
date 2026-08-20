# Dagger CI

## Overview

The CI pipeline for this repository is a **local Dagger module** in [`dagger/`](./dagger) (module name `dagger-kubernetes`). It delegates lint and build to the [`golang`](https://github.com/disaster37/dagger-library-go) module and helm lint to the [`helm`](https://github.com/disaster37/dagger-library-go) module, both pinned at `2.0.10`. Test, UI, docker, and the helm template matrix are implemented locally because the upstream modules cannot express them.

> **Note on the golang dependency:** The upstream `golang` module at tag `2.0.10` has a `replace ../lib/` directive in its `go.mod` but does not declare `"include": ["../lib"]` in its `dagger.json`, which breaks remote loading. The module is therefore vendored at `dagger/deps/golang/` (with the `include` fix applied) and referenced as a local dependency. The `helm` module works remotely and is fetched from GitHub.

| Function | Delegated / Local | Why |
|----------|-------------------|-----|
| `lint`   | Delegated to `golang` module `Lint` | Upstream provides golangci-lint; custom base image pins v2.12.2 |
| `build`  | Delegated to `golang` module `Build` ×2 | Upstream handles CGO_ENABLED=0, ldflags, cross-compile |
| `helm`   | Lint delegated to `helm` module; template matrix local | Upstream `Lint` = `helm dependency update` + `helm lint`; no `helm template` support |
| `test`   | Local | Upstream hardcodes flags (no `-race`, `-vet=off`); `-race` requires CGO |
| `ui`     | Local | Upstream has no UI support |
| `docker` | Local | Upstream has no Dockerfile support |

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
| `ui` | `dagger call -m ./dagger --src . ui export --path ui-dist` | `dist/` directory |
| `build` | `dagger call -m ./dagger --src . build export --path .` | `bin/` directory with both binaries (the returned directory already contains `bin/`, so export to `.`) |
| `docker` | `dagger call -m ./dagger --src . docker` | built `Container` |
| `helm` | `dagger call -m ./dagger --src . helm` | (no return value; fails on error) |

## Direct module usage (bypassing local module)

The `golang` module can be called directly via the vendored copy at
`dagger/deps/golang`. (The remote `github.com/disaster37/dagger-library-go/golang@2.0.10`
reference cannot be used — it fails to load for the `include` reason noted
above.) Because the repo root has no Go files, `--main` is required:

```bash
dagger call -m ./dagger/deps/golang --src . ci --main ./cmd/api --out bin/supervisor export --path .
```

> **Caveat:** upstream `test` runs without `-race` (hardcoded flags). The local module's `Test` is used instead for parity with the original CI. Upstream `Build` omits `-trimpath` (release builds still use the Dockerfile / `release.yml` path, unaffected).

The upstream `helm` module:

```bash
dagger call -m github.com/disaster37/dagger-library-go/helm@2.0.10 --src deploy/helm/dagger-kubernetes lint
```

## Secrets / env

No secrets are required for CI. Helm `push` / `ci` release functions (not used here — releases stay in `release.yml`) would need registry username/password and a git token. An optional `DAGGER_CLOUD_TOKEN` enables Dagger Cloud trace observability.

## Troubleshooting

- **Engine startup on first run:** Dagger pulls the engine image on the first invocation; subsequent runs are faster.
- **`helm dependency update` needs network:** The chart depends on 5 public Helm repos (see `Chart.yaml`); ensure outbound network access is available.
- **golangci-lint version drift:** The local module pins golangci-lint **v2.12.2** via a custom base image. Bump deliberately when upgrading.
- **`dagger/deps/golang/` is vendored:** It is a local copy of `github.com/disaster37/dagger-library-go/golang@2.0.10` with `"include": ["../lib"]` added to its `dagger.json` (upstream omission). To update, re-vendor from the upstream tag and re-apply the `include` fix.
