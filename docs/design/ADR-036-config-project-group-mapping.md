# ADR-036: Config-driven project → group mapping

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** dagger-kubernetes maintainers
- **Related:** ADR-010 (multi-user RBAC), ADR-022 (OAuth allowlists + regex mapping), ADR-030 (mapping diagnostics), ADR-035 (OAuth group-mapping auto-create)

## Context

Projects (CI pipelines identified by repo slug) are assigned to supervisor
groups either explicitly (admin UI/API) or by the per-group
`auto_assign_pattern` regex, which is set through the admin API/UI only. There
is no declarative, Helm-settable way to map a project name to a group, so an
operator who manages groups through the chart must still click through the UI
for every project, and the mapping is not part of the reviewed deployment
changeset.

`auth.oauth.group_mappings` (ADR-022) already established the pattern of an
ordered, config-driven regex mapping with first-match-wins semantics. This ADR
adds the analogous mapping for projects, owned by the subsystem that already
resolves project → group at ingest (`AttributionService`).

## Decision

### D1 — Config schema

A new top-level `attribution` section with a `project_mappings` key:

```yaml
attribution:
  project_mappings: []            # ordered list of {pattern, group}; empty = disabled
    # - pattern: '^github\.com/acme/.*'   # Go regexp vs project name (case-sensitive; (?i) opt-in)
    #   group: 'acme'                      # literal target supervisor group name
```

- `domain.Config.Attribution AttributionConfig` (`mapstructure:"attribution"`).
- `AttributionConfig{ ProjectMappings []ProjectMappingRule }`.
- `ProjectMappingRule{ Pattern, Group string }`.
- Default `[]`; env override
  `DAGGER_KUBERNETES_ATTRIBUTION_PROJECT_MAPPINGS` (documented, but the Helm
  value `supervisor.config.attribution.projectMappings` is the primary path).

`attribution` names the subsystem that consumes the rules; `projects` was
rejected because it is an entity with API CRUD, not a config concern.
`attribution.project_mappings` mirrors the `auth.oauth.group_mappings`
precedent.

### D2 — Ordered, first-match-wins, unanchored, case-sensitive

Rules are applied in config order; the first rule whose pattern matches the
project name wins. Matching uses Go `regexp` `MatchString` semantics — exactly
what `auto_assign_pattern` uses today — so patterns are unanchored and
case-sensitive by default. Operators are told to always anchor (`^...$`) and to
use `(?i)` for case-insensitive matching, the same guidance as
`group_mappings`. Duplicate rules are allowed (first occurrence wins).

### D3 — Missing target group is skipped, never auto-created

When a rule matches but its target group does not exist, the project is skipped
with a `WARN` log. Unlike ADR-035 (OAuth membership auto-create), these groups
are operator-declared in the same Helm change that would create them; silently
creating groups here would surprise operators, and the rules are bounded
(literal names, no capture expansion). A missing target is a config/ops mistake
to surface, not paper over. Auto-create is a possible follow-up.

### D4 — Precedence

Highest to lowest:

1. **Explicit assignment** (`project.GroupID != ""`) — never touched.
2. **Config `project_mappings`** — when a rule matches it is authoritative: if
   its target group is missing, the project is skipped (no fallthrough to
   auto-assign), because the operator wrote an explicit rule and a silent
   fallback to an `auto_assign_pattern` group would be surprising.
3. **Per-group `auto_assign_pattern`** — runs only when no config rule matches
   (or when no rules are configured). Unchanged behavior.

Config mappings are a complement, not a replacement: an empty config leaves
auto-assign exactly as it was.

### D5 — Ingest-only timing, not retroactive

Mapping applies only at OTLP ingest, for projects whose `GroupID` is empty at
ingest time. There is no startup sweep of already-unassigned projects: that
would be a new background job mutating Raft state with ordering/leadership
concerns. Existing unassigned projects are re-evaluated naturally on their next
ingest. Set-once semantics are preserved: once a project is assigned
(explicitly, by mapping, or by auto-assign), `project.GroupID != ""` and no
re-evaluation happens on later ingests.

### D6 — Validation split

`config.Load` fails fast on a rule with an empty pattern, a pattern that does
not compile as a Go regexp, or an empty group. Error strings name the config
path and index (`attribution.project_mappings[i]...`), matching the
`group_mappings` style. Group-name charset/length is **not** validated at
config load because the config package deliberately does not import `service`
(ADR-022 note); instead `cmd/api` logs a startup `WARN` for a `group` that is
not a valid supervisor group name, identical to the ADR-030 warning for
`group_mappings` replacements. A runtime lookup of an invalid name simply
misses and is skipped.

## Consequences

- An operator can declare project → group mappings in Helm; the rules are part
  of the reviewed deployment changeset.
- Empty `project_mappings` (the default) is a no-op: `ProjectMapper.Active()`
  is false and `resolveGroup` falls straight through to `autoAssign`. Existing
  projects that are already assigned are never re-mapped.
- A config file without the `attribution` key loads fine (Viper default).
- All new logic lives in `service` (business) and `config` (validation) plus
  plain structs in `domain`; `handler` and `repository` are unchanged and
  `domain` stays stdlib-only.
- Observability is logrus-only: `INFO` on assignment, `WARN` on a missing
  target group, a persist failure, or an invalid startup group name. A
  Prometheus counter (e.g.
  `dagger_kubernetes_project_attribution_total{source=...}`) is a possible
  follow-up.
- **Known limitation (accepted):** mapping is not retroactive; projects that
  were already ingested without a group are only re-evaluated on their next
  ingest.

## Alternatives considered

- **`projects.mappings`**: rejected — `projects` is an entity with API CRUD, not
  a config concern.
- **Auto-create missing target groups**: rejected — the groups are
  operator-declared in the same changeset; a missing target is a mistake to
  surface (see D3).
- **Fall through to `auto_assign_pattern` when the mapped target is missing**:
  rejected — a silent fallback to a different group than the operator declared
  is surprising (see D4).
- **Startup sweep of unassigned projects**: rejected — new background job with
  Raft ordering/leadership concerns for minimal benefit (see D5).
