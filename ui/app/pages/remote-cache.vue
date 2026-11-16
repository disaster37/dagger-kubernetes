<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Remote Cache</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" />
    <UCard v-if="stats">
      <div class="grid grid-cols-3 gap-4">
        <div><p class="text-[13px] text-muted">Namespace</p><p class="font-semibold">{{ stats.namespace }}</p></div>
        <div><p class="text-[13px] text-muted">Entries</p><p class="font-semibold">{{ stats.entries }}</p></div>
        <div><p class="text-[13px] text-muted">Bytes</p><p class="font-semibold">{{ stats.bytes }}</p></div>
      </div>
    </UCard>
  </div>
</template>

<script setup lang="ts">
import api from '~/api/client'

const stats = ref<any>(null)
const error = ref('')

onMounted(async () => {
  try {
    const { data } = await api.get('/api/v1/remote-cache')
    stats.value = data
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load remote cache stats'
  }
})
</script>
