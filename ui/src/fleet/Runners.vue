<template>
  <div>
    <h1 class="page-title">Runner Fleet</h1>
    <div v-for="version in fleet" :key="version.version" class="card">
      <h3>{{ version.version }}</h3>
      <p style="color: #8b949e; font-size: 13px;">
        {{ version.readyReplicas }}/{{ version.replicas }} ready
      </p>
      <table style="margin-top: 12px;">
        <thead>
          <tr>
            <th>Pod</th>
            <th>Ordinal</th>
            <th>Status</th>
            <th>Sessions</th>
            <th>Uptime</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="ordinal in version.ordinals" :key="ordinal.name">
            <td><code>{{ ordinal.name }}</code></td>
            <td>{{ ordinal.ordinal }}</td>
            <td>
              <span :class="['badge', ordinal.ready ? 'badge-success' : 'badge-failed']">
                {{ ordinal.ready ? 'Ready' : 'Down' }}
              </span>
            </td>
            <td>{{ ordinal.pinnedSessions }}</td>
            <td>{{ ordinal.startedAt ? formatTime(ordinal.startedAt) : '-' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-if="fleet.length === 0" style="padding: 24px; text-align: center; color: #8b949e;">
      No engine fleets deployed. Run a Dagger pipeline to auto-provision engines.
    </p>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { fetchFleetInfo } from '@/api/client'

const fleet = ref<any[]>([])

onMounted(async () => {
  try {
    fleet.value = await fetchFleetInfo()
  } catch (e) {
    console.error('Failed to fetch fleet info', e)
  }
})

function formatTime(t: string): string {
  const d = new Date(t)
  const now = new Date()
  const diff = (now.getTime() - d.getTime()) / 1000
  if (diff < 60) return `${Math.floor(diff)}s`
  if (diff < 3600) return `${Math.floor(diff / 60)}m`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h`
  return `${Math.floor(diff / 86400)}d`
}
</script>
