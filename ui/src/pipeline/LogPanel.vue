<template>
  <div class="log-panel">
    <div class="toolbar">
      <input
        class="search-input"
        type="text"
        placeholder="Search logs…"
        :value="query"
        @input="onQueryInput"
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
      <button v-if="hasMore" class="btn load-more" :disabled="loading" @click="$emit('load-more')">
        Load more
      </button>
    </div>

    <div v-if="error" class="panel-error">
      <span>{{ error }}</span>
      <button class="btn" @click="$emit('retry')">Retry</button>
    </div>

    <div v-follow-logs class="logs">
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
import { computed, ref, watch } from 'vue'
import type { LogSearchMode, TraceLogEntry } from '@/api/types'
import { highlightSegments, logText } from '@/pipeline/spanTree'

const props = defineProps<{
  entries: TraceLogEntry[]
  query: string
  mode: LogSearchMode
  loading: boolean
  error: string | null
  hasMore: boolean
  totalShown: number
  attribution: Map<string, { label: string; ownerSpanID: string }>
}>()

const emit = defineEmits<{
  (e: 'update:query', value: string): void
  (e: 'update:mode', value: LogSearchMode): void
  (e: 'load-more'): void
  (e: 'retry'): void
  (e: 'select-span', ownerSpanID: string): void
}>()

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
function onQueryInput(event: Event) {
  const value = (event.target as HTMLInputElement).value
  localQuery.value = value
  if (debounce) window.clearTimeout(debounce)
  debounce = window.setTimeout(() => emit('update:query', value), 300)
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

<style scoped>
.log-panel {
  margin-top: 8px;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 0;
}

.search-input {
  flex: 1;
  min-width: 160px;
  background: #0d1117;
  border: 1px solid #30363d;
  border-radius: 4px;
  color: #f0f6fc;
  padding: 6px 10px;
  font-size: 13px;
}

.mode-toggle {
  display: flex;
  border: 1px solid #30363d;
  border-radius: 4px;
  overflow: hidden;
}

.mode-btn {
  background: #161b22;
  border: none;
  color: #8b949e;
  padding: 6px 10px;
  font-size: 12px;
  cursor: pointer;
}

.mode-btn.mode-active {
  background: #1f2a3a;
  color: #58a6ff;
}

.match-count {
  font-size: 12px;
  color: #8b949e;
  white-space: nowrap;
}

.load-more {
  font-size: 12px;
  padding: 4px 10px;
}

.panel-error {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px;
  margin-bottom: 8px;
  background: #2d1618;
  border: 1px solid #f85149;
  border-radius: 4px;
  color: #f85149;
  font-size: 13px;
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

.log-ts {
  color: #8b949e;
  flex-shrink: 0;
}

.log-step {
  flex-shrink: 0;
  max-width: 180px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: monospace;
  font-size: 11px;
  color: #58a6ff;
  background: #1f2a3a;
  border-radius: 10px;
  padding: 1px 8px;
  cursor: pointer;
}

.log-step:hover {
  background: #26374d;
}

.log-step-unattributed {
  color: #8b949e;
  background: #21262d;
  cursor: default;
}

.log-msg {
  color: #f0f6fc;
  white-space: pre-wrap;
  word-break: break-word;
}

mark {
  background: #9e6a03;
  color: #f0f6fc;
  border-radius: 2px;
}

.empty {
  padding: 24px;
  color: #8b949e;
  text-align: center;
  font-size: 13px;
}
</style>