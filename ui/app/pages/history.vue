<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">History</h1>

    <div v-if="loading" class="py-12 text-center text-muted">
      <UIcon name="i-lucide-loader-circle" class="mx-auto size-7 animate-spin text-primary" />
      <p class="mt-3">Loading history stats...</p>
    </div>

    <UAlert v-else-if="error" color="error" variant="soft" :description="error">
      <template #actions>
        <UButton color="error" variant="soft" @click="load">Retry</UButton>
      </template>
    </UAlert>

    <div v-else>
      <UCard class="mb-4">
        <h3 class="font-semibold">Pipeline History</h3>
        <UTable :data="historyRows" :columns="historyColumns" />
      </UCard>

      <UCard class="mb-4">
        <h3 class="font-semibold">History auto-purge (GC)</h3>
        <p class="mt-2 text-[13px] text-muted">
          Auto-purge is
          <UBadge :color="info.gc.enabled ? 'success' : 'error'" variant="soft">{{ info.gc.enabled ? 'ON' : 'OFF' }}</UBadge>.
          {{ gcSummary }}
        </p>
        <UTable class="mt-3" :data="gcRows" :columns="gcColumns" />
        <div v-if="info.gc.last_run_summary" class="mt-3">
          <p class="text-[13px] text-muted">
            Last run purged {{ info.gc.last_run_summary.purged_traces }} trace(s),
            deleted {{ info.gc.last_run_summary.logs_deleted }} log streams and
            {{ info.gc.last_run_summary.metrics_deleted }} metric series,
            skipped {{ info.gc.last_run_summary.skipped_running }} running,
            telemetry errors {{ info.gc.last_run_summary.telemetry_errors }},
            errors {{ info.gc.last_run_summary.errors }}.
          </p>
        </div>
      </UCard>

      <UCard v-if="auth.isAdmin">
        <h3 class="font-semibold">Admin</h3>
        <p class="my-2 text-[13px] text-muted">
          Purge removes trace metadata plus its Loki logs and VictoriaMetrics
          series. Running traces are protected by purge-all and the GC sweeper.
        </p>
        <div class="flex flex-wrap items-center gap-2">
          <UInput v-model="traceId" type="text" placeholder="trace ID (hex)" class="min-w-[220px] flex-1" />
          <UButton color="error" variant="soft" :disabled="!traceId" @click="askPurgeTrace">Purge trace</UButton>
        </div>
        <UButton class="mt-3" color="error" variant="soft" @click="askPurgeAll">
          Purge all history older than max_age
        </UButton>
        <UAlert v-if="purgeMessage" class="mt-3" color="neutral" variant="soft" :description="purgeMessage" />
      </UCard>
    </div>

    <UModal v-model:open="confirmOpen" :title="confirmTitle">
      <template #body>
        <p>{{ confirmMessage }}</p>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="neutral" variant="outline" @click="confirmOpen = false">Cancel</UButton>
          <UButton color="error" @click="runConfirmed">Confirm</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { fetchHistoryInfo, purgeHistory, purgeAllHistory } from '~/api/client'
import { useAuthStore } from '~/stores/auth'
import type { HistoryInfo } from '~/api/types'

const auth = useAuthStore()

const info = ref<HistoryInfo>(emptyHistory())
const loading = ref(true)
const error = ref<string | null>(null)
const purgeMessage = ref('')
const traceId = ref('')

const confirmOpen = ref(false)
const confirmTitle = ref('')
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const historyColumns = [
  { id: 'label', accessorKey: 'label', header: '' },
  { id: 'value', accessorKey: 'value', header: '' },
]
const historyRows = computed(() => [
  { label: 'Traces', value: String(info.value.trace_count) },
  { label: 'Oldest update', value: info.value.oldest_updated_at ? formatTime(info.value.oldest_updated_at) : '-' },
  { label: 'Collected', value: formatTime(info.value.collected_at) },
])

const gcColumns = [
  { id: 'label', accessorKey: 'label', header: '' },
  { id: 'value', accessorKey: 'value', header: '' },
]
const gcRows = computed(() => {
  const rows = [
    { label: 'Max age', value: info.value.gc.max_age },
    { label: 'Schedule', value: info.value.gc.schedule },
  ]
  if (info.value.gc.last_run_at) rows.push({ label: 'Last run', value: formatTime(info.value.gc.last_run_at) })
  if (info.value.gc.next_run_at) rows.push({ label: 'Next run (est.)', value: formatTime(info.value.gc.next_run_at) })
  return rows
})

function emptyHistory(): HistoryInfo {
  return {
    trace_count: 0,
    collected_at: '',
    gc: { enabled: false, max_age: '', schedule: '' },
  }
}

const gcSummary = computed(() => {
  const gc = info.value.gc
  if (!gc.enabled) return 'Configured via history.gc.* (disabled by default).'
  return `Traces older than ${gc.max_age} are purged every ${gc.schedule}.`
})

async function load(): Promise<void> {
  loading.value = true
  error.value = null
  try {
    info.value = await fetchHistoryInfo()
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load history info'
  } finally {
    loading.value = false
  }
}

function askPurgeTrace(): void {
  const id = traceId.value.trim()
  if (!id) return
  confirmTitle.value = 'Purge trace'
  confirmMessage.value = `Purge history for trace ${id}? This also deletes its Loki logs and VictoriaMetrics series.`
  confirmAction = () => purgeTrace(id)
  confirmOpen.value = true
}

function askPurgeAll(): void {
  confirmTitle.value = 'Purge all history'
  confirmMessage.value = 'Purge ALL history older than max_age? This removes trace metadata, Loki logs, and VictoriaMetrics series.'
  confirmAction = () => purgeAll()
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

async function purgeTrace(id: string): Promise<void> {
  purgeMessage.value = ''
  try {
    const res = await purgeHistory({ trace_id: id })
    purgeMessage.value = res.already_purged > 0
      ? 'Trace metadata was already absent (telemetry delete attempted anyway).'
      : `Purged ${res.purged_traces} trace(s), deleted ${res.logs_deleted} log streams and ${res.metrics_deleted} metric series.`
    traceId.value = ''
    await load()
  } catch (e: any) {
    purgeMessage.value = e.response?.data?.message || 'Purge failed'
  }
}

async function purgeAll(): Promise<void> {
  purgeMessage.value = ''
  try {
    const res = await purgeAllHistory()
    purgeMessage.value = `Purged ${res.purged_traces} trace(s), ${res.already_purged} already absent, deleted ${res.logs_deleted} log streams and ${res.metrics_deleted} metric series.`
    await load()
  } catch (e: any) {
    purgeMessage.value = e.response?.data?.message || 'Purge failed'
  }
}

function formatTime(t: string): string {
  if (!t) return '-'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '-'
  return d.toLocaleString()
}

onMounted(() => {
  void load()
})
</script>
