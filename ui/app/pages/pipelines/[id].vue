<template>
  <div>
    <div class="mb-4 flex items-center gap-4">
      <UButton to="/pipelines" variant="ghost" color="neutral" icon="i-lucide-arrow-left">Back</UButton>
      <div class="flex-1">
        <h1 class="m-0 text-2xl font-semibold">Pipeline {{ shortId }}</h1>
        <code class="block text-xs text-muted">{{ traceId }}</code>
      </div>
      <div class="flex items-center gap-3">
        <UBadge :color="statusColor(trace.status)" variant="soft">{{ trace.status }}</UBadge>
        <UBadge v-if="trace.version" color="neutral" variant="soft">{{ trace.version }}</UBadge>
        <UBadge color="primary" variant="soft" :title="trace.user_id ? `user_id: ${trace.user_id}` : ''">
          {{ trace.username ? `@${trace.username}` : 'anonymous' }}
        </UBadge>
        <span class="text-xl font-semibold">{{ formatDuration(liveTraceDuration()) }}</span>
      </div>
    </div>

    <UCard v-if="metrics" class="mb-4 border-l-4 border-l-success">
      <h3 class="font-semibold">Engine metrics</h3>
      <div v-if="metrics.series.length === 0" class="empty">No metrics</div>
      <div v-else class="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-2.5 py-2">
        <MetricChart
          v-for="s in metrics.series"
          :key="s.name"
          :series="s"
          :unit="s.unit"
        />
      </div>
    </UCard>

    <UCard class="mb-4 border-l-4 border-l-primary">
      <h3 class="font-semibold">
        Services
        <UBadge v-if="services.length" color="info" variant="soft" class="ml-1">{{ services.length }}</UBadge>
      </h3>
      <div v-if="services.length === 0" class="empty">No services</div>
      <div v-for="svc in services" :key="svc.span.span_id" class="border-b border-default">
        <UCollapsible v-model:open="svc.expanded">
          <div class="service-row">
            <UIcon :name="svc.expanded ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'" class="text-muted" />
            <span :class="['dot', svc.running ? 'dot-running' : `dot-${svc.span.status}`]"></span>
            <span class="service-name">{{ svc.span.name }}</span>
            <UBadge v-if="svc.running" color="info" variant="soft">running</UBadge>
            <span v-if="svc.port != null" class="service-port">:{{ svc.port }}</span>
            <span v-if="svc.url" class="service-url">{{ svc.url }}</span>
            <span v-else-if="!svc.running && svc.port == null" class="service-noport">no port</span>
            <span class="service-duration">{{ formatDuration(liveSpanDuration(svc.span, now)) }}</span>
          </div>
          <template #content>
            <div class="service-detail">
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
          </template>
        </UCollapsible>
        <!-- Collapsed preview: last N lines, always visible -->
        <div v-if="!svc.expanded" class="service-preview">
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
    </UCard>

    <UCard class="mb-4">
      <h3 class="font-semibold">Steps</h3>
      <StepTree :trace-id="traceId" :trace="trace" :refresh-key="searchRefreshKey" />
    </UCard>

    <UCard>
      <h3 class="font-semibold">Details</h3>
      <UTable :data="detailRows" :columns="detailColumns">
        <template #value-cell="{ row }">
          <UBadge v-if="row.original.badge" :color="statusColor(row.original.badge)" variant="soft">
            {{ row.original.badge }}
          </UBadge>
          <span v-else-if="row.original.muted" class="italic text-muted">{{ row.original.value }}</span>
          <span v-else>{{ row.original.value }}</span>
        </template>
      </UTable>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { fetchTrace, fetchTraceLogs, fetchTraceMetrics, connectLiveTrace } from '~/api/client'
import type { ServiceInfo, SpanNode, TraceDetail, TraceLogEntry, TraceMetrics } from '~/api/types'
import { formatDuration, isInternalSpan, isTransparentSpan, liveSpanDuration, logText, spanStartMs } from '~/utils/spanTree'

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

const detailColumns = [
  { id: 'label', accessorKey: 'label', header: '' },
  { id: 'value', accessorKey: 'value', header: '' },
]
const detailRows = computed(() => [
  { label: 'User', value: trace.value.username || 'anonymous', muted: !trace.value.username },
  { label: 'Status', value: trace.value.status, badge: trace.value.status },
  { label: 'Duration', value: formatDuration(liveTraceDuration()) },
  { label: 'Started', value: formatDate(trace.value.start_time) },
  { label: 'Version', value: trace.value.version || '-' },
  { label: 'CI Provider', value: ciLabel(trace.value.ci_provider) },
  { label: 'Repository', value: trace.value.ci_repo || '-' },
])

function statusColor(status: string): 'success' | 'error' | 'info' | 'neutral' {
  if (status === 'success') return 'success'
  if (status === 'failed') return 'error'
  if (status === 'running') return 'info'
  return 'neutral'
}

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
