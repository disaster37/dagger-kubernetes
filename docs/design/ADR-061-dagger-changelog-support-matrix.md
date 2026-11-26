# ADR-061: Dagger changelog support matrix (v0.21.8 → v1.0.0-beta.20)

**Status:** Accepted  
**Date:** 2026-11-26

## Context

The platform provisions and serves Dagger engines/CLIs across a wide version
range (`version.floor = v0.19.0`, empty default allowlist). The upstream
changelog from **v0.21.8 (2026-07-29)** through **v1.0.0-beta.20 (2026-10-22)**
shipped 45 entries. We needed a record of, for every entry, whether the
platform blocks it, must expose something for it, or does nothing by design —
and the small number of plumbing bugs where *our* wrapper/env builders would
strip or override a capability.

Version permissiveness is not the question: the gate already admits the whole
range (proved by `TestResolverAdmitsChangelogRange`). The work is (a) three
plumbing fixes, (b) three platform features that exist in Dagger Cloud and
therefore in our Cloud-compatible surface, (c) one parser robustness fix, and
(d) proofs for the engine-internal entries we must not touch.

## Decision

### D1 — Compatibility matrix format

Every changelog entry is recorded as:

`# | release | feature | upstream mechanism | platform obligation (A/B/C/D) | verdict | proof`

Obligation classes: **A** engine-internal (prove we do not block it), **B**
passthrough/plumbing (our wrapper/env would strip it — fix it), **C** platform
feature (API/config/UI surface), **D** docs/ADR only. Verdicts: **SUPPORTED**,
**GAP** (real work, now shipped), **NO-OP-BY-DESIGN** (documented reason).

### D2 — The 45-row matrix

| # | Release | Feature | Upstream mechanism | Class | Verdict | Proof |
|---|---|---|---|---|---|---|
| 1 | 0.21.8 | Dockerfile layer cache survives unrelated context changes | engine dagql call keying on mounted content | A | SUPPORTED | `TestCachedAttributeIngest`; V4 stamp fixture (host-side) |
| 2 | 0.21.8 | Silent sessions use less memory | CLI `--silent` skips frontend telemetry init | C | GAP → SUPPORTED | `checks.silent`/`workflows.silent`, `TestJobSecretCarriesSilent` |
| 3 | 0.21.8 | `DAGGER_SILENT` == `--silent` | CLI `silentFromEnv` (only ≥ 0.21.8) | B | GAP → SUPPORTED | `silentArgs`, `TestSilentArgs`, `--silent` translation |
| 4 | 0.21.8 | `--x-release` keeps the runner host | CLI re-exec env filtering (PR 13752) | B | GAP → SUPPORTED | `checkXReleaseRunnerHost`, `TestCheckXReleaseRunnerHost` |
| 5 | 0.21.8 | Faster changeset comparison | engine FS-metadata delta | A | SUPPORTED | V5 report-only timing |
| 6 | 0.21.9 | Remote filesystems over SSH (`Volume`) | engine live SSHFS mount | A | SUPPORTED | pods have unrestricted egress; no platform block |
| 7 | 0.21.9 | e-graph cache lock contention | engine internals | A | NO-OP | unobservable from outside the engine |
| 8 | 0.21.9 | Cache identity includes resolved platform | engine cache key | A | SUPPORTED | one-time cold cache after upgrade (deploy notes) |
| 9 | 0.21.9 | Registry auth no longer blocks cross-session cache reuse | engine auth/cache keying | A | SUPPORTED | V3 cross-session cache reuse |
| 10 | 0.21.9 | No false registry errors in progress output | CLI progress reporting | A | SUPPORTED | step state derives from span status/exit, never log text |
| 11 | 0.21.9 | Faster layer copies | engine internals | A | NO-OP | unobservable |
| 12 | 0.21.9 | Include patterns honour exclude order | engine file sync | A | SUPPORTED | engine fix; our module/Dockerfile do not rely on the broken order |
| 13 | 0.21.9 | Changeset merges ignore ctime | engine internals | A | NO-OP | unobservable |
| 14 | 0.21.9 | `withDirectory` restores deleted nested files | engine correctness | A | SUPPORTED | engine fix; no platform dependency |
| 15 | 0.21.9 | Engine prunes cache metadata when too large | engine GC | A | SUPPORTED | relates to `fleet.engine_storage_size`; sizing guidance only |
| 16 | 0.21.9 | `unexpected EOF` on throttled image pushes | engine push retry | A | SUPPORTED | `publish` leg of the CI gate |
| 17 | 0.21.9 | Private module fields keep objects | engine module state | A | SUPPORTED | our module uses no private object fields |
| 18 | 0.21.9 | Client shutdown timeouts | CLI/engine session shutdown | A | SUPPORTED | V3 session count returns to 0, replica idle-expires |
| 19 | 0.21.9 | TypeScript SDK timeouts on Bun | SDK runtime | A | SUPPORTED | SDK-side; TS modules inherit the fix |
| 20 | 0.21.9 | Rust SDK hang | SDK | A | SUPPORTED | SDK-side |
| 21 | 0.21.9 | Cloud never suspends a live engine | Cloud lifecycle guard | C | SUPPORTED | `TestIdleScaleDownSkipsActiveSessions` (V2) |
| 22 | 0.21.10 | `CurrentModule().Source()` with private Git deps | engine module loading | A | SUPPORTED | engine fix; module authors only |
| 23 | 0.21.10 | Engines send CPU/memory metrics to OTel | engine → OTLP `/v1/metrics` | C | GAP → SUPPORTED | `TestEngineMetricsIngest`; PromQL documented below |
| 24 | 0.21.10 | Custom service hostnames across modules | engine DNS aliasing | A | SUPPORTED | data plane is L4 and name-agnostic |
| 25 | v1.0β | Test-native CI (per-test visibility, rerun one test) | engine test spans + typed filters | C | GAP → SUPPORTED | `DeriveTests`, `TestDeriveTests`, `TestCheckRunTestsVisibility` |
| 26 | v1.0β | Much faster engine (containers/FSOps/cache/Dang) | engine rewrite | A | NO-OP | perf-only; the gate measures nothing time-based |
| 27 | v1.0β | Self-orchestrating tests | nested Dagger sessions in a container | A | SUPPORTED | V4b self-orchestrating check |
| 28 | v1.0β | Standalone Go SDK | `core.NewContainer()` | A | SUPPORTED | SDK-only; our module is a Dagger module |
| 29 | v1.0β | Branch filtering (`GitRepository.remotes`, `GitRef.contains`) | core GraphQL Git APIs | A | SUPPORTED | core schema; runners deliver ref/branches and clone a real checkout |
| 30 | v1.0β | Nested pipelines under the initiating step | engine/D-in-D nested sessions | C | GAP → SUPPORTED | `attachDeferred`, `TestOrphanAdoptionNestedPipeline`, `TestNestedPipelineTreeNests` |
| 31 | v1.0β | Typed artifacts (`check -l --all` columns, typed filters, `dag://`) | engine + official modules | C | GAP → SUPPORTED | `parseCheckTable`, `TestCheckDescriptors`, `TestCheckRunLoadDescriptors` |
| 32 | v1.0β | Programs in a container can call Dagger; `disableDaggerInDagger: true` | workspace/module setting | A | NO-OP | we must **not** set it (it breaks self-orchestrating tests); it is a workspace setting, not a platform knob |
| 33 | v1.0β | Dev shells (`dagger shell`, `dagger check -i`) | CLI interactive mode | B | GAP → SUPPORTED | `guardInteractive`, `TestGuardInteractive` |
| 34 | v1.0β | Remote execution (SSH, Kubernetes, Cloud) | `--engine=ssh://…`/`k8s://…`/`cloud` | B | GAP → SUPPORTED | `resolveRunnerHost` stops clobbering an explicit selector |
| 35 | v1.0β | Official modules catalog + recommendations | Cloud recommendation service | C | GAP → NO-OP | `recommendedModules` returns an empty list (no service runs here); `TestParityBetaCLIQueries` |
| 36 | v1.0β | `dagger generate` ahead-of-time codegen | CLI + SDK | A | SUPPORTED | V5b module load |
| 37 | v1.0β | `dagger module migrate` | CLI | D | NO-OP | we keep `dagger/dagger.json`; 1.0 still loads it |
| 38 | v1.0β | Persistent cache lookup errors fixed | engine cache | A | SUPPORTED | read-through remote cache benefits; V3 |
| 39 | v1.0β | Go SDK codegen prunes removed-deps bindings | Go SDK | A | SUPPORTED | V5b module load (3 deps) |
| 40 | v1.0β | Read Git history without `git` | core Git API | A | SUPPORTED | engine-side; pods still ship `git` for clone |
| 41 | v1.0β | `defaultPath` Git args are null outside a checkout | core Git API | A | SUPPORTED | runners always clone into the workdir first |
| 42 | v1.0β | Progress for pulls/unpacks/fetches/downloads/syncs | engine spans + CLI progress | C | GAP → SUPPORTED | `TestAdvanceProgressSpans`; extra CI stages are intentional |
| 43 | v1.0β | Workspaces (`dagger.toml`, `dagger.lock`, discovery) | CLI/workspace config | C | GAP → SUPPORTED | `workflows.discovery_paths`, `--workflow-path` |
| 44 | v1.0β | Cloud Engines (managed, scale-out, distributed cache) | Cloud service | C | SUPPORTED | this platform is the implementation; live E2E |
| 45 | v1.0β | Cloud Checks (push/PR checks on Cloud Engines) | Cloud service | C | GAP → SUPPORTED | ADR-056 base + per-test results and typed filters (G4/G6) |

Counts: **26 SUPPORTED**, **13 GAP (now shipped)**, **6 NO-OP-BY-DESIGN**.

### D3 — Three non-obvious decisions

1. **Silent mode is opt-in, never default.** `checks.silent` /
   `workflows.silent` default to `false`. Silent suppresses *progress*, and a
   silent default would make failures harder to diagnose in CI logs. The
   wrapper still translates the flag for every allowed CLI: `--silent` exists in
   all of them, while `DAGGER_SILENT` is only honoured from 0.21.8.
2. **`--x-release` errors below 0.21.8 instead of silently attaching to a local
   engine.** Before 0.21.8 the re-exec drops `_EXPERIMENTAL_DAGGER_RUNNER_HOST`,
   so the inner CLI provisions a local engine — on a Docker-less agent that
   hangs at "connecting to engine". The wrapper refuses that combination and
   names the escape hatches (`--engine=…`, upgrade, or drop `--x-release`).
3. **Typed artifacts and workflow artifacts are different things.** Workflow
   artifacts are stored objects uploaded by workflow jobs (`/artifacts` page).
   Typed artifacts are discovered by Dagger from `dagger check -l --all`
   (module/package/test selectors) and are shown on the Checks page. They stay
   on different pages and are never conflated.

### D4 — Entry 32 (`disableDaggerInDagger`): NO-OP, with a follow-up

`disableDaggerInDagger` is a **workspace/module** setting. Setting it would
break the self-orchestrating tests that entry 27 ships, so the platform does not
set it anywhere and never emits it. Follow-up (not implemented): an
*operator-facing* knob that a workspace owner could set deliberately is worth
considering later; it is not a platform-level default and must never be forced.

### D5 — The "unobservable" list

Entries 7, 11, 13 and 26 are engine-internal performance/correctness changes
with no externally observable contract (no span name, attribute, exit code or
API field changes). They are marked NO-OP rather than SUPPORTED because there
is nothing to assert; a future measurement would be a benchmark, not a test.

### D6 — CI pin stays at 0.21.8

`.github/workflows/ci.yml` keeps `DAGGER_VERSION: "0.21.8"`. The changelog
range's *blocking* behaviour is 0.21.8–0.21.10, and the live fleet already runs
beta engines, so a beta blocking gate adds beta SDK/codegen churn without
coverage we cannot get from V5b (module load on the beta CLI). A future beta
gate must be a **second, non-blocking** job in a separate changeset.

### D7 — Capture provenance (measured vs inferred)

Honest record of what was verified against real binaries and what was inferred:

- **Measured (live/CLI):** the version-gate behaviour (`TestResolverAdmitsChangelogRange`);
  the check-run Job path (clone → CLI provisioning → `check -l` → `/load` →
  `/complete`) on the live cluster with the provisioned CLI; the OTLP
  `/v1/metrics` accept-and-forward path; the GraphQL beta query matrix (against
  the live schema); `dagger publish` (engine push retry, entries 16/38).
- **Captured from the module graph, not from a live nested run:** the test-span
  attribute keys (`test.case.name`, `test.suite.name`,
  `test.case.result.status`) and the check attributes
  (`dagger.io/check.name`, `dagger.io/check.passed`) come from the pinned
  upstream `github.com/dagger/otel-go` package (its `gotest` integration and
  `attrs.go`). They are centralized in `testAttrKeys`, so one edit corrects
  them if a capture later shows a different key.
- **Inferred (no capture available):** the nested-pipeline span shape (entry 30)
  is implemented for the *same trace id* case (deferred re-parenting) because
  the nested run needs a working engine session that the test environment could
  not establish; the GraphQL recommendation field name (`recommendedModules`)
  is the documented candidate and returns an empty list either way; the
  `dag://` link column key is accepted as `LINK`/`DAG-LINK` when the CLI
  provides it.
- **Not fabricated:** no captured trace or query is claimed as measured. The
  parity fixtures are schema-shaped replays, marked as such in the test.

### D8 — Engine self-reported metrics (entry 23)

The engine sends resource metrics to the OTLP endpoint it is given; our ingest
accepts them at `POST /v1/metrics` and proxies them to the collector, so
VictoriaMetrics stores them. They are **not** surfaced in a dedicated UI: the
Runners page keeps using the cAdvisor-derived numbers, which are already
sufficient for capacity decisions. Operators can query the self-reported series
directly (per version StatefulSet, engine container):

```promql
sum by (dagger_io_engine_version) (
  rate(container_cpu_usage_seconds_total{
    job="kubernetes-pods", container="engine", namespace="<namespace>"
  }[5m])
)
sum by (dagger_io_engine_version) (
  container_memory_working_set_bytes{
    job="kubernetes-pods", container="engine", namespace="<namespace>"
  }
)
```

If the engine's metric names differ in a future release, query
`count by (__name__) ({job="kubernetes-pods", container="engine"})` to discover
them; adding a UI card is a follow-up only if a user asks for it.

### D9 — Operational note for upgrades

Entries 1 and 8 change engine cache identity: the **first builds after an engine
upgrade are cold**. That is expected, not a regression; do not pin an engine
version back to recover a warm cache.

## Consequences

- The wrapper no longer overrides an explicit engine selector, refuses a
  host-losing `x-release` combination, and can run silently.
- Checks expose per-test results (read-time derived) and typed filters;
  GraphQL `rerunChecks`/`Check.filters` are additive.
- Workflow discovery is declarative; a beta workspace layout is a values change.
- `graphql.log_queries` gives a one-flag capture path for future CLI releases.
- No new dependencies, no schema version bump, no data migration. Every new key
  defaults to today's behaviour, so rollback is `helm rollback`.
