<template>
  <div class="step-tree">
    <div v-if="!root" class="empty">
      {{ trace.status === 'running' ? 'No steps yet — waiting for spans...' : 'No steps for this pipeline' }}
    </div>
    <template v-else>
      <div class="breadcrumb">
        <template v-for="(crumb, i) in focusPath" :key="crumb.span_id">
          <span v-if="i > 0" class="crumb-sep">▸</span>
          <button
            class="crumb"
            :class="{ 'crumb-current': i === focusPath.length - 1 }"
            @click="zoomTo(i)"
          >
            {{ crumb.name || crumb.span_id }}
          </button>
        </template>
      </div>

      <div v-if="rows.length === 0" class="empty">No steps for this level</div>
      <div
        v-for="row in rows"
        :key="row.node.span_id"
        class="step"
        :class="{ 'step-highlight': highlightedSpanID === row.node.span_id }"
        :ref="(el) => setRowRef(row.node.span_id, el as HTMLElement | null)"
      >
        <div class="step-row">
          <span
            v-if="row.hasChildren"
            class="chevron"
            @click="toggleExpanded(row.node.span_id)"
          >{{ expanded.has(row.node.span_id) ? '▾' : '▸' }}</span>
          <span v-else class="chevron chevron-empty"></span>
          <span :class="['dot', `dot-${row.node.status}`]"></span>
          <button class="step-name" @click="zoomIn(row.node)">{{ row.node.name || row.node.span_id }}</button>
          <span class="step-duration">{{ formatDuration(liveSpanDuration(row.node, now)) }}</span>
          <button
            v-if="rowCounts.get(row.node.span_id)"
            class="step-logs-badge"
            :title="`${rowCounts.get(row.node.span_id)} matching logs — click to zoom in`"
            @click.stop="zoomIn(row.node)"
          >{{ formatCount(rowCounts.get(row.node.span_id)!) }}</button>
          <span v-if="row.hiddenCount > 0" class="step-hidden">{{ row.hiddenCount }} hidden</span>
        </div>
        <div v-if="expanded.has(row.node.span_id)" class="step-detail">
          <div v-if="row.children.length === 0" class="empty">No sub-spans</div>
          <div
            v-for="child in row.children"
            :key="child.node.span_id"
            class="subspan"
            :style="{ paddingLeft: (12 + child.depth * 16) + 'px' }"
          >
            <span :class="['dot', `dot-${child.node.status}`]"></span>
            <span class="subspan-name">{{ child.node.name || child.node.span_id }}</span>
            <span class="subspan-duration">{{ formatDuration(liveSpanDuration(child.node, now)) }}</span>
          </div>
        </div>
      </div>

      <LogPanel
        :entries="entries"
        :query="query"
        :mode="mode"
        :loading="loading"
        :error="error"
        :has-more="nextCursor !== null"
        :total-shown="entries.length"
        :attribution="attribution"
        @update:query="onQueryChange"
        @update:mode="onModeChange"
        @load-more="loadMore"
        @retry="reload"
        @select-span="selectSpan"
      />
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { fetchTraceSearch } from '@/api/client'
import type { LogSearchMode, SpanNode, TraceDetail, TraceLogEntry } from '@/api/types'
import LogPanel from '@/pipeline/LogPanel.vue'
import {
  computeRowOwners,
  findSpanByID,
  flattenVisibleChildren,
  formatCount,
  formatDuration,
  liveSpanDuration,
  visibleChildren,
} from '@/pipeline/spanTree'

const props = defineProps<{
  traceId: string
  trace: TraceDetail
  refreshKey: number
}>()

const PAGE_SIZE = 500

const root = computed<SpanNode | null>(() => props.trace.root_span)
const focusPath = ref<SpanNode[]>([])
const expanded = ref<Set<string>>(new Set())

const query = ref('')
const mode = ref<LogSearchMode>('contains')
const entries = ref<TraceLogEntry[]>([])
const nextCursor = ref<number | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)

// Per-span matching-log counts from the first search page, rolled up to the
// visible rows for the count badges.
const counts = ref<Record<string, number>>({})
const highlightedSpanID = ref<string | null>(null)
let highlightTimer: number | undefined
const rowEls: Record<string, HTMLElement> = {}

// Live-ticking clock so running durations increase (mirrors PipelineView).
const now = ref<number>(Date.now())
let nowTimer: number | undefined

const focus = computed<SpanNode | null>(() => focusPath.value[focusPath.value.length - 1] ?? null)

interface Row {
  node: SpanNode
  children: { node: SpanNode; depth: number }[]
  hiddenCount: number
  hasChildren: boolean
}

const rows = computed<Row[]>(() => {
  const f = focus.value
  if (!f) return []
  return visibleChildren(f.children).map((node) => {
    const flat = flattenVisibleChildren(node)
    return {
      node,
      children: flat.spans,
      hiddenCount: flat.hidden,
      hasChildren: flat.spans.length > 0,
    }
  })
})

// rowOwners maps every span in the focus subtree to the visible row that owns
// its logs (see computeRowOwners).
const rowOwners = computed(() => (focus.value ? computeRowOwners(focus.value) : new Map<string, string>()))

// rowCounts rolls the raw per-span counts up to the visible rows.
const rowCounts = computed<Map<string, number>>(() => {
  const m = new Map<string, number>()
  for (const [spanID, n] of Object.entries(counts.value)) {
    const owner = rowOwners.value.get(spanID)
    if (owner !== undefined) m.set(owner, (m.get(owner) ?? 0) + n)
  }
  return m
})

// attribution maps every span in the focus subtree to the step badge shown on
// its log lines (label + the row to scroll to on click).
const attribution = computed<Map<string, { label: string; ownerSpanID: string }>>(() => {
  const name = new Map<string, string>()
  if (focus.value) name.set(focus.value.span_id, focus.value.name || focus.value.span_id)
  for (const r of rows.value) name.set(r.node.span_id, r.node.name || r.node.span_id)
  const out = new Map<string, { label: string; ownerSpanID: string }>()
  for (const [spanID, ownerID] of rowOwners.value) {
    out.set(spanID, { label: name.get(ownerID) ?? ownerID, ownerSpanID: ownerID })
  }
  return out
})

// Re-resolve the breadcrumb whenever the trace root changes (e.g. first load
// or a live refresh). The tree is rebuilt on every poll/SSE update, so the
// nodes held in focusPath become stale; map each crumb back to the refreshed
// tree by span_id to preserve the user's focus while showing new spans.
watch(
  root,
  (r) => {
    if (!r) return
    if (focusPath.value.length === 0) {
      focusPath.value = [r]
      return
    }
    const resolved: SpanNode[] = []
    let scope: SpanNode | null = r
    for (const crumb of focusPath.value) {
      const match = findSpanByID(scope, crumb.span_id)
      if (!match) break
      resolved.push(match)
      scope = match
    }
    focusPath.value = resolved.length > 0 ? resolved : [r]
  },
  { immediate: true }
)

// Live refresh: the parent bumps refreshKey on logs_update / the 5s poll.
// Debounce so a burst of events collapses into a single reload, preserving the
// current focus + query.
let refreshDebounce: number | undefined
watch(
  () => props.refreshKey,
  () => {
    if (refreshDebounce) window.clearTimeout(refreshDebounce)
    refreshDebounce = window.setTimeout(() => {
      void reload()
    }, 300)
  }
)

onMounted(() => {
  nowTimer = window.setInterval(() => {
    now.value = Date.now()
  }, 250)
  void reload()
})

// Clear the live-duration timer when the view unmounts.
onUnmounted(() => {
  if (nowTimer) window.clearInterval(nowTimer)
  if (refreshDebounce) window.clearTimeout(refreshDebounce)
  if (highlightTimer) window.clearTimeout(highlightTimer)
})

function zoomIn(node: SpanNode) {
  focusPath.value = [...focusPath.value, node]
  expanded.value = new Set()
  void reload()
}

function zoomTo(index: number) {
  focusPath.value = focusPath.value.slice(0, index + 1)
  expanded.value = new Set()
  void reload()
}

function toggleExpanded(spanID: string) {
  const next = new Set(expanded.value)
  if (next.has(spanID)) next.delete(spanID)
  else next.add(spanID)
  expanded.value = next
}

function setRowRef(spanID: string, el: HTMLElement | null) {
  if (el) rowEls[spanID] = el
  else delete rowEls[spanID]
}

// selectSpan scrolls to and briefly flashes the row that owns a clicked log
// line's span. The row is always visible at the current focus level.
function selectSpan(ownerSpanID: string) {
  highlightedSpanID.value = ownerSpanID
  void nextTick(() => {
    rowEls[ownerSpanID]?.scrollIntoView({ block: 'center', behavior: 'smooth' })
  })
  if (highlightTimer) window.clearTimeout(highlightTimer)
  highlightTimer = window.setTimeout(() => {
    highlightedSpanID.value = null
  }, 1500)
}

function onQueryChange(value: string) {
  query.value = value
  void reload()
}

function onModeChange(value: LogSearchMode) {
  mode.value = value
  void reload()
}

async function reload() {
  // Pre-validate a regex client-side so an invalid pattern shows an inline
  // error without a round-trip (the server would 400 anyway). The server stays
  // authoritative; this is a UX shortcut.
  entries.value = []
  nextCursor.value = null
  counts.value = {}
  if (mode.value === 'regex' && query.value && !isValidRegex(query.value)) {
    error.value = 'Invalid regex'
    return
  }
  await fetchPage(undefined)
}

function isValidRegex(pattern: string): boolean {
  try {
    new RegExp(pattern)
    return true
  } catch {
    return false
  }
}

async function loadMore() {
  if (nextCursor.value === null) return
  await fetchPage(nextCursor.value)
}

async function fetchPage(cursor: number | undefined) {
  loading.value = true
  error.value = null
  try {
    const page = await fetchTraceSearch(props.traceId, {
      span_id: focus.value?.span_id,
      q: query.value || undefined,
      mode: mode.value,
      limit: PAGE_SIZE,
      cursor,
    })
    entries.value = cursor === undefined ? page.entries : [...entries.value, ...page.entries]
    nextCursor.value = page.next ?? null
    if (cursor === undefined) counts.value = page.counts ?? {}
  } catch (e) {
    error.value = 'Failed to search logs'
    console.error('Failed to search trace logs', e)
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.breadcrumb {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
  padding: 8px 0;
  border-bottom: 1px solid #21262d;
}

.crumb {
  background: none;
  border: none;
  color: #58a6ff;
  font-family: monospace;
  font-size: 13px;
  cursor: pointer;
  padding: 2px 4px;
}

.crumb-current {
  color: #f0f6fc;
  font-weight: 600;
  cursor: default;
}

.crumb-sep {
  color: #8b949e;
}

.step {
  border-bottom: 1px solid #21262d;
}

.step-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 8px;
  border-radius: 4px;
}

.step-row:hover {
  background: #1c2128;
}

.step-highlight .step-row {
  background: #1f2a3a;
  box-shadow: inset 0 0 0 1px #58a6ff;
}

.step-logs-badge {
  font-size: 11px;
  color: #58a6ff;
  background: #1f2a3a;
  border: none;
  border-radius: 10px;
  padding: 1px 8px;
  cursor: pointer;
}

.step-logs-badge:hover {
  background: #26374d;
}

.chevron {
  width: 14px;
  color: #8b949e;
  cursor: pointer;
}

.chevron-empty {
  cursor: default;
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

.step-name {
  flex: 1;
  text-align: left;
  background: none;
  border: none;
  font-weight: 600;
  color: #f0f6fc;
  font-family: monospace;
  font-size: 13px;
  cursor: pointer;
  padding: 0;
}

.step-name:hover {
  color: #58a6ff;
}

.step-duration {
  color: #c9d1d9;
  font-size: 13px;
}

.step-hidden {
  font-size: 11px;
  color: #8b949e;
  background: #21262d;
  border-radius: 10px;
  padding: 1px 8px;
}

.step-detail {
  padding: 0 8px 8px 28px;
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

.empty {
  padding: 24px;
  color: #8b949e;
  text-align: center;
  font-size: 13px;
}
</style>