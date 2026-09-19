<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Pipelines</h1>

    <UCard v-if="auth.isAdmin" class="mb-4">
      <div class="flex items-center gap-2">
        <label class="text-sm">Filter by group:</label>
        <USelect v-model="groupFilter" :items="groupOptions" @change="load" />
      </div>
    </UCard>

    <UCard>
      <UTable :data="traces" :columns="columns" :empty="emptyText">
        <template #pipeline-cell="{ row }">
          <div class="pipeline-identity">{{ identity(row.original) }}</div>
          <code class="pipeline-trace-id">{{ row.original.trace_id }}</code>
        </template>
        <template #status-cell="{ row }">
          <UBadge :color="statusColor(row.original.status)" variant="soft">{{ row.original.status }}</UBadge>
        </template>
        <template #duration-cell="{ row }">
          {{ formatDuration(liveRowDuration(row.original)) }}
        </template>
        <template #date-cell="{ row }">
          {{ formatDate(row.original.started_at) }}
        </template>
        <template #ci-cell="{ row }">
          {{ ciLabel(row.original.ci_provider) }}
        </template>
        <template #group-cell="{ row }">
          {{ row.original.group_name || '-' }}
        </template>
        <template #actions-cell="{ row }">
          <UButton size="xs" :to="`/pipelines/${row.original.trace_id}`">View DAG</UButton>
        </template>
      </UTable>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import { fetchTraces, listGroups } from '~/api/client'
import { useAuthStore } from '~/stores/auth'
import type { TraceRow, Group } from '~/api/types'

const auth = useAuthStore()
const traces = ref<TraceRow[]>([])
const groups = ref<Group[]>([])
const groupFilter = ref('')

const columns = [
  { id: 'pipeline', accessorKey: 'trace_id', header: 'Pipeline' },
  { id: 'status', accessorKey: 'status', header: 'Status' },
  { id: 'version', accessorKey: 'version', header: 'Version' },
  { id: 'duration', accessorKey: 'duration_ms', header: 'Duration' },
  { id: 'date', accessorKey: 'started_at', header: 'Date' },
  { id: 'ci', accessorKey: 'ci_provider', header: 'CI' },
  { id: 'group', accessorKey: 'group_name', header: 'Group' },
  { id: 'actions', header: '' },
]

const groupOptions = computed(() => [
  { label: 'All', value: '' },
  { label: 'Unassigned', value: 'unassigned' },
  ...groups.value.map((g) => ({ label: g.name, value: g.id })),
])

const emptyText = 'No pipelines yet. Run dagger call with DAGGER_CLOUD_URL set to this server.'

// Live-ticking clock (1s cadence is enough for the list) used by
// liveRowDuration; a running row recomputes elapsed from now − started_at.
const now = ref<number>(Date.now())
let nowTimer: number | undefined
let pollTimer: number | undefined
let disposed = false

onMounted(async () => {
  if (auth.isAdmin) {
    try {
      groups.value = await listGroups()
    } catch { /* ignore */ }
  }
  await load()

  // The component may have unmounted while load() was in flight; do not
  // register timers/listeners on a dead component.
  if (disposed) return

  nowTimer = window.setInterval(() => {
    now.value = Date.now()
  }, 1000)

  // While any row is running, re-fetch every 10s to detect status transitions
  // and pick up the final server duration_ms. Skipped when every row is done.
  pollTimer = window.setInterval(() => {
    if (traces.value.some((t) => t.status === 'running')) {
      void load()
    }
  }, 10000)

  document.addEventListener('visibilitychange', onVisibilityChange)
})

onUnmounted(() => {
  disposed = true
  if (nowTimer) window.clearInterval(nowTimer)
  if (pollTimer) window.clearInterval(pollTimer)
  document.removeEventListener('visibilitychange', onVisibilityChange)
})

// Background tabs throttle setInterval to ≤1/s; recompute `now` from Date.now()
// the moment the tab becomes visible again so the display never shows a stale
// frozen elapsed after a long absence.
function onVisibilityChange() {
  if (document.visibilityState === 'visible') now.value = Date.now()
}

async function load() {
  try {
    traces.value = await fetchTraces(groupFilter.value || undefined)
  } catch (e) {
    console.error('Failed to fetch traces', e)
  }
}

function statusColor(status: string): 'success' | 'error' | 'info' | 'neutral' {
  if (status === 'success') return 'success'
  if (status === 'failed') return 'error'
  if (status === 'running') return 'info'
  return 'neutral'
}

// liveRowDuration ticks upward for a running trace (now − started_at) and
// freezes at the server duration_ms once finished. Returns ms.
function liveRowDuration(trace: TraceRow): number {
  if (trace.status !== 'running') return trace.duration_ms
  const start = Date.parse(trace.started_at)
  if (!Number.isFinite(start) || start <= 0) return trace.duration_ms
  return Math.max(0, now.value - start)
}

function formatDuration(ms: number): string {
  if (!ms) return '-'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${(s % 60).toFixed(0)}s`
}

function formatDate(iso: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

// Derive a short, human-friendly name from the repo/project identity:
//   https://github.com/org/repo.git -> org/repo
//   github.com/org/repo            -> org/repo
//   /path/to/folder                -> folder
//   nogit-proj                     -> nogit-proj
function shortDisplayName(v: string): string {
  let s = (v || '').trim()
  if (!s) return ''
  s = s.replace(/^[a-z][a-z0-9+.-]*:\/\//i, '') // strip scheme (https://, git://, ...)
  const scp = s.match(/^[^@\s]+@[^:\s]+:(.+)$/)  // scp-style git@host:org/repo
  if (scp) s = scp[1] ?? ''
  s = s.replace(/\.git$/, '')                    // strip .git suffix
  s = s.replace(/[@#].*$/, '')                   // strip @ref / #ref
  s = s.replace(/\/+$/, '')                      // strip trailing slashes
  const seg = s.split('/').filter(Boolean)
  if (seg.length === 0) return ''
  // host/org/repo -> org/repo
  if (seg.length >= 3 && seg[0]?.includes('.')) {
    return `${seg[seg.length - 2]}/${seg[seg.length - 1]}`
  }
  // filesystem path or bare name -> basename
  return seg[seg.length - 1] ?? ''
}

function identity(trace: TraceRow): string {
  const name = shortDisplayName(trace.ci_repo || trace.project_name)
  const user = trace.username ? `@${trace.username}` : ''
  if (user && name) return `${user} · ${name}`
  if (user) return user
  if (name) return name
  return '-'
}

// ciLabel maps the stored ci_provider value to a human-readable label. Local
// (manual) runs store "" or "false" (and the JSON field is omitted via
// omitempty, so it may also be undefined at runtime); a bare "true" is shown as
// "ci"; otherwise the provider name is shown verbatim. Mirrors PipelineView.vue.
function ciLabel(ci?: string): string {
  if (!ci || ci === 'false') return 'manual'
  if (ci === 'true') return 'ci'
  return ci
}
</script>
