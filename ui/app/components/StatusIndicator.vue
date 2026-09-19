<template>
  <UButton to="/services" variant="ghost" color="neutral" size="sm" :title="title">
    <span class="status-dot" :class="`status-${status.state}`"></span>
    <UBadge :color="badgeColor" variant="soft" size="sm">{{ label }}</UBadge>
  </UButton>
</template>

<script setup lang="ts">
import { useStatusStore } from '~/stores/status'

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

const badgeColor = computed(() => {
  switch (status.state) {
    case 'ok':
      return 'success'
    case 'degraded':
      return 'warning'
    case 'down':
      return 'error'
    default:
      return 'neutral'
  }
})

const title = computed(() => {
  if (status.lastError) return status.lastError
  return `${status.services.length} services monitored`
})
</script>
