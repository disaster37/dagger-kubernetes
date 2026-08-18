<template>
  <div>
    <h1 class="page-title">History</h1>

    <div v-if="loading" class="loading-state">
      <div class="spinner"></div>
      <p>Loading history stats...</p>
    </div>

    <div v-else-if="error" class="error-banner">
      <p>{{ error }}</p>
      <button class="btn" @click="load">Retry</button>
    </div>

    <div v-else>
      <div class="card">
        <h3>Pipeline History</h3>
        <table>
          <tbody>
            <tr><td>Traces</td><td>{{ info.trace_count }}</td></tr>
            <tr><td>Oldest update</td><td>{{ info.oldest_updated_at ? formatTime(info.oldest_updated_at) : '-' }}</td></tr>
            <tr><td>Collected</td><td>{{ formatTime(info.collected_at) }}</td></tr>
          </tbody>
        </table>
      </div>

      <div class="card">
        <h3>History auto-purge (GC)</h3>
        <p style="margin-top: 8px; font-size: 13px; color: #8b949e;">
          Auto-purge is <strong :class="info.gc.enabled ? 'status-ok' : 'status-down'">{{ info.gc.enabled ? 'ON' : 'OFF' }}</strong>.
          {{ gcSummary }}
        </p>
        <table style="margin-top: 12px;">
          <tbody>
            <tr><td>Max age</td><td>{{ info.gc.max_age }}</td></tr>
            <tr><td>Schedule</td><td>{{ info.gc.schedule }}</td></tr>
            <tr v-if="info.gc.last_run_at"><td>Last run</td><td>{{ formatTime(info.gc.last_run_at) }}</td></tr>
            <tr v-if="info.gc.next_run_at"><td>Next run (est.)</td><td>{{ formatTime(info.gc.next_run_at) }}</td></tr>
          </tbody>
        </table>
        <div v-if="info.gc.last_run_summary" style="margin-top: 12px;">
          <p style="font-size: 13px; color: #8b949e;">
            Last run purged {{ info.gc.last_run_summary.purged_traces }} trace(s),
            deleted {{ info.gc.last_run_summary.logs_deleted }} log streams and
            {{ info.gc.last_run_summary.metrics_deleted }} metric series,
            skipped {{ info.gc.last_run_summary.skipped_running }} running,
            telemetry errors {{ info.gc.last_run_summary.telemetry_errors }},
            errors {{ info.gc.last_run_summary.errors }}.
          </p>
        </div>
      </div>

      <div v-if="auth.isAdmin" class="card">
        <h3>Admin</h3>
        <p style="font-size: 13px; color: #8b949e; margin: 8px 0;">
          Purge removes trace metadata plus its Loki logs and VictoriaMetrics
          series. Running traces are protected by purge-all and the GC sweeper.
        </p>
        <div style="display: flex; gap: 8px; align-items: center; flex-wrap: wrap;">
          <input
            v-model="traceId"
            type="text"
            placeholder="trace ID (hex)"
            style="flex: 1; min-width: 220px;"
          />
          <button class="btn btn-danger" :disabled="!traceId" @click="purgeTrace">Purge trace</button>
        </div>
        <button class="btn btn-danger" style="margin-top: 12px;" @click="purgeAll">
          Purge all history older than max_age
        </button>
        <p v-if="purgeMessage" style="margin-top: 12px; font-size: 13px;">{{ purgeMessage }}</p>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { fetchHistoryInfo, purgeHistory, purgeAllHistory } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import type { HistoryInfo } from '@/api/types'

const auth = useAuthStore()

const info = ref<HistoryInfo>(emptyHistory())
const loading = ref(true)
const error = ref<string | null>(null)
const purgeMessage = ref('')
const traceId = ref('')

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

async function purgeTrace(): Promise<void> {
  const id = traceId.value.trim()
  if (!id) return
  if (!confirm(`Purge history for trace ${id}? This also deletes its Loki logs and VictoriaMetrics series.`)) return
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
  if (!confirm('Purge ALL history older than max_age? This removes trace metadata, Loki logs, and VictoriaMetrics series.')) return
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
