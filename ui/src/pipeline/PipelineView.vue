<template>
  <div>
    <div class="header">
      <router-link to="/pipelines" class="btn">&larr; Back</router-link>
      <div class="header-id">
        <h1 class="page-title" style="margin: 0;">Pipeline {{ shortId }}</h1>
        <code class="trace-id">{{ traceId }}</code>
      </div>
      <div class="header-meta">
        <span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span>
        <span v-if="trace.version" class="version-chip">{{ trace.version }}</span>
        <span class="user-chip" :title="trace.user_id ? `user_id: ${trace.user_id}` : ''">
          {{ trace.username ? `@${trace.username}` : 'anonymous' }}
        </span>
        <span class="duration">{{ formatDuration(liveTraceDuration()) }}</span>
      </div>
    </div>

    <div v-if="metrics" class="card metrics-card">
      <h3>Engine metrics</h3>
      <div v-if="metrics.series.length === 0" class="empty">No metrics</div>
      <div v-else class="metrics-grid">
        <MetricChart
          v-for="s in metrics.series"
          :key="s.name"
          :series="s"
          :unit="s.unit"
        />
      </div>
    </div>

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
          <span v-else-if="!svc.running && svc.port == null" class="service-noport">no port</span>
          <span class="service-duration">{{ formatDuration(liveSpanDuration(svc.span, now)) }}</span>
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
          <div v-follow-logs class="logs">
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

    <div class="card">
      <h3>Steps</h3>
      <StepTree :trace-id="traceId" :trace="trace" :refresh-key="searchRefreshKey" />
    </div>

    <details class="card" :open="unmatchedLogs.length > 0 && logs.length > 0 && unmatchedLogs.length === logs.length">
      <summary>Unmatched / general logs ({{ unmatchedLogs.length }})</summary>
      <div v-follow-logs class="logs">
        <template v-for="(log, i) in unmatchedLogs" :key="i">
          <div v-if="logText(log.line) !== null" class="log-line">
            <span class="log-ts">{{ formatTime(log.timestamp) }}</span>
            <span class="log-msg">{{ logText(log.line) }}</span>
          </div>
        </template>
        <p v-if="logsLoading" class="empty">Loading logs...</p>
        <p v-else-if="unmatchedLogs.length === 0" class="empty">No unmatched logs</p>
      </div>
    </details>

    <div class="card">
      <h3>Details</h3>
      <table>
        <tbody>
          <tr>
            <td>User</td>
            <td>
              <span v-if="trace.username">{{ trace.username }}</span>
              <span v-else class="empty-value">anonymous</span>
            </td>
          </tr>
          <tr><td>Status</td><td><span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span></td></tr>
          <tr><td>Duration</td><td>{{ formatDuration(liveTraceDuration()) }}</td></tr>
          <tr><td>Started</td><td>{{ formatDate(trace.start_time) }}</td></tr>
          <tr><td>Version</td><td>{{ trace.version || '-' }}</td></tr>
          <tr><td>CI Provider</td><td>{{ ciLabel(trace.ci_provider) }}</td></tr>
          <tr><td>Repository</td><td>{{ trace.ci_repo || '-' }}</td></tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { fetchTrace, fetchTraceLogs, fetchTraceMetrics, connectLiveTrace } from '@/api/client'
import type { ServiceInfo, SpanNode, TraceDetail, TraceLogEntry, TraceMetrics } from '@/api/types'
import { vFollowLogs } from '@/directives/followLogs'
import MetricChart from '@/pipeline/MetricChart.vue'
import StepTree from '@/pipeline/StepTree.vue'
import { formatDuration, isInternalSpan, isTransparentSpan, liveSpanDuration, logText, spanStartMs } from '@/pipeline/spanTree'

const route = useRoute()
const traceId = route.params.id as string

const trace = ref<TraceDetail>({
  trace_id: traceId,
  root_span: null,
  status: 'running',
  start_time: '',
  duration_ns: 0,
  duration_ms: 0,
  version: '',
})
const logs = ref<TraceLogEntry[]>([])
const logsLoading = ref(true)
const metrics = ref<TraceMetrics | null>(null)

// Bumped on every live logs_update / poll so StepTree refreshes its search
// results (debounced there) while preserving focus + query.
const searchRefreshKey = ref(0)

// Live-ticking clock: refreshed every 250ms while mounted so running durations
// visibly increase (see liveSpanDuration/liveTraceDuration below).
const now = ref<number>(Date.now())

// Tunable detection constants — isolated so they can be adjusted after a real-trace
// inspection without touching logic.
const SERVICE_LOG_BODY = 'tunnel started'
const SERVICE_URL_ATTRS = ['http_url', 'https_url'] as const
// Span names that indicate a host-tunnel service (verify on a live trace).
const SERVICE_SPAN_NAMES = new Set<string>(['up', 'host.tunnel', 'Service.Up', 'service.Up'])

const SERVICE_TAIL_LINES = 50

interface ServiceRow extends ServiceInfo {
  expanded: boolean
}

const services = ref<ServiceRow[]>([])

let pollTimer: number | undefined
let eventSource: EventSource | undefined
let traceDebounce: number | undefined
let logsDebounce: number | undefined
let nowTimer: number | undefined
let disposed = false

const shortId = computed(() => (traceId.length > 12 ? `${traceId.slice(0, 12)}…` : traceId))

// Logs keyed by their span_id so they can be attached to the span tree.
// The backend normalises the Loki span_id label to the same base64 form used
// by Tempo's span IDs, so string equality is sufficient.
const logsBySpan = computed<Map<string, TraceLogEntry[]>>(() => {
  const map = new Map<string, TraceLogEntry[]>()
  for (const log of logs.value) {
    if (!log.span_id) continue
    const bucket = map.get(log.span_id)
    if (bucket) bucket.push(log)
    else map.set(log.span_id, [log])
  }
  return map
})

// ownerBySpanID maps every span in the tree to the span ID that owns its logs:
//   - a visible span owns its own logs;
//   - a passthrough/encapsulated span is transparent, so its logs belong to the
//     nearest visible ancestor (RUN/exec output stays under the step);
//   - an internal/name-internal span is noise, so its logs are dropped (null).
// Children of a hidden span inherit the same owner, so user-relevant children
// of an internal span still surface under the nearest visible ancestor.
const ownerBySpanID = computed<Map<string, string | null>>(() => {
  const map = new Map<string, string | null>()
  const walk = (n: SpanNode | null, owner: string) => {
    if (!n) return
    let nextOwner = owner
    if (isInternalSpan(n)) {
      map.set(n.span_id, null)
    } else if (isTransparentSpan(n)) {
      map.set(n.span_id, owner)
    } else {
      map.set(n.span_id, n.span_id)
      nextOwner = n.span_id
    }
    for (const c of n.children) walk(c, nextOwner)
  }
  walk(trace.value.root_span, '')
  return map
})

// logsByOwner buckets logs by their resolved visible owner. Logs whose span is
// internal (owner null) or absent from the tree (unmatched) are not bucketed.
const logsByOwner = computed<Map<string, TraceLogEntry[]>>(() => {
  const map = new Map<string, TraceLogEntry[]>()
  for (const log of logs.value) {
    if (!log.span_id) continue
    const owner = ownerBySpanID.value.get(log.span_id)
    if (owner === undefined || owner === null) continue
    const bucket = map.get(owner)
    if (bucket) bucket.push(log)
    else map.set(owner, [log])
  }
  return map
})

const unmatchedLogs = computed<TraceLogEntry[]>(() =>
  logs.value.filter((l) => !l.span_id || !ownerBySpanID.value.has(l.span_id))
)

onMounted(async () => {
  await loadAll()

  // The component may have unmounted while loadAll() was in flight; do not
  // register timers/listeners on a dead component.
  if (disposed) return

  now.value = Date.now()
  nowTimer = window.setInterval(() => {
    now.value = Date.now()
  }, 250)
  document.addEventListener('visibilitychange', onVisibilityChange)

  pollTimer = window.setInterval(async () => {
    if (trace.value.status === 'success' || trace.value.status === 'failed') return
    await loadAll()
    searchRefreshKey.value++
  }, 5000)

  // Live SSE updates: the supervisor broadcasts a lightweight re-fetch signal
  // for each trace ID present in ingested OTLP spans/logs. Debounce so a burst
  // of events collapses into a single refetch.
  eventSource = connectLiveTrace(traceId)
  eventSource.onmessage = (event) => {
    try {
      const data = JSON.parse(event.data)
      if (data.type === 'trace_update') scheduleTraceRefetch()
      else if (data.type === 'logs_update') scheduleLogsRefetch()
    } catch {
      // Ignore malformed / non-JSON events (e.g. keepalives).
    }
  }
})

onUnmounted(() => {
  disposed = true
  if (pollTimer) window.clearInterval(pollTimer)
  if (traceDebounce) window.clearTimeout(traceDebounce)
  if (logsDebounce) window.clearTimeout(logsDebounce)
  if (eventSource) eventSource.close()
  if (nowTimer) window.clearInterval(nowTimer)
  document.removeEventListener('visibilitychange', onVisibilityChange)
})

// Background tabs throttle setInterval to ≤1/s; recompute `now` from Date.now()
// the moment the tab becomes visible again so the display never shows a stale
// frozen elapsed after a long absence.
function onVisibilityChange() {
  if (document.visibilityState === 'visible') now.value = Date.now()
}

function scheduleTraceRefetch() {
  if (traceDebounce) window.clearTimeout(traceDebounce)
  traceDebounce = window.setTimeout(() => {
    void loadTrace()
  }, 300)
}

function scheduleLogsRefetch() {
  if (logsDebounce) window.clearTimeout(logsDebounce)
  logsDebounce = window.setTimeout(() => {
    void loadLogs()
    searchRefreshKey.value++
  }, 300)
}

async function loadAll() {
  await Promise.all([loadTrace(), loadLogs(), loadMetrics()])
}

async function loadMetrics() {
  try {
    metrics.value = await fetchTraceMetrics(traceId)
  } catch (e) {
    // Metrics are best-effort: a disabled/unconfigured backend must not break
    // the pipeline view.
    console.error('Failed to fetch trace metrics', e)
  }
}

async function loadTrace() {
  try {
    trace.value = await fetchTrace(traceId)
    if (trace.value.root_span) normalizeChildren(trace.value.root_span)
    recomputeServices()
  } catch (e) {
    console.error('Failed to fetch trace', e)
  }
}

// The backend emits leaf spans with `children: null` (Go nil slice); guard
// against that so every node has an iterable `children` array before the tree
// walks it.
function normalizeChildren(node: SpanNode): void {
  if (!node.children) node.children = []
  for (const c of node.children) normalizeChildren(c)
}

async function loadLogs() {
  try {
    logs.value = await fetchTraceLogs(traceId)
    recomputeServices()
  } catch (e) {
    console.error('Failed to fetch logs', e)
  } finally {
    logsLoading.value = false
  }
}

function logsForSubtree(node: SpanNode): TraceLogEntry[] {
  const out: TraceLogEntry[] = []
  const walk = (n: SpanNode) => {
    const l = logsByOwner.value.get(n.span_id)
    if (l) out.push(...l)
    for (const c of n.children) walk(c)
  }
  walk(node)
  return out.sort((a, b) => Date.parse(a.timestamp) - Date.parse(b.timestamp))
}

// --- Step grouping --------------------------------------------------------
//
// The drill-down step tree lives in StepTree.vue; the shared span-visibility
// and duration helpers are in spanTree.ts.

// liveTraceDuration ticks upward while the trace is running and freezes at the
// server duration_ms once finished. Returns ms.
function liveTraceDuration(): number {
  if (trace.value.status !== 'running' || !trace.value.start_time) return trace.value.duration_ms
  const start = Date.parse(trace.value.start_time)
  if (Number.isNaN(start)) return trace.value.duration_ms
  return Math.max(0, now.value - start)
}

// --- Services -------------------------------------------------------------
//
// Dagger services started via dagger.Up() / service.Up() / --up resolve to
// host.tunnel, which emits a "tunnel started" slog record on the `up` span.
// The up span stays running for the pipeline lifetime, so we detect services
// by their log signal and surface them in a dedicated top-level summary.

// attrValue normalizes an OTLP attribute that may reach Loki either flat
// (`attributes.http_url = "..."`) or as an OTLP AnyValue object
// (`{ stringValue: "..." }` / `{ intValue: 80 }`). Returns null when absent or
// empty.
function attrValue(a: Record<string, unknown>, k: string): string | null {
  const v = a[k]
  if (v == null) return null
  if (typeof v === 'string') return v === '' ? null : v
  if (typeof v === 'object' && v !== null) {
    const o = v as Record<string, unknown>
    if (typeof o.stringValue === 'string') return o.stringValue
    // OTLP/JSON encodes int64 as a string (proto3 JSON mapping), so accept both.
    if (typeof o.intValue === 'number') return String(o.intValue)
    if (typeof o.intValue === 'string') return o.intValue === '' ? null : o.intValue
    if (typeof o.doubleValue === 'number') return String(o.doubleValue)
  }
  return null
}

// serviceLogAttrs parses a log line once and returns its attributes map when
// the line carries a service signal ("tunnel started" body or a non-empty
// http_url/https_url attribute), otherwise null. Sharing a single parse keeps
// detection and metadata extraction consistent.
function serviceLogAttrs(entry: TraceLogEntry): Record<string, unknown> | null {
  try {
    const obj = JSON.parse(entry.line) as { body?: unknown; attributes?: Record<string, unknown> }
    const attrs = obj.attributes ?? {}
    if (obj.body === SERVICE_LOG_BODY) return attrs
    if (SERVICE_URL_ATTRS.some((k) => attrValue(attrs, k) !== null)) return attrs
  } catch {
    // not JSON / malformed — not a service signal
  }
  return null
}

function isServiceLog(entry: TraceLogEntry): boolean {
  return serviceLogAttrs(entry) !== null
}

function isServiceSpan(span: SpanNode, logsBySpan: Map<string, TraceLogEntry[]>): boolean {
  const logs = logsBySpan.get(span.span_id) ?? []
  return logs.some(isServiceLog)
}

// isServiceSpanByName detects a host-tunnel service from the span identity
// alone, so a service still appears when the engine's log signal differs from
// the expected "tunnel started" body/attribute shape.
function isServiceSpanByName(span: SpanNode): boolean {
  return SERVICE_SPAN_NAMES.has(span.name)
}

function extractServiceMeta(logs: TraceLogEntry[]): {
  url: string | null
  port: number | null
  protocol: string | null
  description: string | null
} {
  let url: string | null = null
  let port: number | null = null
  let protocol: string | null = null
  let description: string | null = null

  for (const entry of logs) {
    const attrs = serviceLogAttrs(entry)
    if (attrs === null) continue
    if (url === null) url = attrValue(attrs, 'http_url') ?? attrValue(attrs, 'https_url')
    if (port === null) {
      const rawPort = attrValue(attrs, 'port')
      if (rawPort !== null) {
        const p = Number(rawPort)
        if (Number.isFinite(p)) port = p
      }
    }
    if (protocol === null) protocol = attrValue(attrs, 'protocol')
    if (description === null) description = attrValue(attrs, 'description')
  }
  return { url, port, protocol, description }
}

function computeServices(root: SpanNode | null, logsBySpan: Map<string, TraceLogEntry[]>): ServiceRow[] {
  if (!root) return []
  const rows: ServiceRow[] = []
  const walk = (n: SpanNode) => {
    if (isServiceSpan(n, logsBySpan) || isServiceSpanByName(n)) {
      const svcLogs = logsForSubtree(n)
      const meta = extractServiceMeta(svcLogs)
      rows.push({
        span: n,
        running: n.status === 'running',
        ...meta,
        logs: svcLogs,
        expanded: false,
      })
    }
    for (const c of n.children) walk(c)
  }
  walk(root)
  return rows.sort((a, b) => spanStartMs(a.span) - spanStartMs(b.span))
}

function serviceTailLogs(svc: ServiceRow): TraceLogEntry[] {
  return svc.logs.slice(-SERVICE_TAIL_LINES)
}

// Recompute the Services summary from the current trace + logs. Preserves each
// service's expanded state across refetches (SSE logs_update / the 5s poll
// otherwise collapse any service the user expanded) and keeps the live preview
// in sync when only logs change (logs_update does not carry new span data).
function recomputeServices() {
  const expanded = new Set(
    services.value.filter((s) => s.expanded).map((s) => s.span.span_id)
  )
  services.value = computeServices(trace.value.root_span, logsBySpan.value)
  for (const svc of services.value) {
    if (expanded.has(svc.span.span_id)) svc.expanded = true
  }
}

// --- Formatting -----------------------------------------------------------

function formatTime(ts: string): string {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  return d.toISOString().slice(11, 23)
}

function formatDate(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// ciLabel maps the stored ci_provider value to a human-readable label. Local
// (manual) runs store "" or "false"; a bare "true" is shown as "ci"; otherwise
// the provider name is shown verbatim.
function ciLabel(value?: string): string {
  if (!value || value === 'false') return 'manual'
  if (value === 'true') return 'ci'
  return value
}
</script>

<style scoped>
.header {
  display: flex;
  align-items: center;
  gap: 16px;
  margin-bottom: 16px;
}

.header-id {
  flex: 1;
}

.trace-id {
  display: block;
  margin-top: 2px;
  font-size: 12px;
  color: #8b949e;
}

.header-meta {
  display: flex;
  align-items: center;
  gap: 12px;
}

.duration {
  font-size: 20px;
  font-weight: 600;
  color: #f0f6fc;
}

.user-chip {
  font-size: 13px;
  font-weight: 600;
  color: #58a6ff;
  background: #1f2a3a;
  border-radius: 10px;
  padding: 2px 10px;
}

.version-chip {
  font-size: 12px;
  font-weight: 600;
  color: #8b949e;
  background: #21262d;
  border-radius: 10px;
  padding: 2px 10px;
  font-family: monospace;
}

.empty-value {
  color: #8b949e;
  font-style: italic;
}

.empty {
  padding: 24px;
  color: #8b949e;
  text-align: center;
  font-size: 13px;
}

.step,
.service {
  border-bottom: 1px solid #21262d;
}

.step-row,
.service-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 8px;
  cursor: pointer;
  border-radius: 4px;
}

.step-row:hover,
.service-row:hover {
  background: #1c2128;
}

.step-detail,
.service-detail {
  padding: 0 8px 8px 28px;
}

.chevron {
  width: 14px;
  color: #8b949e;
}

.dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex-shrink: 0;
}

.dot-success { background: #3fb950; }
.dot-failed { background: #f85149; }
.dot-running { background: #58a6ff; }
.dot-unset { background: #8b949e; }

.step-name,
.service-name {
  flex: 1;
  font-weight: 600;
  color: #f0f6fc;
  font-family: monospace;
  font-size: 13px;
}

.step-duration,
.service-duration {
  color: #c9d1d9;
  font-size: 13px;
}

.step-logs-badge,
.service-running,
.count-badge {
  font-size: 11px;
  color: #58a6ff;
  background: #1f2a3a;
  border-radius: 10px;
  padding: 1px 8px;
}

.step-hidden {
  font-size: 11px;
  color: #8b949e;
  background: #21262d;
  border-radius: 10px;
  padding: 1px 8px;
}

.subspans {
  padding: 0 0 8px 0;
}

.subspan-block {
  padding: 2px 0;
}

.subspan {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 3px 8px;
}

.subspan-name {
  flex: 1;
  color: #c9d1d9;
  font-family: monospace;
  font-size: 12px;
}

.subspan-duration {
  color: #8b949e;
  font-size: 12px;
}

.logs {
  max-height: 500px;
  overflow-y: auto;
  font-family: monospace;
  font-size: 12px;
  background: #0d1117;
  padding: 12px;
  border-radius: 4px;
}

.log-line {
  display: flex;
  gap: 10px;
  padding: 2px 0;
}

.subspan-log {
  padding-left: 24px;
}

.log-ts {
  color: #8b949e;
  flex-shrink: 0;
}

.log-msg {
  color: #f0f6fc;
  white-space: pre-wrap;
  word-break: break-word;
}

/* --- Engine metrics section --- */

.metrics-card {
  border-left: 3px solid #3fb950;
}

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 10px;
  padding: 8px 0;
}

/* --- Services section --- */

.services-card {
  border-left: 3px solid #58a6ff;
}

.count-badge {
  display: inline-block;
  margin-left: 6px;
  font-weight: 600;
  vertical-align: middle;
}

.service-port {
  color: #c9d1d9;
  font-size: 13px;
  font-family: monospace;
}

.service-url {
  color: #58a6ff;
  font-size: 12px;
  font-family: monospace;
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.service-noport {
  font-size: 12px;
  color: #8b949e;
}

.service-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  padding: 8px 0;
  font-size: 13px;
  color: #c9d1d9;
}

.service-meta strong {
  color: #8b949e;
  font-weight: 600;
}

.service-meta code {
  font-family: monospace;
  color: #f0f6fc;
}

.service-preview {
  padding: 0 8px 8px 28px;
}

.service-preview-log {
  opacity: 0.75;
}

.service-more {
  padding-top: 4px;
  font-size: 12px;
  color: #8b949e;
}
</style>
