<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Services</h1>

    <div v-if="status.loading" class="py-12 text-center text-muted">
      <UIcon name="i-lucide-loader-circle" class="mx-auto size-7 animate-spin text-primary" />
      <p class="mt-3">Checking platform services...</p>
    </div>

    <UAlert
      v-else-if="status.lastError"
      color="error"
      variant="soft"
      :description="status.lastError"
    >
      <template #actions>
        <UButton color="error" variant="soft" @click="status.refresh()">Retry</UButton>
      </template>
    </UAlert>

    <div v-else>
      <UCard class="mb-4">
        <h3 class="font-semibold">Overall</h3>
        <p class="mt-2 flex items-center gap-2">
          <span class="status-dot" :class="`status-${status.state}`"></span>
          <span :class="`status-${status.state}`" class="capitalize">{{ status.state }}</span>
          <span class="ml-3 text-[13px] text-muted">Last checked {{ formatTime(checkedAt) }}</span>
        </p>
      </UCard>

      <UCard>
        <UTable :data="status.services" :columns="columns">
          <template #name-cell="{ row }">
            <code>{{ row.original.name }}</code>
          </template>
          <template #state-cell="{ row }">
            <UBadge v-if="!row.original.configured" color="info" variant="soft">not configured</UBadge>
            <template v-else>
              <span class="status-dot" :class="`status-${row.original.state}`" :title="row.original.state"></span>
              <span :class="`status-${row.original.state}`" class="ml-2 capitalize">{{ row.original.state }}</span>
            </template>
          </template>
          <template #message-cell="{ row }">
            <span class="text-muted">{{ row.original.message || '—' }}</span>
          </template>
        </UTable>
      </UCard>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useStatusStore } from '~/stores/status'

const status = useStatusStore()

const columns = [
  { id: 'name', accessorKey: 'name', header: 'Service' },
  { id: 'category', accessorKey: 'category', header: 'Category' },
  { id: 'state', accessorKey: 'state', header: 'State' },
  { id: 'message', accessorKey: 'message', header: 'Details' },
]

const checkedAt = computed(() => status.services[0]?.checked_at ?? '')

// The layout owns the polling lifecycle (start/stop via the auth watcher).
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
