<template>
  <div class="mt-2">
    <div class="flex items-center gap-2.5 py-2">
      <UInput
        class="min-w-[160px] flex-1"
        type="text"
        placeholder="Search logs…"
        :model-value="localQuery"
        @update:model-value="onQueryInput"
      />
      <div class="mode-toggle">
        <button
          :class="['mode-btn', mode === 'contains' ? 'mode-active' : '']"
          @click="$emit('update:mode', 'contains')"
        >
          contains
        </button>
        <button
          :class="['mode-btn', mode === 'regex' ? 'mode-active' : '']"
          @click="$emit('update:mode', 'regex')"
        >
          regex
        </button>
      </div>
      <span class="match-count">{{ totalShown }} shown</span>
      <UButton v-if="hasMore" size="xs" color="neutral" variant="outline" :loading="loading" @click="$emit('load-more')">
        Load more
      </UButton>
    </div>

    <UAlert v-if="error" class="mb-2" color="error" variant="soft" :description="error">
      <template #actions>
        <UButton color="error" variant="soft" size="xs" @click="$emit('retry')">Retry</UButton>
      </template>
    </UAlert>

    <button v-if="newCount > 0" class="new-logs" @click="onResume">
      ↓ {{ newCount }} new logs
    </button>

    <div ref="logEl" v-follow-logs class="logs" @scroll="onScroll">
      <template v-for="(entry, i) in rendered" :key="`log-${i}`">
        <div class="log-line">
          <span class="log-ts">{{ formatTime(entry.timestamp) }}</span>
          <span
            v-if="entry.badge"
            class="log-step"
            :title="entry.badge.label"
            @click="$emit('select-span', entry.badge.ownerSpanID)"
          >{{ entry.badge.label }}</span>
          <span v-else class="log-step log-step-unattributed">unattributed</span>
          <span class="log-msg">
            <template v-for="(seg, j) in entry.segments" :key="`seg-${j}`">
              <mark v-if="seg.match">{{ seg.text }}</mark>
              <template v-else>{{ seg.text }}</template>
            </template>
          </span>
        </div>
      </template>
      <p v-if="loading && entries.length === 0" class="empty">Loading logs…</p>
      <p v-else-if="entries.length === 0" class="empty">
        {{ query ? 'No matching logs' : 'No logs for this level' }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import type { LogSearchMode, TraceLogEntry } from '~/api/types'
import { followLogsPin, followLogsPinned } from '~/utils/followLogs'
import { highlightSegments, logText } from '~/utils/spanTree'

const props = defineProps<{
  entries: TraceLogEntry[]
  query: string
  mode: LogSearchMode
  loading: boolean
  error: string | null
  hasMore: boolean
  totalShown: number
  attribution: Map<string, { label: string; ownerSpanID: string }>
  newCount: number
}>()

const emit = defineEmits<{
  (e: 'update:query', value: string): void
  (e: 'update:mode', value: LogSearchMode): void
  (e: 'load-more'): void
  (e: 'retry'): void
  (e: 'select-span', ownerSpanID: string): void
  (e: 'pinned-change', pinned: boolean): void
  (e: 'resume'): void
}>()

const logEl = ref<HTMLElement | null>(null)

function onScroll() {
  // Defer one frame so the directive's own scroll handler has updated `pinned`
  // before we read it (independent of listener registration order).
  requestAnimationFrame(() => emit('pinned-change', followLogsPinned(logEl.value)))
}

function onResume() {
  followLogsPin(logEl.value)
  emit('pinned-change', true)
  emit('resume')
}

onMounted(() => emit('pinned-change', followLogsPinned(logEl.value)))

// Local mirror of the query so typing stays responsive; the debounced emit
// drives the actual server request.
const localQuery = ref(props.query)
watch(
  () => props.query,
  (v) => {
    if (v !== localQuery.value) localQuery.value = v
  }
)

let debounce: number | undefined
function onQueryInput(value: string | number) {
  const v = String(value)
  localQuery.value = v
  if (debounce) window.clearTimeout(debounce)
  debounce = window.setTimeout(() => emit('update:query', v), 300)
}

interface RenderedLine {
  timestamp: string
  segments: { text: string; match: boolean }[]
  badge: { label: string; ownerSpanID: string } | null
}

const rendered = computed<RenderedLine[]>(() => {
  const out: RenderedLine[] = []
  for (const entry of props.entries) {
    const text = logText(entry.line)
    if (text === null) continue
    out.push({
      timestamp: entry.timestamp,
      segments: highlightSegments(text, props.query, props.mode),
      badge: (entry.span_id && props.attribution.get(entry.span_id)) || null,
    })
  }
  return out
})

function formatTime(ts: string): string {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  return d.toISOString().slice(11, 23)
}
</script>
