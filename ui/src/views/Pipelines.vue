<template>
  <div>
    <h1 class="page-title">Pipelines</h1>

    <div v-if="auth.isAdmin" class="card" style="margin-bottom: 16px;">
      <label style="font-size: 14px; margin-right: 8px;">Filter by group:</label>
      <select v-model="groupFilter" @change="load" style="padding: 4px 10px; background: #0d1117; border: 1px solid #30363d; border-radius: 6px; color: #c9d1d9;">
        <option value="">All</option>
        <option value="unassigned">Unassigned</option>
        <option v-for="g in groups" :key="g.id" :value="g.id">{{ g.name }}</option>
      </select>
    </div>

    <div class="card">
      <table>
        <thead>
          <tr>
            <th>Trace ID</th>
            <th>Status</th>
            <th>Version</th>
            <th>Duration</th>
            <th>CI</th>
            <th>Group</th>
            <th>Project</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="trace in traces" :key="trace.trace_id">
            <td><code>{{ trace.trace_id }}</code></td>
            <td>
              <span :class="['badge', `badge-${trace.status}`]">{{ trace.status }}</span>
            </td>
            <td>{{ trace.version || '-' }}</td>
            <td>{{ formatDuration(trace.duration_ms) }}</td>
            <td>{{ trace.ci_provider || '-' }}</td>
            <td>{{ trace.group_name || '-' }}</td>
            <td>{{ trace.project_name || trace.ci_repo || '-' }}</td>
            <td>
              <router-link :to="`/pipelines/${trace.trace_id}`" class="btn">View DAG</router-link>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="traces.length === 0" style="padding: 24px; text-align: center; color: #8b949e;">
        No pipelines yet. Run <code>dagger call</code> with DAGGER_CLOUD_URL set to this server.
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { fetchTraces, listGroups } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import type { TraceRow, Group } from '@/api/types'

const auth = useAuthStore()
const traces = ref<TraceRow[]>([])
const groups = ref<Group[]>([])
const groupFilter = ref('')

onMounted(async () => {
  if (auth.isAdmin) {
    try {
      groups.value = await listGroups()
    } catch { /* ignore */ }
  }
  await load()
})

async function load() {
  try {
    traces.value = await fetchTraces(groupFilter.value || undefined)
  } catch (e) {
    console.error('Failed to fetch traces', e)
  }
}

function formatDuration(ms: number): string {
  if (!ms) return '-'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${(s % 60).toFixed(0)}s`
}
</script>
