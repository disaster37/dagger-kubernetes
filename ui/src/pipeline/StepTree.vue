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
      <div v-for="row in rows" :key="row.node.span_id" class="step">
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
        @update:query="onQueryChange"
        @update:mode="onModeChange"
        @load-more="loadMore"
        @retry="reload"
      />
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { fetchTraceSearch } from '@/api/client'
import type { LogSearchMode, SpanNode, TraceDetail, TraceLogEntry } from '@/api/types'
import LogPanel from '@/pipeline/LogPanel.vue'
import { flattenVisible, formatDuration, liveSpanDuration, visibleChildren } from '@/pipeline/spanTree'

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
    const flat = flattenVisible(node, 0)
    return {
      node,
      children: flat.spans,
      hiddenCount: flat.hidden,
      hasChildren: flat.spans.length > 0,
    }
  })
})

// Reset the breadcrumb whenever the trace root changes (e.g. first load).
watch(
  root,
  (r) => {
    if (r && focusPath.value.length === 0) focusPath.value = [r]
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

function onQueryChange(value: string) {
  query.value = value
  void reload()
}

function onModeChange(value: LogSearchMode) {
  mode.value = value
  void reload()
}

async function reload() {
  entries.value = []
  nextCursor.value = null
  await fetchPage(undefined)
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