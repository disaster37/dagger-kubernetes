<template>
  <div>
    <h1 class="page-title">Pipelines</h1>
    <div class="card">
      <table>
        <thead>
          <tr>
            <th>Trace ID</th>
            <th>Status</th>
            <th>Version</th>
            <th>Duration</th>
            <th>CI</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="trace in traces" :key="trace.trace_id">
            <td>
              <code>{{ trace.trace_id }}</code>
            </td>
            <td>
              <span :class="['badge', `badge-${trace.status}`]">
                {{ trace.status }}
              </span>
            </td>
            <td>{{ trace.version }}</td>
            <td>{{ formatDuration(trace.duration_ms) }}</td>
            <td>{{ trace.ci_provider || '-' }}</td>
            <td>
              <router-link :to="`/pipelines/${trace.trace_id}`" class="btn">
                View DAG
              </router-link>
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
import { fetchTraces } from '@/api/client'

const traces = ref<any[]>([])

onMounted(async () => {
  try {
    traces.value = await fetchTraces()
  } catch (e) {
    console.error('Failed to fetch traces', e)
  }
})

function formatDuration(ms: number): string {
  if (!ms) return '-'
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)}s`
  const m = Math.floor(s / 60)
  return `${m}m ${(s % 60).toFixed(0)}s`
}
</script>
