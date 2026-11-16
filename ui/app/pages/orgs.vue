<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Organizations</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" />
    <UCard v-for="org in orgs" :key="org.id" class="mb-3">
      <div class="flex items-center justify-between">
        <div>
          <h3 class="font-semibold">{{ org.display_name || org.name }}</h3>
          <p class="text-[13px] text-muted">{{ org.name }} · {{ org.features.join(', ') }}</p>
        </div>
        <UBadge color="neutral" variant="soft">{{ org.id }}</UBadge>
      </div>
    </UCard>
    <UEmpty v-if="!orgs.length && !error" icon="i-lucide-building-2" title="No organizations" description="Create one via the API or Helm bootstrap." />
  </div>
</template>

<script setup lang="ts">
import api from '~/api/client'

const orgs = ref<any[]>([])
const error = ref('')

onMounted(async () => {
  try {
    const { data } = await api.get('/api/v1/orgs')
    orgs.value = data
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load organizations'
  }
})
</script>
