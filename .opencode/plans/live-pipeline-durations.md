# Plan — Live (ticking) pipeline & step durations in the UI

## Goal
Make elapsed durations visibly tick upward while a pipeline/step is running, on
all three surfaces:

1. **Pipeline Details view** — per-step duration inside the Steps list.
2. **Pipeline Details view** — global pipeline duration (header + Details table).
3. **Pipelines list view** — per-row Duration column in the table.

Once a pipeline/step finishes, the duration must freeze at the server-provided
final value and stop changing.

## Root cause
All durations are computed purely from **stored span end timestamps at render
time** and are never re-evaluated between server fetches:

- `internal/repository/trace_store.go` `traceDurationMS()` (line 200) computes
  the trace duration as `maxEnd - minStart` over span end times. While the trace
  is running, the latest end time is the last *finished* span, so the value is
  frozen until another span finishes.
- `ui/src/pipeline/PipelineView.vue` `subtreeDuration()` (line 480) computes
  each step's wall-clock from `spanEndMs()` = `start + duration_ms`. A running
  span has `duration_ms == 0` (no end time), so `subtreeDuration` returns `0` →
  the running step shows `-` and never increases until the span finishes and a
  refetch happens.
- The Details view only refetches on (a) an SSE `trace_update` event (fired when
  a span *finishes*) or (b) the 5s poll. Between finishes, the running step's
  duration is stale/zero.
- `ui/src/views/Pipelines.vue` calls `load()` **once** on mount (line 64-71).
  There is no poll and no SSE. The Duration column is frozen until a full page
  reload.

## Chosen approach — client-side ticking from server absolute timestamps
The backend already returns absolute start timestamps:
- `TraceDetail.start_time` (root span start, from Tempo) — `TraceInfo.StartTime`.
- `SpanNode.start_time` (per span) — `domain.SpanNode.StartTime`.
- `TraceRow.started_at` (from `trace_meta.StartedAt`, populated at OTLP ingest
  from the root span's `startTimeUnixNano` — see `service/otlp_extract.go:238`).

The frontend will keep a reactive `now` ref updated by a `setInterval`, and
compute live durations as `Date.now() - start_time` while the entity is
`running`, falling back to the server's stored `duration_ms` once finished or
when `start_time` is missing/zero.

**Why absolute timestamps (not "tick from last known duration"):** the server
start time is the authoritative epoch; the client recomputes `now - start` on
every tick, so the display self-corrects after tab throttling, sleep, or a
stale refetch. The final value (when `status` becomes `success`/`failed`) is
the server's `duration_ms`, which overrides the tick — so the frozen final
value is exact and immune to client clock skew. While running, client clock
skew produces a constant offset, which is acceptable and is the standard
pattern (Dagger Cloud does the same).

**No backend changes required.** No new endpoints, no SSE additions, no payload
changes. The existing `/api/v1/traces` list and `/api/v1/traces/:id` detail
already carry everything needed.

## Files to modify

### 1. `ui/src/pipeline/PipelineView.vue` (Details view — steps + global duration)
Add a reactive `now` ref + a 250ms tick interval (250ms so the tenths digit in
`formatDuration` updates smoothly for sub-minute durations). Add a
`visibilitychange` listener that forces an immediate `now` refresh on tab
return (setInterval is throttled to ≤1/s in background tabs; recomputing from
`Date.now()` on return corrects any drift). Clear the interval and remove the
listener in `onUnmounted` (the component already clears `pollTimer`,
`eventSource`, etc. there).

Add a helper:

```ts
// Live elapsed for a span: ticks while the span is running, freezes at the
// stored duration_ms once finished. Returns ms.
function liveSpanDuration(node: SpanNode): number {
  if (node.status !== 'running') {
    // Finished: use the subtree wall-clock (handles children that outlive the
    // parent span's own end record).
    return subtreeDuration(node)
  }
  const start = spanStartMs(node)
  if (!start) return node.duration_ms || 0
  return Math.max(0, now.value - start)
}
```

Template changes:
- Line 15 (header duration): `formatDuration(trace.duration_ms)` →
  `formatDuration(liveTraceDuration())` where `liveTraceDuration()` returns
  `trace.status === 'running' && trace.start_time ? max(0, now.value - Date.parse(trace.start_time)) : trace.duration_ms`.
- Line 150 (Details table Duration row): same swap to
  `formatDuration(liveTraceDuration())`.
- Line 84 (step row duration): `formatDuration(step.durationMs)` →
  `formatDuration(liveSpanDuration(step.span))`.
- Line 107 (sub-span duration): `formatDuration(s.node.duration_ms)` →
  `formatDuration(liveSpanDuration(s.node))`.
- Line 31 (service row duration): `formatDuration(subtreeDuration(svc.span))`
  → `formatDuration(liveSpanDuration(svc.span))` (services use the same
  running/finished logic; `svc.running` is already derived from
  `span.status === 'running'`).

`now` must be referenced inside the computed/template so Vue re-evaluates on
tick. Because `now` is a `ref` read inside these functions called from the
template, Vue tracks it and re-renders every tick.

### 2. `ui/src/views/Pipelines.vue` (table — per-row Duration + list refresh)
Add a reactive `now` ref + a 1000ms tick interval (1s is enough for a table
with potentially many rows; tenths are not critical here). Add a 10s poll that
re-calls `load()` to detect status transitions and pick up final
`duration_ms` — only while at least one trace is `running` (optimization: if
all rows are finished, skip the poll to avoid needless requests). Add a
`visibilitychange` listener for immediate correction on tab return. Clear
both intervals and the listener in `onUnmounted`.

Add a helper:

```ts
function liveRowDuration(trace: TraceRow): number {
  if (trace.status !== 'running') return trace.duration_ms
  const start = Date.parse(trace.started_at)
  if (!Number.isFinite(start) || start <= 0) return trace.duration_ms
  return Math.max(0, now.value - start)
}
```

Template change:
- Line 37: `formatDuration(trace.duration_ms)` →
  `formatDuration(liveRowDuration(trace))`.

Add `onUnmounted` import (currently only `ref, onMounted` are imported).

### 3. `docs/README.md` — Pipeline UI section (lines ~1077-1080)
Update the "Duration" bullet to document the live-ticking behavior:

> - **Duration** — shown prominently in the viewer header next to the status
>   and in the details table; while a pipeline or step is `running`, the
>   displayed duration ticks live every 250ms (Details) / 1s (list) from the
>   server-provided `start_time`/`started_at` absolute timestamp, and freezes
>   at the final server `duration_ms` once the run finishes. The
>   `/api/v1/traces/:id` response returns `duration_ms` in milliseconds
>   (matching the list endpoint), with the raw value available as
>   `duration_ns`.

Also update the "Pipeline list" bullet (line ~1058) to note the list now
auto-refreshes every 10s while any run is in flight.

### 4. ADR — OUT OF SCOPE (recommended)
Client-side ticking from server timestamps is a minor UI behavior pattern, not
an architectural decision. No ADR is required. If the team wants to record the
"live-duration = client now − server start_time; freeze at server
duration_ms" pattern for future reference, add `docs/design/ADR-019-live-durations.md`
and register it in `docs/design/index.md` — but this is optional and marked out
of scope for this change.

## No Go changes
No Go files are modified. Therefore:
- No new config keys → `config/config.app.yaml.sample` unchanged.
- No Go packages touched → the AGENTS.md "100% coverage for every package"
  mandate does not apply (N/A — no Go package is changed).
- No new/changed API or SSE payloads.

## Edge cases covered

| Case | Behavior |
|---|---|
| Pipeline finished (`success`/`failed`) | `liveTraceDuration`/`liveRowDuration` return server `duration_ms`; timer keeps running but value is constant. |
| Step finished while sibling steps run | `liveSpanDuration` returns `subtreeDuration(node)` for that step; siblings keep ticking. |
| Pipeline running with zero started steps | Steps list shows existing "No steps yet — waiting for spans..." (unchanged); global duration still ticks from `trace.start_time`. |
| Missing/zero `start_time` or `started_at` (legacy traces) | `liveSpanDuration`/`liveTraceDuration`/`liveRowDuration` fall back to stored `duration_ms` (no ticking). |
| Malformed timestamp (`Date.parse` → NaN) | `spanStartMs` already returns 0 on NaN; helpers fall back to `duration_ms`. |
| Clock skew between server and client | Running duration has a constant offset; **final** value is the server's `duration_ms` (exact), so the displayed value snaps to the correct final on finish. |
| Tab throttling (background tab) | `setInterval` throttled to ≤1/s; recomputing from `Date.now()` on each tick auto-corrects. `visibilitychange` listener forces an immediate refresh on return. |
| Component unmount | `clearInterval` + remove `visibilitychange` listener in `onUnmounted` (both views). Prevents leaks across route changes. |
| Rounding | Unchanged `formatDuration`: tenths for <60s, whole seconds for minutes. |
| No-JS fallback | N/A — the SPA requires JS; the static `duration_ms` is rendered before the first tick, so a JS failure leaves the last server value visible. |
| Negative elapsed (client clock behind server) | `Math.max(0, now - start)` clamps to 0 → displays `-`. |

## Error handling & validation
- All timestamp parsing reuses existing `spanStartMs()` (returns 0 on NaN) and
  `Date.parse()` with `Number.isFinite()` guards in the new row helper.
- No network calls are added to the tick path; the 10s list poll reuses the
  existing `load()` which already `try/catch`es and logs to `console.error`.
- No new user-facing error states.

## Frontend conventions (from repo)
- Vue 3 `<script setup lang="ts">`, Composition API, `ref`/`computed`.
- No new dependencies (`setInterval`, `Date.now`, `Date.parse` are stdlib).
- `formatDuration` is duplicated in both views; do **not** extract a shared
  util in this change (keep the diff minimal and match the existing pattern of
  per-view helpers). The new `liveSpanDuration`/`liveTraceDuration`/
  `liveRowDuration` helpers are local to each view.
- TypeScript strict mode (`tsconfig.json`); ensure `now` is typed
  `ref<number>`.

## Verification

### Build & typecheck (local, before pushing)
```bash
cd /projects/dagger-cache/ui
npm install      # if node_modules absent
npm run typecheck   # vue-tsc --noEmit — must pass
npm run build       # vite build — must succeed, produces ui-dist/
```

### Go build (sanity — no Go changed, but the embed must still compile)
```bash
cd /projects/dagger-cache
go build ./...
go test ./...        # must remain green (no Go changes)
```

### Local cluster deploy (MANDATORY per AGENTS.local.md §6)
Follow AGENTS.local.md §4 exactly:

```bash
# 4.1 Build the image (includes the rebuilt UI via ui-dist embed)
cd /projects/dagger-cache
docker build -t docker.io/disaster/dagger-kubernetes:dev .

# 4.2 Push
docker push docker.io/disaster/dagger-kubernetes:dev

# 4.3 Capture current values
helm --kubeconfig /home/user/.kube/home get values dagger-cache-test \
  -n dagger-cache-test -o yaml > /tmp/dagger-cache-test.values.yaml

# 4.4 Upgrade
helm --kubeconfig /home/user/.kube/home upgrade --install dagger-cache-test \
  ./deploy/helm/dagger-kubernetes \
  --namespace dagger-cache-test \
  -f /tmp/dagger-cache-test.values.yaml \
  --set supervisor.config.raft.replicas=1 \
  --set supervisor.image.tag=dev \
  --set supervisor.image.pullPolicy=Always \
  --set supervisor.image.repository=docker.io/disaster/dagger-kubernetes

# 4.5 Force rollout (dev tag is mutable)
kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test \
  rollout restart statefulset/dagger-cache-test-dagger-kubernetes
kubectl --kubeconfig /home/user/.kube/home -n dagger-cache-test \
  rollout status statefulset/dagger-cache-test-dagger-kubernetes --timeout=300s
```

### Agent verification (AGENTS.local.md §5.1)
```bash
export KUBECONFIG=/home/user/.kube/home
kubectl -n dagger-cache-test get pods -l app.kubernetes.io/name=dagger-kubernetes
# → Running + Ready
kubectl -n dagger-cache-test port-forward svc/dagger-cache-test-dagger-kubernetes-control 8080:80 &
curl -sk https://localhost:8080/healthz   # 200 "ok"
curl -sk https://localhost:8080/readyz    # 200
kubectl -n dagger-cache-test logs statefulset/dagger-cache-test-dagger-kubernetes --tail=100
# → no fatal errors
```

### Human verification (AGENTS.local.md §5.2) — the actual feature check
At `https://dagger.home.webcenter.fr` (login `admin` / `DaggerHome!2026`):

1. **Trigger a running pipeline** (e.g. `dagger call` with `DAGGER_CLOUD_URL`
   pointed at the server, or watch an existing in-flight run).
2. **Pipelines list view** (`/pipelines`): the Duration column for the running
   row must visibly increase every second. Finished rows must show a static
   value. Switch tabs away for 30s, return — the running row must jump to the
   correct elapsed (not drift by 30s of frozen display).
3. **Pipeline Details view** (`/pipelines/<id>`):
   - The header duration (top-right) must tick ~4x/s (250ms) while running.
   - The Details table "Duration" row must tick identically.
   - In the Steps list, the currently-running step's duration must tick
     upward; already-finished steps must show their static final duration.
   - Expand a running step: its sub-spans that are still running must tick;
     finished sub-spans must be static.
4. **Finish the pipeline**: once it transitions to `success`/`failed`, all
   durations (header, Details, steps, sub-spans, and the list row) must freeze
   at the final server value and stop changing within one refetch cycle
   (SSE `trace_update` → immediate, or ≤10s list poll).
5. **Refresh the page** on a finished pipeline: durations must render the
   final value immediately and not tick.

## Open questions / assumptions (recommended answers, non-blocking)

1. **Tick cadence for the Details view: 250ms vs 1000ms?**
   Recommended: **250ms** so the tenths digit (shown for <60s durations) updates
   smoothly. Cost is negligible (one ref bump + a handful of computed
   re-evaluations on a single trace). If the team prefers uniformity, 1000ms
   everywhere is acceptable (tenths will update once per second).

2. **Should the Pipelines list poll be stopped when no row is running?**
   Recommended: **yes** — skip the 10s `load()` poll when every row is
   `success`/`failed`, to avoid needless requests on a quiet dashboard. The
   1s duration tick still runs but is a no-op (all rows return stored
   `duration_ms`). Re-enable the poll when a new running trace appears (the
   tick itself doesn't fetch; the user must reload or filter to see new
   traces — acceptable, matches current behavior).

3. **Should services (the Services card) also tick?**
   Recommended: **yes**, via the same `liveSpanDuration` helper, for
   consistency. A running `up`/tunnel span currently shows a static
   `subtreeDuration` of 0; ticking it matches user expectation. Low risk.

4. **Extract a shared `useLiveDuration` composable?**
   Recommended: **no** for this change — keep the diff minimal and match the
   existing per-view helper pattern. A follow-up refactor can extract
   `ui/src/composables/useNow.ts` if more views need it.

5. **ADR?**
   Recommended: **skip** — this is a UI behavior fix, not an architectural
   decision. Marked out of scope.
