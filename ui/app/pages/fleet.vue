<template>
  <div>
    <h1 class="mb-6 text-2xl font-semibold">Runner Fleet</h1>

    <div v-if="loading" class="py-12 text-center text-muted">
      <UIcon name="i-lucide-loader-circle" class="mx-auto size-7 animate-spin text-primary" />
      <p class="mt-3">Loading fleet...</p>
    </div>

    <UAlert v-else-if="error" color="error" variant="soft" :description="error">
      <template #actions>
        <UButton color="error" variant="soft" @click="load">Retry</UButton>
      </template>
    </UAlert>

    <UEmpty
      v-else-if="fleet.length === 0"
      icon="i-lucide-server-off"
      title="No engine fleets deployed"
      description="Run a Dagger pipeline to auto-provision engines."
    />

    <div v-else>
      <UCard v-for="version in fleet" :key="version.version" class="mb-4">
        <div class="flex items-center justify-between gap-3">
          <h3 class="font-semibold">{{ version.version }}</h3>
          <UButton
            v-if="auth.isAdmin"
            color="neutral"
            variant="outline"
            :loading="purging[version.version]"
            @click="askPurge(version.version)"
          >
            {{ purging[version.version] ? 'Purging…' : 'Purge local cache' }}
          </UButton>
        </div>
        <p class="text-[13px] text-muted">
          {{ version.readyReplicas }}/{{ version.replicas }} ready
        </p>

        <UAlert
          v-if="purgeResults[version.version]"
          class="mt-2"
          color="neutral"
          variant="soft"
          :description="purgeSummary(purgeResults[version.version]!)"
        >
          <template v-if="failedPods(purgeResults[version.version]!).length" #description>
            <p>{{ purgeSummary(purgeResults[version.version]!) }}</p>
            <ul class="mt-1 ml-4 list-disc">
              <li v-for="pod in failedPods(purgeResults[version.version]!)" :key="pod.pod_name">
                <code>{{ pod.pod_name }}</code>: {{ pod.error }}
              </li>
            </ul>
          </template>
        </UAlert>

        <UAlert
          v-if="purgeErrors[version.version]"
          class="mt-2"
          color="error"
          variant="soft"
          :description="purgeErrors[version.version]"
        />

        <UTable class="mt-3" :data="version.ordinals" :columns="podColumns">
          <template #name-cell="{ row }">
            <code>{{ row.original.name }}</code>
          </template>
          <template #ready-cell="{ row }">
            <UBadge :color="row.original.ready ? 'success' : 'error'" variant="soft">
              {{ row.original.ready ? 'Ready' : 'Down' }}
            </UBadge>
          </template>
          <template #startedAt-cell="{ row }">
            {{ formatTime(row.original.startedAt) }}
          </template>
        </UTable>
      </UCard>
    </div>

    <UModal v-model:open="confirmOpen" title="Purge local cache">
      <template #body>
        <p>{{ confirmMessage }}</p>
      </template>
      <template #footer>
        <div class="flex justify-end gap-2">
          <UButton color="neutral" variant="outline" @click="confirmOpen = false">Cancel</UButton>
          <UButton color="error" @click="runConfirmed">Confirm</UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>

<script setup lang="ts">
import { fetchFleetInfo, purgeEngineCache } from '~/api/client'
import { useAuthStore } from '~/stores/auth'
import type { EngineCachePurgeResult, EnginePodPurgeResult, FleetInfo } from '~/api/types'

const REFRESH_MS = 10_000

const auth = useAuthStore()
const fleet = ref<FleetInfo[]>([])
const loading = ref(true)
const error = ref<string | null>(null)
const purging = ref<Record<string, boolean>>({})
const purgeResults = ref<Record<string, EngineCachePurgeResult>>({})
const purgeErrors = ref<Record<string, string>>({})
let timer: number | undefined
let loadingInFlight = false

const confirmOpen = ref(false)
const confirmMessage = ref('')
let confirmAction: (() => void) | null = null

const podColumns = [
  { id: 'name', accessorKey: 'name', header: 'Pod' },
  { id: 'ordinal', accessorKey: 'ordinal', header: 'Ordinal' },
  { id: 'ready', accessorKey: 'ready', header: 'Status' },
  { id: 'pinnedSessions', accessorKey: 'pinnedSessions', header: 'Sessions' },
  { id: 'startedAt', accessorKey: 'startedAt', header: 'Uptime' },
]

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

function askPurge(version: string): void {
  confirmMessage.value = `Purge the local BuildKit cache on every ${version} engine pod? Running pipelines are not interrupted.`
  confirmAction = () => purge(version)
  confirmOpen.value = true
}

function runConfirmed() {
  confirmOpen.value = false
  confirmAction?.()
  confirmAction = null
}

async function purge(version: string): Promise<void> {
  purging.value = { ...purging.value, [version]: true }
  purgeErrors.value = { ...purgeErrors.value, [version]: '' }
  try {
    const result = await purgeEngineCache(version)
    purgeResults.value = { ...purgeResults.value, [version]: result }
    void load()
  } catch (e) {
    const err = e as { response?: { data?: { message?: string } } }
    purgeErrors.value = { ...purgeErrors.value, [version]: err.response?.data?.message || 'Purge failed' }
  } finally {
    purging.value = { ...purging.value, [version]: false }
  }
}

function purgeSummary(result: EngineCachePurgeResult): string {
  const pruned = result.pods.filter((p) => p.pruned).length
  return `Pruned ${pruned}/${result.pods.length} pods`
}

function failedPods(result: EngineCachePurgeResult): EnginePodPurgeResult[] {
  return result.pods.filter((p) => p.error)
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
