# Issue #6 — Config-driven project → group mapping (Helm-settable)

## Goal

Add a Helm-settable, ordered list of `{pattern, group}` rules that maps a
**project name** (CI repo slug, e.g. `github.com/acme/api`) to a **supervisor
group name** during OTLP ingest. This is the config/declarative counterpart to
the existing per-group `auto_assign_pattern` (which is set via the admin
API/UI only). Explicit project assignment always wins; when a config rule
matches it is authoritative; otherwise the existing per-group auto-assign runs
unchanged. An empty mapping list changes **nothing** (backward compatible).

Out of scope (explicitly): retroactive/startup sweep of already-unassigned
projects, group auto-creation for mapping targets, capture substitution in the
target name, and a new Prometheus metric.

---

## Key design decisions (rationale included)

### 1. Config schema

New top-level config section `attribution` (the concern that owns project→group
resolution today is `AttributionService`), key `project_mappings`.

```yaml
attribution:
  project_mappings: []            # ordered list of {pattern, group}; empty = disabled
    # - pattern: '^github\.com/acme/.*'   # Go regexp vs project name (case-sensitive; (?i) opt-in)
    #   group: 'acme'                      # literal target supervisor group name
```

- `domain.Config` gains `Attribution AttributionConfig `mapstructure:"attribution"``.
- `AttributionConfig{ ProjectMappings []ProjectMappingRule `mapstructure:"project_mappings"` }`.
- `ProjectMappingRule{ Pattern string `mapstructure:"pattern"`; Group string `mapstructure:"group"` }`.
- Default: `v.SetDefault("attribution.project_mappings", []domain.ProjectMappingRule{})`.
- Env override: `DAGGER_KUBERNETES_ATTRIBUTION_PROJECT_MAPPINGS` (unwieldy for a
  list; documented but not the primary path — the Helm value is).
- Helm path: `supervisor.config.attribution.projectMappings` (see §7).

*Alternative considered:* `projects.mappings`. Rejected — `projects` is an
entity with API CRUD, not a config concern; `attribution` names the subsystem
that consumes the rules. `attribution.project_mappings` mirrors the existing
`auth.oauth.group_mappings` precedent for config-driven regex mapping.

### 2. Semantics

- **Ordered first-match-wins.** Rules are applied in config order to the
  project name; the first rule whose pattern matches wins. Deterministic and
  identical to `auth.oauth.group_mappings` (ADR-022) and the current
  `autoAssign` "first group by id" scan.
- **Unanchored regex, case-sensitive.** Go `regexp` `MatchString` semantics —
  exactly what `auto_assign_pattern` uses today. Document "always anchor
  (`^...$`); `(?i)` for case-insensitive", same guidance as README's
  `group_mappings` note.
- **Multiple rules match → first wins.** No ambiguity.
- **Target group missing → skip with a `Warn` log; do NOT auto-create.** Unlike
  ADR-035 (OAuth membership auto-create), these groups are operator-declared in
  the same Helm change that would create them; silently creating groups here
  would surprise operators and the rules are bounded (literal names, no
  capture expansion). A missing target is a config/ops mistake to surface, not
  paper over. *(Auto-create is a possible follow-up; note in ADR.)*
- **Precedence (highest→lowest):**
  1. Explicit assignment (`project.GroupID != ""`) — never touched.
  2. Config `project_mappings` — when a rule matches it is **authoritative**:
     if its target group is missing, the project is **skipped** (no fallthrough
     to auto-assign), because the operator wrote an explicit rule and a silent
     fallback to an `auto_assign_pattern` group would be surprising.
  3. Per-group `auto_assign_pattern` — runs **only when no config rule matches**
     (or when no rules are configured). Unchanged behavior.
- **Complement, not replace.** Config mappings run *before* auto-assign; empty
  config → auto-assign behaves exactly as today.

### 3. When mapping applies

- **Only at OTLP ingest**, for projects whose `GroupID` is empty at ingest time.
- **Not retroactive at startup.** Rationale: minimal scope; a startup sweep
  mutating Raft state would be a new background job with ordering/leadership
  concerns. Existing unassigned projects are re-evaluated naturally on their
  next ingest. Set-once semantics are preserved: once a project is assigned
  (explicitly, by mapping, or by auto-assign), `project.GroupID != ""` and no
  re-evaluation happens on later ingests.

### 4. Validation (fail-fast at config load)

New `validateProjectMappings` in `config/loader.go`, called from `Load` right
after `validateGroupMappings`. It checks (and **only** these, matching the
`group_mappings` split of responsibilities in ADR-022):

- `pattern` non-empty and compiles as a Go regexp;
- `group` non-empty.

Exact error strings (config-path-named, matching existing style):

- `attribution.project_mappings[%d].pattern must not be empty`
- `attribution.project_mappings[%d].pattern %q: %w` (wrap the `regexp` error)
- `attribution.project_mappings[%d].group must not be empty`

The `Load` wrapper is `validate project mappings: %w` (mirrors `validate group
mappings: %w`).

**Group-name charset/length is NOT validated at config load.** The config
package deliberately does not import `service` (ADR-022 note). Instead, the
startup Warn in `cmd/api/main.go` (see §6) flags a `group` that is not a valid
supervisor group name — identical to the existing ADR-030 warning for
`group_mappings` replacements. A runtime lookup of an invalid name simply
misses and is skipped.

**Duplicate rules are NOT rejected** (first-match-wins makes them harmless;
`group_mappings` also allows duplicates). Noted, not enforced.

### 5. Observability

Logrus only (no new Prometheus metric — out of scope; noted as an optional
follow-up counter, e.g. `dagger_kubernetes_project_attribution_total{source=...}`):

- `INFO` `"attribution: project_mappings assigned"` with `project_id`,
  `project`, `group`, `group_id`.
- `WARN` `"attribution: project_mappings target group not found, skipping"`
  with `project_id`, `project`, `group` (and `WithError`).
- `WARN` `"attribution: project_mappings persist failed"` with `project_id`
  (mirrors the existing `auto-assign: persist failed`; the trace is still
  attributed, only persistence failed).
- Startup `WARN` `"attribution: project_mappings group is not a valid supervisor
  group name; it can never match an existing group"` with `rule_index`, `group`.

### 6. Security

- **ReDoS: none.** Go `regexp` is RE2 (linear-time), same rationale as
  `group_mappings` (ADR-022) and `auto_assign_pattern`.
- **Unbounded input: already bounded.** `Ingest` clamps `ci_repo`/`gitRemote` to
  `maxIngestFieldLen` (256) before any matching; `ProjectService` enforces
  `maxProjectNameLen` (256) on upsert. The pattern is matched against an
  already-bounded project name.
- **Group-name validation:** the *target* name is a literal from operator
  config (trusted), but runtime lookup still goes through
  `GroupRepository.GetByName` and invalid/missing names are skipped — never
  stored, never fatal.
- **No privilege escalation:** mapping only affects trace attribution/visibility
  within groups; the rules are set by the operator (Helm), the same trust level
  as admin API access.

### 7. Rollout / backward compatibility

Empty `project_mappings` (the default) ⇒ `ProjectMapper.Active() == false` ⇒
`resolveGroup` falls straight through to the existing `autoAssign`. No behavior
change. Existing projects that are already assigned are never re-mapped.
Config file without the `attribution` key loads fine (Viper default).

---

## Data structures & exact signatures

### `internal/domain/config.go`

```go
// (add field to Config)
Attribution AttributionConfig `mapstructure:"attribution"`

// AttributionConfig governs project → group attribution at OTLP ingest.
type AttributionConfig struct {
    // ProjectMappings maps a project name (CI repo slug) to a supervisor group
    // name. Ordered; first-match-wins. Empty = disabled (per-group
    // AutoAssignPattern and explicit assignment still apply).
    ProjectMappings []ProjectMappingRule `mapstructure:"project_mappings"`
}

// ProjectMappingRule maps a project name to a supervisor group name. Pattern is
// a Go regexp matched against the project name (case-sensitive; (?i) opt-in);
// Group is the literal target supervisor group name (no capture substitution).
type ProjectMappingRule struct {
    Pattern string `mapstructure:"pattern"`
    Group   string `mapstructure:"group"`
}
```

### `internal/service/project_mapper.go` (new file)

```go
package service

// ProjectMapper maps project names to supervisor group names using an ordered
// list of regex rules. First-match-wins; Go regexp is RE2 (linear-time, no
// ReDoS). The target group is a literal name (no capture substitution).
type ProjectMapper struct {
    rules []projectMappingRule
}

type projectMappingRule struct {
    pattern *regexp.Regexp
    group   string
}

// NewProjectMapper compiles the configured rules. A nil or empty list yields an
// inactive mapper. Errors name the offending rule index (defense-in-depth;
// config.Load already validated them).
func NewProjectMapper(rules []domain.ProjectMappingRule) (*ProjectMapper, error)

// Active reports whether any rules are configured.
func (m *ProjectMapper) Active() bool

// Map returns the target group of the first matching rule, or ("", false) when
// no rule matches.
func (m *ProjectMapper) Map(projectName string) (string, bool)
```

`NewProjectMapper` error strings: `project_mappings[%d] pattern must not be
empty` and `project_mappings[%d] pattern %q: %w`.

### `internal/service/attribution_service.go` (modify)

```go
// struct field added:
projectMapper *ProjectMapper

// SetProjectMapper wires the optional config-driven project→group mapper. A nil
// or inactive mapper disables the feature (backward compatible). Mirrors the
// AuthService.SetOAuthRevalidator optional-dependency pattern.
func (a *AttributionService) SetProjectMapper(m *ProjectMapper)

// resolveGroup assigns proj to a group via, in order: config project_mappings
// (first-match-wins, authoritative when matched), then per-group
// AutoAssignPattern. Returns the group id, or "" when nothing matches.
func (a *AttributionService) resolveGroup(ctx context.Context, proj *domain.Project) string

// mappingAssign resolves proj.Name against projectMapper. Returns (groupID,
// matched). matched=false only when no rule matched (caller falls through to
// autoAssign). matched=true with empty groupID means a rule matched but the
// target group is missing (skipped; caller must NOT fall through).
func (a *AttributionService) mappingAssign(ctx context.Context, proj *domain.Project) (string, bool)
```

`Ingest` change: replace the `groupID = a.autoAssign(ctx, proj)` line with
`groupID = a.resolveGroup(ctx, proj)` (still guarded by `groupID == ""` after
`proj.GroupID`).

---

## Implementation checklist (dependency order)

### Step 1 — Domain (`internal/domain/config.go`)
Add `Attribution` field + `AttributionConfig` + `ProjectMappingRule` (above).
Domain stays stdlib-only (no new imports).

### Step 2 — Config (`config/loader.go`)
1. Add default `v.SetDefault("attribution.project_mappings", []domain.ProjectMappingRule{})`
   near the other slice defaults (after `auth.oauth` block, ~line 59).
2. Add call in `Load` (after `validateGroupMappings`, ~line 249):
   ```go
   if err := validateProjectMappings(&cfg); err != nil {
       return nil, fmt.Errorf("validate project mappings: %w", err)
   }
   ```
3. Add `func validateProjectMappings(cfg *domain.Config) error` (uses
   `regexp.Compile`, error strings from §4). **Do not** import `service`.

### Step 3 — Service (`internal/service/`)
1. New `project_mapper.go` (above).
2. `attribution_service.go`: add field, `SetProjectMapper`, `resolveGroup`,
   `mappingAssign`; update `Ingest`.
   - `mappingAssign` uses `a.groups.GetByName(ctx, name)`; on
     `domain.ErrNotFound` (or any error) it logs the Warn and returns
     `("", true)`; on success it calls `a.projects.Assign(ctx, proj.ID, g.ID)`
     (persist error logged, still returns `(g.ID, true)`).

### Step 4 — Wiring (`cmd/api/main.go`)
After line 198 (`attributionSvc := service.NewAttributionService(...)`), add:
```go
projectMapper, err := service.NewProjectMapper(cfg.Attribution.ProjectMappings)
if err != nil {
    return fmt.Errorf("compile project mappings: %w", err)
}
attributionSvc.SetProjectMapper(projectMapper)

for i, rule := range cfg.Attribution.ProjectMappings {
    if !service.ValidateGroupName(rule.Group) {
        logger.WithFields(logrus.Fields{
            "rule_index": i,
            "group":      rule.Group,
        }).Warn("attribution: project_mappings group is not a valid supervisor group name; it can never match an existing group")
    }
}
```
(No new imports: `service`, `logrus` already imported. This block is
independent of OAuth — do not nest it inside the `if cfg.Auth.OAuth.Enabled`.)

### Step 5 — Helm (`deploy/helm/dagger-kubernetes/`)
1. `values.yaml` — under `supervisor.config`, after the `cli:` block (before
   `## @param supervisor.config.logLevel`), add:
   ```yaml
   ## @param supervisor.config.attribution.projectMappings [array] Ordered list of {pattern, group} mapping a project name (CI repo slug; Go regexp, case-sensitive) to a supervisor group name. First-match-wins; no match falls through to per-group auto_assign_pattern; empty = disabled. Target groups must already exist.
       attribution:
         projectMappings: []
         # projectMappings example (regex → supervisor group name):
         # projectMappings:
         #   - pattern: '^github\.com/acme/.*'
         #     group: 'acme'
   ```
   (Mind the 4-space indent: this sits under the `config:` key.)
2. `templates/configmap.yaml` — add a top-level block (suggest after the
   `cors:` block, before `database:`):
   ```yaml
       attribution:
         project_mappings:
   {{ toYaml .Values.supervisor.config.attribution.projectMappings | indent 8 }}
   ```
3. Regenerate `deploy/helm/dagger-kubernetes/README.md` from the `## @param`
   annotations (run `scripts/update-helm-docs.sh`; verify the new
   `supervisor.config.attribution.projectMappings` row appears).

### Step 6 — Docs (`config/*.yaml`, `docs/`, `docs/design/`)
1. `config/config.app.yaml.sample` — add a documented `attribution:` block
   (place near the end, e.g. before `# --- Logging ---`), with the full comment
   set (ordered, first-match-wins, unanchored case-sensitive regex, target must
   exist, precedence over `auto_assign_pattern`).
2. `config/config.app.yaml` — add a minimal `attribution: project_mappings: []`
   block with a one-line comment (mirrors how `group_mappings: []` is listed).
3. `docs/README.md`:
   - Config table: add rows under a new `attribution` section
     (`project_mappings` → `[]` → notes).
   - "Groups, projects, and quota" section (~line 1163): document the three-tier
     precedence (explicit → `project_mappings` → `auto_assign_pattern`).
   - Add a short "Config-driven project → group mapping" subsection under the
     Authentication/Groups area documenting the YAML shape, first-match-wins,
     anchor guidance, missing-group skip.
4. New `docs/design/ADR-036-config-project-group-mapping.md` (Status: accepted;
   Related: ADR-010, ADR-022, ADR-030, ADR-035). Document decisions D1–D6 above
   (schema, first-match-wins/unanchored/case-sensitive, missing-target skip =
   no auto-create, precedence, ingest-only timing, validation split). Add a row
   to `docs/design/index.md`.

### Step 7 — Tests (see §8)

### Step 8 — Verify (see §10) and open PR.

---

## Edge cases table

| # | Case | Expected behavior |
|---|------|-------------------|
| 1 | `project_mappings` empty / key absent | `Active()==false`; auto-assign unchanged. |
| 2 | Invalid regex in a rule | `config.Load` fails: `validate project mappings: attribution.project_mappings[i].pattern ...`. |
| 3 | Empty `pattern` / empty `group` | `config.Load` fails with the index-named message. |
| 4 | Target group does not exist | Warn + skip; **no** fallthrough to auto-assign (rule matched = authoritative); `group_id` empty. |
| 5 | Project already explicitly assigned | `proj.GroupID != ""` → mapping never consulted (set-once). |
| 6 | Project previously auto-assigned | Same as #5: `GroupID` persisted, no re-mapping. |
| 7 | Concurrent ingests for a new project | `GetOrCreateByName` is race-safe (conflict retry); both then resolve the same group name and `Assign` the same id (idempotent value). |
| 8 | Very long project name (>256) | Bounded by `maxIngestFieldLen` before matching (dropped → empty repo → no project row). |
| 9 | Mapping target group deleted later | Raft `deleteGroup` nulls `projects.group_id` (fsm.go ~line 611); next ingest re-evaluates mapping. |
| 10 | Multiple rules match | First (config-order) wins. |
| 11 | No rule matches | Falls through to `auto_assign_pattern`. |
| 12 | `group` invalid supervisor name (space/len) | Startup Warn; runtime lookup misses → skip (never stored). |
| 13 | Pattern matches only part of name (unanchored) | Matches (prefix); documented "always anchor `^...$`". |
| 14 | Duplicate rules | Allowed; first occurrence wins. |
| 15 | `groups.List`/`GetByName` store error | Warn (best-effort), `group_id` empty; ingest never fails. |

---

## Tests

### Unit — `internal/service/project_mapper_test.go` (new)
Table-driven, stdlib `testing` only:
- `TestProjectMapperMap`: first-match-wins; no-match → `false`; case-sensitivity
  (`Foo` vs `foo`); `(?i)` opt-in.
- `TestProjectMapperActive`: nil/empty inactive; non-empty active.
- `TestProjectMapperNewErrors`: empty pattern → `project_mappings[0] pattern must
  not be empty`; invalid regex `[` → error containing `project_mappings[0]`.
- (Optional) `TestProjectMapperLinearTime` mirroring `group_mapper_test.go` RE2
  linear-time note.

### Unit — `internal/service/attribution_service_test.go` (extend)
Add a helper (and update `newAttributionForTest` to build the mapper):
```go
func newAttributionWithMapper(t *testing.T, rules []domain.ProjectMappingRule) (*AttributionService, *GroupService, *UserService, *repos)
```
- `TestAttributionIngestProjectMappingAssigns` — rule matches; `traceMeta.GroupID`
  equals the mapped group; project persisted with `GroupID`.
- `TestAttributionIngestProjectMappingPrecedenceExplicitWins` — explicit assign
  overrides a matching rule.
- `TestAttributionIngestProjectMappingAuthoritativeOverAutoAssign` — a matching
  rule (valid target) is chosen even though an `auto_assign_pattern` group also
  matches (mapping wins).
- `TestAttributionIngestProjectMappingNoMatchFallsBackToAutoAssign` — rule does
  not match → `auto_assign_pattern` applies.
- `TestAttributionIngestProjectMappingMissingGroupSkipped` — rule matches but
  group absent → `group_id == ""` AND no fallthrough to a matching
  `auto_assign_pattern` group.
- `TestAttributionIngestProjectMappingFirstMatchWins` — two rules, first wins.
- `TestAttributionIngestProjectMappingEmptyConfigPreservesBehavior` — nil mapper
  → auto-assign as before.

### Unit — `config/loader_test.go` (extend)
- `TestValidateProjectMappings` (table): empty valid; valid rules; empty pattern;
  invalid regex `[`; empty group — asserting exact `attribution.project_mappings[i]...`
  substrings.
- Add default assertion to the existing defaults test (~line 131): 
  `cfg.Attribution.ProjectMappings` non-nil and empty.
- `TestLoadRejectsInvalidProjectMappings` — write a config file with a bad
  pattern; assert `Load` error contains `validate project mappings` and
  `attribution.project_mappings[0].pattern`.
- `TestLoadProjectMappings` — write a file with two valid rules; assert decoded
  `[]domain.ProjectMappingRule` matches (verifies the `collectSettings`/
  mapstructure path end-to-end, same shape as `group_mappings`).

### Integration — `tests/integration/project_mapping_test.go` (new)
Mirror `oauth_group_autocreate_test.go` construction: real in-memory Raft store
+ `NewProjectMapper(rules)` + `NewAttributionService(...)` + `SetProjectMapper`
+ `Ingest`, assert `traceMeta.Get(...).GroupID` equals the mapped group and the
project row is assigned. Use `freeListener`/existing store helpers; **no
hardcoded ports** (per AGENTS.md).

### Helm template
No change to `dagger/main.go` (matrix is fixed). Verify (see §10) that the
default render emits `attribution:\n  project_mappings: []` and that a `--set`
with a rule renders correctly. `helm lint` must stay green.

### How to run
- `go test ./config/... ./internal/service/...` for the unit suites.
- `go test ./...` for everything.
- `go test -race -covermode=atomic ./...` (CI parity).
- Full gate: `dagger call -m ./dagger --src . ci export --path out` (with
  Docker); minimum without Docker: `go build ./... && go vet ./... &&
  go test ./...` plus `dagger call -m ./dagger --src . lint`.

---

## Definition of done

- [ ] `go build ./... && go vet ./...` clean.
- [ ] `go test ./...` passes; `go test -race -covermode=atomic ./...` passes.
- [ ] `dagger call -m ./dagger --src . lint` passes (golangci-lint — **no dead
      symbols**; the new `ProjectMapper`, `SetProjectMapper`, `resolveGroup`,
      `mappingAssign`, `validateProjectMappings` are all referenced).
- [ ] `dagger call -m ./dagger --src . ci export --path out` passes (or the
      documented minimum when no Docker daemon).
- [ ] `helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug`
      renders; `helm lint deploy/helm/dagger-kubernetes` clean; the chart's
      `README.md` regenerated via `scripts/update-helm-docs.sh`.
- [ ] `config/config.app.yaml.sample`, `config/config.app.yaml`, `docs/README.md`
      (config table + auto-assign/precedence section), `docs/design/index.md`
      and the new ADR-036 are updated in the same changeset.
- [ ] `DAGGER.md` is NOT touched (no change to `dagger/`, CI scripts, or
      `.github/workflows/`).
- [ ] Manual redeploy validation per `AGENTS.local.md` §6 (cluster `home`,
      release `dagger-kubernetes-test`) if a cluster is available.

---

## Branch / PR workflow

- Branch: `fix/issue-6-project-group-mapping` from `main`.
- Commit the changeset atomically (code + config + helm + docs + tests).
- Push, open PR against `main`, and confirm the GitHub Actions `Dagger CI` gate
  (`dagger call -m ./dagger --src . ci export --path out`) goes green.
- Run before committing:
  ```bash
  go build ./... && go vet ./... && go test ./...
  dagger call -m ./dagger --src . lint
  helm template dagger-kubernetes deploy/helm/dagger-kubernetes --debug >/dev/null
  ```
