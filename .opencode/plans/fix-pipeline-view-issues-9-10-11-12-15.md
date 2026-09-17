# Plan: Fix pipeline-view issues #9, #10, #11, #12, #15

Branch: `fix/pipeline-view-issues` (from `main`). One PR covering all five issues.

This plan is self-contained: a coder can implement it without re-reading the GitHub
issues. All file paths are absolute under `/projects/dagger-cache`. Root-cause
statements cite real code with file + line references (HEAD as inspected).

---

## 0. Executive summary

| # | Issue | Where the fix lives | Backend / API change? |
|---|-------|---------------------|-----------------------|
| 9  | Clean pipeline logs (empty + internal noise) | UI `PipelineView.vue` | No |
| 10 | Services section shows no services | UI `PipelineView.vue` (+ `types.ts`) | No |
| 11 | `exec`/Dockerfile `RUN` stdout/stderr missing | UI `PipelineView.vue` | No |
| 12 | Engine resource metrics charts | Backend (new endpoint) + UI chart + Helm scrape config | Yes |
| 15 | OTLP `413` on big log batches | Backend config + Helm ingress/collector | Yes (config only) |

Dependency rule is preserved: new backend work for #12 follows
`handler → service → domain ← repository`; #15 adds config + a handler constant
path; #9/#10/#11 are pure frontend.

---

## 1. Issue #9 — clean pipeline view logs

### 1.1 Root cause (grounded)

Two independent sources of noise:

1. **Internal transport spans leak into the step tree.** The CI step builder already
   knows Dagger emits internal spans that are *not* marked `dagger.io/ui.internal`
   — they are identified by **span name**. `internal/service/ci_steps.go:448-471`
   defines `internalSpanPrefixes` (`GET `, `POST `, `PUT `, `DELETE `, `PATCH `,
   `HEAD `, `OPTIONS `, `Read `, `Write `, `Query.`, `Address.`, `parsing `) and
   `internalSpanExact` (`connect`), and `flattenTrace` folds them away. The pipeline
   UI only filters on `dagger.io/ui.*` boolean attributes
   (`ui/src/pipeline/PipelineView.vue:434-460` `flattenVisible` / `topLevelSpans`),
   so internal HTTP/BuildKit spans (`POST /query`, `GET /blobs`, …) surface as
   steps/sub-spans with their noise logs.

2. **Empty/noise log records.** `logText` (`PipelineView.vue:663-707`) already drops
   `stdio.eof` markers and `Stdout:`/`Stderr:`-only records, but logs whose `span_id`
   belongs to an internal span are still pulled in by `logsForSubtree`
   (`PipelineView.vue:364-373`), which walks *all* descendants including hidden ones.
   The result is a wall of empty/internal lines when a step is expanded.

### 1.2 Decision: filter in the UI (client-side), mirror the CI name rules

Filtering **at the UI** is correct because span visibility already lives there
(the trace tree with `dagger.io/ui.*` attributes) and because the CI code already
proves the name-based internal-span classification is stable. Doing it in the
backend would require correlating Loki logs with Tempo span names (a second
dependency between the log and trace stores) for no gain. Empty-line filtering
stays client-side (already in `logText`) and is extended.

### 1.3 Files to modify

- `ui/src/pipeline/PipelineView.vue`

### 1.4 Changes

Add an `isInternalSpanName(name: string): boolean` helper (port of
`internal/service/ci_steps.go:448-471`, no runtime deps):

```ts
const INTERNAL_SPAN_PREFIXES = [
  'GET ', 'POST ', 'PUT ', 'DELETE ', 'PATCH ', 'HEAD ', 'OPTIONS ',
  'Read ', 'Write ', 'Query.', 'Address.', 'parsing ',
] as const
const INTERNAL_SPAN_EXACT = new Set<string>(['connect'])

function isInternalSpanName(name: string): boolean {
  return INTERNAL_SPAN_PREFIXES.some((p) => name.startsWith(p)) || INTERNAL_SPAN_EXACT.has(name)
}

// A span is hidden when marked internal by Dagger's UI hints OR by name.
function isHiddenSpan(n: SpanNode): boolean {
  return (
    attrBool(n, 'dagger.io/ui.internal') ||
    attrBool(n, 'dagger.io/ui.encapsulated') ||
    isInternalSpanName(n.name)
  )
}
```

Change the step-grouping walk so internal spans are folded away (same semantics as
`flattenTrace`): `flattenVisible` currently only checks `passthrough` / `internal` /
`encapsulated`. Replace the `internal`/`encapsulated` branches to also skip
`isInternalSpanName(n.name)` (count `hidden++`, still recurse into children so their
logs can be attributed to the nearest visible ancestor).

Change log collection so internal-span logs are dropped, not attributed:

- Add a computed `hiddenSpanIDs = computed<Set<string>>` built by walking the whole
  tree and collecting `span_id` for any span where `isHiddenSpan(span)` is true.
- `logsForSpan` / `logsForSubtree` / `unmatchedLogs` must exclude entries whose
  `span_id` is in `hiddenSpanIDs` (fall back to current behaviour when `span_id` is
  empty/unknown — those still land in "unmatched").
- `stepLogCount` consequently stops counting internal-span logs.

Extend `logText` to drop more noise:

- Trim before returning; return `null` when the final text is empty/whitespace.
- Drop records whose body is exactly `Stdout:` / `Stderr:` with no following content
  (already handled) and records whose `body` is only a newline.

### 1.5 Edge cases

- Internal span with user-relevant children: children are still walked (their logs
  attributed to the nearest visible ancestor), so real output is preserved.
- Root span must never be filtered (it has no parent and is never name-internal; the
  `topLevelSpans` walk only promotes passthrough, never hides root).
- A log with no `span_id` (unmatched) is never dropped by `hiddenSpanIDs` — it still
  appears under "Unmatched / general logs".
- Empty body that is not a `stdio.eof` marker: dropped by the new `logText` trim.

### 1.6 Tests / validation

- No frontend test runner exists (per `AGENTS.md`); validation is
  `cd ui && npm run typecheck` (vue-tsc) + `npm run build` (Vite build in CI).
- Manual: run a pipeline that builds an image and confirm `POST /query`/`GET /blobs`
  spans no longer appear, their logs no longer flood expanded steps, and user
  `echo`/`RUN` output is intact.

---

## 2. Issue #10 — services section shows no services

### 2.1 Root cause (grounded)

The Services section already exists (`PipelineView.vue:19-66` template,
`:509-620` detection) and is documented in `.opencode/plans/dagger-services-display.md`.
Its detection is **log-signal based**:

- `serviceLogAttrs` (`PipelineView.vue:520-530`) treats a log as a service signal only
  when `JSON.parse(entry.line).body === 'tunnel started'` **or** when
  `attributes.http_url` / `attributes.https_url` are non-empty.
- `isServiceSpan` (`:536-539`) requires ≥1 such log attached to the span's own
  `span_id`.

That plan explicitly noted it was **never verified against a live trace** (the auth
tool limitation; `.opencode/plans/dagger-services-display.md:35-42,301-313`). The
consequence is what #10 reports: the `tunnel started` body/attribute keys don't match
what the installed engine actually emits, so `services.length === 0` even when
services exist.

### 2.2 Decision: make detection robust + still frontend-only

Two orthogonal fixes, both in the UI:

1. **Detect from span identity, not just logs.** A Dagger `up`/`service.Up()` resolves
   to a span whose name is the `up` host tunnel (`up`, `host.tunnel`, or
   `Service.Up`). Treat a span as a service candidate when its **name** matches a
   tunable service-name set, in addition to the existing log signal.
2. **Parse log attributes leniently.** OTLP attributes may reach Loki either flat
   (`attributes.http_url`) or as OTLP `AnyValue` objects
   (`attributes.http_url = { stringValue: "..." }`, `attributes.port = { intValue: 80 }`).
   `extractServiceMeta` (`:541-567`) only handles flat values. Normalize both forms.

Keep the detection constants isolated in one block (as the prior plan recommended) so
they can be tuned after a real-trace inspection.

### 2.3 Files to modify

- `ui/src/pipeline/PipelineView.vue`
- `ui/src/api/types.ts` (no change strictly required; `ServiceInfo` already exists)

### 2.4 Changes

Add near the existing service constants:

```ts
const SERVICE_LOG_BODY = 'tunnel started'
const SERVICE_URL_ATTRS = ['http_url', 'https_url'] as const
// Tunable: span names that indicate a host-tunnel service (verify on a live trace).
const SERVICE_SPAN_NAMES = new Set<string>(['up', 'host.tunnel', 'Service.Up', 'service.Up'])
```

Normalize attributes in `serviceLogAttrs`:

```ts
function attrValue(a: Record<string, unknown>, k: string): string | null {
  const v = a[k]
  if (v == null) return null
  if (typeof v === 'string') return v === '' ? null : v
  if (typeof v === 'object' && v !== null) {
    const o = v as Record<string, unknown>
    if (typeof o.stringValue === 'string') return o.stringValue
    if (typeof o.intValue === 'number') return String(o.intValue)
    if (typeof o.doubleValue === 'number') return String(o.doubleValue)
  }
  return null
}
```

- `isServiceLog` uses `attrValue` for `http_url`/`https_url`.
- Add `isServiceSpanByName(span)` → `SERVICE_SPAN_NAMES.has(span.name)`.
- `computeServices` marks a span as a service when `isServiceSpan(n, logsBySpan) ||
  isServiceSpanByName(n)` (and, when name-matched but no `tunnel started` log is
  present, `logs` falls back to `logsForSubtree(n)` and `url`/`port` are `null`).
- `extractServiceMeta` reads `url`/`port`/`protocol`/`description` via `attrValue`
  (handles both flat and nested forms).

### 2.5 Edge cases

- Name-matched service with no log: row renders name + running state, `port`/`url`
  `null` (existing template already handles `no port`).
- Multiple services: unchanged (each row, sorted by `spanStartMs`).
- Service that also appears under a step: unchanged (Services is a summary, not a
  removal) — see prior plan edge-case table.
- Log JSON unparseable: `serviceLogAttrs` try/catch returns `null` (unchanged).

### 2.6 Tests / validation

- `npm run typecheck` + `npm run build`.
- Manual against a real `service.Up()`/`--up` pipeline (see §9 verification).

---

## 3. Issue #11 — exec / Dockerfile `RUN` stdout/stderr not shown

### 3.1 Root cause (grounded)

Two cooperating causes:

1. **Over-aggressive stdio filtering in `logText`** (`PipelineView.vue:663-707`). The
   `LogJSON` view model only models `attributes.stdio.eof`, and the prefix-strip only
   matches `^(Stdout|Stderr):\s*\n`. Real engine exec records carry the stdout/stderr
   body in `attributes.stdio` (with `stream`/`eof`) and a `body` that may be the raw
   text *without* a `Stdout:` prefix. Content-bearing records that don't match the
   narrow strip path (or whose body is a bare stream marker) end up returned as `null`
   and are hidden.
2. **Exec/`RUN` spans are folded as hidden.** `with-exec`/Dockerfile `RUN` are emitted
   as `dagger.io/ui.passthrough` (or `encapsulated`) child spans. With issue #9's
   current folding, those spans are hidden and, because `logsForSubtree` walked all
   descendants, their logs were only *sometimes* surfaced; once #9 drops hidden-span
   logs by `span_id`, exec logs could disappear entirely unless they are **attributed
   to the visible ancestor** (the `passthrough` semantics: promote children, and their
   logs belong to the nearest visible ancestor).

### 3.2 Decision: render stdout/stderr correctly and route passthrough/exec logs to the visible ancestor

- `logText` must **render** content-bearing stdio records (strip only the stream
  prefix/ANSI, keep the payload) and only `null`-out true empty markers
  (`stdio.eof` with empty body, or whitespace-only bodies). Model `stdio` fully.
- `logsForSubtree`/log collection must treat `dagger.io/ui.passthrough` spans as
  transparent for log ownership: logs attached to a passthrough span (or any hidden
  span) are attributed to the nearest **visible** ancestor — never dropped. This
  preserves `RUN`/exec output while #9 still removes genuine internal noise (which is
  identified by name/internal attributes, not by passthrough).

### 3.3 Files to modify

- `ui/src/pipeline/PipelineView.vue`

### 3.4 Changes

Model stdio fully and simplify rendering:

```ts
interface LogJSON {
  body?: unknown
  attributes?: { stdio?: { stream?: number; eof?: boolean } }
}
```

`logText(line)`:
1. Parse JSON; if `body` is a string, use it.
2. Strip ANSI.
3. Strip a leading `Stdout:\n` / `Stderr:\n` / `Stdout: ` / `Stderr: ` prefix.
4. If after trimming the text is empty:
   - return `null` when `attributes.stdio.eof === true`, else return the raw text
     (never hide a non-empty stream marker that has no content yet — but empty →
     `null`).
5. Keep the existing base64-progress collapse.

Replace the log-ownership helpers so hidden/passthrough spans route logs to the
nearest visible ancestor instead of dropping them:

```ts
// Returns the nearest visible ancestor span ID for a log's span, or "" for
// unmatched/root logs. Passthrough spans are transparent; internal/name-internal
// spans are noise (dropped per #9); encapsulated spans keep their logs attached to
// the visible ancestor.
function visibleOwner(spanID: string): string { /* walk the tree once */ }
```

`logsForSpan`/`logsForSubtree`/`unmatchedLogs` build on `visibleOwner`:
- passthrough/encapsulated span logs → visible ancestor (so `RUN`/exec output shows
  under the step).
- name-internal / `dagger.io/ui.internal` span logs → dropped (issue #9).
- `unmatchedLogs` = logs whose `span_id` resolves to no span in the tree.

### 3.5 Edge cases

- `RUN` with binary/ANSI output: ANSI stripped, base64 protobufs collapsed (unchanged).
- `stdio.eof` empty record: hidden (no blank line).
- A `RUN` step with no visible ancestor (orphan): logs fall back to "unmatched".
- stdout vs stderr interleaving: preserved by timestamp sort (already in place).

### 3.6 Tests / validation

- `npm run typecheck` + `npm run build`.
- Manual: `container().build()` a Dockerfile with `RUN echo hello` and confirm
  `hello` (stdout) and any stderr appear under the build step.

---

## 4. Issue #12 — engine resource metrics (CPU / memory / disk IO / network)

### 4.1 Root cause / current state (grounded)

There is no metrics surface in the pipeline view today. The only PromQL access is the
raw reverse proxy `GET /api/v1/metrics{/query,/query_range}` (`internal/handler/metrics.go:12-28`,
`internal/handler/server.go:611-612,652-676`), which is not trace-scoped and not
consumed by the UI. VictoriaMetrics only receives OTLP metrics via
`prometheusremotewrite` (`deploy/helm/dagger-kubernetes/values.yaml:803-820`); the
chart configures **no kubelet/cAdvisor scraper**, and
`docs/README.md:880-886` confirms the metrics currently emitted (BuildKit cache
counters, engine metrics) are aggregate with no trace association. Therefore pod-level
CPU/memory/disk/network for the engine is **not in VictoriaMetrics yet**.

### 4.2 Decision — data path

**Source:** cAdvisor `container_*` metrics scraped from the kubelet into
VictoriaMetrics. Justification: the Dagger engine's OTLP metrics are BuildKit/engine
aggregates and do **not** include the engine pod's CPU/memory/disk/network; the
canonical source for those is kubelet cAdvisor (`container_cpu_usage_seconds_total`,
`container_memory_working_set_bytes`, `container_fs_reads_bytes_total` /
`container_fs_writes_bytes_total`, `container_network_receive_bytes_total` /
`container_network_transmit_bytes_total`).

**Query path:** a **new, trace-scoped supervisor endpoint**
`GET /api/v1/traces/:traceID/metrics` (auth-gated by the existing
`authorizeTraceRequest`). The supervisor builds scoped PromQL and queries
VictoriaMetrics server-side; the UI never writes PromQL. This keeps the raw
`/api/v1/metrics` proxy for operators while giving the pipeline view a curated,
per-pipeline view.

**Scoping:** by time window + engine pod label selector, derived from `trace_meta`
(`domain.TraceMeta` `:38-51` has `Version`, `StartedAt`, `DurationMS`):

- time window = `[StartedAt, StartedAt + DurationMS]` (fallback: last 24h when unknown).
- pod selector = `{namespace="<fleet.namespace>", pod=~"<stsName>-.*", container="engine"}`
  where `stsName` comes from `domain.StsName(version)` (engine StatefulSet name).

**Charting:** hand-rolled SVG line/area chart (no new dependency). `ui/package.json`
has no charting library (only `vue`, `vue-router`, `pinia`, `axios`), and adding a
heavy chart lib is not justified for 4 small time-series panels.

### 4.3 Files to create/modify

Backend:
- `internal/domain/telemetry.go` — add metric types + queryer interface.
- `internal/repository/metrics_store.go` — add `QueryRange` method (+ `_test.go`).
- `internal/service/engine_metrics.go` (new) — `EngineMetricsService` (+ `_test.go`).
- `internal/handler/metrics.go` — add `handleTraceMetrics`.
- `internal/handler/server.go` — register route, add `ServerConfig` field, add `Deps`
  field.
- `cmd/api/main.go` — wire the service.

Config:
- `internal/domain/config.go` — `PipelineMetricsConfig`.
- `config/loader.go` — defaults + `validatePipelineMetricsConfig`.
- `config/config.app.yaml.sample` — document.

Helm:
- `deploy/helm/dagger-kubernetes/values.yaml` — `supervisor.config.pipeline.metrics.*`
  + VictoriaMetrics kubelet scrape config.
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — render the block.

UI:
- `ui/src/api/types.ts` — metric types.
- `ui/src/api/client.ts` — `fetchTraceMetrics`.
- `ui/src/pipeline/MetricChart.vue` (new) — SVG chart component.
- `ui/src/pipeline/PipelineView.vue` — metrics card + fetch.

### 4.4 Data structures + signatures

`internal/domain/telemetry.go`:

```go
type MetricPoint struct {
    T int64   `json:"t"` // unix seconds
    V float64 `json:"v"`
}

type MetricSeries struct {
    Name   string        `json:"name"`
    Label  string        `json:"label"`
    Unit   string        `json:"unit"`
    Points []MetricPoint `json:"points"`
}

type TraceMetrics struct {
    TraceID     string         `json:"trace_id"`
    StartTime   time.Time      `json:"start_time"`
    EndTime     time.Time      `json:"end_time"`
    StepSeconds int64          `json:"step_seconds"`
    Series      []MetricSeries `json:"series"`
}

// MetricsQueryer runs one PromQL range query against the metrics backend and
// returns the aggregate (summed across matched series) points.
type MetricsQueryer interface {
    QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]MetricPoint, error)
}
```

`internal/repository/metrics_store.go` (add to existing `MetricsClient`):

```go
// QueryRange runs a PromQL query_range against VictoriaMetrics and returns the
// points summed across all matched series (engine-fleet aggregate).
func (c *MetricsClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]domain.MetricPoint, error)
```

- Validates `query` non-empty and `victoriaURL` set; builds
  `GET {victoriaURL}/api/v1/query_range?query=…&start=<unix>&end=<unix>&step=<sec>`.
- Decodes the Prometheus matrix response (`data.result[].values`), sums values
  per timestamp, returns sorted `[]MetricPoint`. Empty result → empty slice (not error).

`internal/service/engine_metrics.go` (new):

```go
type EngineMetricsService struct {
    queryer   domain.MetricsQueryer
    namespace string
    step      time.Duration
    logger    *logrus.Logger
}

func NewEngineMetricsService(queryer domain.MetricsQueryer, namespace string, step time.Duration, logger *logrus.Logger) *EngineMetricsService

// TraceMetrics builds the scoped PromQL for the trace's engine + time window and
// runs them. meta may be nil (unknown trace) — then it returns an empty-but-valid
// TraceMetrics rather than erroring.
func (s *EngineMetricsService) TraceMetrics(ctx context.Context, meta *domain.TraceMeta) (*domain.TraceMetrics, error)
```

Default PromQL constants (isolated for post-deploy tuning, mirroring the pattern in
`.opencode/plans/magiccache-dashboard.md`):

```go
// cAdvisor metric names; verify against the live cluster (§9). Template vars:
// {ns} namespace, {pod} engine StatefulSet name.
var defaultMetricQueries = []struct{ name, label, unit, promql string }{
    {"cpu", "CPU", "cores", `rate(container_cpu_usage_seconds_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[5m])`},
    {"memory", "Memory", "bytes", `container_memory_working_set_bytes{namespace="{ns}",pod=~"{pod}-.*",container="engine"}`},
    {"disk_read", "Disk read", "bytes/s", `rate(container_fs_reads_bytes_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[5m])`},
    {"disk_write", "Disk write", "bytes/s", `rate(container_fs_writes_bytes_total{namespace="{ns}",pod=~"{pod}-.*",container="engine"}[5m])`},
    {"net_rx", "Network rx", "bytes/s", `rate(container_network_receive_bytes_total{namespace="{ns}",pod=~"{pod}-.*"}[5m])`},
    {"net_tx", "Network tx", "bytes/s", `rate(container_network_transmit_bytes_total{namespace="{ns}",pod=~"{pod}-.*"}[5m])`},
}
```

`internal/handler/metrics.go`:

```go
// handleTraceMetrics serves engine resource metrics for a trace (auth-gated).
func (s *Server) handleTraceMetrics(ctx context.Context, c *app.RequestContext)
```

- `authorizeTraceRequest` → traceID.
- `s.traceMeta.Get` → meta (missing meta = empty series, HTTP 200).
- `s.engineMetrics.TraceMetrics` → `writeJSON`.
- If `s.engineMetrics == nil` (disabled/unconfigured) → `writeError(501, "metrics unavailable")`.

`internal/handler/server.go`:
- `ServerConfig`: add `EngineMetrics *service.EngineMetricsService`? No — keep
  `ServerConfig` transport-only. Add to `Deps`:
  `EngineMetrics *service.EngineMetricsService` (nil = disabled). Store as
  `engineMetrics` on `Server`.
- Route: `h.GET("/api/v1/traces/:traceID/metrics", s.handleTraceMetrics)` (place with
  the other trace routes at `server.go:604-609`).

`internal/domain/config.go`:

```go
type PipelineConfig struct {
    DisconnectGrace time.Duration      `mapstructure:"disconnect_grace"`
    StaleSweep      PipelineStaleSweep `mapstructure:"stale_sweep"`
    Metrics         PipelineMetricsConfig `mapstructure:"metrics"`
}

type PipelineMetricsConfig struct {
    Enabled bool          `mapstructure:"enabled"`
    Step    time.Duration `mapstructure:"step"` // query_range step; default 15s
}
```

`config/loader.go`:
- `v.SetDefault("pipeline.metrics.enabled", true)`
- `v.SetDefault("pipeline.metrics.step", 15*time.Second)`
- `validatePipelineMetricsConfig`: `step > 0` when enabled (else fail fast).

`cmd/api/main.go` wiring (after `metricsClient := repository.NewMetricsClient(...)`):

```go
var engineMetricsSvc *service.EngineMetricsService
if cfg.Pipeline.Metrics.Enabled {
    engineMetricsSvc = service.NewEngineMetricsService(metricsClient, cfg.Fleet.Namespace, cfg.Pipeline.Metrics.Step, logger)
}
// Deps.EngineMetrics: engineMetricsSvc
```

UI (`ui/src/api/types.ts`):

```ts
export interface MetricPoint { t: number; v: number }
export interface MetricSeries {
  name: string
  label: string
  unit: string
  points: MetricPoint[]
}
export interface TraceMetrics {
  trace_id: string
  start_time: string
  end_time: string
  step_seconds: number
  series: MetricSeries[]
}
```

UI (`ui/src/api/client.ts`):

```ts
export async function fetchTraceMetrics(id: string): Promise<TraceMetrics> {
  const { data } = await api.get(`/api/v1/traces/${id}/metrics`)
  return data as TraceMetrics
}
```

`MetricChart.vue` (SVG, no deps): props `series: MetricSeries`, `unit: string`;
renders an SVG polyline area with min/max axis, scaled to the container, empty-state
when `points.length < 2`. `PipelineView.vue` fetches metrics in `loadAll()` and
renders a card at the top (above Services).

### 4.5 Helm scrape config

VictoriaMetrics single-node supports a built-in `promscrape`. Add under the
`victoria` subchart values (values.yaml) a scrape job targeting the kubelet cAdvisor
endpoint, e.g.:

```yaml
victoria:
  server:
    scrape:
      enabled: true
      config:
        scrape_configs:
          - job_name: kubelet-cadvisor
            scheme: https
            kubernetes_sd_configs:
              - role: node
            bearer_token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
            tls_config:
              ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
              insecure_skip_verify: true
            relabel_configs:
              - source_labels: [__meta_kubernetes_node_name]
                target_label: node
                replacement: ${1}
            metrics_path: /metrics/cadvisor
```

(Exact subchart key names must be verified against the installed
`victoria-metrics-single` chart version during implementation; the intent is
"scrape kubelet cAdvisor with the ServiceAccount token".) The engine pods already
run in `fleet.namespace`, so `container_*` metrics are labeled with that namespace +
pod name.

### 4.6 Edge cases

- Unknown/missing `trace_meta`: empty series, HTTP 200 (never 404) — the card shows
  "no metrics".
- No metrics backend (`victoriaURL` empty): `QueryRange` returns a wrapped
  `"victoria URL not configured"`; handler maps to empty series + logs a WARN.
- Running pipeline with no end time: window = `[started, now]`.
- Multiple engine pods (replicas): summed aggregate.
- No data in window: empty `points` → chart empty state.
- Huge window / step too small: clamp step to ≥1s; bound the time window to
  `[started, min(now, started+maxWindow)]` where `maxWindow` = 24h (CWE-400).

### 4.7 Tests / validation

Unit (table-driven, stdlib only):
- `internal/repository/metrics_store_test.go` — `QueryRange`: happy matrix decode +
  sum, empty result, unconfigured URL, non-200, invalid JSON, bad query param.
- `internal/service/engine_metrics_test.go` — `TraceMetrics`: nil meta, unknown
  version, disabled queryer, correct PromQL template substitution (assert `{ns}`/`{pod}`
  replaced), step clamp, empty series.

Integration (`tests/integration/`, use `freeListener(t)` + timed shutdown in
`t.Cleanup`, per `AGENTS.md`): a `pipeline_metrics_test.go` that starts a fake
VictoriaMetrics `httptest.Server` and asserts `GET /api/v1/traces/:id/metrics`
returns the expected `series` shape for a trace whose meta has a version + start/duration.

UI: `npm run typecheck` + `npm run build`; manual chart check against a real pipeline.

---

## 5. Issue #15 — OTLP `413` on large log batches

### 5.1 Root cause (grounded)

Three independent body-size ceilings stack up between the Dagger CLI and the
collector:

1. **nginx ingress** default `client_max_body_size` is `1m`. The control-plane
   ingress (`deploy/helm/dagger-kubernetes/templates/ingress.yaml`) sets only
   `nginx.ingress.kubernetes.io/backend-protocol: HTTPS` (line 10) — no
   `proxy-body-size` — so any `/v1/logs` batch > 1 MiB is rejected with `413` by
   nginx before it reaches the supervisor.
2. **Supervisor Hertz handler** rejects OTLP bodies larger than `maxControlBody`
   (4 MiB) up front: `internal/handler/server.go:38-46` (constant) and
   `:856-861` (`if cl := c.Request.Header.ContentLength(); cl > maxControlBody`).
   The 4 MiB cap was sized for JSON control-API bodies, not OTLP batches.
3. **OTel collector** `otlp` HTTP receiver has its own default
   `max_request_body_size` (20 MiB in the contrib collector 0.108.0), configured at
   `values.yaml:780-784` with no override.

### 5.2 Decision

Add a dedicated, larger **OTLP ingest** limit (config-driven, default 64 MiB),
separate from the 4 MiB control-API cap; raise the ingress and collector limits to
match; keep the control-API `maxControlBody` untouched.

- New config `otel.ingest_max_body_size` (bytes; default `67108864` = 64 MiB).
- `handleOTel` uses it instead of `maxControlBody`.
- nginx: `nginx.ingress.kubernetes.io/proxy-body-size: "64m"` (default annotation,
  overridable via a new `ingress.proxyBodySize` value).
- Collector: set `receivers.otlp.protocols.http.max_request_body_size` (bytes) to the
  same default.

### 5.3 Files to modify

- `internal/domain/config.go` — `OTelConfig.IngestMaxBodySize`.
- `config/loader.go` — default + validation.
- `config/config.app.yaml.sample` — document.
- `internal/handler/server.go` — `ServerConfig.OTelMaxBodyBytes`; use in `handleOTel`.
- `cmd/api/main.go` — wire `cfg.OTel.IngestMaxBodySize` into `ServerConfig`.
- `deploy/helm/dagger-kubernetes/templates/ingress.yaml` — default `proxy-body-size`.
- `deploy/helm/dagger-kubernetes/values.yaml` — `ingress.proxyBodySize`,
  `supervisor.config.otel.ingestMaxBodySize`, collector `max_request_body_size`.
- `deploy/helm/dagger-kubernetes/templates/configmap.yaml` — render the otel key.

### 5.4 Changes

`internal/domain/config.go`:

```go
type OTelConfig struct {
    OTLPEndpoint      string `mapstructure:"otlp_endpoint"`
    IngestMaxBodySize int64  `mapstructure:"ingest_max_body_size"` // bytes; OTLP ingest cap
}
```

`config/loader.go`:

```go
v.SetDefault("otel.ingest_max_body_size", int64(64<<20)) // 64 MiB
```

`internal/handler/server.go`:

```go
const defaultOTLPMaxBodyBytes = int64(64 << 20) // 64 MiB

// ServerConfig gains:
OTelMaxBodyBytes int64 // 0 = default 64 MiB
```

In `handleOTel`, replace the `maxControlBody` check:

```go
limit := s.cfg.OTelMaxBodyBytes
if limit <= 0 {
    limit = defaultOTLPMaxBodyBytes
}
if cl := c.Request.Header.ContentLength(); cl > limit {
    s.metrics.OTelIngestTotal.WithLabelValues(signal, "error").Inc()
    writeError(c, consts.StatusRequestEntityTooLarge, "otel body too large")
    return
}
```

`cmd/api/main.go`:

```go
&handler.ServerConfig{
    ...
    OTelMaxBodyBytes: cfg.OTel.IngestMaxBodySize,
    ...
}
```

`ingress.yaml` (default annotation, override via `ingress.annotations` still works
because `toYaml` of user annotations renders after and wins on key collisions):

```yaml
  annotations:
    nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"
    nginx.ingress.kubernetes.io/proxy-body-size: {{ .Values.ingress.proxyBodySize | default "64m" | quote }}
    {{- with .Values.ingress.annotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
```

`values.yaml`:

```yaml
ingress:
  enabled: true
  className: ""
  proxyBodySize: "64m"
  annotations: {}
  ...
supervisor:
  config:
    otel:
      otlpEndpoint: ""
      ingestMaxBodySize: 67108864
```

`configmap.yaml` otel block:

```yaml
{{- with .Values.supervisor.config.otel }}
    otel:
      otlp_endpoint: {{ .otlpEndpoint | quote }}
      ingest_max_body_size: {{ .ingestMaxBodySize | default 67108864 }}
{{- end }}
```

Collector (`values.yaml` `opentelemetry-collector.config.receivers.otlp.protocols.http`):

```yaml
    receivers:
      otlp:
        protocols:
          http:
            endpoint: 0.0.0.0:4318
            max_request_body_size: 67108864
```

### 5.5 Edge cases

- Chunked bodies (no `Content-Length`): the pre-check is bypassed; the body is still
  bounded by the collector's own limit and, for the extraction read, Hertz buffers it.
  Documented limitation; acceptable for the OTLP path (the collector rejects
  oversized batches). Do **not** lower the control-API `maxControlBody`.
- `ingest_max_body_size <= 0`: validated in `config.Load` — reject (fail fast) so a
  misconfiguration can't silently disable the cap.
- Config env override: `DAGGER_KUBERNETES_OTEL_INGEST_MAX_BODY_SIZE`.

### 5.6 Validation rules

`config/loader.go`:

```go
func validateOTelConfig(cfg *domain.Config) error {
    if cfg.OTel.IngestMaxBodySize < 0 {
        return fmt.Errorf("otel.ingest_max_body_size must be >= 0")
    }
    return nil
}
```

### 5.7 Tests / validation

- `config` loader test: default = 64 MiB; env override; negative → error.
- `internal/handler` `handleOTel` test: `Content-Length` just under/over the limit
  (413 vs pass-through), using the existing `server_test.go` harness.
- Helm: `helm template` matrix (already in CI) must render the annotation +
  `ingest_max_body_size` + collector `max_request_body_size` for all three data-plane
  TLS cases.
- Manual: POST a >4 MiB OTLP log batch through the ingress and confirm no `413`.

---

## 6. Docs & ADRs (part of the changeset)

- `config/config.app.yaml.sample` — add `otel.ingest_max_body_size` and
  `pipeline.metrics.{enabled,step}` with comments/defaults.
- `docs/README.md` —
  - Pipeline UI section (`:1610-1672`): add Services section behaviour, log
    filtering (internal-span + empty-line removal), exec/`RUN` stdout/stderr, and the
    new engine-metrics card; add the `GET /api/v1/traces/:id/metrics` endpoint to the
    API table.
  - Telemetry/ingress: document `otel.ingest_max_body_size`, the
    `nginx.ingress.kubernetes.io/proxy-body-size` default, and the collector
    `max_request_body_size`.
- `docs/design/ADR-037-pipeline-view-observability.md` (new) — records the
  architectural decisions for #9–#12: name-based internal-span filtering in the UI
  (aligned with ADR-024), robust service detection, exec-log rendering, the
  trace-scoped metrics endpoint + cAdvisor data path + hand-rolled SVG charting, and
  the scoping-by-trace-meta approach. Update `docs/design/index.md` table.
- `docs/design/ADR-024-ci-nested-steps.md` — add a note that the pipeline UI now
  shares the same `internalSpanPrefixes`/`internalSpanExact` name rules as the CI
  builder (single source of truth for internal-span names).
- `DAGGER.md` — **not** required (no changes to `dagger/`, CI scripts, or
  `.github/workflows/`); the UI build and Helm template matrix are already covered.

---

## 7. Ordered implementation checklist

1. `git checkout main && git pull && git checkout -b fix/pipeline-view-issues`.
2. **#15 backend**: add `OTelConfig.IngestMaxBodySize`; loader default + validation;
   `ServerConfig.OTelMaxBodyBytes` + `defaultOTLPMaxBodyBytes`; update `handleOTel`;
   wire in `cmd/api/main.go`. Add config + handler tests.
3. **#15 Helm**: `ingress.yaml` default annotation; `values.yaml` (`ingress.proxyBodySize`,
   `supervisor.config.otel.ingestMaxBodySize`, collector `max_request_body_size`);
   `configmap.yaml` otel block.
4. **#12 backend**: domain types + `MetricsQueryer`; `MetricsClient.QueryRange` +
   tests; `service/engine_metrics.go` + tests; `handler.handleTraceMetrics`; route +
   `Deps.EngineMetrics`; `cmd/api/main.go` wiring; config
   (`PipelineMetricsConfig` + loader default + validation) + sample.
5. **#12 Helm**: `configmap.yaml` pipeline.metrics block; `values.yaml`
   `supervisor.config.pipeline.metrics.*`; VictoriaMetrics kubelet scrape config.
6. **#9/#11 UI**: `PipelineView.vue` — `isInternalSpanName`/`isHiddenSpan`,
   `visibleOwner` log-routing, `logText` stdio fix, hidden-span-id log exclusion,
   `hiddenCount`; ensure `RUN`/exec output routes to visible ancestors.
7. **#10 UI**: `PipelineView.vue` + `types.ts` — span-name service detection,
   lenient attribute parsing, `computeServices` update.
8. **#12 UI**: `types.ts` metric types; `client.ts` `fetchTraceMetrics`;
   `MetricChart.vue`; metrics card + fetch in `PipelineView.vue`.
9. **Docs**: `config/config.app.yaml.sample`, `docs/README.md`, new ADR-037 +
   `index.md`, ADR-024 note.
10. **Integration tests**: `tests/integration/pipeline_metrics_test.go` (fake VM,
    `freeListener(t)`, timed shutdown).
11. Local gate (minimum): `go build ./... && go vet ./... && go test ./...` +
    `dagger call -m ./dagger --src . lint`.
12. Full CI gate: `dagger call -m ./dagger --src . ci export --path out` (must pass
    golangci-lint incl. `unused`, `go test -race -covermode=atomic ./...`, UI build,
    binary builds, Dockerfile smoke, Helm lint/template matrix).
13. Redeploy + verify per `AGENTS.local.md` §4–§6 (build/push/helm-upgrade/rollout;
    agent checks; human UI verification of Services, filtered logs, `RUN` output,
    metrics charts, and a large-log ingestion).

---

## 8. Non-goals / out-of-scope

- No patching the upstream Dagger CLI (see ADR-021).
- No adding a charting library (hand-rolled SVG only).
- No change to the raw `/api/v1/metrics` PromQL proxy contract.
- No multi-metric-name auto-discovery; PromQL expressions are constants (tunable via
  code, verified live).
- No per-replica (per-pod) metric breakdown — engine metrics are summed across
  replicas (fleet aggregate).
- No changing the 4 MiB control-API `maxControlBody`.
- No bound-service detection for #10 (only host-tunnel `up`/`service.Up()` services;
  bound services without an `up`/tunnel remain undetected, as documented).
- No backend log-store schema change; #9/#10/#11 remain frontend-only.

---

## 9. Open questions / risks (to resolve at implementation time)

1. **#10/#11 exact engine signal shape** — the `tunnel started` body text and the
   stdout/stderr log attribute nesting must be confirmed against a **real** live
   trace on the cluster (the `?token=` auth fallback is SSE-only, so inspect via the
   authenticated UI/API or `kubectl`-proxied `GET /api/v1/traces/:id/logs`). Detection
   constants are isolated for one-line tuning.
2. **#12 cAdvisor metric names/labels + VictoriaMetrics subchart scrape key** — verify
   `container_*` metric availability and the exact `victoria.server.scrape.*` subchart
   value shape before wiring; the endpoint is designed to be tolerant (empty series on
   no data) if cAdvisor scraping is not yet present.
3. **#12 kubelet scraping authorization** — requires a ServiceAccount token with
   `nodes/metrics` RBAC (or `insecure_skip_verify` + token); decide the least
   privilege that works on the k3s cluster.
4. **#15 collector default** — confirm the contrib collector 0.108.0 `otlp` receiver's
   actual `max_request_body_size` default before hardcoding; the config override is
   harmless either way.
