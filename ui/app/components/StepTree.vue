<template>
  <div class="step-tree">
    <div v-if="!root" class="empty">
      {{ trace.status === 'running' ? 'No steps yet — waiting for spans...' : 'No steps for this pipeline' }}
    </div>
    <template v-else>
      <UBreadcrumb :items="breadcrumbItems" separator-icon="i-lucide-chevron-right" class="border-b border-default py-2">
        <template #item="{ item, index }">
          <UButton
            variant="link"
            color="neutral"
            :class="{ 'font-semibold': index === focusPath.length - 1 }"
            @click="zoomTo(index)"
          >
            {{ item.label }}
          </UButton>
        </template>
      </UBreadcrumb>

      <div class="flex items-center gap-2.5 px-2 pt-2.5 pb-1">
        <span :class="['dot', `dot-${focus?.status ?? 'unset'}`]"></span>
        <span class="focus-name">{{ focus?.name || focus?.span_id }}</span>
        <span class="focus-duration">{{ focus ? formatDuration(liveSpanDuration(focus, now)) : '' }}</span>
        <span class="focus-sub">logs for this step and its descendants</span>
        <UButton
          variant="ghost"
          color="neutral"
          size="xs"
          square
          :icon="panelOpen ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
          @click="panelOpen = !panelOpen"
        />
      </div>

      <LogPanel
        v-if="panelOpen"
        :key="focus?.span_id ?? 'none'"
        :entries="entries"
        :query="query"
        :mode="mode"
        :loading="loading"
        :error="error"
        :has-more="nextCursor !== null"
        :total-shown="entries.length"
        :attribution="attribution"
        :new-count="newCount"
        @update:query="onQueryChange"
        @update:mode="onModeChange"
        @load-more="loadMore"
        @retry="resetAndLoad"
        @select-span="selectSpan"
        @pinned-change="onPinnedChange"
        @resume="onResume"
      />

      <div v-if="rows.length === 0" class="empty">No steps for this level</div>
      <div
        v-for="row in rows"
        :key="row.node.span_id"
        class="step"
        :class="{ 'step-highlight': highlightedSpanID === row.node.span_id }"
        :ref="(el) => setRowRef(row.node.span_id, el as HTMLElement | null)"
      >
        <div class="step-row">
          <UButton
            v-if="row.hasChildren"
            variant="ghost"
            color="neutral"
            size="xs"
            square
            :icon="expanded.has(row.node.span_id) ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
            @click="toggleExpanded(row.node.span_id)"
          />
          <span v-else class="w-3.5"></span>
          <span :class="['dot', `dot-${row.node.status}`]"></span>
          <UButton variant="link" color="neutral" class="step-name" @click="zoomIn(row.node)">
            {{ row.node.name || row.node.span_id }}
          </UButton>
          <span class="step-duration">{{ formatDuration(liveSpanDuration(row.node, now)) }}</span>
          <UBadge
            v-if="rowCounts.get(row.node.span_id)"
            color="info"
            variant="soft"
            class="cursor-pointer"
            :title="`${rowCounts.get(row.node.span_id)} matching logs — click to zoom in`"
            @click.stop="zoomIn(row.node)"
          >{{ formatCount(rowCounts.get(row.node.span_id)!) }}</UBadge>
          <UBadge v-if="row.hiddenCount > 0" color="neutral" variant="soft">{{ row.hiddenCount }} hidden</UBadge>
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
    </template>
  </div>
</template>

<script setup lang="ts">
import { fetchTraceSearch } from '~/api/client'
import type { LogSearchMode, SpanNode, TraceDetail, TraceLogEntry } from '~/api/types'
import {
  computeRowOwners,
  entryKey,
  findSpanByID,
  flattenVisibleChildren,
  formatCount,
  formatDuration,
  liveSpanDuration,
  maxEntryTimestampNanos,
  visibleChildren,
} from '~/utils/spanTree'

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

// Dedicated focused-step panel state.
const panelOpen = ref(true)
const pinned = ref(true)
const newCount = ref(0)
const seen = new Set<string>() // entryKey dedupe set for merge/append

// Monotonic request generation. resetAndLoad() bumps it; every in-flight
// fetch/refresh captures the value it started under and discards its response
// when the generation moved on (focus/query/mode changed mid-flight), so a
// stale response can never append another step's logs into the current panel.
let loadSeq = 0

const searchActive = computed(() => query.value !== '')
const paused = computed(() => !pinned.value || searchActive.value)

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

const breadcrumbItems = computed(() =>
  focusPath.value.map((crumb) => ({ label: crumb.name || crumb.span_id }))
)

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
// Debounce so a burst of events collapses into a single non-disruptive refresh,
// preserving the current focus + query and the user's scroll position.
let refreshDebounce: number | undefined
watch(
  () => props.refreshKey,
  () => {
    if (refreshDebounce) window.clearTimeout(refreshDebounce)
    refreshDebounce = window.setTimeout(() => {
      void refresh()
    }, 300)
  }
)

onMounted(() => {
  nowTimer = window.setInterval(() => {
    now.value = Date.now()
  }, 250)
  void resetAndLoad()
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
  panelOpen.value = true
  void resetAndLoad()
}

function zoomTo(index: number) {
  focusPath.value = focusPath.value.slice(0, index + 1)
  expanded.value = new Set()
  panelOpen.value = true
  void resetAndLoad()
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
  void resetAndLoad()
}

function onModeChange(value: LogSearchMode) {
  mode.value = value
  void resetAndLoad()
}

function onPinnedChange(p: boolean) {
  pinned.value = p
  if (p && !searchActive.value) newCount.value = 0
}

function onResume() {
  pinned.value = true
  newCount.value = 0
}

// resetAndLoad — full reset + page-1 fetch (navigation / retry only).
async function resetAndLoad() {
  const seq = ++loadSeq
  // Pre-validate a regex client-side so an invalid pattern shows an inline
  // error without a round-trip (the server would 400 anyway). The server stays
  // authoritative; this is a UX shortcut.
  entries.value = []
  nextCursor.value = null
  counts.value = {}
  seen.clear()
  newCount.value = 0
  if (mode.value === 'regex' && query.value && !isValidRegex(query.value)) {
    error.value = 'Invalid regex'
    loading.value = false
    return
  }
  await fetchPage(undefined, seq)
}

// refresh — non-disruptive live update on refreshKey bump. Fetches only the
// strictly-newer tail (forward cursor) and appends it, so the list is never
// cleared and the scroll position never jumps.
async function refresh() {
  const seq = loadSeq
  if (entries.value.length === 0) {
    await fetchPage(undefined, seq) // first load
    return
  }
  const cursor = maxEntryTimestampNanos(entries.value) + 1
  loading.value = true
  error.value = null
  try {
    const page = await searchPage(cursor)
    if (seq !== loadSeq) return // focus/query changed mid-flight; discard
    const appended = mergeEntries(page.entries)
    // refresh() continues from the max loaded timestamp, so the returned page is
    // the next contiguous page and page.next is the correct forward continuation
    // (it can only advance, never point backwards). Keep it in sync so "Load
    // more" does not re-fetch an already-merged page.
    nextCursor.value = page.next ?? null
    if (paused.value && appended > 0) newCount.value += appended
  } catch (e) {
    if (seq !== loadSeq) return
    // Non-disruptive: keep the current list on a failed background refresh.
    console.error('Failed to refresh logs', e)
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// mergeEntries appends + dedupes, returning the number of entries actually added.
function mergeEntries(newEntries: TraceLogEntry[]): number {
  let added = 0
  for (const e of newEntries) {
    const k = entryKey(e)
    if (seen.has(k)) continue
    seen.add(k)
    entries.value.push(e)
    added++
  }
  return added
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

// searchPage issues the subtree-scoped search for the current focus/query/mode.
function searchPage(cursor: number | undefined) {
  return fetchTraceSearch(props.traceId, {
    span_id: focus.value?.span_id,
    q: query.value || undefined,
    mode: mode.value,
    limit: PAGE_SIZE,
    cursor,
  })
}

async function fetchPage(cursor: number | undefined, seq: number = loadSeq) {
  loading.value = true
  error.value = null
  try {
    const page = await searchPage(cursor)
    if (seq !== loadSeq) return // superseded by a newer navigation/query
    if (cursor === undefined) {
      entries.value = page.entries
      seen.clear()
      for (const e of page.entries) seen.add(entryKey(e))
      counts.value = page.counts ?? {}
    } else {
      mergeEntries(page.entries)
    }
    nextCursor.value = page.next ?? null
  } catch (e) {
    if (seq !== loadSeq) return
    error.value = 'Failed to search logs'
    console.error('Failed to search trace logs', e)
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}
</script>
