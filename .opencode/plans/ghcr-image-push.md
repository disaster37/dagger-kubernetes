# Plan: Publish the Docker image to GHCR via Dagger

## 1. Overview & goals

Add a reliable, documented way to **build the supervisor Docker image and push it
to GHCR (ghcr.io) from the local Dagger module** in `dagger/`. The concrete
deliverable is a `publish` function on the existing `DaggerKubernetes` module
that builds the root `Dockerfile` (reusing the existing `Docker` build + smoke
test) and **delegates the registry push to the
`github.com/disaster37/dagger-library-go/image@2.0.19` dependency module**. The
"last dev version" image (`dev` tag on `ghcr.io/disaster37/dagger-kubernetes`) is
what the local "home" Kubernetes cluster will consume so a human can deploy and
validate it (per `AGENTS.local.md` §6).

### Non-goals (explicitly out of scope)

- **No change to the CI merge gate.** `.github/workflows/ci.yml` stays read-only
  and never pushes. The `publish` function is a local/operator command.
- **No change to `.github/workflows/release.yml`.** It already publishes the
  Docker image to `ghcr.io/${{ github.repository }}` via `docker/build-push-action`
  on tag releases. Leave it as-is (battle-tested); the Dagger `publish` is the
  **dev/adhoc** path, not the release path.
- **No multi-arch builds.** The image builds for the engine's native platform
  (linux/amd64 on the dev box and on GH Actions). The local k3s node is amd64.
  Multi-arch (`--platform`) is future work.

## 2. Assumptions & decisions (with rationale)

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | **Refactor the existing `Publish`** rather than add a second publish function. | `dagger/main.go` already contains a `Publish` (lines 284–328) and `dagger/dagger.gen.go` already wires it. It is buggy and undocumented: it hardcodes the image name `disaster/dagger-kubernetes`, attaches registry auth to the constant `"ghcr.io"` instead of the `registry` parameter, strips a `v` prefix from the version, and only supports a semver `version` (no `dev` tag). Refactor it in place; do not leave a second, competing publish entry point. |
| D2 | **GHCR owner is `disaster37`** (default image `disaster37/dagger-kubernetes`). | Evidence: `.github/workflows/release.yml` tags `ghcr.io/${{ github.repository }}` (= `ghcr.io/disaster37/dagger-kubernetes`); `deploy/helm/dagger-kubernetes/values.yaml` default `supervisor.image.repository` is `ghcr.io/disaster37/dagger-kubernetes`; `docs/README.md` references `ghcr.io/disaster37/dagger-kubernetes:latest`. The Go module path `github.com/disaster/dagger-kubernetes` is a *module* path, not the GHCR owner. |
| D3 | **`tag` maps to the dependency's `version`; semver tags are normalized (leading `v` stripped), non-semver tags pass through verbatim.** | The `image` dependency's `Push` runs go-semver normalization: `--tag v0.1.0` publishes as `0.1.0`, while `--tag dev`, a git SHA, or a duration like `1h` pass through unchanged. **Accepted + documented** (§8, §10.1). Note: `release.yml` pushes `ghcr.io/…:v0.1.0` *with* the `v` (docker/build-push-action + `github.ref_name`), so a semver published via Dagger `publish` lands at `0.1.0` (no `v`) — two different paths, different tag conventions; acceptable because `release.yml` is the canonical release path and `publish` is the dev/adhoc path. |
| D4 | **Single tag per invocation; multiple tags = multiple calls.** | Dagger caches the build, so publishing `dev`, a SHA, and a semver is three `dagger call … publish --tag …` runs that reuse the same cached image. This avoids a multi-tag `Container.Publish` matrix and keeps the API trivial. |
| D5 | **`Publish` = build + `-h` smoke test + push, with an opt-in `--gates` flag.** | Pushing a broken image is bad, but running the full `-race` test suite on every dev-loop push is slow and redundant with the CI merge gate. Default: reuse `m.Docker` (build + `-h` smoke). `--gates=true` additionally runs `Lint` + `Test` + `Ui` first (what the old `Publish` did), for human release verification. |
| D6 | **Validation helpers live in a new pure package `dagger/internal/ref`**, not in `package main`. | The generated `dagger/dagger.gen.go` + `internal/dagger` SDK panic at `init()` if `DAGGER_SESSION_PORT` is unset, so any test that imports `package main` (or the SDK) cannot run under plain `go test`. A stdlib-only package can be unit-tested without an engine. Ref *building* was removed (the `image` dependency composes the final reference), so `ref` only validates. |
| D7 | **The `AGENTS.local.md` edit (build/push step → Dagger `publish`, GHCR pull-secret, §3 image row) is done by the Coder as part of this task; only the actual cluster deployment is post-merge.** | `AGENTS.local.md` is gitignored (`.gitignore` line 31: `AGENTS.local.md`; and its §9 "Never commit this file … already listed in `.gitignore`"), so the file edit is local-only and never part of the commit/PR. The Coder updates the build→push→helm-upgrade→rollout workflow and §3 image row now; the human/operator runs the actual publish → helm upgrade → rollout → §5.1/§5.2 verification against the "home" cluster after merge. |
| D8 | **Add `image@2.0.19` only; do NOT bump `golang`/`helm` from 2.0.12.** | Minimal blast radius: `golang`/`helm` at 2.0.12 drive the CI merge gate (lint/build/helm-lint) and are proven green; bumping them is unrelated to image push and would require re-verifying lint/build/helm-lint. The `image` module is new and must be pinned at its current release 2.0.19. `dagger.json` pins each dependency independently, so mixed pins (2.0.12 + 2.0.19) are fine; `DAGGER.md` must document the true pins: `golang`+`helm` @2.0.12, `image` @2.0.19. |

## 3. Affected files

| Path | Change |
|------|--------|
| `dagger/internal/ref/ref.go` | **NEW** — pure validation (tag/registry/image) + a `Validate` helper (stdlib only). |
| `dagger/internal/ref/ref_test.go` | **NEW** — table-driven unit tests for the surviving `ref` symbols (stdlib `testing`). |
| `dagger/main.go` | **MODIFIED** — refactor `Publish` to delegate to the `image` dependency (`New(nil, ctr)` → `Build` → `Push`); credentials become `*dagger.Secret`; import `ref`. |
| `dagger/dagger.json` | **MODIFIED** — add dependency `github.com/disaster37/dagger-library-go/image@2.0.19`. |
| `dagger/go.mod` / `dagger/go.sum` | **REGENERATED** — the SDK records the `image` dependency; updated by `dagger develop`. |
| `dagger/dagger.gen.go` | **REGENERATED** — `dagger develop -m ./dagger` (do not hand-edit); pulls the `image` dependency's generated types. |
| `DAGGER.md` | **MODIFIED** — add `publish` to the function tables + a publish/credential section + troubleshooting; correct stale pins (2.0.10 → 2.0.12) and the obsolete vendored-`golang` note. |
| `docs/README.md` | **MODIFIED** — add a "Publishing the image (GHCR)" subsection under Development. |
| `AGENTS.local.md` | **MODIFIED by the Coder in this task (gitignored → edited locally, NOT committed)** — §4.1/§4.2 build+push step becomes the Dagger `publish` command (old `docker build/push` kept as a deprecated fallback), add the one-time GHCR `ghcr-pull` secret step + `pullSecrets`/`repository`/`tag` overrides (§10.2), and §3 image row → `ghcr.io/disaster37/dagger-kubernetes:dev`. |

No change to: `config/` (image repo/tag are Helm values, not app config), `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `deploy/helm/*`.

## 4. Data structures & function signatures

### 4.1 New package `dagger/internal/ref/ref.go`

Module path `dagger/dagger-kubernetes` (see `dagger/go.mod`), so the import in
`main.go` is `"dagger/dagger-kubernetes/internal/ref"`.

```go
// Package ref validates container image reference parts (tag, registry, image)
// for the dagger-kubernetes publish function. It is pure (stdlib only) so it
// can be unit-tested without a running Dagger engine. It does NOT compose the
// final reference — the image dependency builds and publishes it.
package ref

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// OCI distribution tag: [A-Za-z0-9_][A-Za-z0-9._-]{0,127} ("dev", "v0.1.0", "latest", git SHA).
	tagRe      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	// Bare registry host, optionally host:port (e.g. "ghcr.io", "localhost:5000").
	registryRe = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	// Repository path owner/name[/sub...], lowercase (GHCR lowercases), components
	// separated by ".", "_", or "-".
	imageRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
)

// ValidateTag reports whether tag is a valid container image tag.
func ValidateTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("tag must not be empty")
	}
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("invalid image tag %q: must match [A-Za-z0-9_][A-Za-z0-9._-]{0,127}", tag)
	}
	return nil
}

// ValidateRegistry reports whether registry is a bare host[:port] with no scheme,
// path, or whitespace.
func ValidateRegistry(registry string) error {
	if registry == "" {
		return fmt.Errorf("registry must not be empty")
	}
	if strings.Contains(registry, "://") || strings.ContainsAny(registry, "/ \t\n") {
		return fmt.Errorf("invalid registry %q: must be a bare host[:port] (no scheme, path, or whitespace)", registry)
	}
	if !registryRe.MatchString(registry) {
		return fmt.Errorf("invalid registry %q", registry)
	}
	return nil
}

// ValidateImage reports whether image is a repository path of the form
// owner/name[/sub/...] with no scheme and no leading/trailing slash.
func ValidateImage(image string) error {
	if image == "" {
		return fmt.Errorf("image must not be empty")
	}
	if strings.Contains(image, "://") || strings.HasPrefix(image, "/") || strings.HasSuffix(image, "/") {
		return fmt.Errorf("invalid image %q: must be owner/name without scheme or leading/trailing slash", image)
	}
	if !imageRe.MatchString(image) {
		return fmt.Errorf("invalid image %q: must be lowercase owner/name[/sub...]", image)
	}
	return nil
}

// Validate reports the first invalid part among registry, image, and tag, or
// nil if all three are valid. It is the fail-fast gate Publish calls before
// delegating to the image dependency (which composes the final reference).
func Validate(registry, image, tag string) error {
	if err := ValidateRegistry(registry); err != nil {
		return err
	}
	if err := ValidateImage(image); err != nil {
		return err
	}
	if err := ValidateTag(tag); err != nil {
		return err
	}
	return nil
}
```

### 4.2 Refactored `Publish` in `dagger/main.go`

Replace the existing `Publish` (lines 284–328) with:

```go
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
```

Add the import `"dagger/dagger-kubernetes/internal/ref"` to the import block in
`dagger/main.go`. No new import is needed for the `image` dependency: its
generated types (`dagger.Image`, `dagger.ImageOpts`, `dagger.ImageBuildOpts`,
`dagger.ImageBuildPushOpts`) land in the existing
`"dagger/dagger-kubernetes/internal/dagger"` import (already aliased `dagger`),
and `dag` is the existing global client.

Notes for the implementer:

- **Dependency invocation pattern (mirror the existing `helm`/`golang` calls in
  `dagger/main.go`):** the generated accessor is `dag.<Type>(…)` on the global
  `dag` client — e.g. `dag.Golang(m.Src, dagger.GolangOpts{Base: base}).Lint(ctx)`
  and `dag.Helm(chart).Lint(ctx)`. The `image` dependency generates
  `dag.Image(dagger.ImageOpts{BaseHadolintContainer, BuildContainer})`,
  `(*Image).Build(source, dagger.ImageBuildOpts{Dockerfile, WithDirectories})`,
  and `(*ImageBuild).Push(ctx, repositoryName, version, registryUrl,
  dagger.ImageBuildPushOpts{WithRegistryUsername, WithRegistryPassword})`. Inject
  our smoke-tested container via `BuildContainer` so the published image is
  exactly what CI's `docker` function builds (`-h` smoke included — the exec
  history is not part of the packaged filesystem).
- After `dagger mod use` + `dagger develop`, the exact generated names are in
  `dagger/internal/dagger/image.gen.go`; they follow the same `<Type>Opts`
  convention as `golang.gen.go` (`GolangOpts`, `GolangBuildOpts`, …). Match them.
- `Push` normalizes semver tags (`v0.1.0` → `0.1.0`) and **silently skips auth**
  if either secret is nil — our `ref.Validate` + exactly-one-of checks run first.
- `Push` returns the published reference string of the form
  `ghcr.io/disaster37/dagger-kubernetes:dev@sha256:…` (fully-qualified address
  with digest).

## 5. Input validation & error handling

- **Validation** is centralized in `internal/ref` (see §4.1). `Publish` calls `ref.Validate` first, so bad `registry`/`image`/`tag` fail fast before any build work.
- **Credentials**: both `registryUsername` and `registryPassword` are `*dagger.Secret`. Exactly-one-set is an error (the dependency silently skips auth when either secret is nil, so we must catch it ourselves). Both nil → anonymous push (works for e.g. `ttl.sh`; GHCR rejects). Both set → passed to `ImageBuild.Push`, which attaches `WithRegistryAuth`.
- **Error wrapping** follows AGENTS.md: every downstream failure is wrapped with `fmt.Errorf("context: %w", err)`. Validation errors are leaf errors (no `%w`) since nothing is wrapped.
- No `fmt` string concatenation with `+` anywhere — all formatting uses `fmt.Sprintf`.

## 6. Edge cases & failure modes

| Case | Behaviour |
|------|-----------|
| Missing/invalid tag, registry, image | `ref.Validate` returns a descriptive error; `Publish` returns it unchanged (no partial build). |
| Empty `registry`/`image` | Fall back to `ghcr.io` / `disaster37/dagger-kubernetes` before validation. |
| Registry auth failure (bad token, 401/403) | `Container.Publish` returns an error; wrapped as `docker publish: %w`. Dagger does not leave a half-pushed tag (layers are content-addressed; the manifest write is the last, atomic-ish step). |
| Missing `Dockerfile` at repo root | `Directory.DockerBuild()` fails during `Docker`; wrapped as `docker: %w`. |
| Tag collision / overwrite | `dev` and `latest` are **mutable by design** (this is what the cluster's `imagePullPolicy: Always` expects). Semver tags *can* be overwritten (GHCR does not enforce immutability) but should not be — documented guidance only. |
| Network failure mid-push | `Container.Publish` errors; retry by re-running the command (Dagger reuses the cached build; layers already pushed are deduplicated). |
| Only one credential provided | Immediate error (no build attempted). |
| `dagger.gen.go` drift after editing `main.go` | Always regenerate with `dagger develop -m ./dagger` and commit it. The CI gate loads the module and will fail to compile if `dagger.gen.go` is stale. |
| `go test` panic in `package main` | Do **not** add tests that import `package main` or `internal/dagger`: `init()` panics without `DAGGER_SESSION_PORT`. Tests live in `internal/ref` (stdlib only). |
| CI vs local divergence | `ci`/`ci.yml` never call `Publish`; `publish` never calls `Ci`. The two remain independent; the merge gate is unchanged. |
| Dependency silently skips auth (one secret set) | Our `ref.Validate` + exactly-one-of check run first; if both secrets are nil the push is anonymous and GHCR rejects at the registry (surfaced by `docker publish: %w`). |
| Semver tag normalization | `--tag v0.1.0` publishes as `0.1.0` (dependency strips the `v`); `--tag dev`/SHA/`1h` pass through verbatim (D3). |
| Dependency `lint`/`Ci` (hadolint) | Available on the `image` module but **out of scope**; our build path remains `m.Docker` (Dockerfile build + `-h` smoke), not hadolint. |
| Dependency version drift | `image` is pinned 2.0.19 while `golang`/`helm` stay 2.0.12 (D8). If a future bump of `image` changes `Build`/`Push` signatures, `dagger develop` regenerates `dagger.gen.go` and `main.go` must be re-verified. |

## 7. Testing strategy

- **Unit tests (stdlib `testing`, table-driven):** `dagger/internal/ref/ref_test.go`
  covers `ValidateTag`, `ValidateRegistry`, `ValidateImage`, and `Validate` with
  valid and invalid tables (empty tag; tag with illegal chars/128+ chars;
  `ghcr.io` vs `http://ghcr.io` vs `ghcr.io/path` vs `host:5000`; image
  `disaster37/dagger-kubernetes` vs missing owner vs uppercase vs leading `/`;
  `Validate` composition). **Only surviving symbols are tested** — `Address` is
  deleted (the dependency composes the ref), so no dead symbols. Run with
  `cd dagger && go test ./internal/ref/`. These tests do not need a Dagger engine.
- **`dagger call` smoke (no credentials):** validate the full build+push path
  against an anonymous throwaway registry. ttl.sh encodes the image lifetime in
  the **tag**, so use a duration tag (per the image module's own `--version 1m`
  example):
  `dagger call -m ./dagger --src . publish --registry ttl.sh --image smoke/dagger-kubernetes --tag 1h`
  → prints `ttl.sh/smoke/dagger-kubernetes:1h@sha256:…`. This exercises
  `Docker` → `Image.Build`/`ImageBuild.Push` end-to-end without secrets.
- **Full CI gate (mandatory before PR):** `dagger call -m ./dagger --src . ci export --path out`
  (lint, vet, `-race` test, UI, builds, Dockerfile smoke, Helm matrix). This is
  the merge gate and must stay green.
- **What cannot be tested locally and why:** (1) an actual GHCR push — requires
  a real PAT with `write:packages` on `disaster37`; (2) the cluster deploy —
  requires the "home" cluster and a human (§5 verification). Both are the
  post-merge operator/human steps in §10.

## 8. Documentation updates

### 8.1 `DAGGER.md`

1. Add `publish` to the "Function | Delegated / Local | Why" table:
   `| \`publish\` | Delegated to the \`image\` module \`Build\`+\`Push\` (build container injected) | Build/push to GHCR |`.
2. Add a row to "Individual functions":
   `| \`publish\` | \`dagger call -m ./dagger --src . publish --tag dev --registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN\` | image reference + digest (string) |`.
3. Add a **"Publishing the image (GHCR)"** section: command, `--tag`/`--registry`/`--image`/`--gates` flags, the `env:` secret-passing syntax, the semver-normalization note (`v0.1.0`→`0.1.0`), and the `dev`-tag + `imagePullPolicy: Always` mutability note.
4. Troubleshooting additions: GHCR auth failure (401/403 → check PAT has `write:packages`); `DAGGER_SESSION_PORT` panic when running `go test` in `package main` (tests belong in `internal/ref`); `dev`/`latest` tags are mutable; the `image` module **silently skips auth** when only one secret is passed.
5. **Correct pre-existing drift (same changeset):** change "pinned at `2.0.10`" → "`2.0.12`" (matches `dagger/dagger.json`), and replace the stale "vendored at `dagger/deps/golang/`" note with the current state (deps are remote at `github.com/disaster37/dagger-library-go/{golang,helm}@2.0.12`, pinned in `dagger.json`; the `dagger/deps/` directory no longer exists). **Document the true final pins:** `golang`+`helm` @2.0.12, `image` @2.0.19.

### 8.2 `docs/README.md`

Under `## Development` (line 2214), add a "Publishing the image (GHCR)"
subsection with the exact commands from §10 (publish `dev`, opt-in `--gates`,
ttl.sh smoke, the semver-normalization note, and a note that `release.yml`
publishes semver tags on release via docker/build-push-action).

### 8.3 `AGENTS.local.md` (edited by the Coder in this task; gitignored → not committed)

The Coder edits this machine-specific file **as part of this task** (not deferred
to post-merge). It is gitignored (`.gitignore` line 31: `AGENTS.local.md`; and its
§9 "Never commit this file … already listed in `.gitignore`"), so the edit is
local-only and must **not** be `git add`-ed or committed. Make these edits:

- §3 image row → `ghcr.io/disaster37/dagger-kubernetes:dev` (tag `dev`).
- §4.1/§4.2 "Build the container image" / "Push the image" → **replace** the
  `docker build -t docker.io/disaster/dagger-kubernetes:dev .` +
  `docker push docker.io/disaster/dagger-kubernetes:dev` pair with the Dagger
  build+push command (with the credential env vars):
  ```bash
  export GHCR_USERNAME="<github-username>"
  export GHCR_TOKEN="<PAT with write:packages>"
  dagger call -m ./dagger --src . publish --tag dev \
    --registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN
  ```
  Keep the old `docker build/push docker.io/disaster/dagger-kubernetes:dev` path
  in a note as a **deprecated fallback** (still works, but GHCR/Dagger is preferred).
- Add a **one-time** GHCR pull-secret step before §4.4 (the
  `kubectl create secret docker-registry ghcr-pull --docker-server=ghcr.io …`
  block from §10.2).
- §4.4 → add the GHCR image overrides so the cluster consumes the last dev
  version from GHCR: `--set supervisor.image.repository=ghcr.io/disaster37/dagger-kubernetes`,
  `--set supervisor.image.tag=dev`, `--set supervisor.image.pullSecrets[0].name=ghcr-pull`.
- §7 deployed-values reference → update `supervisor.image.repository`/`tag` to
  the GHCR values **when the migration is actually applied on the cluster**
  (post-merge deployment, §11 step 11) — §7 reflects the live cluster state.

## 9. Git / PR workflow

1. Create branch from `main`: `git checkout -b feat/ghcr-image-push`.
2. Implement §4–§8, **including editing `AGENTS.local.md` (§8.3)**. Note: `AGENTS.local.md` is gitignored, so edit it on the machine but do **not** `git add` or commit it.
3. Add the dependency + regenerate: `dagger mod use -m ./dagger github.com/disaster37/dagger-library-go/image@2.0.19` then `dagger develop -m ./dagger` (commits `dagger/dagger.json`, `dagger/dagger.gen.go`, `dagger/go.mod`, `dagger/go.sum`).
4. Local verification (all must pass):
   - `cd dagger && go test ./internal/ref/`
   - `cd dagger && go vet ./... && gofmt -l . && goimports -l -local github.com/disaster/dagger-kubernetes .` (empty output)
   - root: `go build ./... && go vet ./... && go test ./...`
   - full gate: `dagger call -m ./dagger --src . ci export --path out`
   - ttl.sh smoke publish (§7).
5. Grep for dead symbols touched by the refactor (remove anything orphaned —
   the `unused` linter fails CI).
6. Commit + push, open a PR. **Only after** step 4 is fully green.

## 10. Example commands a human runs

### 10.1 Publish to GHCR

```bash
export GHCR_USERNAME="<github-username>"
export GHCR_TOKEN="<personal-access-token with write:packages>"

# Publish the mutable "last dev version" image:
dagger call -m ./dagger --src . publish \
  --tag dev \
  --registry-username env:GHCR_USERNAME \
  --registry-password env:GHCR_TOKEN

# Same, but run the full quality gate (lint + vet + test -race + UI) first:
dagger call -m ./dagger --src . publish --tag dev --gates=true \
  --registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN

# Publish a semver tag — the image dependency normalizes it to 0.1.0 (no "v"):
dagger call -m ./dagger --src . publish --tag v0.1.0 \
  --registry-username env:GHCR_USERNAME --registry-password env:GHCR_TOKEN
```

### 10.2 Deploy to the "home" cluster (after merge; per `AGENTS.local.md`)

```bash
export KUBECONFIG=/home/user/.kube/home
NS=dagger-kubernetes-test

# One-time: create a docker-registry pull secret for the (private) GHCR package.
kubectl --kubeconfig /home/user/.kube/home -n $NS create secret docker-registry ghcr-pull \
  --docker-server=ghcr.io \
  --docker-username="$GHCR_USERNAME" \
  --docker-password="$GHCR_TOKEN"

# Capture live values (do NOT let helm upgrade reset overrides):
helm --kubeconfig /home/user/.kube/home get values dagger-kubernetes-test \
  -n $NS -o yaml > /tmp/dagger-kubernetes-test.values.yaml

helm --kubeconfig /home/user/.kube/home upgrade --install dagger-kubernetes-test \
  ./deploy/helm/dagger-kubernetes \
  --namespace $NS \
  -f /tmp/dagger-kubernetes-test.values.yaml \
  --set supervisor.image.repository=ghcr.io/disaster37/dagger-kubernetes \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.pullSecrets[0].name=ghcr-pull

kubectl --kubeconfig /home/user/.kube/home -n $NS \
  rollout restart statefulset/dagger-kubernetes-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n $NS \
  rollout status statefulset/dagger-kubernetes-test-dagger-kubernetes --timeout=300s
```

Then run the §5.1 agent verification (pods Ready, `/healthz`/`/readyz`, authed
API smoke, logs free of fatal errors, `-control`/`-data` endpoints list all 3
pods) and §5.2 human UI verification. Update `AGENTS.local.md` §3/§7 accordingly.

## 11. Ordered implementation steps (each independently verifiable)

1. `git checkout -b feat/ghcr-image-push` (from `main`).
2. Add the dependency: `dagger mod use -m ./dagger github.com/disaster37/dagger-library-go/image@2.0.19` (or edit `dagger/dagger.json` to add `{"name":"image","source":"github.com/disaster37/dagger-library-go/image@2.0.19"}`), then `dagger develop -m ./dagger` — regenerates `dagger/dagger.gen.go` + `dagger/internal/dagger/image.gen.go` and updates `dagger/go.mod`/`go.sum`.
3. Create `dagger/internal/ref/ref.go` (exact content §4.1).
4. Create `dagger/internal/ref/ref_test.go` (table-driven). Verify: `cd dagger && go test ./internal/ref/`.
5. Refactor `Publish` in `dagger/main.go` to delegate to `dag.Image(…).Build(…).Push(…)` + add the `internal/ref` import (§4.2). Verify it compiles: `dagger develop -m ./dagger`.
6. Smoke publish to `ttl.sh` (§7, `--tag 1h`) — verifies build+push wiring with no secrets.
7. Update `DAGGER.md` (§8.1), `docs/README.md` (§8.2).
8. Edit `AGENTS.local.md` (§8.3): swap the §4.1/§4.2 build+push step to the Dagger `publish` command (both credentials via `env:`; keep the old `docker build/push` as a deprecated fallback), add the one-time GHCR pull-secret step + `pullSecrets`/`repository`/`tag` overrides, update the §3 image row. Do **not** `git add` this file (gitignored).
9. Full local gate: `dagger call -m ./dagger --src . ci export --path out`; plus root `go build ./... && go vet ./... && go test ./...`; `gofmt`/`goimports` clean on the dagger module.
10. Grep for dead symbols introduced/removed by the refactor; delete anything orphaned (incl. any leftover `Address` helper).
11. Commit, push, open PR (only after step 9 is green).
12. Post-merge (operator/human, not in the PR): publish `--tag dev` to GHCR, create the `ghcr-pull` secret, helm-upgrade the home cluster to `ghcr.io/disaster37/dagger-kubernetes:dev`, rollout, run §5.1/§5.2 verification, and update `AGENTS.local.md` §7 deployed-values to reflect the live GHCR image.
