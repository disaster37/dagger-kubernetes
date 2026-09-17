# Plan: Fix "manual CI is not displayed" in the Pipelines list (issue #8)

## 1. Problem statement & root cause

The **Pipelines list** (`ui/src/views/Pipelines.vue`) renders the CI column with
`{{ ciLabel(trace.ci_provider) }}` (line 40). Its local helper renders `-` for a
manual (local) run:

`ui/src/views/Pipelines.vue:180-186`
```ts
// The CLI reports "dagger.io/ci" as "true"/"false" (whether a CI is detected),
// not a provider name. Render a human-friendly label for the CI column.
function ciLabel(ci: string): string {
  if (!ci || ci === 'false') return '-'      // <-- BUG: manual shows "-"
  if (ci === 'true') return 'CI'
  return ci
}
```

The backend **intentionally** stores an empty `ci_provider` for manual runs and
documents that the frontend should render the label `manual`:

`internal/service/otlp_extract.go:283-299`
```go
// Absent or "false" means a manual (local) run, so no provider is recorded
// and the frontend renders "manual".
switch ci {
case "true":
    if ciVendor != "" { sum.CIProvider = ciVendor } else { sum.CIProvider = "ci" }
case "", "false":
    sum.CIProvider = ""
default:
    sum.CIProvider = ci
}
```

The **detail view** already does the right thing:

`ui/src/pipeline/PipelineView.vue:649-653`
```ts
function ciLabel(value?: string): string {
  if (!value || value === 'false') return 'manual'
  if (value === 'true') return 'ci'
  return value
}
```

**Root cause:** the two duplicated `ciLabel` implementations have drifted. The
list view (`Pipelines.vue`) maps empty/`false` → `-` while the detail view
(`PipelineView.vue`) maps it → `manual`. `History.vue` does not render
`ci_provider` at all, so it is unaffected. This is a **UI-only, display-only**
bug; the backend storage is correct and must not change.

**Chosen fix (minimal + correct):** make `Pipelines.vue`'s `ciLabel` identical to
`PipelineView.vue`'s, so manual runs render `manual` in the list and both views
agree on the `manual` / `ci` / vendor-name labels. No backend change. `manual`
remains a **display-only label**; the stored value stays `""` (and, because of
`omitempty`, the JSON field is absent → `undefined` on the wire).

## 2. Exact files to change

1. `ui/src/views/Pipelines.vue` — the fix (the only source change).
2. `internal/handler/ui-dist/` — **regenerated** embedded assets (the Vue fix
   must be compiled into the `//go:embed` bundle; see §4). Old content-hashed
   files (e.g. `internal/handler/ui-dist/assets/Pipelines-Pw3XBxN1.js`) are
   replaced by new hashes.
3. `ui/dist/` — regenerated Vite output (build artifact). Confirm with
   `git status` whether it is tracked; if so, commit its regeneration too. It is
   excluded from the Docker build context via `.dockerignore:30` (`ui/dist`),
   so it is **not** required by CI — only `internal/handler/ui-dist/` is.

No backend (Go) files change. No `api/types.ts` / `api/client.ts` change is
required (the existing `ci_repo` convention already types `ci_provider` as
`string` and relies on truthiness guards for the `omitempty` case).

## 3. Data structures / function signatures

Only one function signature changes. No Go changes.

`ui/src/views/Pipelines.vue` — replace the existing helper (and its comment):

```ts
// ciLabel maps the stored ci_provider value to a human-readable label. Local
// (manual) runs store "" or "false" (and the JSON field is omitted via
// omitempty, so it may also be undefined at runtime); a bare "true" is shown as
// "ci"; otherwise the provider name is shown verbatim. Mirrors PipelineView.vue.
function ciLabel(ci?: string): string {
  if (!ci || ci === 'false') return 'manual'
  if (ci === 'true') return 'ci'
  return ci
}
```

Notes on the signature change:
- `ci: string` → `ci?: string`: `TraceMeta.CIProvider` has the JSON tag
  `json:"ci_provider,omitempty"` (`internal/domain/tracemeta.go:46`), so the
  list response omits the key entirely for manual runs and the field is
  `undefined` in JS. The optional parameter documents this. `PipelineView.vue`
  already uses `value?: string`.
- The `ci === 'true'` → `'ci'` change (was `'CI'`) makes the two helpers
  identical. It has **no practical effect on the list** because ingest
  normalizes bare `"true"` to the literal `"ci"` (`otlp_extract.go:293`), so the
  list never receives `"true"`. It only removes drift and matches the detail
  view.

## 4. Step-by-step implementation instructions

1. Edit `ui/src/views/Pipelines.vue`, replacing lines 180-186 (the comment +
   `ciLabel` function) with the snippet in §3.
2. Rebuild the SPA and regenerate the embed source (from repo root):
   ```bash
   cd ui && npm ci && npm run typecheck && npm run build
   cd ..
   rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist
   ```
   - `npm ci` (Dockerfile/`dagger` use `npm ci`) — fall back to `npm install`
     if the lockfile is not present locally.
   - `rm -rf` first is required: Vite content-hashes filenames, so the old
     `Pipelines-<oldhash>.js` must not linger unreferenced next to the new one.
   - This mirrors the root `Dockerfile:20`
     (`COPY --from=ui-builder /ui/dist ./internal/handler/ui-dist/`). For local
     `go build` / CI the checked-in `internal/handler/ui-dist/` must exist
     because of `//go:embed all:ui-dist` (`internal/handler/ui.go:16`).
3. Confirm `git status` shows the regenerated files under
   `internal/handler/ui-dist/` (and `ui/dist/` if tracked). Commit the source
   change together with the regenerated assets in the same changeset.
4. Redeploy to the local cluster per `AGENTS.local.md` §4-§6 (build → push →
   capture values → `helm upgrade` → `rollout restart` → agent checks §5.1 →
   human verification §5.2). The image tag is mutable `dev` with
   `imagePullPolicy: Always`, so a forced `rollout restart` is mandatory
   (`AGENTS.local.md` §4.5).

## 5. Edge cases

| `ci_provider` value | Rendered label | Rationale |
|---|---|---|
| `""` (empty) | `manual` | manual run (backend stores `""`) |
| `undefined` / `null` | `manual` | `omitempty` omits the key; `!ci` covers both |
| `"false"` | `manual` | legacy/detail-path value (Tempo reconstruction can emit `"false"`) |
| `"true"` | `ci` | bare CI detected; defensive (ingest already normalizes to `"ci"`) |
| `"ci"` | `ci` | literal stored by ingest when `dagger.io/ci=true` with no vendor |
| `"github"`, `"gitlab"`, `"circleci"`, … | verbatim | vendor name from `ci.vendor`/`ci.provider` or direct `ci` value |
| `"True"` / `"TRUE"` / `" False "` | verbatim | case/whitespace-sensitive exact match, matching the backend (`otlp_extract.go` `switch ci` cases) |

Deliberate non-goals: no case-folding and no whitespace trimming. The backend is
the source of truth and only emits lowercase literals; the frontend mirrors its
exact-match semantics to avoid any display/storage divergence.

## 6. Error handling & validation

None. `ciLabel` is a pure, total display function: every input (including
`undefined`, `null`, empty, unknown strings) produces a non-empty string and
never throws. Malformed/unknown input falls through to verbatim display, which
is the safe behavior.

## 7. Tests to add/update

- **No frontend unit-test framework exists.** `ui/package.json` scripts are
  `dev` / `build` / `preview` / `typecheck` only — no `vitest`/`jest`, and
  `glob('ui/**/*.{test,spec}.*')` returns no files. **Do not add a test
  framework for this fix.** Verification is `npm run typecheck` (§9) plus manual
  UI verification.
- **No new Go tests.** The fix is UI-only. The backend normalization is already
  covered by `internal/service/otlp_extract_test.go`:
  `TestExtractTraceSummariesCIProvider` (cases: `ci-true-with-vendor` →
  `github`, `ci-true-no-vendor` → `ci`, `ci-false-manual` → `""`,
  `ci-absent-manual` → `""`, `ci-vendor-name-direct` → `gitlab`,
  `span-level-ci-provider` → `circleci`).

### Manual verification (replace unit tests)

With a manual/CI run available in the UI:
1. Open the **Pipelines** list; a manual (local `dagger call`) run's CI column
   shows **`manual`** (previously `-`).
2. Open the same run's **detail** view (`/pipelines/<trace_id>`); the Details
   table "CI Provider" row shows **`manual`** (already did).
3. A CI run (e.g. GitHub Actions, `dagger.io/ci.vendor=github`) shows `github`
   in **both** the list and detail.
4. A bare-CI run (no vendor) shows `ci` in both views.
5. `npm run typecheck` passes.

## 8. Documentation updates

**No doc updates required.** This is a display-only bug fix: no config key, no
new feature, no design decision change. Per AGENTS.md, docs
(`config/config.app.yaml.sample`, `docs/README.md`, `docs/design/` ADRs) are
updated only when those change. Specifically:
- `docs/README.md` §"Pipeline UI" (lines 1616-1630) describes the list's
  name/status/version/duration columns but does **not** document the CI-column
  label, so nothing to update.
- No ADR documents the `manual` label; `ADR-010` references `dagger.io/ci.repo`
  (attribution) but not the CI display label.
- `DAGGER.md`/`.github/workflows/` are untouched, so no `DAGGER.md` update.

## 9. Verification commands

Run in this order from the repo root:

```bash
# UI typecheck + build (must pass after the .vue edit)
cd ui && npm ci && npm run typecheck && npm run build && cd ..

# Regenerate the embedded assets
rm -rf internal/handler/ui-dist && cp -r ui/dist internal/handler/ui-dist

# Go minimum gate (always required)
go build ./... && go vet ./... && go test ./...

# Full CI gate (run when a Docker daemon is available — see below)
dagger call -m ./dagger --src . ci export --path out
```

Docker availability is not known at plan time. Rules per `AGENTS.md`:
- **Docker daemon available:** run the full `dagger call -m ./dagger --src . ci
  export --path out`. It runs `golangci-lint`, `go vet ./...`,
  `go test -race -covermode=atomic ./...`, the UI build, binary builds, the
  Dockerfile smoke test, and the Helm lint/template matrix.
- **Docker not available (minimum):** `go build ./... && go vet ./... &&
  go test ./...` **plus** `dagger call -m ./dagger --src . lint` for the lint
  step.

Additional lint/safety reminders (from `AGENTS.md`, not triggered by this diff
but do not regress them):
- **Dead symbols (`unused` linter):** this fix removes no call site and adds no
  orphaned symbol, but `golangci-lint` fails on any zero-reference
  function/type/var. The updated `ciLabel` is still referenced by line 40.
- **Integration-test ports:** `tests/integration/` must bind via `freeListener(t)`
  (never probe-then-close a port). No integration test is added here, so no
  change, but keep the rule in mind if you extend coverage.

## 10. Risks / rollback

- **Content-hash churn:** regenerating the bundle changes hashed asset filenames
  under `internal/handler/ui-dist/assets/` (e.g. `Pipelines-*.js`,
  `index-*.js`, `index-*.css`). This is expected; `index.html` is regenerated to
  reference the new hashes. Always `rm -rf` before `cp -r` so stale hashed files
  are not committed.
- **`-` → `manual` display change:** rows that previously showed `-` for empty/
  `false` now show `manual`. This is the intended fix. There is no "unknown"
  semantic being lost — empty/`false` always meant "manual run" by contract.
- **`CI` → `ci` (bare-true branch):** effectively unreachable in the list
  (ingest normalizes to `ci`), so zero real-world impact; aligns with the detail
  view. If reviewers prefer to keep `CI`, this single line can be reverted
  independently of the bug fix.
- **`omitempty`/`undefined` handling:** the new `ci?: string` + `!ci` guard
  covers the omitted-field case; no `TypeError` possible.
- **Rollback:** `git revert` the single changeset (source + regenerated assets).
  If already deployed, `helm --kubeconfig /home/user/.kube/home rollback
  dagger-kubernetes-test -n dagger-kubernetes-test` to the previous revision
  (see `AGENTS.local.md` for the current live revision).

## Validation checklist

- [ ] `ui/src/views/Pipelines.vue` `ciLabel` updated per §3
- [ ] `cd ui && npm run typecheck` passes
- [ ] `internal/handler/ui-dist/` regenerated and no stale `Pipelines-*.js`
- [ ] `go build ./... && go vet ./... && go test ./...` green
- [ ] Full `dagger call -m ./dagger --src . ci export --path out` green (or the
      documented minimum gate + `lint` if no Docker daemon)
- [ ] Manual UI: manual run shows `manual` in list and detail; CI/vendor runs
      show the vendor name in both
- [ ] Redeployed + verified on the `home` cluster per `AGENTS.local.md` §4-§6
