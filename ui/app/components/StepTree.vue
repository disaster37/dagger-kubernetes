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

      <div v-if="focusExec" class="exec-block">
        <div v-if="focusCommand" class="exec-command" title="Click to copy" @click="copyCommand">
          <code>{{ focusCommand }}</code>
        </div>
        <div class="exec-badges">
          <UBadge v-if="focusExec.cwd" color="neutral" variant="soft">cwd: {{ focusExec.cwd }}</UBadge>
          <UBadge v-if="focusExec.user" color="neutral" variant="soft">user: {{ focusExec.user }}</UBadge>
          <UBadge
            v-if="focusExec.exit_code !== null && focusExec.exit_code !== undefined"
            :color="exitBadgeColor(focusExec.exit_code)"
            variant="soft"
          >
            exit {{ focusExec.exit_code }}
          </UBadge>
          <UBadge
            v-else-if="focus?.status === 'running'"
            color="neutral"
            variant="soft"
          >
            running
          </UBadge>
          <UBadge v-if="focusExec.kind" color="neutral" variant="soft">{{ focusExec.kind }}</UBadge>
        </div>
        <details v-if="focusExec.env && focusExec.env.length" class="exec-env">
          <summary>Environment ({{ focusExec.env.length }})</summary>
          <div v-for="e in focusExec.env" :key="e.name" class="exec-env-row">
            <span class="exec-env-name">{{ e.name }}</span>
            <span class="exec-env-value">{{ e.is_secret ? '<secret>' : (e.value ?? '') }}</span>
          </div>
        </details>
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
            @click.stop="toggleExpanded(row.node.span_id)"
          />
          <span v-else class="w-3.5"></span>
          <span :class="['dot', `dot-${row.node.status}`]"></span>
          <UButton variant="link" color="neutral" class="step-name" @click="zoomIn(row.node)">
            {{ row.node.name || row.node.span_id }}
          </UButton>
          <span v-if="inlineDetail(row.node)" class="step-exec" :title="inlineDetailTitle(row.node)">
            <code>{{ inlineDetail(row.node) }}</code>
          </span>
          <UBadge
            v-if="exitLabel(row.node)"
            :color="exitBadgeColor(row.node.exec?.exit_code)"
            variant="soft"
            class="step-exit"
          >{{ exitLabel(row.node) }}</UBadge>
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
            class="subspan subspan-clickable"
            :style="{ paddingLeft: (12 + child.depth * 16) + 'px' }"
            :title="`Zoom into ${child.node.name || child.node.span_id}`"
            @click="zoomInSubspan(row.node, child.node)"
          >
            <span :class="['dot', `dot-${child.node.status}`]"></span>
            <span class="subspan-name">{{ child.node.name || child.node.span_id }}</span>
            <span v-if="inlineDetail(child.node)" class="subspan-exec" :title="inlineDetailTitle(child.node)">
              <code>{{ inlineDetail(child.node) }}</code>
            </span>
            <UBadge
              v-if="exitLabel(child.node)"
              :color="exitBadgeColor(child.node.exec?.exit_code)"
              variant="soft"
            >{{ exitLabel(child.node) }}</UBadge>
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
  exitCodeTone,
  findSpanByID,
  findSpanPathByID,
  flattenVisibleChildren,
  formatArgv,
  formatCount,
  formatDuration,
  formatEnvInline,
  hasMeaningfulExec,
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
// Spans the user explicitly collapsed: auto-expand must never re-open these
// across live refreshes.
const userCollapsed = ref<Set<string>>(new Set())

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
const focusExec = computed(() => focus.value?.exec ?? null)
// focusCommand is the formatted command line; empty for exec-like spans that
// carry no argv (e.g. a publish operation), which hides the copy affordance.
const focusCommand = computed(() => (focusExec.value ? formatArgv(focusExec.value) : ''))

// exitBadgeColor maps the exec exit-code tone to a UBadge color.
function exitBadgeColor(code: number | null | undefined): 'success' | 'error' | 'neutral' {
  const tone = exitCodeTone(code)
  return tone === 'muted' ? 'neutral' : tone
}

// exitLabel renders the exit badge text for a span: an "running" label only
// when the span is genuinely still running with no exit code; a finished span
// with no exit code shows nothing (the badge is hidden by the caller).
function exitLabel(node: SpanNode): string {
  const code = node.exec?.exit_code
  if (code === null || code === undefined) return node.status === 'running' ? 'running' : ''
  return `exit ${code}`
}

// inlineDetail renders the one-line call summary for a row/sub-span: the
// command line when present, else an env summary, else a cwd/user hint. Returns
// '' when the span has nothing worth surfacing inline.
function inlineDetail(node: SpanNode): string {
  const exec = node.exec
  if (!exec) return ''
  if (exec.args && exec.args.length > 0) return formatArgv(exec)
  if (exec.command) return formatArgv(exec)
  if (exec.env && exec.env.length > 0) return formatEnvInline(exec.env)
  if (exec.cwd) return `cwd: ${exec.cwd}`
  if (exec.user) return `user: ${exec.user}`
  return ''
}

// inlineDetailTitle is the full (untruncated) tooltip for an inline summary.
function inlineDetailTitle(node: SpanNode): string {
  const exec = node.exec
  if (!exec) return ''
  const parts: string[] = []
  const cmd = inlineDetail(node)
  if (cmd) parts.push(cmd)
  if (exec.cwd) parts.push(`cwd: ${exec.cwd}`)
  if (exec.user) parts.push(`user: ${exec.user}`)
  if (exec.env && exec.env.length > 0 && !(exec.args && exec.args.length > 0)) {
    parts.push(formatEnvInline(exec.env))
  }
  return parts.join('  ')
}

// copyCommand copies the focused step's command line to the clipboard.
async function copyCommand() {
  if (!focusCommand.value) return
  try {
    await navigator.clipboard.writeText(focusCommand.value)
  } catch {
    // Clipboard unavailable (insecure context / denied); ignore.
  }
}

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

// Auto-expand rows that carry a meaningful exec whenever the focused level
// changes (navigation, first load, live refresh). Preserves manual collapse via
// userCollapsed.
watch(
  () => focus.value?.span_id,
  () => {
    void nextTick(autoExpandExecRows)
  }
)
watch(
  () => rows.value,
  () => {
    autoExpandExecRows()
  }
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
  userCollapsed.value = new Set()
  panelOpen.value = true
  void resetAndLoad()
}

function zoomTo(index: number) {
  focusPath.value = focusPath.value.slice(0, index + 1)
  expanded.value = new Set()
  userCollapsed.value = new Set()
  panelOpen.value = true
  void resetAndLoad()
}

// zoomInSubspan focuses a sub-span (e.g. Container.withExec under E2E.envVar)
// by setting the breadcrumb to its full ancestor path.
function zoomInSubspan(parent: SpanNode, child: SpanNode) {
  const base = focusPath.value[focusPath.value.length - 1] === parent
    ? focusPath.value
    : [...focusPath.value, parent]
  const pathToChild = findSpanPathByID(parent, child.span_id)
  focusPath.value = pathToChild ? [...base, ...pathToChild.slice(1)] : [...base, child]
  expanded.value = new Set()
  userCollapsed.value = new Set()
  panelOpen.value = true
  void resetAndLoad()
}

function toggleExpanded(spanID: string) {
  const next = new Set(expanded.value)
  const collapsed = new Set(userCollapsed.value)
  if (next.has(spanID)) {
    next.delete(spanID)
    collapsed.add(spanID)
  } else {
    next.add(spanID)
    collapsed.delete(spanID)
  }
  expanded.value = next
  userCollapsed.value = collapsed
}

// autoExpandExecRows opens top-level rows that contain a meaningful exec
// sub-span (so `Container.withExec sh -c …` shows without a click), leaving
// rows the user has manually collapsed closed. Rows with more than 20 children
// stay closed to avoid a wall of sub-spans.
function autoExpandExecRows() {
  const next = new Set(expanded.value)
  for (const row of rows.value) {
    if (userCollapsed.value.has(row.node.span_id)) continue
    if (!row.hasChildren || row.children.length > 20) continue
    if (next.has(row.node.span_id)) continue
    if (row.children.some((c) => hasMeaningfulExec(c.node))) {
      next.add(row.node.span_id)
    }
  }
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
