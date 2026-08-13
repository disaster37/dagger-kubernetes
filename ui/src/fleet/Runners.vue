<template>
  <div>
    <h1 class="page-title">Runner Fleet</h1>

    <div v-if="loading" class="loading-state">
      <div class="spinner"></div>
      <p>Loading fleet...</p>
    </div>

    <div v-else-if="error" class="error-banner">
      <p>{{ error }}</p>
      <button class="btn" @click="load">Retry</button>
    </div>

    <div v-else-if="fleet.length === 0" class="empty-state">
      <p>No engine fleets deployed. Run a Dagger pipeline to auto-provision engines.</p>
    </div>

    <div v-else>
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
              <td>{{ formatTime(ordinal.startedAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { fetchFleetInfo } from '@/api/client'
import type { FleetInfo } from '@/api/types'

const REFRESH_MS = 10_000

const fleet = ref<FleetInfo[]>([])
const loading = ref(true)
const error = ref<string | null>(null)
let timer: number | undefined
let loadingInFlight = false

async function load(): Promise<void> {
  if (loadingInFlight) return
  loadingInFlight = true
  try {
    fleet.value = await fetchFleetInfo()
    error.value = null
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load fleet'
  } finally {
    loading.value = false
    loadingInFlight = false
  }
}

onMounted(() => {
  void load()
  timer = window.setInterval(() => void load(), REFRESH_MS)
})

onUnmounted(() => {
  if (timer !== undefined) window.clearInterval(timer)
})

function formatTime(t: string): string {
  if (!t) return '-'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '-'
  if (d.getFullYear() < 2000) return '-'
  const diff = (Date.now() - d.getTime()) / 1000
  if (diff < 0) return '-'
  if (diff < 60) return `${Math.floor(diff)}s`
  if (diff < 3600) return `${Math.floor(diff / 60)}m`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h`
  return `${Math.floor(diff / 86400)}d`
}
</script>
