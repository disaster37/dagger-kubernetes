<template>
  <div>
    <h1 class="page-title">Services</h1>

    <div v-if="status.loading" class="loading-state">
      <div class="spinner"></div>
      <p>Checking platform services...</p>
    </div>

    <div v-else-if="status.lastError" class="error-banner">
      <p>{{ status.lastError }}</p>
      <button class="btn" @click="status.refresh()">Retry</button>
    </div>

    <div v-else>
      <div class="card">
        <h3>Overall</h3>
        <p style="margin-top: 8px;">
          <span class="status-dot" :class="`status-${status.state}`"></span>
          <span :class="`status-${status.state}`" style="margin-left: 8px; text-transform: capitalize;">
            {{ status.state }}
          </span>
          <span style="color: #8b949e; margin-left: 12px; font-size: 13px;">
            Last checked {{ formatTime(checkedAt) }}
          </span>
        </p>
      </div>

      <div class="card">
        <table>
          <thead>
            <tr>
              <th>Service</th>
              <th>Category</th>
              <th>State</th>
              <th>Details</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="svc in status.services" :key="svc.name">
              <td><code>{{ svc.name }}</code></td>
              <td>{{ svc.category }}</td>
              <td>
                <span v-if="!svc.configured" class="badge badge-running">not configured</span>
                <span v-else class="status-dot" :class="`status-${svc.state}`" :title="svc.state"></span>
                <span v-if="svc.configured" :class="`status-${svc.state}`" style="margin-left: 8px; text-transform: capitalize;">
                  {{ svc.state }}
                </span>
              </td>
              <td style="color: #8b949e;">{{ svc.message || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useStatusStore } from '@/stores/status'

const status = useStatusStore()

const checkedAt = computed(() => status.services[0]?.checked_at ?? '')

// App.vue owns the polling lifecycle (start/stop via the auth watcher).
// start() is idempotent: it guarantees polling is running in case this page
// is opened directly, and refresh() fetches fresh data on navigation.
onMounted(() => {
  status.start()
  status.refresh()
})

function formatTime(t: string): string {
  if (!t) return '-'
  const d = new Date(t)
  if (isNaN(d.getTime())) return '-'
  return d.toLocaleString()
}
</script>
