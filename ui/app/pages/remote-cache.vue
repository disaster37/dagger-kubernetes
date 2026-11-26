<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Remote Cache</h1>
    <UAlert v-if="error" color="error" variant="soft" :description="error" class="mb-4" />
    <UAlert v-if="stats && !stats.enabled" color="neutral" variant="soft" description="Remote cache is disabled (cache.remote.enabled: false)." class="mb-4" />
    <UCard>
      <div class="mb-4 flex flex-wrap items-end gap-3">
        <UFormField label="Organization">
          <span class="text-sm font-medium">{{ org.activeOrg?.display_name || org.activeSlug || 'none' }}</span>
        </UFormField>
        <UButton @click="load">Refresh</UButton>
        <UButton v-if="auth.isAdmin" color="error" variant="soft" @click="showPurge = true">Purge cache</UButton>
      </div>
      <div v-if="stats && stats.enabled" class="grid grid-cols-3 gap-4">
        <div><p class="text-[13px] text-muted">Namespace</p><p class="font-semibold">{{ stats.namespace }}</p></div>
        <div><p class="text-[13px] text-muted">Entries</p><p class="font-semibold">{{ stats.entries }}</p></div>
        <div><p class="text-[13px] text-muted">Size</p><p class="font-semibold">{{ formatBytes(stats.bytes || 0) }}</p></div>
      </div>
    </UCard>

    <UModal v-model:open="showPurge">
      <UCard>
        <template #header><h2 class="font-semibold">Purge {{ namespace }}?</h2></template>
        <p class="text-[13px] text-muted">This deletes every cached object and index entry in the namespace.</p>
        <div class="mt-4 flex justify-end gap-2">
          <UButton variant="soft" @click="showPurge = false">Cancel</UButton>
          <UButton color="error" @click="purge">Purge</UButton>
        </div>
      </UCard>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { fetchRemoteCacheStats, purgeRemoteCache } from '~/api/client'
import type { RemoteCacheStats } from '~/api/types'
import { useAuthStore } from '~/stores/auth'
import { useOrgStore } from '~/stores/org'

const auth = useAuthStore()
const org = useOrgStore()
const namespace = ref('')
const stats = ref<RemoteCacheStats | null>(null)
const error = ref('')
const showPurge = ref(false)

function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MiB`
  return `${(bytes / 1024 / 1024 / 1024).toFixed(1)} GiB`
}
async function load() {
  error.value = ''
  namespace.value = org.activeSlug
  if (!namespace.value) {
    stats.value = null
    return
  }
  try {
    stats.value = await fetchRemoteCacheStats(namespace.value)
  } catch (e: any) {
    error.value = e?.response?.data?.message ?? 'Failed to load remote cache stats'
  }
}
async function purge() {
  await purgeRemoteCache(namespace.value)
  showPurge.value = false
  await load()
}

onMounted(load)
</script>
