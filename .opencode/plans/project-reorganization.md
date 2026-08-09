# Project Reorganization + Dagger CI Migration

## 1. Summary

Reorganize `github.com/disaster/dagger-kubernetes` following clean-structure conventions and migrate the entire GitHub Actions CI to Dagger. Structural changes: move `helm/dagger-kubernetes/` → `deploy/helm/dagger-kubernetes/`, delete the empty `internal/dataplane/` and unused `internal/rpc/` (drops the speculative Kitex scale-out path), move `cmd/dagger-cache.sh` → `hack/dagger-cache.sh`, fix the broken GHA composite action (`ci-integrations/gha/action.yml` references a script that doesn't exist in its directory), delete the empty root `ui-dist/` leftover, and clean up `.gitignore`/tracked build artifacts. CI changes: replace **all** jobs in `.github/workflows/ci.yml` (lint, ui, test, build, docker, helm) with a single job running a new **local Dagger module in `dagger/`** that delegates to the Daggerverse modules `github.com/disaster37/dagger-library-go/golang@2.0.10` and `helm@2.0.10` where possible, and locally implements what they cannot express: `go vet` + `go test -race`, UI typecheck/build, Dockerfile build + smoke test, and the helm template matrix. All Dagger commands are documented in a new `DAGGER.md` and a new CONTRIBUTING.md section. `release.yml` is **not** migrated — only its helm paths are fixed.

## 2. Locked Decisions (from planning interview)

| # | Decision | Choice |
|---|----------|--------|
| D1 | CI replacement strategy | Local Dagger module that **delegates** to existing Daggerverse modules; not reimplementation, not off-the-shelf-only (they can't express `-race`, UI, docker) |
| D2 | Module location | `dagger/` subdirectory (root `dagger.json` impossible: repo root already has the project `go.mod`; a Go-SDK Dagger module needs its own module dir) |
| D3 | ci.yml shape | Single `ci` job: install pinned Dagger CLI → `dagger call -m ./dagger ci export --path out` → upload artifacts |
| D4 | Broken GHA composite action | Fix in this plan: copy wrapper script to `ci-integrations/gha/dagger-cache.sh` |
| D5 | Root `ui-dist/` (empty, untracked) | Delete leftover; document the real pipeline `ui/` → `ui/dist` → `internal/api/ui-dist/` |
| D6 | .gitignore | Full cleanup: `/dagger-cache-ci`, `/supervisor`, `bin/`, `out/`, fix helm tgz pattern; `git rm --cached` tracked artifacts |
| D7 | ADR | **None** (explicit user decision; noted deviation from AGENTS.md's ADR mandate) |

## 3. Verified Facts (cross-checked against code)

- `internal/dataplane/` is empty; `internal/rpc/` holds only `supervisor.proto` (self-referencing `go_package`, zero Go imports). Safe deletes.
- Embedded UI: `internal/api/ui.go` `//go:embed all:ui-dist` consumes `internal/api/ui-dist/` (tracked, pre-built). Vite outputs `ui/dist`; `Dockerfile` copies `ui/dist` → `internal/api/ui-dist/`. Root `ui-dist/` is an empty leftover.
- `ci-integrations/gha/action.yml` L29 runs `${GITHUB_ACTION_PATH}/dagger-cache.sh` — file absent there (pre-existing bug).
- `coverage.out` already matched by `*.out` in `.gitignore`; root binaries `dagger-cache-ci` **and** `supervisor` exist in the working tree.
- Dagger golang module `Test` hardcodes flags `-p=1 -count=1 -vet=off -timeout=60m -covermode=atomic` — **no way to pass `-race`** (verified in `golang/main.go`, v2 branch). `Lint` installs the *latest* golangci-lint (CI pins v2.12.2). `Build` builds one binary per call; this repo has two.
- Dagger helm module `Lint` = `helm dependency update` + `helm lint .`; no `helm template` support.
- Tag `2.0.10` exists (no `v` prefix) on `disaster37/dagger-library-go`. Latest Dagger CLI: **v0.21.8**. Current SDK Dockerfile API: `Directory.DockerBuild(opts)`.
- Chart deps come from 5 public repos (Chart.yaml) — `helm dependency update` needs network inside Dagger.
- `.kilo/plans/*` reference old paths — historical agent docs, **do not modify**.
- Task-description corrections: `docs/README.md` helm refs are L136/137/153 (not 184-187, which is the architecture diagram); `cmd/dagger-cache.sh` refs at L178/545/554; additional missed helm refs in `.gitignore` L31, `config.app.yaml.sample` L15/L19, chart's own `README.md` (L95/111/117/284/285) and `values.yaml` (L4).

## 4. Ordered Tasks

### T1 — Git hygiene & .gitignore
1. Check tracking: `git ls-files dagger-cache-ci supervisor coverage.out`. For each tracked: `git rm --cached <file>` (keep on disk).
2. Edit `.gitignore`:
   - L31: `helm/**/charts/*.tgz` → `deploy/helm/**/charts/*.tgz`
   - Append section:
     ```
     # Local build artifacts
     /dagger-cache-ci
     /supervisor
     bin/
     out/
     ```

### T2 — Move Helm chart → deploy/helm/
1. `mkdir -p deploy/helm && git mv helm/dagger-kubernetes deploy/helm/dagger-kubernetes` (preserves history; vendored `charts/*.tgz` are gitignored, so only tracked files move — re-fetch locally afterwards with `helm dependency update deploy/helm/dagger-kubernetes`). Remove now-empty `helm/`.
2. Update self-references inside the moved chart:
   - `deploy/helm/dagger-kubernetes/README.md`: L95, L111, L117, L284, L285 — `helm/dagger-kubernetes` → `deploy/helm/dagger-kubernetes`
   - `deploy/helm/dagger-kubernetes/values.yaml`: L4 — `./helm/dagger-kubernetes` → `./deploy/helm/dagger-kubernetes`
3. Update external references:
   - `.github/workflows/release.yml`: L82 (`sed` on Chart.yaml), L84 (`helm lint`), L88 (`helm dependency update`), L89 (`helm package`), L122 (`git add deploy/helm/dagger-kubernetes/README.md`)
   - `docs/README.md`: L136 (`helm dependency build deploy/helm/dagger-kubernetes`), L137 (`helm install ... deploy/helm/dagger-kubernetes`), L153 (link text + target → `../deploy/helm/dagger-kubernetes/README.md`)
   - `CONTRIBUTING.md`: L144 (structure block), L161, L167, L186, L191, L192
   - `README.md`: layout table (see T10)
   - `hack/update-helm-docs.sh`: L5 `CHART_DIR="${ROOT}/deploy/helm/dagger-kubernetes"`
   - `config.app.yaml.sample`: L15 → `deploy/helm/dagger-kubernetes/values.yaml`; L19 → `deploy/helm/dagger-kubernetes/README.md` (fixes pre-existing bogus `helm/README.md` ref)
   - `.github/workflows/ci.yml` helm job: **no patch needed** — file is wholesale rewritten in T6.

### T3 — Delete dead directories
1. `rm -rf internal/dataplane` (verify empty first; not a git object).
2. `git rm -r internal/rpc` (only `supervisor.proto`).
3. `rm -rf ui-dist` at repo root (empty untracked leftover; verify empty first).

### T4 — Relocate wrapper script + fix GHA action
1. `git mv cmd/dagger-cache.sh hack/dagger-cache.sh`.
2. `cp hack/dagger-cache.sh ci-integrations/gha/dagger-cache.sh` — **byte-identical copy** (fixes the broken composite action). Sync rule is documented in CONTRIBUTING.md (T9), not via file comments, so the copies stay diffable as identical.
3. Update references:
   - `README.md` L21: `cmd/dagger-cache-ci`, `hack/dagger-cache.sh`
   - `docs/README.md` L178, L554: `./hack/dagger-cache.sh call ...`; L545: `hack/dagger-cache.sh` wires up...

### T5 — Create local Dagger module in `dagger/`
Files: `dagger/dagger.json`, `dagger/main.go`, then run `dagger develop` inside `dagger/` to generate `dagger.gen.go`, `internal/`, and normalize `go.mod`/`go.sum` (all committed). Module name: **`dagger-cache`**.

`dagger/dagger.json`:
```json
{
  "name": "dagger-cache",
  "sdk": "go",
  "dependencies": {
    "golang": "github.com/disaster37/dagger-library-go/golang@2.0.10",
    "helm": "github.com/disaster37/dagger-library-go/helm@2.0.10"
  }
}
```
(Pin engine/CLI to v0.21.8; `dagger develop` normalizes the file — accept its output.)

`dagger/main.go` — function spec (see §5 for design details):
- `New(src *dagger.Directory) *DaggerCache` — repo root as source.
- `Lint(ctx) (string, error)` — delegated: `dag.Golang(...)` with custom base container `golang:1.26` + golangci-lint **v2.12.2** preinstalled (official install script), preserving the CI pin.
- `Test(ctx) (*dagger.File, error)` — local: `golang:1.26` (debian, **not** alpine — `-race` needs CGO/gcc), mount src at `/src`, `go vet ./...` then `go test -race -coverprofile=coverage.out -covermode=atomic ./...`; return `coverage.out`.
- `Ui(ctx) (*dagger.Directory, error)` — local: `node:22-alpine`, workdir `/ui`, mount `ui/`, `npm ci || npm install` (mirrors Dockerfile), `npm run typecheck`, `npm run build`; return `dist/` dir.
- `Build(ctx) (*dagger.Directory, error)` — delegated: two `dag.Golang(...).Build(...)` calls (`main=./cmd/supervisor/ out=bin/supervisor`, `main=./cmd/dagger-cache-ci/ out=bin/dagger-cache-ci`, default ldflags `["-s","-w"]`, CGO_ENABLED=0 is set by the module); merge both binaries into one directory under `bin/`.
- `Docker(ctx) (*dagger.Container, error)` — local: `m.Src.DockerBuild()` (root `Dockerfile`), smoke test `ctr.WithExec([]string{"-h"}).Sync(ctx)` (entrypoint `supervisor` + `-h` ≙ `docker run --rm image -h`).
- `Helm(ctx) error` — delegated lint: `dag.Helm(src.Directory("deploy/helm/dagger-kubernetes")).Lint(ctx)`; then local template matrix in `alpine/helm:3.14.0`: `helm dependency update deploy/helm/dagger-kubernetes` + three `helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug` runs with the exact `--set` combos from current ci.yml L125/127/129.
- `Ci(ctx) (*dagger.Directory, error)` — runs Lint → Test → Ui → Build → Docker → Helm; returns directory containing `bin/supervisor`, `bin/dagger-cache-ci`, `coverage.out`.

Constant: `chartDir = "deploy/helm/dagger-kubernetes"` (single source for the chart path).

### T6 — Rewrite `.github/workflows/ci.yml`
Replace all six jobs with one; keep `name`, `on`, `concurrency` unchanged:
```yaml
env:
  DAGGER_VERSION: "0.21.8"

jobs:
  ci:
    name: Dagger CI
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Install Dagger CLI
        run: |
          curl -fsSL https://dl.dagger.io/dagger/install.sh | DAGGER_VERSION="${DAGGER_VERSION}" sh
          echo "$HOME/bin" >> "$GITHUB_PATH"
      - name: Run Dagger CI
        run: dagger call -m ./dagger ci export --path out
      - name: Upload binaries
        uses: actions/upload-artifact@v4
        with:
          name: binaries
          path: out/bin/
      - name: Upload coverage
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: coverage
          path: out/coverage.out
          if-no-files-found: ignore
```
(Implementer: verify install-script PATH behavior on ubuntu-latest and adjust; artifact names `binaries`/`coverage` preserved for downstream consumers.)

### T7 — Documentation
1. **`DAGGER.md`** (new, repo root) — content per §6.
2. **`CONTRIBUTING.md`**:
   - Structure block (L136-145) → new block per §7.
   - New `## Dagger` section after `## Linting`: one-paragraph explanation (local module in `dagger/` delegates to dagger-library-go golang/helm modules; why test/docker/ui are local) + the three key commands + link to `DAGGER.md`.
   - Helm path updates per T2.3.
   - Add sync rule under Documentation maintenance: `hack/dagger-cache.sh` and `ci-integrations/gha/dagger-cache.sh` must stay byte-identical.
   - All existing coding rules untouched.
3. **`README.md`** layout table: script row → `hack/dagger-cache.sh`; `internal/` description drops "dataplane"; add rows `deploy/helm` (Helm chart), `dagger/` (Dagger CI module), `hack/` (dev scripts), `DAGGER.md` (Dagger command reference).
4. **`docs/README.md`** Development section (L624+): add
   ```bash
   # Run the full CI pipeline locally (Dagger CLI required — see DAGGER.md)
   dagger call -m ./dagger ci export --path out
   ```
5. **`AGENTS.md`** Project structure block: add `dagger/`, `deploy/helm/`, `hack/` entries (keeps agent instructions accurate).

### T8 — Validation (run everything in §8)

## 5. Dagger Module Design Notes

- **Delegation map:** lint → golang module `Lint` (pinned linter via custom `base` container, which `New(base, version, src)` accepts and validates); builds → golang module `Build` ×2; helm lint → helm module `Lint`. Everything else local.
- **Why `Test` is local:** upstream `Test` hardcodes go flags (no `-race`, `-vet=off`); parity with current CI (`go vet` + `-race`) is mandatory.
- **`-race` requires CGO:** use debian `golang:1.26` image, do **not** set `CGO_ENABLED=0` in the test container.
- **Bare upstream `ci` fails for this repo:** `Ci` ends in `Build` with no `main` → `go build` at repo root finds no Go files. Direct upstream usage must pass `--main`/`--out` (documented in DAGGER.md). The local module never calls upstream `Ci`, only `Lint`/`Build`.
- **Known parity deltas (accepted, document in DAGGER.md):** upstream `Build` omits `-trimpath` (current CI uses it); upstream `Test` semantics differ (hence local). Release builds still use the Dockerfile/release.yml paths, unaffected.
- **`dagger/` is a separate Go module:** root `go build/vet/test ./...` and golangci-lint do not see it (intended). The 100%-coverage rule applies to app packages; the module is validated by executing its functions. Do not add `dagger/` to any root `go.work` (none exists).
- **Commit codegen:** `dagger.gen.go` + generated `internal/` under `dagger/` must be committed.

## 6. DAGGER.md Content Spec

Sections (concise, command-first):
1. **Overview** — repo's CI is the local module `dagger/` (module name `dagger-cache`), delegating to `github.com/disaster37/dagger-library-go` `golang@2.0.10` + `helm@2.0.10`; what is delegated vs local and why (short table).
2. **Prerequisites** — Dagger CLI `0.21.8` (version pinned in CI; newer may work), running Docker daemon.
3. **Full CI:** `dagger call -m ./dagger ci export --path out` — outputs `out/bin/supervisor`, `out/bin/dagger-cache-ci`, `out/coverage.out`.
4. **Individual functions table:** `lint`, `test`, `ui`, `build`, `docker`, `helm` with exact `dagger call -m ./dagger <fn>` commands and return values.
5. **Direct Daggerverse module usage (bypassing local module):**
   - `dagger call -m github.com/disaster37/dagger-library-go/golang@2.0.10 --src . ci --main ./cmd/supervisor --out bin/supervisor export --path .` — note `--main` is required (repo root has no Go files); upstream `test` runs without `-race`.
   - `dagger call -m github.com/disaster37/dagger-library-go/helm@2.0.10 --src deploy/helm/dagger-kubernetes lint`
6. **Secrets / env:** none required for CI. Helm `push`/`ci` release functions (not used here — releases stay in `release.yml`) would need registry username/password + git token secrets. Optional `DAGGER_CLOUD_TOKEN` for Dagger Cloud observability.
7. **Troubleshooting** — engine startup on first run; `helm dependency update` needs network to 5 chart repos; lint pinned to golangci-lint v2.12.2 via custom base image (drift policy: bump deliberately).

No `hack/dagger.go` helper (superseded by the module). No root `dagger.json` (impossible with root `go.mod`).

## 7. New CONTRIBUTING.md Structure Block

```
cmd/supervisor/      Main server entry point (urfave/cli)
cmd/dagger-cache-ci/ CI helper binary (urfave/cli)
internal/            Private packages (api, auth, ca, cache, config, fleet, observ, session, telemetry, version)
dagger/              Local Dagger module — CI pipeline (delegates to dagger-library-go golang/helm)
hack/                Dev scripts (dagger-cache.sh client wrapper, update-helm-docs.sh)
test/                Integration / functional tests
docs/design/         Architecture decision records
ui/                  Vue 3 SPA (Vite + TypeScript); embeds via internal/api/ui-dist/
deploy/docker        Docker Compose dev stack
deploy/k8s           Kubernetes manifests
deploy/helm/         Helm chart
```

## 8. Validation Steps

1. **Git state:** `git ls-files | grep -E '^(dagger-cache-ci|supervisor|coverage.out)$'` → empty. `git status --porcelain` shows artifacts ignored.
2. **Path sweeps** (exclude `.git`, `.kilo`):
   - `grep -rn 'helm/dagger-kubernetes' . | grep -v 'deploy/helm/dagger-kubernetes'` → no hits.
   - `grep -rn 'cmd/dagger-cache.sh' .` → no hits.
   - `grep -rn 'internal/dataplane\|internal/rpc' .` → no hits outside `.kilo/`.
3. **Root module green:** `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run ./...`.
4. **Helm:** `helm dependency update deploy/helm/dagger-kubernetes && helm lint deploy/helm/dagger-kubernetes` + the 3 `helm template` variants (default; otelCollector+registry disabled; all tools disabled).
5. **Scripts:** `bash -n hack/dagger-cache.sh ci-integrations/gha/dagger-cache.sh hack/update-helm-docs.sh`; `cmp hack/dagger-cache.sh ci-integrations/gha/dagger-cache.sh` → identical; `hack/update-helm-docs.sh` (no arg) runs and produces no diff at current version.
6. **Dagger functions individually:** `dagger call -m ./dagger lint` / `test` / `ui` / `build` / `docker` / `helm` each succeed from repo root.
7. **Full pipeline:** `dagger call -m ./dagger ci export --path out`; verify `out/bin/supervisor --help` and `out/bin/dagger-cache-ci --help` run, `out/coverage.out` non-empty.
8. **Documented upstream commands** (§6.5) run verbatim successfully.
9. **Links:** `docs/README.md` L153 link resolves to `../deploy/helm/dagger-kubernetes/README.md`.
10. **Remote:** push branch; the single CI job passes on GitHub Actions.

## 9. Edge Cases & Error Handling

| Risk | Handling |
|---|---|
| Bare upstream `ci` fails (no Go files at repo root) | Local module calls `Lint`/`Build` only; DAGGER.md documents required `--main` |
| `-race` impossible upstream (hardcoded flags) | Local `Test`; debian image + CGO enabled |
| golangci-lint version drift (upstream installs latest) | Custom base image pins v2.12.2; fallback if flaky: accept latest and document drift |
| `git mv` of chart: vendored `charts/*.tgz` are gitignored | After move, run `helm dependency update deploy/helm/dagger-kubernetes` to re-fetch locally |
| Tracked artifacts (`dagger-cache-ci`, `supervisor`, maybe `coverage.out`) | `git rm --cached` if tracked; `.gitignore` prevents re-add |
| Empty dirs (`internal/dataplane`, root `ui-dist`) are not git objects | Verify empty, plain `rm -rf` |
| `.dockerignore` may not be honored by `Directory.DockerBuild` | Verify image content; final stage only COPYs specific builder artifacts so impact is cosmetic; add `WithoutDirectory` exclusions if needed |
| Helm dep update inside Dagger needs outbound network to 5 chart repos | Expected to work on GH runners; surface clear error if blocked |
| Dagger CLI install PATH on GH runner | Verify during implementation; adjust `GITHUB_PATH` step |
| `dagger/` codegen out of sync | `dagger develop` regenerates; commit generated files |
| `.kilo/plans/*` contain stale old-path references | Historical docs — intentionally untouched |
| No ADR for decision-grade changes | Explicit user decision; recorded here (deviation from AGENTS.md mandate) |

## 10. Out of Scope

- `release.yml` workflow logic (only helm path fixes); no Dagger migration of release/publish.
- `ci-integrations/jenkins`, `ci-integrations/drone` (no affected references).
- `Dockerfile` changes (Dagger docker step reuses it as-is).
- Tests for the `dagger/` module itself.
- ADR creation (declined by user).
- `.kilo/` historical plans.

## 11. File Operations Index

**Create:** `dagger/dagger.json`, `dagger/main.go`, `dagger/go.mod`+`go.sum`+`dagger.gen.go`+`internal/` (generated), `DAGGER.md`, `ci-integrations/gha/dagger-cache.sh`, `.opencode/plans/project-reorganization.md`.
**Move:** `helm/dagger-kubernetes/` → `deploy/helm/dagger-kubernetes/` (git mv); `cmd/dagger-cache.sh` → `hack/dagger-cache.sh` (git mv).
**Modify:** `.gitignore`, `.github/workflows/ci.yml` (rewrite), `.github/workflows/release.yml`, `README.md`, `CONTRIBUTING.md`, `AGENTS.md`, `docs/README.md`, `config.app.yaml.sample`, `hack/update-helm-docs.sh`, `deploy/helm/dagger-kubernetes/README.md`, `deploy/helm/dagger-kubernetes/values.yaml`.
**Delete:** `internal/dataplane/`, `internal/rpc/`, root `ui-dist/`, empty `helm/` after move; untrack artifacts via `git rm --cached` if tracked.
