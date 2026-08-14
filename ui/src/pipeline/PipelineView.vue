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
        <span class="duration">{{ formatDuration(trace.duration_ms) }}</span>
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

    <div class="card">
      <h3>Steps</h3>
      <div v-if="traceLoading" class="empty">Loading steps...</div>
      <div v-else-if="traceError" class="empty">
        <p>Failed to load steps.</p>
        <button class="btn" @click="loadTrace()">Retry</button>
      </div>
      <div v-else-if="steps.length === 0" class="empty">
        {{ trace.status === 'running' ? 'No steps yet — waiting for spans...' : 'No steps for this pipeline' }}
      </div>
      <template v-else>
        <div v-for="step in steps" :key="step.span.span_id" class="step">
          <div class="step-row" @click="step.expanded = !step.expanded">
            <span class="chevron">{{ step.expanded ? '▾' : '▸' }}</span>
            <span :class="['dot', `dot-${step.span.status}`]"></span>
            <span class="step-name">{{ step.span.name }}</span>
            <span class="step-duration">{{ formatDuration(step.durationMs) }}</span>
            <span v-if="stepLogCount(step) > 0" class="step-logs-badge">{{ stepLogCount(step) }} logs</span>
            <span v-if="step.hiddenCount > 0" class="step-hidden">{{ step.hiddenCount }} hidden</span>
          </div>
          <div v-if="step.expanded" class="step-detail">
            <template v-for="(log, i) in logsForSpan(step.span)" :key="`s-${i}`">
              <div v-if="logText(log.line) !== null" class="log-line">
                <span class="log-ts">{{ formatTime(log.timestamp) }}</span>
                <span class="log-msg">{{ logText(log.line) }}</span>
              </div>
            </template>
            <div class="subspans">
              <div
                v-for="s in step.subSpans"
                :key="s.node.span_id"
                class="subspan-block"
                :style="{ paddingLeft: (12 + s.depth * 16) + 'px' }"
              >
                <div class="subspan">
                  <span :class="['dot', `dot-${s.node.status}`]"></span>
                  <span class="subspan-name">{{ s.node.name }}</span>
                  <span class="subspan-duration">{{ formatDuration(s.node.duration_ms) }}</span>
                </div>
                <template v-for="(log, i) in logsForSubtree(s.node)" :key="`ss-${i}`">
                  <div v-if="logText(log.line) !== null" class="log-line subspan-log">
                    <span class="log-ts">{{ formatTime(log.timestamp) }}</span>
                    <span class="log-msg">{{ logText(log.line) }}</span>
                  </div>
                </template>
              </div>
              <div v-if="step.subSpans.length === 0" class="empty">No sub-spans</div>
            </div>
            <div v-if="stepLogCount(step) === 0" class="empty">No logs for this step</div>
          </div>
        </div>
      </template>
    </div>

    <details class="card" :open="unmatchedLogs.length > 0 && logs.length > 0 && unmatchedLogs.length === logs.length">
      <summary>Unmatched / general logs ({{ unmatchedLogs.length }})</summary>
      <div class="logs">
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
          <tr><td>Status</td><td><span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span></td></tr>
          <tr><td>Duration</td><td>{{ formatDuration(trace.duration_ms) }}</td></tr>
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
import { fetchTrace, fetchTraceLogs, connectLiveTrace } from '@/api/client'
import type { ServiceInfo, SpanNode, TraceDetail, TraceLogEntry } from '@/api/types'

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
const traceLoading = ref(true)
const traceError = ref(false)
const steps = ref<Step[]>([])

// Tunable detection constants — isolated so they can be adjusted after a real-trace
// inspection without touching logic.
const SERVICE_LOG_BODY = 'tunnel started'
const SERVICE_URL_ATTRS = ['http_url', 'https_url'] as const

const SERVICE_TAIL_LINES = 50

interface ServiceRow extends ServiceInfo {
  expanded: boolean
}

const services = ref<ServiceRow[]>([])

let pollTimer: number | undefined
let eventSource: EventSource | undefined
let traceDebounce: number | undefined
let logsDebounce: number | undefined

interface DisplaySpan {
  node: SpanNode
  depth: number
}

interface Step {
  span: SpanNode
  durationMs: number
  subSpans: DisplaySpan[]
  hiddenCount: number
  expanded: boolean
}

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

const allSpanIDs = computed<Set<string>>(() => {
  const ids = new Set<string>()
  const walk = (n: SpanNode | null) => {
    if (!n) return
    ids.add(n.span_id)
    for (const c of n.children) walk(c)
  }
  walk(trace.value.root_span)
  return ids
})

const unmatchedLogs = computed<TraceLogEntry[]>(() =>
  logs.value.filter((l) => !l.span_id || !allSpanIDs.value.has(l.span_id))
)

onMounted(async () => {
  await loadAll()
  pollTimer = window.setInterval(async () => {
    if (trace.value.status === 'success' || trace.value.status === 'failed') return
    await loadAll()
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
  if (pollTimer) window.clearInterval(pollTimer)
  if (traceDebounce) window.clearTimeout(traceDebounce)
  if (logsDebounce) window.clearTimeout(logsDebounce)
  if (eventSource) eventSource.close()
})

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
  }, 300)
}

async function loadAll() {
  await Promise.all([loadTrace(), loadLogs()])
}

async function loadTrace() {
  traceLoading.value = true
  traceError.value = false
  try {
    trace.value = await fetchTrace(traceId)
    if (trace.value.root_span) normalizeChildren(trace.value.root_span)
    steps.value = computeSteps(trace.value.root_span)
    recomputeServices()
  } catch (e) {
    traceError.value = true
    console.error('Failed to fetch trace', e)
  } finally {
    traceLoading.value = false
  }
}

// The backend emits leaf spans with `children: null` (Go nil slice); guard
// against that so every node has an iterable `children` array before the step
// grouping walks the tree.
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

function logsForSpan(node: SpanNode): TraceLogEntry[] {
  const l = logsBySpan.value.get(node.span_id) ?? []
  return l.slice().sort((a, b) => Date.parse(a.timestamp) - Date.parse(b.timestamp))
}

function logsForSubtree(node: SpanNode): TraceLogEntry[] {
  const out: TraceLogEntry[] = []
  const walk = (n: SpanNode) => {
    const l = logsBySpan.value.get(n.span_id)
    if (l) out.push(...l)
    for (const c of n.children) walk(c)
  }
  walk(node)
  return out.sort((a, b) => Date.parse(a.timestamp) - Date.parse(b.timestamp))
}

function stepLogCount(step: Step): number {
  return logsForSubtree(step.span).length
}

// --- Step grouping --------------------------------------------------------
//
// Dagger emits OpenTelemetry spans carrying `dagger.io/ui.*` attributes that
// describe how the trace tree should be presented:
//   - `dagger.io/ui.passthrough`  : hide this span, promote its children
//   - `dagger.io/ui.internal`     : implementation detail, hide it
//   - `dagger.io/ui.encapsulated` : child of an encapsulated span, hide it
//   - `dagger.io/ui.encapsulate`  : collapse this span (kept as one row)
//
// We surface the root span's direct children as high-level "steps" (after
// applying passthrough promotion), then collapse every sub-span underneath a
// step. Expanding a step reveals its sub-spans with passthrough/internal spans
// filtered out and counted in "hidden".

function attrBool(n: SpanNode, key: string): boolean {
  return n.attributes?.[key] === 'true'
}

function topLevelSpans(nodes: SpanNode[]): SpanNode[] {
  const out: SpanNode[] = []
  for (const n of nodes) {
    if (attrBool(n, 'dagger.io/ui.passthrough')) {
      out.push(...topLevelSpans(n.children))
    } else {
      out.push(n)
    }
  }
  return out
}

function computeSteps(root: SpanNode | null): Step[] {
  if (!root) return []
  const top = topLevelSpans(root.children).slice().sort((a, b) => spanStartMs(a) - spanStartMs(b))
  return top.map((span) => {
    const visible = flattenVisibleChildren(span)
    return {
      span,
      durationMs: subtreeDuration(span),
      subSpans: visible.spans,
      hiddenCount: visible.hidden,
      expanded: false,
    }
  })
}

function flattenVisibleChildren(span: SpanNode): { spans: DisplaySpan[]; hidden: number } {
  const result = { spans: [] as DisplaySpan[], hidden: 0 }
  for (const child of span.children) {
    const r = flattenVisible(child, 0)
    result.spans.push(...r.spans)
    result.hidden += r.hidden
  }
  return result
}

function flattenVisible(node: SpanNode, depth: number): { spans: DisplaySpan[]; hidden: number } {
  if (attrBool(node, 'dagger.io/ui.passthrough')) {
    const result = { spans: [] as DisplaySpan[], hidden: 0 }
    for (const child of node.children) {
      const r = flattenVisible(child, depth)
      result.spans.push(...r.spans)
      result.hidden += r.hidden
    }
    return result
  }
  if (attrBool(node, 'dagger.io/ui.internal') || attrBool(node, 'dagger.io/ui.encapsulated')) {
    const result = { spans: [] as DisplaySpan[], hidden: 1 }
    for (const child of node.children) {
      const r = flattenVisible(child, depth)
      result.spans.push(...r.spans)
      result.hidden += r.hidden
    }
    return result
  }
  const result = { spans: [{ node, depth } as DisplaySpan], hidden: 0 }
  for (const child of node.children) {
    const r = flattenVisible(child, depth + 1)
    result.spans.push(...r.spans)
    result.hidden += r.hidden
  }
  return result
}

function spanStartMs(n: SpanNode): number {
  const t = Date.parse(n.start_time)
  return Number.isNaN(t) ? 0 : t
}

function spanEndMs(n: SpanNode): number {
  const start = spanStartMs(n)
  const duration = n.duration_ms || (n.duration_ns ? n.duration_ns / 1e6 : 0)
  return start ? start + duration : 0
}

// subtreeDuration measures the wall-clock time spanned by a node and all of
// its descendants (some Dagger spans, e.g. "connect", have no end time but
// their children do).
function subtreeDuration(node: SpanNode): number {
  let minStart = Infinity
  let maxEnd = 0
  const walk = (n: SpanNode) => {
    const s = spanStartMs(n)
    if (s) minStart = Math.min(minStart, s)
    const e = spanEndMs(n)
    if (e) maxEnd = Math.max(maxEnd, e)
    for (const c of n.children) walk(c)
  }
  walk(node)
  if (minStart === Infinity || maxEnd <= minStart) return node.duration_ms || 0
  return Math.round(maxEnd - minStart)
}

// --- Services -------------------------------------------------------------
//
// Dagger services started via dagger.Up() / service.Up() / --up resolve to
// host.tunnel, which emits a "tunnel started" slog record on the `up` span.
// The up span stays running for the pipeline lifetime, so we detect services
// by their log signal and surface them in a dedicated top-level summary.

// serviceLogAttrs parses a log line once and returns its attributes map when
// the line carries a service signal ("tunnel started" body or a non-empty
// http_url/https_url attribute), otherwise null. Sharing a single parse keeps
// detection and metadata extraction consistent.
function serviceLogAttrs(entry: TraceLogEntry): Record<string, unknown> | null {
  try {
    const obj = JSON.parse(entry.line) as { body?: unknown; attributes?: Record<string, unknown> }
    const attrs = obj.attributes ?? {}
    if (obj.body === SERVICE_LOG_BODY) return attrs
    if (SERVICE_URL_ATTRS.some((k) => attrs[k] != null && attrs[k] !== '')) return attrs
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

  const firstNonEmpty = (value: unknown): string | null =>
    value != null && String(value) !== '' ? String(value) : null

  for (const entry of logs) {
    const attrs = serviceLogAttrs(entry)
    if (attrs === null) continue
    if (url === null) url = firstNonEmpty(attrs.http_url) ?? firstNonEmpty(attrs.https_url)
    if (port === null && attrs.port != null && attrs.port !== '') {
      const p = Number(attrs.port)
      if (Number.isFinite(p)) port = p
    }
    if (protocol === null) protocol = firstNonEmpty(attrs.protocol)
    if (description === null) description = firstNonEmpty(attrs.description)
  }
  return { url, port, protocol, description }
}

function computeServices(root: SpanNode | null, logsBySpan: Map<string, TraceLogEntry[]>): ServiceRow[] {
  if (!root) return []
  const rows: ServiceRow[] = []
  const walk = (n: SpanNode) => {
    if (isServiceSpan(n, logsBySpan)) {
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

function formatDuration(ms: number | null | undefined): string {
  if (!ms || ms <= 0) return '-'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${(s % 60).toFixed(0)}s`
}

function formatTime(ts: string): string {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  return d.toISOString().slice(11, 23)
}

// ciLabel maps the stored ci_provider value to a human-readable label. Local
// (manual) runs store "" or "false"; a bare "true" is shown as "ci"; otherwise
// the provider name is shown verbatim.
function ciLabel(value?: string): string {
  if (!value || value === 'false') return 'manual'
  if (value === 'true') return 'ci'
  return value
}

interface LogJSON {
  body?: unknown
  attributes?: { stdio?: { eof?: boolean } }
}

// Loki stores each log record as a JSON object; extract the human-readable
// `body` field when present and strip ANSI colour escapes. Returns null for
// stdio marker records that carry no content so the renderer can skip them.
function logText(line: string): string | null {
  let text = line
  let obj: LogJSON | null = null
  try {
    obj = JSON.parse(line)
    if (obj && typeof obj.body === 'string') text = obj.body
  } catch {
    // not JSON; render the raw line
  }

  // stdio markers: the Dagger engine emits empty/"Stdout:"/"Stderr:" records
  // that carry no content (e.g. {"body":"Stdout:\n","attributes":{"stdio.stream":2}}
  // and {"body":"","attributes":{"stdio.eof":true}}). Skip them and strip the
  // stream prefixes from records that do carry content.
  if (obj && typeof obj.body === 'string') {
    const body = obj.body
    if (body.trim() === '' && obj.attributes?.stdio?.eof === true) return null
    if (body !== body.replace(/^(Stdout|Stderr):\s*\n/, '')) {
      text = body.replace(/^(Stdout|Stderr):\s*\n/, '')
      if (text.trim() === '') return null
    }
  }

  text = text.replace(/\u001b\[[0-9;]*m/g, '')
  text = text.replace(/\n+$/, '')

  // The Dagger engine serialises verbose progress payloads (module schemas,
  // telemetry graphs) as JSON-quoted base64 protobufs in the log body. Those
  // are binary, not human-readable, so decode base64 that is valid UTF-8 text
  // and collapse binary payloads to a placeholder instead of rendering base64.
  let candidate = text.trim()
  if (candidate.length > 2 && candidate.startsWith('"') && candidate.endsWith('"')) {
    try {
      const inner = JSON.parse(candidate)
      if (typeof inner === 'string') candidate = inner
    } catch {
      // keep the quoted candidate as-is
    }
  }
  if (isBase64(candidate)) {
    const decoded = decodeBase64UTF8(candidate)
    return decoded !== null ? decoded : '[binary log data]'
  }
  return text
}

function isBase64(s: string): boolean {
  if (s.length < 8 || s.length % 4 !== 0) return false
  return /^[A-Za-z0-9+/]+={0,2}$/.test(s)
}

function decodeBase64UTF8(s: string): string | null {
  try {
    const bin = atob(s)
    const bytes = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
    return new TextDecoder('utf-8', { fatal: true }).decode(bytes)
  } catch {
    return null
  }
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
