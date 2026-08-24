# ADR-024: CI integration — rebuild Dagger's internal step view in CI (nested steps, state, logs)

- **Status:** accepted
- **Date:** 2026-08-24
- **Deciders:** dagger-kubernetes maintainers

## Context

A CI (Jenkins) invokes **one** Dagger function through the `dagger-kubernetes-ci`
wrapper / `daggerKubernetes.groovy` shared library and sees a **single
monolithic step** with one log stream. The user wants the CI to surface Dagger's
**internal execution tree** — every nested call/operation the engine runs — as
**individual nested steps**, each with its own **state**
(pending/running/succeeded/failed) and its own **live logs**, rendered in
**Jenkins Blue Ocean**.

The supervisor **already rebuilds this exact tree server-side**:
`repository.SpanTreeReconstructor` reconstructs the nested span tree from the OTLP
traces the engine exports (`/v1/traces` → Tempo), merges duplicate start/finish
span records, and links children to parents; per-span logs are already correlated
by `span_id` in Loki; the UI already renders the nested tree with per-step logs.
The gap is **client-side**: the CI wrapper never consumes that tree.

## Decision

### 1. Data source: the supervisor's reconstructed span tree (single source of truth)

The CI wrapper polls the supervisor's existing, auth-gated REST surface —
`GET /api/v1/traces/:traceID` (reconstructed span tree) and
`GET /api/v1/traces/:traceID/logs` (per-span logs). The wrapper sets
`OTEL_EXPORTER_OTLP_TRACES_LIVE=1` so the engine exports in-flight spans live
(otherwise spans only appear at completion). **No new supervisor endpoint is
required**; the CI view matches the platform pipeline UI because it consumes the
same reconstruction.

**Simplification:** the wrapper does **not** subscribe to the supervisor's
`GET /api/v1/traces/:traceID/live` SSE "re-fetch" ping in v1. The interval poll
alone provides the snapshot cadence, and a long-lived SSE client adds failure
modes (reconnect, auth-over-query-string, goroutine lifecycle) with little gain
at a ~2s poll. This can be revisited if lower latency is required.

### 2. Event model: a normalized, idempotent CI event stream

A new `internal/service.StepEventBuilder` diffs successive tree+log snapshots and
emits an **ordered, idempotent** `[]domain.CIEvent` stream:

- `node_started` — a span first appears (pending → running).
- `node_finished` — a span transitions running → succeeded|failed.
- `log_chunk` — a bounded batch of log lines attributed to a node.
- `pipeline_done` — the root span (or `trace.Status`) reaches a terminal state;
  carries `success`/`failed`/`canceled`. Emitted at most once.

Events are emitted in **depth-first order** — `node_started`, then its children
(each with their own started/logs/finished), then the node's own `log_chunk`s,
then its `node_finished`. This is what lets a sequential renderer (Jenkins
scripted pipeline) open and close nested `stage()` blocks without reordering,
and guarantees a node's logs always precede its `node_finished`.

`StepEventBuilder.Finalize(status, errMsg)` guarantees a **terminal event** is
always emitted when the Dagger command exits — it closes any still-running nodes
(in child-before-parent order) and emits exactly one `pipeline_done` carrying
the authoritative build status — even when the trace never indexed, the root
never resolved, or the engine failed before printing a trace id.

All the hard edge cases (dedupe, out-of-order, orphans, depth clamp, log
watermarking incl. equal-timestamp identity dedupe) live here, unit-tested to
100%.

### 3. Wire format: NDJSON on stdout

An `NDJSONEventSink` writes one JSON object per line — a stable, CI-agnostic
protocol consumed by the Jenkins shared library (and, later, Drone/GHA/plugin).
In `--steps` mode the wrapper reserves **stdout** for the event protocol; the
human pipeline-view link and `dagger` stderr stay on **stderr**.

### 4. Rendering: scripted-pipeline nested `stage()` in the shared library

The Jenkins shared library gains a `dynamicStages` mode that consumes the NDJSON
stream and renders **nested scripted-pipeline `stage()`** blocks with `echo`
per-step logs and `catchError`-derived per-stage status. Live rendering is
achieved by launching the wrapper in the background of the enclosing `node` and
rendering stages from the event stream as it grows.

| Dagger / supervisor concept | Jenkins concept |
|---|---|
| Trace (one `dagger call`) | One Jenkins **run** |
| `SpanNode` (operation) | A nested **`stage()`** (scripted pipeline) |
| `SpanNode.ParentSpanID` | Nested stage under its parent stage |
| `SpanNode.Status` running/success/failed | Stage **state**: running / success / failure |
| `LogEntry.SpanID` + `Line` | `echo` lines inside the owning stage |
| Root span status success/failed | Overall **build result** (wired to the `dagger` exit code) |

### 5. Feature gate and config

`ci.jenkins.dynamic_stages` (previously documented but a no-op) becomes the
feature gate. New keys `ci.jenkins.steps_poll_interval` (default `2s`) and
`ci.jenkins.steps_max_depth` (default `8`, `0` = unlimited) are added. With the
feature disabled, the wrapper's behaviour is byte-for-byte today's behaviour.

### 6. Layering

Follows the dependency rule (`handler → service → domain ← repository`):

- `internal/domain/ci_steps.go` — `StepState`, `CIEventType`, `CIEvent`,
  `StepNode`, `LogChunk`, `CIEventSink`, `TraceSnapshotSource` (stdlib only).
  `TraceSnapshotSource` is a composite of the already-defined
  `TraceRepository.GetTrace` and `LogRepository.QueryTraceLogs` signatures.
- `internal/service/ci_steps.go` — `StepEventBuilder` (snapshot diff → events).
- `internal/service/ci_render_ndjson.go` — `NDJSONEventSink`.
- `internal/repository/supervisor_client.go` — `SupervisorTraceClient` (HTTP
  client implementing `domain.TraceSnapshotSource`, Bearer auth, timeouts).
- `cmd/ci/main.go` — live trace-id capture, `--steps`/`--steps-poll-interval`/
  `--steps-max-depth` flags, poller goroutine, wiring.
- `ci-integrations/jenkins/daggerKubernetes.groovy` — `dynamicStages` mode.

## Alternatives considered

- **Static Jenkinsfile with pre-declared nested stages.** Rejected: the Dagger
  tree is not known until runtime, and a monolithic function's internal calls vary
  per run (cache hits, module resolution). Cannot express dynamic nesting
  statically.
- **Scraping the Blue Ocean / Pipeline Steps REST API to inject nodes.**
  Rejected: Blue Ocean's API is read-only for the graph; there is no supported
  write path to create arbitrary nested flow nodes from outside the flow.
- **Client-side Dagger GraphQL (`currentFunctionCall` / `_experimental`) as the
  primary source.** Rejected as primary because (a) it duplicates the supervisor's
  reconstruction, (b) the experimental surface is unstable and version-dependent,
  and (c) it would diverge from the UI. Kept as a documented fallback for
  zero-supervisor or low-latency cases.
- **Re-driving each Dagger sub-step as a separate `dagger call` stage.**
  Rejected: Dagger cannot resume into the middle of a function's call tree; each
  `dagger call` is an independent invocation.
- **Post-run replay only (emit nested `stage()` after `dagger` exits).**
  Rejected as insufficient: the user wants live state + logs, and Jenkins cannot
  append stages to an already-completed flow.

## Consequences

- The CI view matches the pipeline UI (same reconstruction, same source of truth).
- No supervisor change is required for v1; the diff logic is a pure client-side
  service, testable end-to-end without touching the control plane.
- The hard part (snapshot diffing) is separated from the Jenkins-specific part
  (Groovy rendering), so the same stream can drive other CIs or a future plugin
  without rework.
- **Fidelity ceiling:** scripted-pipeline nested `stage()` gives nested stages
  with per-stage console logs and statuses. True per-step Blue Ocean *flow nodes*
  (each Dagger span as its own clickable step with its own log pane, live)
  requires a Jenkins **plugin** implementing a `Step` that spawns child
  `FlowNode`s via the `FlowNode`/`StepContext` API — a separate deliverable. The
  NDJSON wire protocol is designed to feed that plugin unchanged.
- Stream errors are non-fatal (logged, retried); the `dagger` exit code is
  authoritative for the build result.

## Open questions

- Exact `dagger` CLI stderr wording for the trace id (the `[a-f0-9]{32,}` regex
  is already in use; confirm the id is emitted before completion so live capture
  works).
- Whether Tempo's `/api/traces/{id}` returns partial trees quickly enough for a
  ~2s poll (the existing `SpanTreeReconstructor` already handles partial trees;
  tune `steps_poll_interval` if not).
- Jenkins CPS tolerance of deeply nested dynamic `stage()` in a shared library
  (the known fragile spot — hence the `steps_max_depth` clamp and the plugin
  escape hatch).
