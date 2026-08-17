<template>
  <router-link to="/services" class="status-indicator" :class="`status-${status.state}`" :title="title">
    <span class="status-dot" :class="`status-${status.state}`"></span>
    <span class="status-label">{{ label }}</span>
  </router-link>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useStatusStore } from '@/stores/status'

const status = useStatusStore()

const label = computed(() => {
  switch (status.state) {
    case 'ok':
      return 'All systems operational'
    case 'degraded':
      return 'Degraded'
    case 'down':
      return 'Service down'
    default:
      return 'Checking…'
  }
})

const title = computed(() => {
  if (status.lastError) return status.lastError
  return `${status.services.length} services monitored`
})
</script>

<style scoped>
.status-indicator {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: #8b949e;
  text-decoration: none;
  white-space: nowrap;
}
.status-indicator:hover .status-label {
  color: #f0f6fc;
}
</style>
