<template>
  <div class="mb-4 flex flex-wrap gap-2">
    <UButton
      v-for="item in items"
      :key="item.to"
      :to="item.to"
      :color="isActive(item.to) ? 'primary' : 'neutral'"
      :variant="isActive(item.to) ? 'soft' : 'ghost'"
      size="sm"
    >
      {{ item.label }}
    </UButton>
  </div>
</template>

<script setup lang="ts">
import { useOrgStore } from '~/stores/org'

const org = useOrgStore()
const route = useRoute()

const items = computed(() => {
  const base = `/orgs/${org.activeSlug}`
  return [
    { label: 'Overview', to: base },
    { label: 'Groups', to: `${base}/groups` },
    { label: 'Projects', to: `${base}/projects` },
    { label: 'Members', to: `${base}/members` },
  ]
})

function isActive(to: string): boolean {
  return route.path === to || (to !== `/orgs/${org.activeSlug}` && route.path.startsWith(to))
}
</script>
