# Plan: Display Dagger Services in the Pipeline Detail UI

## Goal
Surface Dagger **services** (containers started via `dagger.Up()` / `service.Up()` / `--up`,
i.e. long-running host-tunnel services) in `ui/src/pipeline/PipelineView.vue`:

1. A **"Services" section at the top** of the pipeline view, above the existing "Steps" card.
2. Each service rendered on a **dedicated row** with distinct visual treatment.
3. Each row shows **running state** and the **exposed port/URL** (if any).
4. The **last N (50) lines of logs** shown **live** per service.
5. **Click to expand** a service row → full logs + port/URL + all details.

## Key findings (investigated, not guessed)

### How Dagger services are represented in OTLP/Loki data
Verified against the Dagger engine source (`dagger/dagger`, `core/service.go` +
`core/schema/service.go`) and the SDK attribute registry (`dagger/otel-go` `attrs.go`):

- `Service.Up()` / `container.up()` / `--up` resolves to `host.tunnel { service, ports, native }`,
  starts the tunnel, then **blocks on `<-ctx.Done()`**. The `up` span therefore stays
  **running** (no end time, OTLP status UNSET → mapped to `"running"` by
  `trace_store.mapStatus`) for the entire pipeline lifetime.
- The `up` resolver emits slog log records:
  `slog.Info("tunnel started", "port", port.Port, "protocol", ..., httpKey, "http://localhost:PORT", "description", ...)`
  (`httpKey` is `http_url` normally, `https_url` when port == 443). These records are
  tied to the `up` span's `span_id` and carry `attributes.http_url`/`https_url`/`port`/
  `protocol`/`description`.
- The synthetic service **exec span** (`exec <args>`) is marked
  `dagger.io/ui.passthrough = true`; its stdio logs are routed to the **installing**
  span, not to itself. There is **no** dedicated `dagger.io/service` span attribute.
- The Loki `line` JSON already contains the full `attributes` map. The frontend
  `logText()` currently only reads `body` + `stdio.eof`, but the raw JSON has
  `attributes.http_url`/`port`/etc. available without any backend change.

### Live-cluster inspection
The live cluster (`https://dagger.home.webcenter.fr`) could **not** be queried directly:
the `webfetch` tool cannot set the `Authorization` header, and the `?token=` query-param
auth fallback is restricted to the SSE `/live` route only (see
`internal/handler/middleware.go` `requireAuthWithQueryFallback`). The design below is
therefore based on the engine source above. The detection keys are isolated in a
single constants block so they can be adjusted after a real-trace inspection without
touching logic.

## Decision: frontend-only change, NO backend change
Service metadata (port, URL, running state, logs) is fully derivable from data already
returned by `GET /api/v1/traces/:id` (span tree + attributes) and
`GET /api/v1/traces/:id/logs` (Loki entries with `span_id` + raw JSON `line`). The
existing SSE `/api/v1/traces/:id/live` (`trace_update` / `logs_update`) and 5s polling
fallback already drive debounced re-fetches. **No Go changes are required.**

If a future real-trace inspection reveals a cleaner span attribute (e.g. a
`dagger.io/service.*` key), detection can move to span attributes with no API change
since `SpanNode.attributes` is already exposed verbatim.

## Files to modify
- `ui/src/pipeline/PipelineView.vue` — add Services section, service detection, rendering, expand behavior.
- `ui/src/api/types.ts` — add `ServiceInfo` interface (frontend-only view model; no API contract change).

No backend files. No new files.

## Detection logic (frontend)

A span is classified as a **service** when it has ≥1 associated log record whose parsed
JSON matches the service signal. Concretely, in `PipelineView.vue`:

```ts
// Tunable detection constants — isolated so they can be adjusted after a real-trace
// inspection without touching logic.
const SERVICE_LOG_BODY = 'tunnel started'
const SERVICE_URL_ATTRS = ['http_url', 'https_url'] as const

function isServiceLog(entry: TraceLogEntry): boolean {
  try {
    const obj = JSON.parse(entry.line) as { body?: unknown; attributes?: Record<string, unknown> }
    if (obj.body === SERVICE_LOG_BODY) return true
    const attrs = obj.attributes ?? {}
    return SERVICE_URL_ATTRS.some((k) => attrs[k] != null && attrs[k] !== '')
  } catch {
    return false
  }
}

function isServiceSpan(span: SpanNode, logsBySpan: Map<string, TraceLogEntry[]>): boolean {
  const logs = logsBySpan.get(span.span_id) ?? []
  return logs.some(isServiceLog)
}
```

- **Running state**: `span.status === 'running'` (the `up` span has no end time while the
  pipeline is in flight; `trace_store` maps UNSET/unfinished → `"running"`). When the
  pipeline ends, ctx cancels, the `up` finish record merges in, status flips — the row
  updates automatically on the next refetch.
- **Exposed port / URL**: extracted from the service-signal log record's `attributes`:
  - `url`  = `attributes.http_url` or `attributes.https_url` (first present)
  - `port` = numeric `attributes.port` (parsed; may be absent)
  - `protocol` = `attributes.protocol`
  - `description` = `attributes.description`
- **Service logs (last N)**: `logsForSubtree(span)` (reuses the existing helper) so the
  service's own `tunnel started` records **and** any child exec-span logs routed into the
  subtree are captured. Sorted ascending by timestamp; the **last 50** are shown in the
  collapsed row; **all** are shown when expanded.

### Why subtree logs
The engine routes the service container's stdio to the installing span and marks the
synthetic exec span `dagger.io/ui.passthrough`. Using `logsForSubtree(serviceSpan)`
captures both the `up` span's `tunnel started` logs and any descendant logs, matching
how step rows already aggregate logs. `logsForSpan` alone would miss routed child logs.

## Data structures

### `ui/src/api/types.ts` — add:
```ts
// Frontend-only view model derived from span + logs; not part of any API contract.
export interface ServiceInfo {
  span: SpanNode
  /** true while the up span has no end time (status === 'running') */
  running: boolean
  /** exposed host:port URL from the "tunnel started" log attributes, if present */
  url: string | null
  /** exposed port number, if present */
  port: number | null
  /** protocol string e.g. "tcp", if present */
  protocol: string | null
  /** tunnel description, if present */
  description: string | null
  /** all logs for the service subtree, sorted ascending by timestamp */
  logs: TraceLogEntry[]
}
```

No changes to `SpanNode`, `TraceDetail`, or `TraceLogEntry`.

### `PipelineView.vue` — add (script):
```ts
const SERVICE_TAIL_LINES = 50

interface ServiceRow extends ServiceInfo {
  expanded: boolean
}

const services = ref<ServiceRow[]>([])

// computed in loadTrace() after steps.value is computed:
//   services.value = computeServices(trace.value.root_span, logsBySpan.value)
```

## Function signatures (PipelineView.vue)

```ts
function isServiceLog(entry: TraceLogEntry): boolean
function isServiceSpan(span: SpanNode, logsBySpan: Map<string, TraceLogEntry[]>): boolean
function extractServiceMeta(span: SpanNode, logs: TraceLogEntry[]): {
  url: string | null
  port: number | null
  protocol: string | null
  description: string | null
}
function computeServices(root: SpanNode | null, logsBySpan: Map<string, TraceLogEntry[]>): ServiceRow[]
function serviceTailLogs(svc: ServiceRow): TraceLogEntry[]   // last SERVICE_TAIL_LINES of svc.logs
```

- `computeServices` walks the whole tree (reusing the existing `normalizeChildren`-guaranteed
  `children` arrays), collects spans where `isServiceSpan` is true, sorts by `spanStartMs`,
  and builds `ServiceRow[]` with `expanded: false`.
- `extractServiceMeta` scans the service-signal log record(s) for the first non-empty
  `http_url`/`https_url`/`port`/`protocol`/`description`.
- `serviceTailLogs` returns `svc.logs.slice(-SERVICE_TAIL_LINES)`.

`computeServices` is called inside `loadTrace()` right after `steps.value = computeSteps(...)`,
using the **already-computed** `logsBySpan.value` (no extra fetch). Because `logsBySpan` is a
`computed`, read it via `.value` at call time.

## Template changes (PipelineView.vue)

Insert a new card **above** the existing "Steps" card (before `<div class="card"><h3>Steps</h3>`):

```vue
<div class="card services-card">
  <h3>Services <span v-if="services.length" class="count-badge">{{ services.length }}</span></h3>
  <div v-if="services.length === 0" class="empty">No services</div>
  <div v-for="svc in services" :key="svc.span.span_id" class="service">
    <div class="service-row" @click="svc.expanded = !svc.expanded">
      <span class="chevron">{{ svc.expanded ? '▾' : '▸' }}</span>
      <span :class="['dot', svc.running ? 'dot-running' : `dot-${svc.span.status}`]"></span>
      <span class="service-name">{{ svc.span.name }}</span>
      <span v-if="svc.running" class="service-running">running</span>
      <span v-if="svc.port != null" class="service-port">:{{ svc.port }}</span>
      <span v-if="svc.url" class="service-url">{{ svc.url }}</span>
      <span v-else-if="!svc.running" class="service-noport">no port</span>
      <span class="service-duration">{{ formatDuration(subtreeDuration(svc.span)) }}</span>
    </div>
    <div v-if="svc.expanded" class="service-detail">
      <div class="service-meta">
        <span v-if="svc.url"><strong>URL:</strong> {{ svc.url }}</span>
        <span v-if="svc.port != null"><strong>Port:</strong> {{ svc.port }}</span>
        <span v-if="svc.protocol"><strong>Protocol:</strong> {{ svc.protocol }}</span>
        <span v-if="svc.description"><strong>Tunnel:</strong> {{ svc.description }}</span>
        <span><strong>Status:</strong> {{ svc.running ? 'running' : svc.span.status }}</span>
        <span><strong>Span:</strong> <code>{{ svc.span.span_id }}</code></span>
      </div>
      <div class="logs">
        <template v-for="(log, i) in svc.logs" :key="`sv-${i}`">
          <div v-if="logText(log.line) !== null" class="log-line">
            <span class="log-ts">{{ formatTime(log.timestamp) }}</span>
            <span class="log-msg">{{ logText(log.line) }}</span>
          </div>
        </template>
        <div v-if="svc.logs.length === 0" class="empty">No logs for this service</div>
      </div>
    </div>
    <!-- Collapsed preview: last N lines, always visible -->
    <div v-else class="service-preview">
      <template v-for="(log, i) in serviceTailLogs(svc)" :key="`svp-${i}`">
        <div v-if="logText(log.line) !== null" class="log-line service-preview-log">
          <span class="log-ts">{{ formatTime(log.timestamp) }}</span>
          <span class="log-msg">{{ logText(log.line) }}</span>
        </div>
      </template>
      <div v-if="svc.logs.length === 0" class="empty">No logs yet</div>
      <div v-else-if="svc.logs.length > SERVICE_TAIL_LINES" class="service-more">
        +{{ svc.logs.length - SERVICE_TAIL_LINES }} more — click to expand
      </div>
    </div>
  </div>
</div>
```

Reuses existing helpers: `formatDuration`, `subtreeDuration`, `formatTime`, `logText`.
Reuses existing CSS classes (`dot`, `dot-running`, `logs`, `log-line`, `log-ts`, `log-msg`,
`chevron`, `empty`). Add a small scoped block for service-specific classes
(`.services-card`, `.service`, `.service-row`, `.service-name`, `.service-running`,
`.service-port`, `.service-url`, `.service-noport`, `.service-preview`, `.service-meta`,
`.count-badge`) — distinct visual treatment (e.g. left accent border, monospace name,
blue "running" pill) per requirement #1.

## Edge cases

| Case | Handling |
|---|---|
| No services | Services card renders `No services` empty state; Steps card unaffected. |
| Service span present but no port | `port`/`url` are `null`; row shows name + running state only; expanded meta omits port/URL lines. |
| Service still running | `svc.running` true (status `running`); blue `running` pill + `dot-running`. |
| Service exited | `running` false; status dot uses `dot-${status}`; port/URL still shown from the `tunnel started` log. |
| Logs missing/empty | `svc.logs.length === 0` → `No logs yet` (collapsed) / `No logs for this service` (expanded). |
| Live updates (SSE) | Reuses existing `trace_update`/`logs_update` debounce → `loadTrace`/`loadLogs` → `computeServices` recomputes from fresh `logsBySpan`. No new SSE event type. |
| N-line truncation | Collapsed preview shows `logs.slice(-50)`; `+N more — click to expand` hint when `logs.length > 50`. Expanded shows all. |
| Port/host missing | `url`/`port` derived solely from `tunnel started` log attributes; absent → omitted from UI. |
| Multiple services | Each gets its own row, sorted by `spanStartMs`. |
| Service span also appears under a Step | The span remains in the span tree and may still render as a sub-span under its step. Services section is a **deduplicated top-level summary**, not a removal. (Acceptable: same span shown in both places; the Services section is the dedicated surface.) |
| Bound services (no `up`/tunnel) | Not detected by the `tunnel started`/`http_url` signal (see Limitations). |

## Live / dynamic logs
- No new endpoint. The existing SSE `onmessage` handler already calls
  `scheduleTraceRefetch()` / `scheduleLogsRefetch()` (300ms debounce) on
  `trace_update` / `logs_update`. `loadTrace` recomputes `services.value` from the
  refreshed `logsBySpan`; `loadLogs` refreshes the underlying `logs` array. The
  collapsed preview and expanded view reactively update because they read
  `services` / `logsBySpan` which are reactive refs/computeds.
- 5s polling fallback (`pollTimer`) already re-runs `loadAll` while the trace is
  `running`, so services update even without SSE.

## Click-to-expand
- Implemented as an **expandable row** (toggle `svc.expanded`), consistent with the
  existing step-row expand pattern (no modal). Expanded state shows: full meta
  (URL/port/protocol/description/status/span id) + **all** subtree logs in a scrollable
  `.logs` box (reuses the existing `max-height: 500px; overflow-y: auto` style).
- A modal was rejected to stay consistent with the existing step UX and avoid new
  overlay machinery.

## Interaction with existing sections
- **Services card** is inserted **above** the Steps card (requirement #5).
- **Steps card** is unchanged; service spans may still appear as sub-spans under a
  step (the Services section is an additional dedicated surface, not a filter).
- **Unmatched / general logs** `<details>` card is unchanged and remains below Steps.
  Service logs are matched by `span_id` and therefore excluded from "unmatched".
- **Details card** is unchanged.

## Error handling & validation
- `isServiceLog` wraps `JSON.parse` in try/catch (mirrors `logText`); non-JSON lines
  return `false`, never throw.
- `extractServiceMeta` treats missing/empty attributes as `null`; numeric `port` is
  parsed with `Number()` and guarded with `Number.isFinite`.
- `computeServices` guards `root == null` (returns `[]`), consistent with `computeSteps`.
- All service rendering reuses `logText` (which already returns `null` for `stdio.eof`
  markers and binary payloads), so the same skip-empty logic applies.

## Test approach
- **No frontend unit-test framework is configured and none is added** (per constraints).
- Validation for the frontend:
  - `cd ui && npm run typecheck` (vue-tsc --noEmit) must pass with the new
    `ServiceInfo` interface and `ServiceRow`.
  - Manual smoke test against a pipeline that calls `service.Up()` / `--up`: verify
    Services section appears, running pill + port/URL show, last 50 logs stream live,
    expand shows all logs + meta; verify a pipeline with no services shows `No services`
    and Steps render unchanged.
- **No Go changes** → no Go tests required. If detection is later moved to span
  attributes in `trace_store.go`, add table-driven tests in
  `internal/repository/telemetry_test.go` covering: service span with port attr,
  service span without port, non-service span, running vs finished.

## Risks / notes
- **Detection is log-signal based** (`tunnel started` body / `http_url`|`https_url`
  attributes). This precisely covers the `dagger.Up()`/`service.Up()`/`--up` tunnel
  path, which is the task's explicit scope. **Bound services** started implicitly via
  `WithServiceBinding` (no `up`/tunnel) emit no `tunnel started` log and are NOT
  detected in v1. This is a documented limitation; the detection constants are
  isolated so a future span-attribute signal can be added.
- **Live-cluster trace could not be inspected** (auth tool limitation; `?token=` is
  SSE-only). The `tunnel started` log shape and `http_url`/`https_url`/`port`
  attribute keys are taken from the Dagger engine source
  (`core/schema/service.go` `up` resolver). If a real trace shows the attributes
  nested differently (e.g. under `attributes.http_url.value`), adjust
  `extractServiceMeta` / `isServiceLog` only.
- **Duplicate display**: a service span may render both in the Services section and as
  a sub-span under its step. Intentional — the Services section is the dedicated
  surface; deduplicating out of the Steps tree would complicate step grouping and risk
  hiding context.
- **N = 50** chosen for the collapsed preview tail; trivially tunable via
  `SERVICE_TAIL_LINES`.
- **Performance**: `computeServices` walks the tree once per `loadTrace` and reads
  `logsBySpan` (already built for steps). Cost is negligible relative to the existing
  step computation. `logsForSubtree` per service is O(service-subtree-logs); acceptable
  for typical service counts.
